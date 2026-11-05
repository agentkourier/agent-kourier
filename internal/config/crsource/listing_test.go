package crsource_test

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/config/crsource"
)

// atUnknownFieldsLine is a log handler that runs f when Set says, while it decodes an object, that the object has fields the broker
// does not know: a point between the listing that Set made and the fingerprint it returns, which a test can reach without a hook in
// the code.
type atUnknownFieldsLine struct {
	slog.Handler
	f func()
}

func (atUnknownFieldsLine) Enabled(context.Context, slog.Level) bool { return true }
func (h atUnknownFieldsLine) WithAttrs([]slog.Attr) slog.Handler     { return h }
func (h atUnknownFieldsLine) WithGroup(string) slog.Handler          { return h }
func (h atUnknownFieldsLine) Handle(_ context.Context, r slog.Record) error {
	if strings.Contains(r.Message, "fields this broker does not know") {
		h.f()
	}
	return nil
}

// The fingerprint that Set returns is that of the listing it decoded, not of what the caches hold when the decoding is done. A
// Reloader that is PerObject publishes the Set under it and relies on the next poll finding what was edited since: a fingerprint
// taken after the decoding would hide an edit made during it, and the edit would not be applied until another came. The edit
// here is made, and has reached the Binding cache, at a point in the middle of Set. The test depends on no timing, so the edits of
// the loop test cannot mask a fingerprint taken too late.
func TestSetReturnsTheFingerprintOfTheListingItDecoded(t *testing.T) {
	objs := corpus(t)
	var b *unstructured.Unstructured
	for _, u := range objs {
		if u.GetKind() == "Binding" {
			b = u
			break
		}
	}
	if b == nil {
		t.Fatal("no Binding in the corpus")
	}
	// An object with a field that the broker's types lack makes Set log the line that the test acts at.
	odd := b.DeepCopy()
	odd.SetName("odd")
	odd.SetUID(types.UID("uid-odd"))
	_ = unstructured.SetNestedField(odd.Object, "COOD", "spec", "chat", "channel")
	_ = unstructured.SetNestedField(odd.Object, "x", "spec", "brandNewField")
	c := newCluster(t, append(objs, odd)...)

	var src *crsource.Source
	edited := false
	h := atUnknownFieldsLine{f: func() {
		if edited {
			return
		}
		edited = true
		before := src.BindingEvents()
		u := b.DeepCopy()
		u.SetGeneration(b.GetGeneration() + 1)
		if _, err := c.resource(u).Update(context.Background(), u, metav1.UpdateOptions{}); err != nil {
			t.Error(err)
		}
		for until := time.Now().Add(5 * time.Second); src.BindingEvents() == before && time.Now().Before(until); {
			time.Sleep(time.Millisecond)
		}
	}}
	var err error
	if src, err = crsource.New(crsource.Options{Client: c.client, Connection: own, Logger: slog.New(h)}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(src.Stop)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if err := src.Start(ctx); err != nil {
		t.Fatal(err)
	}
	set, fp, err := src.Set(t.Context())
	if err != nil || !edited {
		t.Fatalf("Set = %v, edited during it: %v; want it to read, and the edit to land in the middle of it", err, edited)
	}
	if got := bindingGeneration(set, b.GetName()); got != b.GetGeneration() {
		t.Fatalf("the Set holds generation %d of the edited Binding; want the %d that the listing held, not the edit's", got, b.GetGeneration())
	}
	if src.Fingerprint(t.Context()) == fp {
		t.Error("an edit that landed during the read does not move the fingerprint past the one Set returned: the edit would never be applied")
	}
}

// bindingGeneration is the generation of the Binding of the Set that is called name, or -1.
func bindingGeneration(set *config.Set, name string) int64 {
	for _, b := range set.Bindings {
		if b.Name == name {
			return b.Generation
		}
	}
	return -1
}
