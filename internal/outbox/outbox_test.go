package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

var t0 = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

// openStore returns a SQLite store at path.
func openStore(t *testing.T, path string) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// poster is a Poster that records its calls. onPost and onFind, if set, run first and may fail the call.
type poster struct {
	mu             sync.Mutex
	posts, finds   int
	found          map[string]string // ts of a message already in Slack, by outbox ID
	onPost, onFind func(ctx context.Context) error
}

func (p *poster) Post(ctx context.Context, _, _ string, _ slack.Content, _ string) (string, error) {
	p.mu.Lock()
	p.posts++
	n, fn := p.posts, p.onPost
	p.mu.Unlock()
	if fn != nil {
		if err := fn(ctx); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("100.%03d", n), nil
}

func (p *poster) Find(ctx context.Context, _, _, id string, _ time.Time) (string, bool, error) {
	p.mu.Lock()
	p.finds++
	fn, ts := p.onFind, p.found[id]
	p.mu.Unlock()
	if fn != nil {
		if err := fn(ctx); err != nil {
			return "", false, err
		}
	}
	return ts, ts != "", nil
}

func (p *poster) counts() (posts, finds int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.posts, p.finds
}

func newOutbox(t *testing.T, st store.Outbox, p outbox.Poster) *outbox.Outbox {
	t.Helper()
	return outbox.New(outbox.Config{Store: st, Poster: p, Now: func() time.Time { return t0 },
		CallTimeout: time.Second, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func reply(source string) outbox.Post {
	return outbox.Post{Source: source, Role: store.OutboxReply, Channel: "C1", ThreadTS: "1.1", Content: slack.Content{Text: "hello " + source}}
}

func TestIDDerivesFromSourceAndRole(t *testing.T) {
	if got := outbox.ID("ev1", store.OutboxReply); got != "ev1/reply" {
		t.Fatalf("ID = %q; want ev1/reply", got)
	}
	if outbox.ID("ev1", store.OutboxReply) == outbox.ID("ev1", store.OutboxNotice) {
		t.Fatal("two roles of one source share an ID")
	}
}

// TestSendWritesTheRowBeforePostingAndRecordsAfter has the poster look at the row as it posts.
func TestSendWritesTheRowBeforePostingAndRecordsAfter(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	p := &poster{}
	p.onPost = func(ctx context.Context) error {
		row, err := st.GetOutbox(ctx, "ev1/reply")
		if err != nil || row.MessageRef != "" || row.Payload == "" {
			return fmt.Errorf("while posting, the row is %+v, %w; want it written, unposted, with its payload", row, err)
		}
		return nil
	}
	ob := newOutbox(t, st, p)
	ts, err := ob.Send(t.Context(), reply("ev1"))
	if err != nil || ts != "100.001" {
		t.Fatalf("Send = %q, %v", ts, err)
	}
	if row, _ := st.GetOutbox(t.Context(), "ev1/reply"); row.MessageRef != ts || row.Role != store.OutboxReply || row.EventID != "ev1" || !row.CreatedAt.Equal(t0) {
		t.Fatalf("row after Send = %+v; want the ref recorded", row)
	}
	// A repeat returns the recorded ref and does nothing in Slack: not a post, not even a lookup.
	if again, err := ob.Send(t.Context(), reply("ev1")); err != nil || again != ts {
		t.Fatalf("second Send = %q, %v; want %q", again, err, ts)
	}
	if posts, finds := p.counts(); posts != 1 || finds != 0 {
		t.Fatalf("posts = %d, finds = %d; want 1 and 0 (a new row posts blind)", posts, finds)
	}
}

func TestSendRejectsEmptyContent(t *testing.T) {
	ob := newOutbox(t, openStore(t, filepath.Join(t.TempDir(), "c.db")), &poster{})
	p := reply("ev1")
	p.Content = slack.Content{}
	if _, err := ob.Send(t.Context(), p); !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Send with no content = %v; want ErrInvalid", err)
	}
}

// TestSendOfAnUnpostedRowLooksFirst: a row left unposted by a failed post may have been posted
// anyway, so the next Send finds it before it posts.
func TestSendOfAnUnpostedRowLooksFirst(t *testing.T) {
	for _, tt := range []struct {
		name, inSlack string
		wantPosts     int
	}{
		{"it was posted", "55.5", 1}, // only the first, failed attempt
		{"it was not", "", 2},
	} {
		st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
		p := &poster{found: map[string]string{"ev1/reply": tt.inSlack}}
		failing := true
		p.onPost = func(context.Context) error {
			if failing {
				return errors.New("connection reset")
			}
			return nil
		}
		ob := newOutbox(t, st, p)
		if _, err := ob.Send(t.Context(), reply("ev1")); err == nil {
			t.Fatalf("%s: Send with a failing post succeeded", tt.name)
		}
		if row, _ := st.GetOutbox(t.Context(), "ev1/reply"); row.MessageRef != "" {
			t.Fatalf("%s: a failed post left the ref %q", tt.name, row.MessageRef)
		}
		failing = false
		ts, err := ob.Send(t.Context(), reply("ev1"))
		if posts, finds := p.counts(); err != nil || posts != tt.wantPosts || finds != 1 || (tt.inSlack != "" && ts != tt.inSlack) {
			t.Errorf("%s: retry = %q, %v; posts = %d, finds = %d; want %d posts, 1 find, and the found ts", tt.name, ts, err, posts, finds, tt.wantPosts)
		}
	}
}

func TestALookupErrorNeverPosts(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "ev1/reply", EventID: "ev1", Role: store.OutboxReply, Channel: "C1",
		CreatedAt: t0, Payload: `{"text":"hi"}`}); err != nil {
		t.Fatal(err)
	}
	boom := errors.New("history unavailable")
	p := &poster{onFind: func(context.Context) error { return boom }}
	ob := newOutbox(t, st, p)
	if _, err := ob.Send(t.Context(), reply("ev1")); !errors.Is(err, boom) {
		t.Fatalf("Send = %v; want the lookup's error", err)
	}
	if err := ob.Reconcile(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Reconcile = %v; want the lookup's error", err)
	}
	if posts, _ := p.counts(); posts != 0 {
		t.Fatalf("posted %d times after a failed lookup; want 0", posts)
	}
}

func TestReconcile(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	row := func(src, payload string, at time.Duration) {
		t.Helper()
		e := store.OutboxEntry{ID: src + "/reply", EventID: src, Role: store.OutboxReply, Channel: "C1", CreatedAt: t0.Add(at), Payload: payload}
		if _, err := st.CreateOutbox(t.Context(), e); err != nil {
			t.Fatal(err)
		}
	}
	row("adopted", `{"text":"a"}`, 0)
	row("absent", `{"text":"b"}`, time.Second)
	row("stream", "", 2*time.Second) // no payload: a stream, which the renderer records itself
	row("broken", `{"text":"c"}`, 3*time.Second)
	boom := errors.New("history unavailable")
	p := &poster{found: map[string]string{"adopted/reply": "55.5"}}
	ob := newOutbox(t, st, p)
	// The lookup of "broken" fails; the rows around it are still reconciled.
	lookups := 0
	p.onFind = func(context.Context) error {
		if lookups++; lookups == 3 {
			return boom
		}
		return nil
	}
	if err := ob.Reconcile(t.Context()); !errors.Is(err, boom) || errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Reconcile = %v; want the one lookup error, and none for the stream row", err)
	}
	for id, want := range map[string]string{"adopted/reply": "55.5", "absent/reply": "100.001", "stream/reply": "", "broken/reply": ""} {
		if got, _ := st.GetOutbox(t.Context(), id); got.MessageRef != want {
			t.Errorf("%s: ref = %q; want %q", id, got.MessageRef, want)
		}
	}
	if posts, finds := p.counts(); posts != 1 || finds != 3 {
		t.Errorf("posts = %d, finds = %d; want 1 and 3: the stream row is not looked up or posted", posts, finds)
	}
}

