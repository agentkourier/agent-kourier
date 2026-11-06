package crsource_test

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// This file measures what a broker holds, beyond the informer's cache, for each of the largest Bindings that Validate accepts:
// the figures that docs/explanation/security.md and the object-count row of docs/reference/limits.md state. Like
// TestLargestObjectsAreWhatTheSecurityPageSays it logs them (run it with -v) and asserts bounds that are wide enough for the noise
// of a heap measurement and tight enough that a change that doubles a figure fails.

func retainLiveHeap() uint64 {
	for range 3 {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// clusterObject is obj as the informer holds it: the JSON it marshals to, decoded and slimmed, with a uid and generation 1.
func clusterObject(t *testing.T, obj any, uid string) *unstructured.Unstructured {
	t.Helper()
	body, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	u := &unstructured.Unstructured{}
	if err := u.UnmarshalJSON(body); err != nil {
		t.Fatal(err)
	}
	u.SetUID(types.UID(uid))
	u.SetGeneration(1)
	u.SetResourceVersion("1")
	crsource.SlimForTest(u)
	return u
}

// largestOwnedBinding is the largest Binding as the cluster holds it: number i of them, on the broker's own connection and on the
// backend, each on a channel of its own so that every one is served.
func largestOwnedBinding(t *testing.T, i, generation int) *unstructured.Unstructured {
	t.Helper()
	b := largestBinding(t, 50)
	b.Name = fmt.Sprintf("big-%02d", i)
	b.Spec.Chat.Channel = fmt.Sprintf("C%031d", i)
	b.Spec.Chat.ConnectionRef = own
	b.Spec.Agent.BackendRef = v1alpha1.ObjectRef{Namespace: "tenant-backends", Name: "agent"}
	b.Default()
	if err := b.Validate(); err != nil {
		t.Fatalf("the largest Binding is refused: %.400v", err)
	}
	u := clusterObject(t, b, fmt.Sprintf("uid-big-%d", i))
	u.SetGeneration(int64(generation))
	return u
}

// What a broker holds of the largest Bindings once it has read them, for each, and what a reload adds. The informer's own copy of an
// object is measured by TestLargestObjectsAreWhatTheSecurityPageSays and is not counted here: this is what the source's decoded
// object, the Snapshot and the trigger table that the intake compiles hold on top of it. The figures are of a test process. Every
// Binding is owned, as one that names no connection that exists is by every broker, and costs each of them this.
func TestWhatABrokerHoldsForEachLargestOwnedBinding(t *testing.T) {
	leakCheck(t)
	const n = 6
	allowed := []string{strings.Repeat("a", 63)}
	backend := &v1alpha1.AgentBackend{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "AgentBackend"},
		ObjectMeta: metav1.ObjectMeta{Namespace: "tenant-backends", Name: "agent"},
		Spec:       v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: "https://agent.example", AllowedNamespaces: allowed}}
	conn := &v1alpha1.ChatConnection{TypeMeta: metav1.TypeMeta{APIVersion: v1alpha1.APIVersion, Kind: "ChatConnection"},
		ObjectMeta: metav1.ObjectMeta{Namespace: own.Namespace, Name: own.Name},
		Spec:       v1alpha1.ChatConnectionSpec{Platform: "slack", CredentialsSecretRef: v1alpha1.SecretRef{Name: "creds"}, AllowedNamespaces: allowed}}
	objs := []*unstructured.Unstructured{clusterObject(t, backend, "uid-backend"), clusterObject(t, conn, "uid-conn")}
	for i := range n {
		objs = append(objs, largestOwnedBinding(t, i, 1))
	}
	c := newCluster(t, objs...)
	src := c.start(t)

	// The baseline holds the fake API server's copy of each object and the informer's, which are not the broker's own.
	h0 := retainLiveHeap()
	start := time.Now()
	read := func() *config.Snapshot {
		s, _, err := src.Set(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		snap, rejections := config.ResolvePartial(s)
		if len(snap.Bindings()) != n || len(rejections) != 0 {
			t.Fatalf("the Snapshot serves %d of the %d Bindings, rejections %+v", len(snap.Bindings()), n, rejections)
		}
		return snap
	}
	snap := read()
	firstRead := time.Since(start)
	start = time.Now()
	read()
	secondRead := time.Since(start)
	h1 := retainLiveHeap()

	var h2 uint64
	func() { // the tables are released when it returns: they hold the Binding they were compiled from, which the reload below replaces
		tables := make([]*chat.Replay, 0, n)
		for _, rb := range snap.Bindings() {
			r, err := chat.NewReplay(rb.Binding)
			if err != nil {
				t.Fatal(err)
			}
			tables = append(tables, r)
		}
		h2 = retainLiveHeap()
		runtime.KeepAlive(tables)
	}()

	// Every Binding is edited, which is the most that one reload can find changed. The Snapshot of the read before is still held
	// (by a session that has not finished with it) while the new one is made, and then released.
	for i := range n {
		edited := largestOwnedBinding(t, i, 2)
		until(t, func() { c.apply(edited) }, func() bool {
			return src.CachedGenerationForTest(config.KindBinding, edited.GetNamespace(), edited.GetName()) == 2
		})
	}
	start = time.Now()
	snap2 := read()
	reloadRead := time.Since(start)
	h3 := retainLiveHeap()
	runtime.KeepAlive(snap)
	runtime.KeepAlive(snap2)
	h4 := retainLiveHeap()
	runtime.KeepAlive(snap2)

	mib := func(a, b uint64) float64 { return float64(int64(b)-int64(a)) / n / (1 << 20) }
	decoded, tableCost, reload := mib(h0, h1), mib(h1, h2), mib(h4, h3)
	t.Logf("for each of %d largest owned Bindings: the decoded object and the Snapshot hold %.2f MiB, the trigger table %.2f MiB; while a reload that found it edited holds the Snapshot before it too, that one holds %.2f MiB more",
		n, decoded, tableCost, reload)
	t.Logf("a read of the %d Bindings took %s the first time (%s for each), %s when none had changed, and %s when all had (under -race these are several times slower)",
		n, firstRead.Round(time.Millisecond), (firstRead / n).Round(time.Millisecond), secondRead.Round(time.Microsecond), reloadRead.Round(time.Millisecond))
	if decoded < 1 || decoded > 2.5 {
		t.Errorf("the decoded object and the Snapshot hold %.2f MiB for each Binding; want 1 to 2.5, one typed copy and no more", decoded)
	}
	if tableCost < 0.2 || tableCost > 1.5 {
		t.Errorf("the trigger table holds %.2f MiB for each Binding; want 0.2 to 1.5", tableCost)
	}
	if reload < 0.8 || reload > 2.5 {
		t.Errorf("a reload that finds a Binding edited adds %.2f MiB; want 0.8 to 2.5, one more typed copy", reload)
	}
}

// typedMiB decodes body n times as the source does (config.DecodeObject) and returns what each decoded object holds, in MiB.
func typedMiB[T any, PT interface {
	*T
	Default()
	Validate() error
}](t *testing.T, body []byte, n int) float64 {
	t.Helper()
	before := retainLiveHeap()
	keep := make([]*T, n)
	for i := range keep {
		obj, _, errs := config.DecodeObject[T, PT](body)
		if obj == nil {
			t.Fatalf("refused: %v", errs)
		}
		keep[i] = obj
	}
	after := retainLiveHeap()
	runtime.KeepAlive(keep)
	return float64(int64(after)-int64(before)) / float64(n) / (1 << 20)
}

// An AgentBackend of the cluster is decoded whether or not a Binding names it, and the source keeps what it decoded, so the largest
// one costs its decoded copy on top of the informer's: this is what that copy holds.
func TestWhatTheDecodedLargestAgentBackendHolds(t *testing.T) {
	body, err := json.Marshal(largestBackend())
	if err != nil {
		t.Fatal(err)
	}
	held := typedMiB[v1alpha1.AgentBackend](t, body, 100)
	t.Logf("the largest AgentBackend decoded, as the source keeps it: %.3f MiB", held)
	if held < 0.05 || held > 0.5 {
		t.Errorf("the decoded largest AgentBackend holds %.3f MiB; want 0.05 to 0.5", held)
	}
}

// A writer of a Binding's status (a grant on bindings/status) can fill it up to the CRD's bounds: 16 conditions with a message of up
// to 32768 bytes. The informer holds it with the rest of the object, and the decoded Binding does not (see decodeCached). This
// measures what a full status adds to an object in the informer's cache.
func TestWhatAFullBindingStatusAddsToTheCache(t *testing.T) {
	const copies = 40
	conditions := make([]any, 16)
	for i := range conditions {
		conditions[i] = map[string]any{"type": fmt.Sprintf("T%02d", i), "status": "True", "reason": "R", "message": strings.Repeat("m", 32768),
			"lastTransitionTime": "2026-11-04T12:00:00Z", "observedGeneration": int64(1)}
	}
	object := func(status map[string]any) map[string]any {
		o := map[string]any{"apiVersion": v1alpha1.APIVersion, "kind": "Binding",
			"metadata": map[string]any{"namespace": "tenant", "name": "b", "uid": "u1", "generation": int64(1)}, "spec": map[string]any{}}
		if status != nil {
			o["status"] = status
		}
		return o
	}
	held := func(obj map[string]any) float64 {
		body, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		return sizeHeldMiB(t, body, copies)
	}
	without, with := held(object(nil)), held(object(map[string]any{"conditions": conditions}))
	t.Logf("a Binding with no status holds %.3f MiB in the cache and one with 16 conditions of 32768-byte messages %.3f MiB: a full status adds %.3f MiB (%.0f KiB)",
		without, with, with-without, (with-without)*1024)
	if add := with - without; add < 0.4 || add > 1.2 {
		t.Errorf("a full status adds %.3f MiB to the cached Binding; want 0.4 to 1.2", add)
	}
}
