package broker

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/watch"
	k8stesting "k8s.io/client-go/testing"
)

// A running broker keeps its own connection when it is deleted and made again invalid, which is another object with a new
// uid: its Bindings stay served, and a replica that takes the lease later finds the connection and its tokens, where
// ChooseConnection would otherwise fail and the takeover exit 2.
func TestBootstrapCRDsKeepsTheOwnConnectionWhenItIsMadeAgainInvalid(t *testing.T) {
	dyn := crCluster(t, nil)
	c, err := Bootstrap(t.Context(), cluster(), crOptions(dyn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)

	res := dyn.Resource(crConnections).Namespace(testConnection.Namespace)
	// The informers' watches may not be open yet, and the fake delivers only to an open one: repeat until it is seen.
	bad := crObject(t, "chatconnection.yaml")
	bad.SetUID("uid-recreated")
	_ = unstructured.SetNestedField(bad.Object, "teams", "spec", "platform")
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := res.Delete(t.Context(), testConnection.Name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			t.Fatal(err)
		}
		if _, err := res.Create(t.Context(), bad.DeepCopy(), metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
		inv := c.Reloader.Report().Invalid
		if len(inv) == 1 && inv[0].Kind == "ChatConnection" && !inv[0].Deleted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the refused connection was not reported in 10s: %+v", inv)
		}
	}

	if n := len(c.Reloader.Current().Bindings()); n != 2 {
		t.Errorf("%d Bindings served; want both, on the connection that was kept", n)
	}
	conn, err := ChooseConnection(t.Context(), c.Reloader.Current(), c.Secrets)
	if err != nil {
		t.Fatalf("ChooseConnection = %v; a takeover would exit with it", err)
	}
	if conn.Ref != testConnection || conn.Platform != "slack" {
		t.Errorf("connection = %+v; want the one that was kept, %v on slack", conn, testConnection)
	}
}

// With the API server's rights revoked while the broker runs, the informers keep their caches, which is right, and the
// config is reported stale (agentkourier_config_stale reads Config.Stale), where the files' Reloader has nothing that makes
// it so.
func TestBootstrapCRDsIsStaleWhileAnInformerCannotWatch(t *testing.T) {
	dyn := crCluster(t, nil)
	var opened atomic.Int32
	dyn.PrependWatchReactor("bindings", func(k8stesting.Action) (bool, watch.Interface, error) {
		if opened.Add(1) > 1 { // the first watch is the start's own probe
			return true, nil, apierrors.NewForbidden(crBindings.GroupResource(), "", errors.New("RBAC revoked"))
		}
		return false, nil, nil
	})
	c, err := Bootstrap(t.Context(), cluster(), crOptions(dyn))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Stop)
	eventually(t, 10*time.Second, "the config was never reported stale", c.Stale)
	if n := len(c.Reloader.Current().Bindings()); n != 2 {
		t.Errorf("%d Bindings served while stale; want both, from the cache that was kept", n)
	}
}