// TestSendAndReconcileNeverPostTwice holds Send inside its post and starts Reconcile, which has
// listed the row as unposted. Reconcile must wait, then see the ref and leave the row alone.
func TestSendAndReconcileNeverPostTwice(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	inPost, release := make(chan struct{}), make(chan struct{})
	p := &poster{onPost: func(context.Context) error { close(inPost); <-release; return nil }}
	ob := newOutbox(t, st, p)
	sent := make(chan error, 1)
	go func() { _, err := ob.Send(t.Context(), reply("ev1")); sent <- err }()
	<-inPost
	reconciled := make(chan error, 1)
	go func() { reconciled <- ob.Reconcile(t.Context()) }()
	select {
	case err := <-reconciled:
		t.Fatalf("Reconcile finished while Send was posting: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-sent; err != nil {
		t.Fatal(err)
	}
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	if posts, finds := p.counts(); posts != 1 || finds != 0 {
		t.Fatalf("posts = %d, finds = %d; want 1 and 0", posts, finds)
	}
}

// TestSlackCallsAreBounded hangs a call until its context ends. The delivery lock must not be
// held for good, so a Send of another message goes through afterwards.
func TestSlackCallsAreBounded(t *testing.T) {
	st := openStore(t, filepath.Join(t.TempDir(), "c.db"))
	hang := func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }
	p := &poster{onPost: hang}
	ob := outbox.New(outbox.Config{Store: st, Poster: p, Now: func() time.Time { return t0 }, CallTimeout: 20 * time.Millisecond})
	done := make(chan error, 1)
	go func() {
		_, err := ob.Send(t.Context(), reply("ev1")) // hangs in Post
		p.mu.Lock()
		p.onPost, p.onFind = nil, hang
		p.mu.Unlock()
		if !errors.Is(err, context.DeadlineExceeded) {
			done <- fmt.Errorf("Send with a hung post = %w; want DeadlineExceeded", err)
			return
		}
		if err := ob.Reconcile(t.Context()); !errors.Is(err, context.DeadlineExceeded) { // the row is unposted: looked up, hangs
			done <- fmt.Errorf("Reconcile with a hung lookup = %w; want DeadlineExceeded", err)
			return
		}
		p.mu.Lock()
		p.onFind = nil
		p.mu.Unlock()
		_, err = ob.Send(t.Context(), reply("ev2"))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a hung Slack call was never cut off")
	}
}

