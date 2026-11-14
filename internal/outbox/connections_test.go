package outbox_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite/sqlitetest"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

const (
	connA = "ns/a"
	connB = "ns/b"
)

// onConn is reply(source) from connection conn.
func onConn(conn, source string) outbox.Post {
	p := reply(source)
	p.Connection = conn
	return p
}

// twoApps is two connections' platforms, each a poster, behind one resolver.
type twoApps struct {
	a, b *poster
	set  chat.PlatformSet[outbox.Platform]
}

func newTwoApps() twoApps {
	a, b := &poster{}, &poster{}
	return twoApps{a: a, b: b, set: chat.PlatformSet[outbox.Platform]{connA: a, connB: b}}
}

// TestEachCallReachesThePlatformOfItsRowsConnection: a post made by Send, a post the drain makes, the lookup before a post and
// a reaction each go to the platform of the connection the row names, with the connection in the Target, and never to the other.
func TestEachCallReachesThePlatformOfItsRowsConnection(t *testing.T) {
	st := sqlitetest.Open(t)
	apps := newTwoApps()
	reacts := map[string]*reactor{connA: {}, connB: {}}
	apps.a.reactor, apps.b.reactor = reacts[connA], reacts[connB]
	o := mustNew(t, outbox.Config{Store: st, Platforms: apps.set, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: testutil.DiscardLogger()})
	ctx := t.Context()

	if _, err := o.Send(ctx, onConn(connA, "sent-a")); err != nil {
		t.Fatalf("Send on a: %v", err)
	}
	if _, err := o.Send(ctx, onConn(connB, "sent-b")); err != nil {
		t.Fatalf("Send on b: %v", err)
	}
	for _, p := range []outbox.Post{onConn(connA, "queued-a1"), onConn(connB, "queued-b"), onConn(connA, "queued-a2")} {
		p.Role = store.OutboxNotice
		if _, err := o.Enqueue(ctx, p); err != nil {
			t.Fatalf("Enqueue %s: %v", p.Source, err)
		}
	}
	for _, c := range []string{connA, connB} {
		r := queuedReaction
		r.Source, r.Connection = "react-"+c, c
		if _, err := o.EnqueueReaction(ctx, r); err != nil {
			t.Fatalf("EnqueueReaction on %s: %v", c, err)
		}
	}
	if err := o.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// A row that exists without a ref is looked up before it is posted again: on its own connection's platform.
	if _, err := st.CreateOutbox(ctx, store.OutboxEntry{ID: "lost/reply", EventID: "lost", Role: store.OutboxReply, Connection: connB, Channel: "C1", Thread: "1.1", CreatedAt: t0, Payload: neutral("lost")}); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Send(ctx, onConn(connB, "lost")); err != nil {
		t.Fatalf("Send of a row to look up: %v", err)
	}

	for conn, p := range map[string]*poster{connA: apps.a, connB: apps.b} {
		p.mu.Lock()
		for _, tg := range append(slices.Clone(p.targets), p.findTargets...) {
			if tg.Conn != conn {
				t.Errorf("%s's platform was asked about %+v; want connection %s", conn, tg, conn)
			}
		}
		p.mu.Unlock()
	}
	// The drain looks each row up before it posts it: two notices on a; the notice and the lost row on b.
	if posts, finds := apps.a.counts(); posts != 3 || finds != 2 {
		t.Errorf("a: %d posts, %d finds; want 3 posts (its Send and two notices) and 2 finds", posts, finds)
	}
	if posts, finds := apps.b.counts(); posts != 3 || finds != 2 {
		t.Errorf("b: %d posts, %d finds; want 3 posts (its Send, its notice and the lost row) and 2 finds", posts, finds)
	}
	for c, r := range reacts {
		if got := r.list(); len(got) != 1 {
			t.Errorf("%s's platform made the reactions %v; want its own one", c, got)
		}
	}
}

// hangs is a poster whose Post waits for release, or for its context: an app that does not answer.
func hangs(release <-chan struct{}) *poster {
	p := &poster{}
	p.onPost = func(ctx context.Context) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return p
}

