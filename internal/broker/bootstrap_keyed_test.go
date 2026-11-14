package broker

import (
	"slices"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// Bootstrap follows each Secret with the keys that the config reads, on both routes, so the Reloader's first Watch finds exactly what
// was followed. Followed with every key instead, a Secret is listed a second time when the Reloader narrows it to the keys it reads, so
// each Secret that the config names must have been listed once when the settling time has passed.
func TestBootstrapFollowsEachSecretWithTheKeysTheConfigReads(t *testing.T) {
	t.Parallel()
	for name, start := range map[string]func(t *testing.T, kube *fake.Clientset) *Config{
		"files": func(t *testing.T, kube *fake.Clientset) *Config {
			c, err := Bootstrap(t.Context(), kube, options(configDir(t)))
			if err != nil {
				t.Fatal(err)
			}
			return c
		},
		"custom resources": func(t *testing.T, kube *fake.Clientset) *Config {
			c, err := Bootstrap(t.Context(), kube, crOptions(crCluster(t, nil)))
			if err != nil {
				t.Fatal(err)
			}
			return c
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			kube := fake.NewClientset(
				secretOf("agent-kourier-system", "slack-main", "botToken", "appToken", "unread"),
				secretOf("sre", "agent-kourier-sre-alerts-oidc", "token", "unread"),
			)
			var mu sync.Mutex
			lists := map[string]int{}
			kube.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
				sel, _ := a.(k8stesting.ListAction).GetListRestrictions().Fields.RequiresExactMatch("metadata.name")
				mu.Lock()
				lists[a.GetNamespace()+"/"+sel]++
				mu.Unlock()
				return false, nil, nil
			})
			c := start(t, kube)
			t.Cleanup(c.Stop)

			time.Sleep(500 * time.Millisecond) // a narrowing would list again within the resolver's 20 ms poll and one list
			mu.Lock()
			defer mu.Unlock()
			for _, n := range []string{"agent-kourier-system/slack-main", "sre/agent-kourier-sre-alerts-oidc"} {
				if lists[n] != 1 {
					t.Errorf("%s was listed %d times at start; want once, with the keys the config reads", n, lists[n])
				}
			}
			keys, err := c.Secrets.Keys(t.Context(), "agent-kourier-system", "slack-main")
			if err != nil || !slices.Equal(keys, []string{"appToken", "botToken"}) {
				t.Errorf("Keys of the connection's Secret = %v, %v; want the two it reads and not the one it does not", keys, err)
			}
		})
	}
}