// failingRecord is a store whose RecordMessageRef fails, as if courier died between the post and
// the record: the message is in Slack and the row has no ref.
type failingRecord struct{ store.Outbox }

func (failingRecord) RecordMessageRef(context.Context, string, string) error {
	return errors.New("killed before recording")
}

// TestCrashAfterPost kills courier between posting and recording, then restarts it: against the
// fake Slack and a real client, there is exactly one message afterwards.
func TestCrashAfterPost(t *testing.T) {
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	client := slack.New(slack.Config{BotToken: secret.New("xoxb-fake-bot-token"), AppToken: secret.New("xapp-fake-app-token"), APIURL: fake.URL() + "/", PageLimit: 2})
	path := filepath.Join(t.TempDir(), "c.db")
	st := openStore(t, path)
	rootTS, err := client.Post(t.Context(), "C1", "", slack.Content{Text: "root"}, "root")
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"", rootTS} { // a post to the channel, and a reply in a thread
		p := reply("ev-" + thread)
		p.ThreadTS = thread
		before := len(fake.Messages("C1"))
		if _, err := newOutbox(t, failingRecord{st}, client).Send(t.Context(), p); err == nil {
			t.Fatal("Send survived the crash before recording")
		}
		if got := len(fake.Messages("C1")); got != before+1 {
			t.Fatalf("the message was not posted: %d messages", got)
		}
		// Courier restarts: a new Outbox over the reopened store reconciles.
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		st = openStore(t, path)
		if err := newOutbox(t, st, client).Reconcile(t.Context()); err != nil {
			t.Fatalf("Reconcile: %v", err)
		}
		msgs := fake.Messages("C1")
		row, err := st.GetOutbox(t.Context(), outbox.ID(p.Source, p.Role))
		if len(msgs) != before+1 || err != nil || row.MessageRef != msgs[len(msgs)-1].TS || row.Payload != "" {
			t.Fatalf("thread %q after restart: %d messages (want %d), row %+v, %v; want one message adopted by the row", thread, len(msgs), before+1, row, err)
		}
	}
}
