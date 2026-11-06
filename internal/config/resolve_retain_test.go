package config

import (
	"fmt"
	"runtime"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

// retainHeap is the live heap after collections.
func retainHeap() uint64 {
	for range 3 {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// retainBinding is a Binding on connection conn and backend backend that holds about 1 MiB of its own, which stands for the
// triggers of a large one. Each call allocates the payload again.
func retainBinding(i int, conn, backend string) v1alpha1.Binding {
	b := v1alpha1.Binding{ObjectMeta: metav1.ObjectMeta{Namespace: "team", Name: fmt.Sprintf("b%d", i)}}
	b.Spec.Agent.BackendRef = v1alpha1.ObjectRef{Namespace: "kourier", Name: backend}
	b.Spec.Chat.ConnectionRef = v1alpha1.ObjectRef{Namespace: "kourier", Name: conn}
	b.Spec.Chat.Channel = fmt.Sprintf("C%010d", i)
	b.Spec.Chat.Triggers = []v1alpha1.Trigger{{PromptTemplate: strings.Repeat("x", 1<<20)}}
	return b
}

// retainedMiB resolves a Set of served Bindings on the connection that exists and refused Bindings on one that does not, with
// a MiB-sized AgentBackend that the served ones name and unusedBackends more that none does, then drops the Set and returns
// what the Snapshot alone keeps alive, and the Snapshot.
func retainedMiB(t *testing.T, served, refused, unusedBackends int) (float64, *Snapshot) {
	t.Helper()
	before := retainHeap()
	set := &Set{}
	set.ChatConnections = []v1alpha1.ChatConnection{{ObjectMeta: metav1.ObjectMeta{Namespace: "kourier", Name: "slack"}}}
	set.ChatConnections[0].Spec.AllowedNamespaces = []string{"team"}
	backend := func(name string) v1alpha1.AgentBackend {
		be := v1alpha1.AgentBackend{ObjectMeta: metav1.ObjectMeta{Namespace: "kourier", Name: name}}
		be.Spec.Dialect, be.Spec.URL, be.Spec.AllowedNamespaces = v1alpha1.DialectA2A, "https://"+strings.Repeat("a", 1<<20), []string{"team"}
		return be
	}
	set.AgentBackends = append(set.AgentBackends, backend("used"))
	for i := range unusedBackends {
		set.AgentBackends = append(set.AgentBackends, backend(fmt.Sprintf("unused%d", i)))
	}
	for i := range served {
		set.Bindings = append(set.Bindings, retainBinding(i, "slack", "used"))
	}
	for i := range refused {
		set.Bindings = append(set.Bindings, retainBinding(100+i, "missing", "used"))
	}
	snap, rejections := ResolvePartial(set)
	set = nil
	if len(snap.Bindings()) != served || len(rejections) != refused {
		t.Fatalf("serves %d, rejects %d; want %d and %d", len(snap.Bindings()), len(rejections), served, refused)
	}
	after := retainHeap()
	return float64(int64(after)-int64(before)) / (1 << 20), snap
}

// The Snapshot keeps what it serves and nothing else: a Binding that is rejected, and an AgentBackend that no served Binding
// names, are not kept alive by it. It used to point into the Set's slices, which kept every object of the Set, whether it was
// served or not.
func TestTheSnapshotRetainsOnlyWhatItServes(t *testing.T) {
	alone, snapA := retainedMiB(t, 1, 0, 0)
	runtime.KeepAlive(snapA)
	with, snapB := retainedMiB(t, 1, 4, 4)
	runtime.KeepAlive(snapB)
	t.Logf("one served Binding and its backend: Snapshot keeps %.2f MiB; with 4 refused Bindings and 4 unused backends in the Set: %.2f MiB", alone, with)
	if alone < 1.5 || alone > 3 {
		t.Fatalf("the measurement is off: one served Binding and its backend keep %.2f MiB, want about 2", alone)
	}
	if with > alone+0.5 {
		t.Errorf("the Snapshot keeps %.2f MiB with 4 refused Bindings and 4 unused backends in the Set, %.2f MiB without; want the same", with, alone)
	}
}
