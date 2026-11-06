package k8ssecrets

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	k8stesting "k8s.io/client-go/testing"

	"github.com/agentkourier/agent-kourier/internal/config"
	secretval "github.com/agentkourier/agent-kourier/internal/secret"
)

// read is the keys of a Secret that a config reads.
func readOf(namespace, name string, keys ...string) config.SecretRead {
	return config.SecretRead{SecretName: sn(namespace, name), Keys: keys}
}

// keyed returns a Resolver that has been told the keys of a config, synced, and stopped at cleanup.
func keyed(t *testing.T, client *api, reads ...config.SecretRead) *Resolver {
	t.Helper()
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(r.Stop)
	if err := r.WatchKeys(t.Context(), reads); err != nil {
		t.Fatal(err)
	}
	return r
}

// manyKeys is a Secret with the keys a, b and c, and n more that nothing reads, each holding a value of the size given.
func manyKeys(namespace, name string, n, size int) *corev1.Secret {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: "1"},
		Data:       map[string][]byte{"a": []byte("value-a"), "b": []byte("value-b"), "c": []byte("value-c")},
	}
	for i := range n {
		s.Data[fmt.Sprintf("junk-%06d", i)] = []byte(strings.Repeat("x", size))
	}
	return s
}

// settled waits until no informer is being filled for the Secret: whatever swap its keys called for has been made. A swap runs in the
// background, and WatchKeys returns before it has, so a test of what the cache keeps looks only after this.
func settled(t *testing.T, r *Resolver, n config.SecretName) {
	t.Helper()
	waitFor(t, "the swap of informers to settle", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return r.entries[n].next == nil
	})
}

// cachedData returns the keys the cache of the Secret holds, sorted.
func cachedData(t *testing.T, r *Resolver, n config.SecretName) []string {
	t.Helper()
	r.mu.Lock()
	inf := r.entries[n].informer
	r.mu.Unlock()
	s, err := inf.lister.Secrets(n.Namespace).Get(n.Name)
	if err != nil {
		t.Fatal(err)
	}
	return slices.Sorted(maps.Keys(s.Data))
}

// The cache of a Secret holds the keys that the config reads and no other, however many the Secret has, and a read of a key that
// the config names costs no API call.
func TestOnlyTheKeysTheConfigReadsAreKept(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 200, 1000))
	r := keyed(t, client, readOf("sre", "creds", "a", "b"))

	if got := cachedData(t, r, sn("sre", "creds")); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatalf("the cache holds the keys %v; want a and b alone", got)
	}
	keys, err := r.Keys(t.Context(), "sre", "creds")
	if err != nil || !slices.Equal(keys, []string{"a", "b"}) {
		t.Errorf("Keys = %v, %v; want the keys that were named", keys, err)
	}
	lists, _ := client.count(sn("sre", "creds"))
	for _, k := range []string{"a", "b"} {
		if v, err := r.Value(t.Context(), "sre", "creds", k); err != nil || v.Reveal() != "value-"+k {
			t.Errorf("Value(%s) = %q, %v; want its value", k, v.Reveal(), err)
		}
	}
	if after, _ := client.count(sn("sre", "creds")); after != lists {
		t.Errorf("a Value of a key that was named listed the Secret again (%d lists, then %d)", lists, after)
	}
}

// A key that is named and that the Secret does not have is missing, and is found once the Secret has it: the names of the keys that
// a Secret has are not what is kept, the ones that are read are.
func TestANamedKeyThatTheSecretLacksIsMissingUntilItHasIt(t *testing.T) {
	s := manyKeys("sre", "creds", 3, 10)
	client := newAPI(t, s)
	r := keyed(t, client, readOf("sre", "creds", "a", "later"))
	client.watching(t, sn("sre", "creds"))

	if keys, _ := r.Keys(t.Context(), "sre", "creds"); !slices.Equal(keys, []string{"a"}) {
		t.Errorf("Keys = %v; want a alone, for later is not in the Secret", keys)
	}
	if _, err := r.Value(t.Context(), "sre", "creds", "later"); !errors.Is(err, secretval.ErrKeyNotFound) {
		t.Errorf("Value of a named key the Secret lacks = %v, want ErrKeyNotFound", err)
	}

	s = s.DeepCopy()
	s.ResourceVersion = "2"
	s.Data["later"] = []byte("value-later")
	if _, err := client.CoreV1().Secrets("sre").Update(t.Context(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the key to be read", func() bool {
		v, err := r.Value(t.Context(), "sre", "creds", "later")
		return err == nil && v.Reveal() == "value-later"
	})
	if keys, _ := r.Keys(t.Context(), "sre", "creds"); !slices.Equal(keys, []string{"a", "later"}) {
		t.Errorf("Keys = %v; want a and later", keys)
	}
}

