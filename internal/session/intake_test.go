package session

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

var (
	t0  = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	key = Key{Connection: "slack", Channel: "C1", ThreadTS: "1700.01"}
)

// spyStore counts touches, over a real SQLite store.
type spyStore struct {
	Store
	touches int
}

func (s *spyStore) TouchSession(ctx context.Context, k Key, at time.Time) error {
	s.touches++
	return s.Store.TouchSession(ctx, k, at)
}

// fakeBindings maps Binding names to their threadTTL.
type fakeBindings map[string]time.Duration

func (b fakeBindings) Backend(name string) (Backend, error) {
	if ttl, ok := b[name]; ok {
		return Backend{ThreadTTL: ttl}, nil
	}
	return Backend{}, ErrUnknownBinding
}

type fixture struct {
	t      *testing.T
	st     *spyStore
	m      *Manager
	woke   int
	onWake func()
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	s, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "courier.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	f := &fixture{t: t, st: &spyStore{Store: s}}
	f.m, err = New(Config{Store: f.st, Bindings: fakeBindings{"ns/b1": time.Hour, "ns/b2": 1000 * time.Hour}, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	f.m.wake = func(Key) { f.onWake(); f.woke++ }
	f.onWake = func() {}
	return f
}

// inbound is the mention in Slack message ts that may create the session under ns/b1.
func inbound(ts string) Inbound {
	return Inbound{Key: key, Binding: "ns/b1", MessageID: SlackMessageID("slack", "C1", ts), MessageTS: ts, UserID: "U1", Text: "hello", Create: true}
}

func (f *fixture) submit(in Inbound) (Result, error) { return f.m.Submit(f.t.Context(), in) }

func (f *fixture) session() (store.Session, error) {
	return f.st.GetSession(f.t.Context(), key, 100*365*24*time.Hour, t0)
}

func (f *fixture) seed(binding string, age time.Duration) store.Session {
	f.t.Helper()
	sess := store.Session{Key: key, ContextID: "c1", TaskID: "t1", Binding: binding, LastActivity: t0.Add(-age)}
	if err := f.st.UpsertSession(f.t.Context(), sess); err != nil {
		f.t.Fatal(err)
	}
	f.st.touches = 0
	return sess
}

func (f *fixture) drain() (out []store.Reply) {
	for {
		r, err := f.st.NextReply(f.t.Context(), key)
		if err != nil {
			return out
		}
		out = append(out, r)
		_ = f.st.MarkReplyDelivered(f.t.Context(), r.ID, t0)
	}
}

func TestSubmitNotOwned(t *testing.T) {
	f := newFixture(t)
	in := inbound("1700.01")
	in.Create = false
	res, err := f.submit(in)
	if _, serr := f.session(); err != nil || res.Disposition != NotOwned || !errors.Is(serr, store.ErrNotFound) || f.woke != 0 || len(f.drain()) != 0 {
		t.Errorf("Submit = %+v, %v; session %v, woke %d; want NotOwned and nothing written or woken", res, err, serr, f.woke)
	}
}

// The row (key, Binding, no contextId) and the turn are written before the runner wakes
// (spec 8.2: the row precedes the first A2A call).
func TestSubmitCreatesTheSessionBeforeWaking(t *testing.T) {
	f := newFixture(t)
	f.onWake = func() {
		sess, err := f.session()
		if err != nil || sess.Binding != "ns/b1" || sess.ContextID != "" || sess.TaskID != "" || !sess.LastActivity.Equal(t0) {
			t.Errorf("session at wake = %+v, %v", sess, err)
		}
		if r, err := f.st.NextReply(t.Context(), key); err != nil || r.ID != "slack:slack:C1:1700.01" || r.UserID != SystemUser || r.Text != "hello" || r.MessageTS != "1700.01" {
			t.Errorf("queue at wake = %+v, %v", r, err)
		}
	}
	in := inbound("1700.01")
	in.UserID = "" // an alert's prompt, which no Slack user wrote
	if res, err := f.submit(in); err != nil || res.Disposition != Created || f.woke != 1 {
		t.Fatalf("Submit = %+v, %v, woke %d; want Created and one wake", res, err, f.woke)
	}
}

// One Slack message arrives as two events and may be redelivered: it is one queued turn
// (8.2), also after it was sent (D21). Every Submit still wakes the runner.
func TestSubmitCollapsesDuplicates(t *testing.T) {
	f := newFixture(t)
	var queued []int
	for range 3 { // the second is the other event; the third comes after the turn was sent
		if _, err := f.submit(inbound("1700.01")); err != nil {
			t.Fatal(err)
		}
		got := f.drain()
		if queued = append(queued, len(got)); f.woke != len(queued) || (len(got) == 1 && (got[0].UserID != "U1" || got[0].Text != "hello")) {
			t.Fatalf("woke %d times after %d Submits; turn %+v", f.woke, len(queued), got)
		}
	}
	if queued[0] != 1 || queued[1] != 0 || queued[2] != 0 {
		t.Errorf("turns queued by each Submit = %v; want one, then none", queued)
	}
}

// An existing session keeps the Binding it was created under (D14, 8.5) and its threadTTL
// (D28), whatever the caller says. Only a live one is touched: an expired session is
// answered and left exactly as it was, even by an @mention. Exactly threadTTL old is live.
func TestSubmitExistingSession(t *testing.T) {
	for _, tc := range []struct {
		name    string
		age     time.Duration
		binding string
		create  bool
		want    Disposition
	}{
		{"live, caller names another Binding", 30 * time.Minute, "ns/b2", true, Accepted},
		{"exactly threadTTL old", time.Hour, "ns/b2", true, Accepted},
		{"a nanosecond older, though ns/b2 would allow it", time.Hour + time.Nanosecond, "ns/b2", true, Expired},
		{"expired, a reply", 2 * time.Hour, "", false, Expired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			seeded := f.seed("ns/b1", tc.age)
			in := inbound("1700.05")
			in.Binding, in.Create = tc.binding, tc.create
			res, err := f.submit(in)
			got, _ := f.session()
			if tc.want == Accepted {
				seeded.LastActivity = t0 // touched, and nothing else changed
			}
			if err != nil || res.Disposition != tc.want || got != seeded {
				t.Fatalf("Submit = %+v, %v; session %+v; want %s and %+v", res, err, got, tc.want, seeded)
			}
			if queued, want := len(f.drain()), map[Disposition]int{Accepted: 1, Expired: 0}[tc.want]; queued != want || f.woke != want || (tc.want == Expired && f.st.touches != 0) {
				t.Errorf("queued %d, woke %d, %d touches; want %d queued and woken, and no touch for Expired", queued, f.woke, f.st.touches, want)
			}
		})
	}
}

func TestSubmitRefusesWhatCannotBeServed(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*Inbound)
		want   error
	}{
		"no message id":      {func(in *Inbound) { in.MessageID = "" }, store.ErrInvalid},
		"create, no Binding": {func(in *Inbound) { in.Binding = "" }, store.ErrInvalid},
		"unknown Binding":    {func(in *Inbound) { in.Binding = "ns/gone" }, ErrUnknownBinding},
	} {
		f, in := newFixture(t), inbound("1700.01")
		tc.mutate(&in)
		_, err := f.submit(in)
		if _, serr := f.session(); !errors.Is(err, tc.want) || !errors.Is(serr, store.ErrNotFound) || f.woke != 0 {
			t.Errorf("%s: Submit = %v, session %v, woke %d; want %v and nothing written", name, err, serr, f.woke, tc.want)
		}
	}
	// A session whose Binding left the config cannot be judged for expiry: an error, for the
	// queue handler to retry, and no change.
	f := newFixture(t)
	seeded := f.seed("ns/gone", 0)
	_, err := f.submit(inbound("1700.01"))
	if got, _ := f.session(); !errors.Is(err, ErrUnknownBinding) || f.woke != 0 || got != seeded || len(f.drain()) != 0 {
		t.Errorf("Submit = %v, session %+v, woke %d; want ErrUnknownBinding and no change", err, got, f.woke)
	}
	f = newFixture(t)
	_ = f.st.Store.(io.Closer).Close()
	if _, err := f.submit(inbound("1700.01")); err == nil || f.woke != 0 {
		t.Errorf("Submit on a closed store = %v, woke %d; want an error and no wake", err, f.woke)
	}
}