// TestAConnectionThatHangsHoldsUpOnlyItsOwnPosts: with the platform of one connection not answering, the posts of another go
// out at once, whether the drain or a Send makes them; the hung connection's post goes out when its platform answers.
func TestAConnectionThatHangsHoldsUpOnlyItsOwnPosts(t *testing.T) {
	st := sqlitetest.Open(t)
	release := make(chan struct{})
	a, b := hangs(release), &poster{}
	o := mustNew(t, outbox.Config{Store: st, Platforms: chat.PlatformSet[outbox.Platform]{connA: a, connB: b},
		Now: func() time.Time { return t0 }, CallTimeout: time.Minute, Interval: time.Hour, Logger: testutil.DiscardLogger()})
	start(t, o)
	ctx := t.Context()

	if _, err := o.Enqueue(ctx, onConn(connA, "stuck")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a's post to be in flight", func() bool { posts, _ := a.counts(); return posts == 1 })

	if _, err := o.Enqueue(ctx, onConn(connB, "drained")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "b's queued post to go out while a hangs", func() bool { return ref(st, outbox.ID("drained", store.OutboxReply)) != "" })
	sent := make(chan error, 1)
	go func() { _, err := o.Send(ctx, onConn(connB, "sent")); sent <- err }()
	select {
	case err := <-sent:
		if err != nil {
			t.Fatalf("Send on b while a hangs: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Send on b waited for a's hung post")
	}
	if got := ref(st, outbox.ID("stuck", store.OutboxReply)); got != "" {
		t.Fatalf("a's row is posted (%q) before its platform answered", got)
	}

	close(release)
	testutil.Eventually(t, "a's post to go out once its platform answers", func() bool { return ref(st, outbox.ID("stuck", store.OutboxReply)) != "" })
}

// lookupHang is a poster that never finishes the lookup of one row.
type lookupHang struct {
	*poster
	stuck string
}

func (l lookupHang) Find(ctx context.Context, t chat.Target, id string, since time.Time) (chat.Ref, bool, error) {
	if id == l.stuck {
		<-ctx.Done()
		return chat.Ref{}, false, ctx.Err()
	}
	return l.poster.Find(ctx, t, id, since)
}

// TestAnotherConnectionsPostsDoNotKeepALookupFromGivingUp: a row whose lookup keeps timing out is abandoned only if a call of
// its own connection's platform worked since the first timeout (the platform is up, this lookup cannot finish). Another
// connection's platform working says nothing of it, so the row waits, as it would if the platform were down.
func TestAnotherConnectionsPostsDoNotKeepALookupFromGivingUp(t *testing.T) {
	stuckID := outbox.ID("stuck", store.OutboxReply)
	for _, tc := range []struct {
		name        string
		healthyConn string
		wantGivenUp bool
	}{
		{"the other posts are another connection's", connB, false},
		{"the other posts are the same connection's", connA, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := sqlitetest.Open(t)
			var clock atomic.Int64
			var abandoned []string
			var mu sync.Mutex
			o := mustNew(t, outbox.Config{Store: st,
				Platforms: chat.PlatformSet[outbox.Platform]{connA: lookupHang{&poster{}, stuckID}, connB: &poster{}},
				Now:       func() time.Time { return t0.Add(time.Duration(clock.Load())) }, CallTimeout: 20 * time.Millisecond, Logger: testutil.DiscardLogger(),
				OnAbandon: func(id, reason string) { mu.Lock(); abandoned = append(abandoned, id+" "+reason); mu.Unlock() }})
			ctx := t.Context()
			if _, err := st.CreateOutbox(ctx, store.OutboxEntry{ID: stuckID, EventID: "stuck", Role: store.OutboxReply, Connection: connA, Channel: "C1", Thread: "1.1", CreatedAt: t0, Payload: neutral("stuck")}); err != nil {
				t.Fatal(err)
			}
			for i := range 8 { // more rounds than the five timeouts it takes, each a few hours on
				clock.Add(int64(3 * time.Hour))
				post := onConn(tc.healthyConn, "healthy"+string(rune('a'+i)))
				post.Role = store.OutboxNotice
				if _, err := o.Enqueue(ctx, post); err != nil {
					t.Fatal(err)
				}
				_ = o.Reconcile(ctx)
			}
			mu.Lock()
			defer mu.Unlock()
			gaveUp := slices.Contains(abandoned, stuckID+" "+outbox.AbandonLookupTimeout)
			if gaveUp != tc.wantGivenUp {
				t.Errorf("abandoned = %v; want the stuck row abandoned for its lookups: %v", abandoned, tc.wantGivenUp)
			}
		})
	}
}

// dynamic is a resolver whose connections come and go.
type dynamic struct {
	mu sync.Mutex
	m  map[string]outbox.Platform
}

func (d *dynamic) Platform(conn string) (outbox.Platform, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return chat.PlatformSet[outbox.Platform](d.m).Platform(conn)
}

func (d *dynamic) set(conn string, p outbox.Platform) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.m[conn] = p
}