// The rotation of a referenced key shows on the next read, and the rotation of one that is not referenced adds nothing to the cache.
func TestTheRotationOfAReferencedKeyIsSeenAndOfAnotherKeyIsNotKept(t *testing.T) {
	s := manyKeys("sre", "creds", 5, 10)
	client := newAPI(t, s)
	r := keyed(t, client, readOf("sre", "creds", "a"))
	client.watching(t, sn("sre", "creds"))
	v0, _ := r.SecretVersion(t.Context(), "sre", "creds")

	s = s.DeepCopy()
	s.ResourceVersion = "2"
	s.Data["a"] = []byte("rotated-a")
	s.Data["b"] = []byte("rotated-b")
	s.Data["junk-new"] = []byte("x")
	if _, err := client.CoreV1().Secrets("sre").Update(t.Context(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rotation", func() bool {
		v, err := r.Value(t.Context(), "sre", "creds", "a")
		return err == nil && v.Reveal() == "rotated-a"
	})
	if v, _ := r.SecretVersion(t.Context(), "sre", "creds"); v == v0 {
		t.Errorf("SecretVersion = %q after a write to the Secret, as before it", v)
	}
	if got := cachedData(t, r, sn("sre", "creds")); !slices.Equal(got, []string{"a"}) {
		t.Errorf("the cache holds the keys %v after the rotation; want a alone", got)
	}
}

// A reload that adds a reference to another key of a Secret that is followed fetches that key without cutting off the ones that are
// read: the informer that has the keys goes on serving until the one with the new keys has listed. Until then Keys fails, as it does
// for a cache that has not synced, and never says that the new key is missing when the Secret has it.
func TestAReferenceToAnotherKeyOfAFollowedSecretIsFetchedBeforeItIsAnswered(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	r := keyed(t, client, readOf("sre", "creds", "a"))
	client.watching(t, sn("sre", "creds"))
	before := entryOf(r, sn("sre", "creds"))
	gate, started := make(chan struct{}), make(chan struct{}, 4)
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-gate // the list of the new keys answers when the test says so
		return false, nil, nil
	})

	done := make(chan error, 1)
	go func() { done <- r.WatchKeys(t.Context(), []config.SecretRead{readOf("sre", "creds", "a", "b")}) }()
	<-started

	begin := time.Now()
	if v, err := r.Value(t.Context(), "sre", "creds", "a"); err != nil || v.Reveal() != "value-a" || time.Since(begin) > 200*time.Millisecond {
		t.Errorf("Value of a key that was read before = %q, %v after %s; want it served at once while the new keys are fetched", v.Reveal(), err, time.Since(begin))
	}
	if _, err := r.SecretVersion(t.Context(), "sre", "creds"); err != nil {
		t.Errorf("SecretVersion while the new keys are fetched = %v; want the version of what is cached", err)
	}
	keys, err := r.Keys(t.Context(), "sre", "creds")
	if err == nil || !errors.Is(err, ErrNotSynced) {
		t.Errorf("Keys while the new keys are fetched = %v, %v; want ErrNotSynced, not an answer that lacks b", keys, err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Fatalf("WatchKeys: %v", err)
	}
	if keys, err := r.Keys(t.Context(), "sre", "creds"); err != nil || !slices.Equal(keys, []string{"a", "b"}) {
		t.Errorf("Keys once the new keys are fetched = %v, %v; want a and b", keys, err)
	}
	if v, err := r.Value(t.Context(), "sre", "creds", "b"); err != nil || v.Reveal() != "value-b" {
		t.Errorf("Value of the new key = %q, %v", v.Reveal(), err)
	}
	if entryOf(r, sn("sre", "creds")) != before {
		t.Error("adding a key replaced the entry of the Secret; want the same entry, with its informer swapped")
	}
	waitFor(t, "the informer that was replaced to end", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(cachedKeysLocked(r.entries[sn("sre", "creds")])) == 2 && r.entries[sn("sre", "creds")].next == nil
	})

	// And a key that is then rotated shows, since the informer that has it receives the watch.
	s, _ := client.CoreV1().Secrets("sre").Get(t.Context(), "creds", metav1.GetOptions{})
	s = s.DeepCopy()
	s.ResourceVersion = "9"
	s.Data["b"] = []byte("rotated-b")
	if _, err := client.CoreV1().Secrets("sre").Update(t.Context(), s, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the rotation of the key added later", func() bool {
		v, err := r.Value(t.Context(), "sre", "creds", "b")
		return err == nil && v.Reveal() == "rotated-b"
	})
}

