package broker

import (
	"slices"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
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

// On the custom resources, Bootstrap follows the own connection's Secret before the Reloader runs, with the keys of the connection's mode: those
// of Socket Mode when the mode is unset or socket, and those of HTTP mode for http. Followed with another mode's keys, the Secret would be listed
// a second time when the Reloader narrows it to the keys the config reads.
func TestBootstrapFollowsTheOwnConnectionsSecretWithTheKeysOfItsMode(t *testing.T) {
	t.Parallel()
	for mode, want := range map[string][]string{"": {"appToken", "botToken"}, "socket": {"appToken", "botToken"}, "http": {"botToken", "signingSecret"}} {
		t.Run("mode "+mode, func(t *testing.T) {
			t.Parallel()
			kube := fake.NewClientset(
				secretOf("agent-kourier-system", "slack-main", "botToken", "appToken", "signingSecret", "unread"),
				secretOf("sre", "agent-kourier-sre-alerts-oidc", "token"),
			)
			var mu sync.Mutex
			lists := 0
			kube.PrependReactor("list", "secrets", func(a k8stesting.Action) (bool, runtime.Object, error) {
				if sel, _ := a.(k8stesting.ListAction).GetListRestrictions().Fields.RequiresExactMatch("metadata.name"); sel == "slack-main" {
					mu.Lock()
					lists++
					mu.Unlock()
				}
				return false, nil, nil
			})
			dyn := crCluster(t, func(objs []*unstructured.Unstructured) {
				for _, o := range objs {
					if o.GetKind() == "ChatConnection" && mode != "" {
						if err := unstructured.SetNestedField(o.Object, mode, "spec", "slack", "mode"); err != nil {
							t.Fatal(err)
						}
					}
				}
			})
			c, err := Bootstrap(t.Context(), kube, crOptions(dyn))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(c.Stop)

			time.Sleep(500 * time.Millisecond) // a narrowing would list again within the resolver's 20 ms poll and one list
			mu.Lock()
			defer mu.Unlock()
			if lists != 1 {
				t.Errorf("the connection's Secret was listed %d times at start; want once, with the keys of its mode", lists)
			}
			keys, err := c.Secrets.Keys(t.Context(), "agent-kourier-system", "slack-main")
			if err != nil || !slices.Equal(keys, want) {
				t.Errorf("Keys of the connection's Secret = %v, %v; want %v, the keys of mode %q", keys, err, want, mode)
			}
		})
	}
}