// TestARowOfAConnectionThatIsDownWaits: nothing runs the connection, so its row stays pending, is not abandoned and is not
// a failure of any platform (no error from the drain, no delay when the connection comes), and it is posted once the
// connection runs. Send says the connection is down, and leaves the row. A prompt that cannot be checked is not invalid.
func TestARowOfAConnectionThatIsDownWaits(t *testing.T) {
	st := sqlitetest.Open(t)
	live, down := &poster{}, &poster{}
	res := &dynamic{m: map[string]outbox.Platform{connA: live}}
	var abandoned atomic.Int32
	o := mustNew(t, outbox.Config{Store: st, Platforms: res, Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: testutil.DiscardLogger(),
		OnAbandon: func(string, string) { abandoned.Add(1) }})
	ctx := t.Context()

	for _, p := range []outbox.Post{onConn(connB, "waiting-b1"), onConn(connA, "going-a"), onConn(connB, "waiting-b2")} {
		p.Role = store.OutboxNotice
		if _, err := o.Enqueue(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := o.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile with b down = %v; want none: a connection that is not running is no failure", err)
	}
	if ref(st, outbox.ID("going-a", store.OutboxNotice)) == "" {
		t.Error("a's row was not posted while b is down")
	}
	for _, src := range []string{"waiting-b1", "waiting-b2"} {
		row, err := st.GetOutbox(ctx, outbox.ID(src, store.OutboxNotice))
		if err != nil || row.MessageRef != "" || !row.AbandonedAt.IsZero() || row.Payload == "" {
			t.Errorf("b's row %s = %+v, %v; want it pending with its text", src, row, err)
		}
	}

	if _, err := o.Send(ctx, onConn(connB, "sent-b")); !errors.Is(err, chat.ErrConnectionDown) {
		t.Errorf("Send on b while down = %v; want ErrConnectionDown", err)
	}
	if row, err := st.GetOutbox(ctx, outbox.ID("sent-b", store.OutboxReply)); err != nil || row.MessageRef != "" || row.Payload == "" {
		t.Errorf("the row of that Send = %+v, %v; want it written and pending", row, err)
	}
	prompt := onConn(connB, "ix-1")
	prompt.Out = chat.Ask(chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "q"}}})
	if _, err := o.Enqueue(ctx, prompt); !errors.Is(err, chat.ErrConnectionDown) || errors.Is(err, store.ErrInvalid) {
		t.Errorf("Enqueue of a prompt on b while down = %v; want ErrConnectionDown and not ErrInvalid", err)
	}

	res.set(connB, down)
	if err := o.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile with b up = %v", err)
	}
	if posts, _ := down.counts(); posts != 3 {
		t.Errorf("b's platform posted %d rows once the connection ran; want its 3 waiting rows, with no backoff to wait out", posts)
	}
	if abandoned.Load() != 0 {
		t.Errorf("%d rows were abandoned; want none", abandoned.Load())
	}
}