// cachedKeysLocked is the keys the entry's serving informer keeps; the caller holds Resolver.mu.
func cachedKeysLocked(e *entry) []string { return slices.Sorted(maps.Keys(e.keys.keys)) }

// A reference that the resolver was never told of, which a turn that began under an older snapshot makes, is fetched for the read and
// not answered as missing: Value of a key that the Secret has and the cache has not kept waits for the list that keeps it.
func TestValueOfAKeyNoWatchNamedFetchesItInsteadOfAnsweringMissing(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	r := keyed(t, client, readOf("sre", "creds", "a"))
	before := entryOf(r, sn("sre", "creds"))

	v, err := r.Value(t.Context(), "sre", "creds", "c")
	if err != nil || v.Reveal() != "value-c" {
		t.Fatalf("Value of a key that was not named = %q, %v; want the Secret's value, not ErrKeyNotFound", v.Reveal(), err)
	}
	if _, err := r.Value(t.Context(), "sre", "creds", "nothing"); !errors.Is(err, secretval.ErrKeyNotFound) {
		t.Errorf("Value of a key the Secret lacks = %v, want ErrKeyNotFound", err)
	}
	if entryOf(r, sn("sre", "creds")) != before {
		t.Error("the Value replaced the entry; want the same entry, with its informer swapped")
	}

	// A Watch that does not name the key leaves it kept, for the snapshot that was read under is still the one that is served, and a
	// Retain of the snapshot that is published lets it go: a read after that fetches it once more.
	reads := []config.SecretRead{readOf("sre", "creds", "a")}
	if err := r.WatchKeys(t.Context(), reads); err != nil {
		t.Fatal(err)
	}
	settled(t, r, sn("sre", "creds"))
	if got := cachedData(t, r, sn("sre", "creds")); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("the cache holds %v after a Watch that did not name c; want a and c, until the Retain", got)
	}
	r.RetainKeys(reads)
	settled(t, r, sn("sre", "creds"))
	if got := cachedData(t, r, sn("sre", "creds")); !slices.Equal(got, []string{"a"}) {
		t.Errorf("the cache holds %v after a Retain that did not name c; want a alone", got)
	}
	if v, err := r.Value(t.Context(), "sre", "creds", "c"); err != nil || v.Reveal() != "value-c" {
		t.Errorf("Value after the key was let go = %q, %v", v.Reveal(), err)
	}
}

// A key that only a holder of an older snapshot reads is not one the config reads: while it is fetched, Keys still answers for the keys
// of the config, so that a reload that resolves does not fail with a cache that "has not synced".
func TestFetchingAKeyForAnOlderSnapshotDoesNotFailKeys(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	r := keyed(t, client, readOf("sre", "creds", "a"))
	gate, started := make(chan struct{}), make(chan struct{}, 4)
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-gate
		return false, nil, nil
	})
	done := make(chan error, 1)
	go func() { _, err := r.Value(t.Context(), "sre", "creds", "c"); done <- err }()
	<-started
	if keys, err := r.Keys(t.Context(), "sre", "creds"); err != nil || !slices.Equal(keys, []string{"a"}) {
		t.Errorf("Keys while a key is fetched for an older snapshot = %v, %v; want a, answered from the cache that serves", keys, err)
	}
	close(gate)
	if err := <-done; err != nil {
		t.Errorf("Value: %v", err)
	}
}

