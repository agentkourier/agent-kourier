package crsource

import (
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// objectJSON is the size of the objects that the cost tests build: under the 1.5 MiB that etcd takes in one request by default.
const objectJSON = 1400 * 1024

// liveHeap is the heap in use after a collection.
func liveHeap() uint64 {
	for range 3 {
		runtime.GC()
	}
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// heldPerObject decodes n copies of body as the informer's cache holds an object, and applies slim to each if transform is set, and
// returns the live heap that the copies cost, in MiB for each.
func heldPerObject(t *testing.T, body []byte, transform bool, n int) float64 {
	t.Helper()
	before := liveHeap()
	held := make([]*unstructured.Unstructured, n)
	for i := range held {
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(body); err != nil {
			t.Fatal(err)
		}
		if transform {
			if _, err := slim(u); err != nil {
				t.Fatal(err)
			}
		}
		held[i] = u
	}
	after := liveHeap()
	runtime.KeepAlive(held)
	return float64(int64(after)-int64(before)) / float64(n) / (1 << 20)
}

// What the cache holds of an object whose size is in what slim drops. The bounds on the spec hold an object far under the size the API
// server takes (see objectsize_test.go), and nothing bounds the labels and annotations of its metadata, so an object can be as large as
// the API server allows with them. The figures are logged and the page "Security" states them; the bound asserted is generous, so that a
// run on a loaded machine or under the race detector passes, and fails if labels and annotations are held again.
func TestWhatTheCacheHoldsOfAnObjectWhoseSizeIsItsLabelsAndAnnotations(t *testing.T) {
	const copies = 16
	labels := map[string]any{}
	for i := 0; len(labels)*14 < objectJSON-300*1024; i++ {
		labels[fmt.Sprintf("l%07d", i)] = ""
	}
	annotations := map[string]any{"team.example/note": strings.Repeat("x", 200*1024)}
	body, err := json.Marshal(map[string]any{"apiVersion": "agentkourier.dev/v1alpha1", "kind": "AgentBackend",
		"metadata": map[string]any{"namespace": "tenant", "name": "wide", "uid": "u1", "generation": 1, "labels": labels, "annotations": annotations},
		"spec":     map[string]any{"dialect": "a2a", "url": "https://agent.example"}})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("%s, GOMAXPROCS %d of %d CPUs; %d objects held at once", runtime.Version(), runtime.GOMAXPROCS(0), runtime.NumCPU(), copies)
	// Slimmed first, so that the garbage of the larger measurement is not in the baseline of the smaller.
	slimmed := heldPerObject(t, body, true, copies)
	kept := heldPerObject(t, body, false, copies)
	t.Logf("%.2f MiB of JSON, %d labels and 200 KiB of annotations: %.3f MiB held for each object, was %.2f MiB when labels and annotations were kept",
		float64(len(body))/(1<<20), len(labels), slimmed, kept)
	if slimmed > 1 {
		t.Errorf("an object whose size is labels and annotations costs %.2f MiB; want under 1 MiB, since slim drops them", slimmed)
	}
}