// TestRetentionIsKeptWhileAnotherConnectionHangs: the posts of a connection that hangs are in flight all the while, and the rows
// of another connection that are past Retention are abandoned all the same: one whose platform answers, and one that no
// platform runs. The abandoning is each connection's own, so no connection's hang puts it off.
func TestRetentionIsKeptWhileAnotherConnectionHangs(t *testing.T) {
	st := sqlitetest.Open(t)
	release := make(chan struct{})
	a, b := hangs(release), &poster{}
	var clock atomic.Int64
	var mu sync.Mutex
	var abandoned []string
	o := mustNew(t, outbox.Config{Store: st, Platforms: chat.PlatformSet[outbox.Platform]{connA: a, connB: b},
		Now: func() time.Time { return t0.Add(time.Duration(clock.Load())) }, CallTimeout: time.Minute, Interval: time.Hour, Retention: time.Hour, Logger: testutil.DiscardLogger(),
		OnAbandon: func(id, reason string) { mu.Lock(); abandoned = append(abandoned, id+" "+reason); mu.Unlock() }})
	start(t, o)
	ctx := t.Context()

	if _, err := o.Enqueue(ctx, onConn(connA, "stuck")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "a's post to be in flight", func() bool { posts, _ := a.counts(); return posts == 1 })

	clock.Add(int64(2 * time.Hour)) // the rows made at t0 are past Retention, and the stuck one made just now is not
	for _, p := range []outbox.Post{onConn(connB, "old-b"), onConn("ns/down", "old-down")} {
		p.Role = store.OutboxNotice
		if _, err := st.CreateOutbox(ctx, store.OutboxEntry{ID: outbox.ID(p.Source, p.Role), EventID: p.Source, Role: p.Role, Connection: p.Connection, Channel: "C1", CreatedAt: t0, Payload: neutral(p.Source)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := o.Enqueue(ctx, onConn(connB, "later")); err != nil {
		t.Fatal(err)
	}
	testutil.Eventually(t, "the old rows to be abandoned while a hangs", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return slices.Contains(abandoned, "old-down/notice "+outbox.AbandonExpired) && slices.Contains(abandoned, "old-b/notice "+outbox.AbandonExpired)
	})
	for _, id := range []string{"old-b/notice", "old-down/notice"} {
		if row, err := st.GetOutbox(ctx, id); err != nil || row.AbandonedAt.IsZero() || row.Payload != "" {
			t.Errorf("row %s = %+v, %v; want it abandoned with its text dropped", id, row, err)
		}
	}
	// The later row may be posted after the old ones are abandoned: wait for its post, then check it was the only one.
	testutil.Eventually(t, "b to post the row that is not past Retention", func() bool { posts, _ := b.counts(); return posts >= 1 })
	if posts, _ := b.counts(); posts != 1 {
		t.Errorf("b posted %d times; want only the row that is not past Retention", posts)
	}
	close(release)
}

// TestARowOfAConnectionThatStaysDownIsAbandonedAfterRetention: nothing posts it, and its text goes at the retention all the same.
func TestARowOfAConnectionThatStaysDownIsAbandonedAfterRetention(t *testing.T) {
	st := sqlitetest.Open(t)
	var clock atomic.Int64
	var abandoned []string
	o := mustNew(t, outbox.Config{Store: st, Platforms: chat.PlatformSet[outbox.Platform]{connA: &poster{}},
		Now: func() time.Time { return t0.Add(time.Duration(clock.Load())) }, CallTimeout: time.Second, Logger: testutil.DiscardLogger(),
		OnAbandon: func(id, reason string) { abandoned = append(abandoned, id+" "+reason) }})
	ctx := t.Context()
	post := onConn(connB, "waiting")
	post.Role = store.OutboxNotice
	id, err := o.Enqueue(ctx, post)
	if err != nil {
		t.Fatal(err)
	}

	clock.Store(int64(7*24*time.Hour - time.Second)) // inside the default 7 days
	if err := o.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if row, _ := st.GetOutbox(ctx, id); !row.AbandonedAt.IsZero() || row.Payload == "" || len(abandoned) != 0 {
		t.Fatalf("row = %+v, abandoned %v; want it waiting inside the retention", row, abandoned)
	}
	clock.Store(int64(7*24*time.Hour + time.Second))
	for range 2 { // the second pass finds it gone and reports nothing more
		if err := o.Reconcile(ctx); err != nil {
			t.Fatalf("Reconcile = %v", err)
		}
	}
	if row, _ := st.GetOutbox(ctx, id); row.AbandonedAt.IsZero() || row.Payload != "" {
		t.Errorf("row = %+v; want it abandoned, with its text dropped, though b never ran", row)
	}
	if want := id + " " + outbox.AbandonExpired; len(abandoned) != 1 || abandoned[0] != want {
		t.Errorf("abandoned = %v; want %q once", abandoned, want)
	}
}

// TestARowWithNoConnectionIsTheDefaultConnectionsUntilItIsAttributed: a row stored before connections were recorded has none, and
// is the own connection's: it is delivered through that connection's platform, in its lane, and not left for a connection ""
// that nothing runs.
func TestARowWithNoConnectionIsTheDefaultConnectionsUntilItIsAttributed(t *testing.T) {
	st := sqlitetest.Open(t)
	own, other := &poster{}, &poster{}
	o := mustNew(t, outbox.Config{Store: st, Platforms: chat.PlatformSet[outbox.Platform]{connA: own, connB: other}, DefaultConnection: connA,
		Now: func() time.Time { return t0 }, CallTimeout: time.Second, Logger: testutil.DiscardLogger()})
	ctx := t.Context()
	if _, err := st.CreateOutbox(ctx, store.OutboxEntry{ID: "old/reply", EventID: "old", Role: store.OutboxReply, Channel: "C1", Thread: "1.1", CreatedAt: t0, Payload: neutral("old")}); err != nil {
		t.Fatal(err)
	}
	if err := o.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile = %v", err)
	}
	if ref(st, "old/reply") == "" {
		t.Fatal("the row with no connection was not posted")
	}
	if posts, _ := own.counts(); posts != 1 {
		t.Errorf("the own connection posted %d times; want 1", posts)
	}
	if got := own.targets; len(got) != 1 || got[0].Conn != connA {
		t.Errorf("targets = %+v; want the own connection in the Target", got)
	}
	if posts, _ := other.counts(); posts != 0 {
		t.Errorf("the other connection posted %d times; want none", posts)
	}
}