// A swap ends the informer that it replaces, with its watch, and does not leave it running for the life of the Resolver: a reference that
// flaps between keys would pile up a watch and a cache for each flap.
func TestASwapEndsTheInformerItReplaces(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	n := sn("sre", "creds")
	ab, a := []config.SecretRead{readOf("sre", "creds", "a", "b")}, []config.SecretRead{readOf("sre", "creds", "a")}
	r := keyed(t, client, a...)
	r.RetainKeys(a)
	var replaced []*informer
	for _, reads := range [][]config.SecretRead{ab, a, ab, a} {
		r.mu.Lock()
		replaced = append(replaced, r.entries[n].informer)
		r.mu.Unlock()
		if err := r.WatchKeys(t.Context(), reads); err != nil {
			t.Fatal(err)
		}
		r.RetainKeys(reads)
		settled(t, r, n)
	}
	for i, inf := range replaced {
		waitFor(t, fmt.Sprintf("informer %d to be told to end", i), func() bool {
			select {
			case <-inf.done:
				return true
			default:
				return false
			}
		})
	}
	r.mu.Lock()
	live := r.entries[n].informer
	r.mu.Unlock()
	select {
	case <-live.done:
		t.Error("the informer that serves was ended")
	default:
	}
}

// Retain with fewer keys lets go of the ones that no snapshot reads, and a key that goes and comes back is fetched again.
func TestRetainKeysDropsTheKeysThatNoSnapshotReads(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	reads := []config.SecretRead{readOf("sre", "creds", "a", "b")}
	r := keyed(t, client, reads...)
	r.RetainKeys(reads)

	narrower := []config.SecretRead{readOf("sre", "creds", "a")}
	if err := r.WatchKeys(t.Context(), narrower); err != nil {
		t.Fatal(err)
	}
	r.RetainKeys(narrower)
	waitFor(t, "the cache to keep a alone", func() bool { return slices.Equal(cachedData(t, r, sn("sre", "creds")), []string{"a"}) })
	if v, err := r.Value(t.Context(), "sre", "creds", "b"); err != nil || v.Reveal() != "value-b" {
		t.Errorf("Value of a key that was let go = %q, %v; want it fetched again", v.Reveal(), err)
	}
}

// A candidate config that names fewer keys, and does not become the snapshot, leaves the keys of the last good snapshot kept.
func TestAFailedReloadKeepsTheKeysOfTheLastGoodSnapshot(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	good := []config.SecretRead{readOf("sre", "creds", "a", "b")}
	r := keyed(t, client, good...)
	r.RetainKeys(good)

	if err := r.WatchKeys(t.Context(), []config.SecretRead{readOf("sre", "creds", "a")}); err != nil {
		t.Fatal(err)
	}
	settled(t, r, sn("sre", "creds")) // a Watch that dropped the keys would swap in an informer without them, and the swap is in the background
	if got := cachedData(t, r, sn("sre", "creds")); !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("the cache holds the keys %v after a Watch that named fewer; want the retained snapshot's a and b", got)
	}
	if v, err := r.Value(t.Context(), "sre", "creds", "b"); err != nil || v.Reveal() != "value-b" {
		t.Errorf("Value of a key of the last good snapshot = %q, %v", v.Reveal(), err)
	}
}

