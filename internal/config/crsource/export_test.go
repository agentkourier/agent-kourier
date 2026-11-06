package crsource

import (
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// SetClockForTest replaces the clock that dates a failed list or watch, which Stale measures against.
func (s *Source) SetClockForTest(now func() time.Time) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	s.now = now
}

// SetProbeForTest sets the pace and the timeout of the probe, before Start.
func (s *Source) SetProbeForTest(every, timeout time.Duration) {
	s.probeEvery, s.probeTimeout = every, timeout
}

// CachedGenerationForTest returns the metadata.generation of the object of the kind ("Binding", "AgentBackend" or
// "ChatConnection") that an informer's cache holds now, and -1 if it holds none: a signal that the informer has seen a change,
// which the source's own reads cannot give without consuming it.
func (s *Source) CachedGenerationForTest(kind, namespace, name string) int64 {
	s.mu.Lock()
	stores := s.stores
	s.mu.Unlock()
	store := map[string]int{"Binding": 0, "AgentBackend": 1, "ChatConnection": 2}[kind]
	if stores[store] == nil {
		return -1
	}
	for _, it := range stores[store].List() {
		if u, ok := it.(*unstructured.Unstructured); ok && u.GetNamespace() == namespace && u.GetName() == name {
			return u.GetGeneration()
		}
	}
	return -1
}

// CachedObjectForTest returns the object of the kind that an informer's cache holds now, and nil if it holds none. It is the cache's
// own object: a test reads it and changes nothing.
func (s *Source) CachedObjectForTest(kind, namespace, name string) *unstructured.Unstructured {
	s.mu.Lock()
	stores := s.stores
	s.mu.Unlock()
	store := map[string]int{"Binding": 0, "AgentBackend": 1, "ChatConnection": 2}[kind]
	if stores[store] == nil {
		return nil
	}
	for _, it := range stores[store].List() {
		if u, ok := it.(*unstructured.Unstructured); ok && u.GetNamespace() == namespace && u.GetName() == name {
			return u
		}
	}
	return nil
}

// LastGoodIsCachedForTest says whether the last good generation of the object is the very object that the informer's cache holds, so
// that holding it costs nothing more than the cache does.
func (s *Source) LastGoodIsCachedForTest(kind, namespace, name string) bool {
	cached := s.CachedObjectForTest(kind, namespace, name)
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.good[key{kind, namespace, name}]
	return ok && cached != nil && g.obj == cached
}

// SetProbeExitHookForTest sets a function that the probe's goroutine calls as it ends, before Start. A test that makes it slow
// can tell whether Stop waited for the goroutine, which a goroutine that ends at once cannot show.
func (s *Source) SetProbeExitHookForTest(f func()) { s.probeExited = f }

// SlimForTest applies the informers' transform to u, as the cache does to each object before it holds it.
func SlimForTest(u *unstructured.Unstructured) {
	_, _ = slim(u)
}

// DecodesForTest counts the objects that Set has decoded and validated: a read that finds an object as it decoded it last does not
// count it.
func (s *Source) DecodesForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decodes
}

// DecodedForTest returns the decoded object (a pointer to the type of the kind) that the source remembers for the object, and nil if
// it remembers none or the object was refused.
func (s *Source) DecodedForTest(kind, namespace, name string) any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.decoded[key{kind, namespace, name}].obj
}

// DecodedEntriesForTest is how many objects the source remembers a decoding of, whether it was good or refused.
func (s *Source) DecodedEntriesForTest() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.decoded)
}
