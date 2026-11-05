package crsource_test

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// weightyTriggers is what the caps allow of one Binding with regexes that are long and harmless: it makes the Binding costly
// to decode, so that a read of the Set takes longer than the pause between two edits of it.
func weightyTriggers(t *testing.T, triggers int) []any {
	t.Helper()
	fields := []v1alpha1.MessageField{"text", "title", "link", "fallback", "footer", "body"}
	long := strings.Repeat("abcdefgh", 60)
	zero := int32(0)
	var ts []v1alpha1.Trigger
	for i := range triggers {
		m, x := map[v1alpha1.MessageField]string{}, map[v1alpha1.MessageField]string{}
		for _, f := range fields {
			m[f] = fmt.Sprintf("%s%04d", long, i)
			x[f] = fmt.Sprintf("%s%04dx", long, i)
		}
		ex := map[string]v1alpha1.Extract{}
		for j := range 32 {
			n := fmt.Sprintf("e%d", j)
			ex[n] = v1alpha1.Extract{Field: "text", Regex: long + "(?P<" + n + ">z)"}
		}
		ts = append(ts, v1alpha1.Trigger{Type: v1alpha1.TriggerBotMessage, From: &v1alpha1.BotSource{ID: "B1", Kind: "bot"}, Match: m, Exclude: x, Extract: ex, PromptTemplate: "x",
			Limits: v1alpha1.TriggerLimits{RateLimit: &v1alpha1.RateLimit{MaxRuns: 1, Per: metav1.Duration{Duration: time.Minute}, MaxConcurrent: 1}, MaxRunsPerDay: &zero}})
	}
	raw, err := json.Marshal(ts)
	if err != nil {
		t.Fatal(err)
	}
	var out []any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// One tenant that edits its own Binding in a loop must not freeze the config for the others, or stop a broker from starting.
// A read of the Set takes longer than the pause between two edits, so the cache has moved again by the time a read is done: a
// Reloader that dropped a read whenever that happened would publish nothing for as long as the loop ran. The edits here are
// against a fake API server in the test process; the loop runs until the other tenant's Binding is served, and the test fails
// if that does not happen.
func TestAnEditLoopDoesNotFreezeTheConfigOrStopAStart(t *testing.T) {
	objs := corpus(t)
	var served *unstructured.Unstructured
	for _, u := range objs {
		if u.GetKind() == "Binding" {
			served = u
			break
		}
	}
	if served == nil {
		t.Fatal("no Binding in the corpus")
	}
	mk := func(name, channel string) *unstructured.Unstructured {
		u := served.DeepCopy()
		u.SetName(name)
		u.SetUID(types.UID("uid-" + name))
		_ = unstructured.SetNestedField(u.Object, channel, "spec", "chat", "channel")
		return u
	}
	heavy := mk("tenant-loop", "CLOOP")
	if err := unstructured.SetNestedSlice(heavy.Object, weightyTriggers(t, 50), "spec", "chat", "triggers"); err != nil {
		t.Fatal(err)
	}

	c := newCluster(t, append(objs, heavy)...)
	src := c.start(t)
	begin := time.Now()
	if _, _, err := src.Set(t.Context()); err != nil {
		t.Fatal(err)
	}
	read := time.Since(begin)
	pause := 10 * time.Millisecond
	if read < 2*pause {
		t.Fatalf("a read of the Set takes %s; the test needs it to take longer than the pause between edits (%s) to mean anything", read, pause)
	}

	newReloader := func(poll time.Duration) *config.Reloader {
		return config.NewReloader(config.ReloaderConfig{Source: src, Secrets: noSecrets{}, PollInterval: poll, SecretsInterval: time.Hour,
			Logger: slog.New(slog.DiscardHandler), PerObject: true})
	}
	r := newReloader(100 * time.Millisecond)
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Wait()
	defer stop()

	var edits atomic.Int64
	loop, endLoop := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for gen := int64(2); loop.Err() == nil; gen++ {
			u := heavy.DeepCopy()
			u.SetGeneration(gen)
			if _, err := c.resource(u).Update(loop, u, metav1.UpdateOptions{}); err == nil {
				edits.Add(1)
			}
			time.Sleep(pause)
		}
	}()
	defer func() { endLoop(); <-done }()
	for edits.Load() < 5 {
		time.Sleep(10 * time.Millisecond)
	}

	// A broker that starts in the middle of the loop starts.
	r2 := newReloader(time.Hour)
	sctx, scancel := context.WithCancel(t.Context())
	defer scancel()
	if err := r2.Start(sctx); err != nil {
		t.Errorf("a broker's first load during an edit loop = %v; want it to start", err)
	} else if r2.Current() == nil {
		t.Error("a broker's first load during an edit loop published nothing")
	}
	scancel()
	r2.Wait()

	// Another tenant adds a Binding, and the running broker serves it while the loop goes on.
	other := mk("other-tenants-new-binding", "COTHER")
	c.apply(other)
	since := time.Now()
	for time.Since(since) < 20*time.Second {
		if _, ok := r.Current().Binding(other.GetNamespace(), other.GetName()); ok {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, ok := r.Current().Binding(other.GetNamespace(), other.GetName()); !ok {
		t.Fatalf("another tenant's Binding is not served %s into an edit loop (%d edits, each read takes %s); one tenant froze the config", time.Since(since), edits.Load(), read)
	}
	if st := r.Status(); st.Stale || st.Errors != 0 {
		t.Errorf("Status during an edit loop = %+v; want a reload that worked", st)
	}
	t.Logf("served %s after it was added, with %d edits made by then; a read takes %s", time.Since(since), edits.Load(), read)
}