// While the list of new keys is refused, the keys that were kept are still served, Keys says it cannot tell, and the Watch names the
// denial: the resolver does not trade a cache that works for one that cannot be filled.
func TestWhileTheNewKeysAreDeniedTheOldOnesAreServed(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	r := keyed(t, client, readOf("sre", "creds", "a"))
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	client.PrependReactor("list", "secrets", func(k8stesting.Action) (bool, k8sruntime.Object, error) { return true, nil, denied() })

	err := func() error {
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		return r.WatchKeys(ctx, []config.SecretRead{readOf("sre", "creds", "a", "b")})
	}()
	if !errors.Is(err, ErrAccessDenied) {
		t.Errorf("WatchKeys of keys whose list is denied = %v; want ErrAccessDenied", err)
	}
	if v, err := r.Value(t.Context(), "sre", "creds", "a"); err != nil || v.Reveal() != "value-a" {
		t.Errorf("Value of a key that was kept = %q, %v; want it served", v.Reveal(), err)
	}
	if _, err := r.Keys(t.Context(), "sre", "creds"); !errors.Is(err, ErrAccessDenied) {
		t.Errorf("Keys while the new keys are denied = %v; want ErrAccessDenied, not an answer that lacks b", err)
	}
	if err := r.Ready(); err != nil {
		t.Errorf("Ready = %v; want ready, since what is served is cached", err)
	}
}

// A Secret that is not Opaque keeps the names of the keys that are read, with no values, and refuses a Value of any.
func TestAnotherTypeKeepsTheNamesOfTheKeysThatAreRead(t *testing.T) {
	s := manyKeys("sre", "tls", 5, 10)
	s.Type = corev1.SecretTypeTLS
	r := keyed(t, newAPI(t, s), readOf("sre", "tls", "a"))
	if got := cachedData(t, r, sn("sre", "tls")); !slices.Equal(got, []string{"a"}) {
		t.Errorf("the cache holds %v; want the key a, with no value", got)
	}
	if _, err := r.Value(t.Context(), "sre", "tls", "a"); !errors.Is(err, secretval.ErrUnsupportedType) {
		t.Errorf("Value of a key of a TLS Secret = %v, want ErrUnsupportedType", err)
	}
}

// The transform keeps the keys it is given, folds in StringData for them as the API server does on write, and nothing else.
func TestTrimToKeepsTheNamedKeysAndFoldsStringData(t *testing.T) {
	in := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "s", ResourceVersion: "4", Annotations: map[string]string{"a": canary}},
		Data:       map[string][]byte{"keep": []byte("data"), "both": []byte("old"), "drop": []byte(canary)},
		StringData: map[string]string{"both": "new", "string": "text", "dropstring": canary},
	}
	out, err := trimTo(keysOf("keep", "both", "string", "absent"))(in)
	if err != nil {
		t.Fatal(err)
	}
	got := out.(*corev1.Secret)
	want := map[string]string{"keep": "data", "both": "new", "string": "text"}
	if len(got.Data) != len(want) {
		t.Errorf("Data = %v; want the three keys", got.Data)
	}
	for k, v := range want {
		if string(got.Data[k]) != v {
			t.Errorf("Data[%s] = %q, want %q", k, got.Data[k], v)
		}
	}
	if got.Annotations != nil || got.StringData != nil || got.ResourceVersion != "4" {
		t.Errorf("the transform kept more than the identity: %+v", got.ObjectMeta)
	}
	if all, _ := trimTo(allKeys())(in); len(all.(*corev1.Secret).Data) != 5 {
		t.Errorf("a transform of all keys kept %d; want 5", len(all.(*corev1.Secret).Data))
	}
}

func TestKeySets(t *testing.T) {
	ab, abc := keysOf("a", "b"), keysOf("a", "b", "c")
	for _, tc := range []struct {
		name          string
		got, want     bool
		gotU, wantSet keySet
	}{
		{"all covers a set", allKeys().covers(abc), true, allKeys().union(ab), allKeys()},
		{"a set does not cover all", ab.covers(allKeys()), false, ab.union(keysOf("c")), abc},
		{"a superset covers", abc.covers(ab), true, ab.union(ab), ab},
		{"a subset does not", ab.covers(abc), false, keySet{}.union(ab), ab},
		{"the empty set is covered", ab.covers(keySet{}), true, keySet{}.union(keySet{}), keySet{}},
	} {
		if tc.got != tc.want || !tc.gotU.equal(tc.wantSet) {
			t.Errorf("%s: covers = %t (want %t), union = %+v (want %+v)", tc.name, tc.got, tc.want, tc.gotU, tc.wantSet)
		}
	}
}

// heapOf is the live heap after a collection.
func heapOf() uint64 {
	runtime.GC()
	runtime.GC()
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return m.HeapAlloc
}

// A Secret of the size the API server allows (1 MiB) with every key of it unreferenced costs a few bytes in the cache, where all of it
// cost the memory of its values and of each key's map entry. The figures are logged: they are the ones the security page states.
func TestAMegabyteOfUnreferencedKeysCostsAlmostNothing(t *testing.T) {
	big := func(keys, valueSize int) *corev1.Secret {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "sre", Name: "big", ResourceVersion: "1"}, Data: map[string][]byte{"token": []byte("t")}}
		for i := range keys {
			s.Data[fmt.Sprintf("k%06d", i)] = []byte(strings.Repeat("x", valueSize))
		}
		return s
	}
	t.Logf("%s, GOMAXPROCS %d of %d CPUs; 4 copies of the Secret held at once", runtime.Version(), runtime.GOMAXPROCS(0), runtime.NumCPU())
	for _, shape := range []struct {
		name string
		make func() *corev1.Secret // a Secret as the informer has just decoded it, which nothing keeps once it is trimmed
	}{
		{"100000 keys of 2 bytes", func() *corev1.Secret { return big(100_000, 2) }},
		{"one key of 1000000 bytes", func() *corev1.Secret { return big(1, 1_000_000) }},
	} {
		measure := func(keep keySet) float64 {
			before := heapOf()
			held := make([]any, 4)
			for i := range held {
				held[i], _ = trimTo(keep)(shape.make())
			}
			after := heapOf()
			runtime.KeepAlive(held)
			return float64(after-before) / float64(len(held)) / (1 << 10)
		}
		all, referenced := measure(allKeys()), measure(keysOf("token"))
		t.Logf("%s: a Secret in the cache is %.0f KiB when every key is kept, %.2f KiB when one is", shape.name, all, referenced)
		if referenced > 4 {
			t.Errorf("%s: %.2f KiB is kept for one referenced key; want a few bytes", shape.name, referenced)
		}
	}
}

// Stop ends the informer a key swap left running, and every goroutine: TestMain's goleak check fails the run otherwise.
func TestStopEndsAnInformerThatIsStillFetchingNewKeys(t *testing.T) {
	client := newAPI(t, manyKeys("sre", "creds", 5, 10))
	r := keyed(t, client, readOf("sre", "creds", "a"))
	listDelay(client, "creds", 2*time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err := r.WatchKeys(ctx, []config.SecretRead{readOf("sre", "creds", "a", "b")}); err == nil {
		t.Fatal("WatchKeys returned before the new keys were fetched")
	}
	r.Stop()
}

// End to end, through a Reloader: a config edit that points a Binding's token at another key of the Secret that it already reads is
// resolved, not refused with "has no key": the reload waits for the list that keeps the new key before it asks which keys there are,
// and the Secret's other keys are never kept.
func TestAReloadThatNamesAnotherKeyOfAFollowedSecretResolves(t *testing.T) {
	const system, sre, oidc = "agent-kourier-system", "sre", "agent-kourier-sre-alerts-oidc"
	token := versioned(sre, oidc, "1", "token", "other", "unreferenced")
	client := newAPI(t, versioned(system, "slack-main", "1", "botToken", "appToken"), token)
	secrets := New(client, WithLogger(slog.New(slog.DiscardHandler)))
	t.Cleanup(secrets.Stop)

	dir := t.TempDir()
	if err := os.CopyFS(dir, os.DirFS("../../../api/v1alpha1/testdata")); err != nil {
		t.Fatal(err)
	}
	rl := config.NewReloader(config.ReloaderConfig{Dir: dir, Secrets: secrets, PollInterval: 20 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err := rl.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Wait)
	if got := cachedData(t, secrets, sn(sre, oidc)); !slices.Equal(got, []string{"token"}) {
		t.Fatalf("the cache holds the keys %v; want the one the Binding reads", got)
	}

	path := filepath.Join(dir, "binding.yaml")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(body), "tokenSecretRef: { name: "+oidc+" }", "tokenSecretRef: { name: "+oidc+", key: other }", 1)
	if edited == string(body) {
		t.Fatal("the test's edit did not apply to the Binding")
	}
	if err := os.WriteFile(path, []byte(edited), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the edit to be served", func() bool {
		for _, b := range rl.Current().Bindings() {
			if ref := b.Binding.Spec.Identity.TokenSecretRef; ref != nil && ref.Key == "other" {
				return true
			}
		}
		return false
	})
	if st := rl.Status(); st.Stale || st.Errors != 0 {
		t.Errorf("Status = %+v after the edit; want no failed reload", st)
	}
	waitFor(t, "the cache to keep the key that is read now and no other", func() bool {
		return slices.Equal(cachedData(t, secrets, sn(sre, oidc)), []string{"other"})
	})
	if v, err := secrets.Value(t.Context(), sre, oidc, "other"); err != nil || v.Reveal() != canary+"1" {
		t.Errorf("Value of the new key = %v", err)
	}
}

// A pinned Secret is Agent Kourier's own, such as the Postgres DSN's, and keeps every key: not the keys that a Watch or a Retain
// names of it, and not fewer once a reload that names only some of them has come and gone.
func TestAPinnedSecretKeepsEveryKeyWhateverTheConfigNames(t *testing.T) {
	pg := sn("agent-kourier", "pg")
	client := newAPI(t, manyKeys("agent-kourier", "pg", 3, 10), manyKeys("sre", "creds", 0, 0))
	r := New(client, WithLogger(slog.New(slog.DiscardHandler)), WithPinned(pg))
	t.Cleanup(r.Stop)
	if err := r.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	all := []string{"a", "b", "c", "junk-000000", "junk-000001", "junk-000002"}
	if got := cachedData(t, r, pg); !slices.Equal(got, all) {
		t.Fatalf("after Start the cache holds %v; want every key %v", got, all)
	}

	narrow := []config.SecretRead{readOf("agent-kourier", "pg", "a")}
	if err := r.WatchKeys(t.Context(), narrow); err != nil {
		t.Fatal(err)
	}
	r.RetainKeys(narrow)
	settled(t, r, pg)
	if got := cachedData(t, r, pg); !slices.Equal(got, all) {
		t.Errorf("after a Watch and a Retain that name key a the cache holds %v; want every key %v", got, all)
	}

	if err := r.WatchKeys(t.Context(), []config.SecretRead{readOf("sre", "creds", "a")}); err != nil { // lets go of what the last Watch asked of pg
		t.Fatal(err)
	}
	r.RetainKeys(nil)
	settled(t, r, pg)
	if got := cachedData(t, r, pg); !slices.Equal(got, all) {
		t.Errorf("after a Watch and a Retain that leave it out the cache holds %v; want every key %v", got, all)
	}
}

// A Watch that names more keys of a retained Secret than the snapshot reads keeps them only until the next Watch: the next one
// that does not name the Secret takes them back, and the informer that serves goes back to the keys that the snapshot reads.
func TestWatchNarrowsARetainedSecretBackToTheKeysTheSnapshotReads(t *testing.T) {
	creds := sn("sre", "creds")
	client := newAPI(t, manyKeys("sre", "creds", 0, 0), manyKeys("sre", "other", 0, 0))
	snapshot := []config.SecretRead{readOf("sre", "creds", "a", "b")}
	r := keyed(t, client, snapshot...)
	r.RetainKeys(snapshot)

	if err := r.WatchKeys(t.Context(), []config.SecretRead{readOf("sre", "creds", "a", "b", "c")}); err != nil {
		t.Fatal(err)
	}
	settled(t, r, creds)
	if got := cachedData(t, r, creds); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Fatalf("after a Watch that names c the cache holds %v; want a, b and c", got)
	}

	if err := r.WatchKeys(t.Context(), []config.SecretRead{readOf("sre", "other", "a")}); err != nil { // a candidate that leaves creds out
		t.Fatal(err)
	}
	waitFor(t, "the cache to go back to the keys of the snapshot", func() bool {
		return slices.Equal(cachedData(t, r, creds), []string{"a", "b"})
	})
}
