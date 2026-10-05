package storetest

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// vote is a vote of user on the message of turn in the session of key, with the attribution of the alert thread of the
// tests, cast at at.
func vote(k store.ConversationKey, turn, user string, v store.Verdict, at time.Time) store.FeedbackVote {
	return store.FeedbackVote{Key: k, Turn: turn, User: user, Binding: "ns/b", Agent: "kagent/sre", Alert: "KubePodCrashLooping", Kind: store.FeedbackInvestigation, Verdict: v, At: at}
}

// sameVote compares what a vote is, with times as instants. first is the instant the person first voted.
func sameVote(got, want store.FeedbackVote, first time.Time) bool {
	return got.Key == want.Key && got.Turn == want.Turn && got.User == want.User && got.Binding == want.Binding && got.Agent == want.Agent &&
		got.Alert == want.Alert && got.Kind == want.Kind && got.Verdict == want.Verdict && got.At.Equal(want.At) && got.First.Equal(first)
}

func (su suite) feedback(t *testing.T) {
	su.run(t, "a vote is stored with its Binding, agent, alert, kind and times, and read back", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		if got, err := s.ListVotes(ctx, key); err != nil || len(got) != 0 {
			t.Fatalf("ListVotes of a session nobody voted in = %+v, %v; want none", got, err)
		}
		v := vote(key, "turn-1", "U1", store.VerdictUseful, t0)
		prev, err := s.RecordVote(ctx, v)
		noErr(t, err, "RecordVote")
		if prev != "" {
			t.Errorf("RecordVote of a first vote returned the previous verdict %q; want none", prev)
		}
		got, err := s.ListVotes(ctx, key)
		if err != nil || len(got) != 1 || !sameVote(got[0], v, t0) {
			t.Fatalf("ListVotes = %+v, %v; want %+v first voted at %v", got, err, v, t0)
		}
		answer := vote(key, "turn-2", "U2", store.VerdictNotUseful, t0.Add(time.Minute))
		answer.Kind, answer.Alert = store.FeedbackAnswer, ""
		_, err = s.RecordVote(ctx, answer)
		noErr(t, err, "RecordVote of an answer with no alert")
		got, err = s.ListVotes(ctx, key)
		if err != nil || len(got) != 2 || !sameVote(got[1], answer, answer.At) {
			t.Fatalf("ListVotes = %+v, %v; want the answer's vote second: %+v", got, err, answer)
		}
	})

	su.run(t, "a later vote of the same person replaces the verdict, and the first time stays", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		first := vote(key, "turn-1", "U1", store.VerdictUseful, t0)
		_, err := s.RecordVote(ctx, first)
		noErr(t, err, "RecordVote")
		changed := vote(key, "turn-1", "U1", store.VerdictNotUseful, t0.Add(time.Hour))
		prev, err := s.RecordVote(ctx, changed)
		noErr(t, err, "RecordVote of a changed vote")
		if prev != store.VerdictUseful {
			t.Errorf("the previous verdict = %q; want %q", prev, store.VerdictUseful)
		}
		got, err := s.ListVotes(ctx, key)
		if err != nil || len(got) != 1 || !sameVote(got[0], changed, t0) {
			t.Fatalf("ListVotes = %+v, %v; want the one vote %+v, first cast at %v", got, err, changed, t0)
		}
		back := vote(key, "turn-1", "U1", store.VerdictUseful, t0.Add(2*time.Hour))
		prev, err = s.RecordVote(ctx, back)
		noErr(t, err, "RecordVote of a vote changed back")
		if got, _ := s.ListVotes(ctx, key); prev != store.VerdictNotUseful || len(got) != 1 || got[0].Verdict != store.VerdictUseful {
			t.Errorf("after changing back: previous %q, votes %+v; want %q and one vote that is useful", prev, got, store.VerdictNotUseful)
		}
	})

	su.run(t, "the same verdict again changes nothing and reports itself as the previous", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		first := vote(key, "turn-1", "U1", store.VerdictUseful, t0)
		_, err := s.RecordVote(ctx, first)
		noErr(t, err, "RecordVote")
		again := vote(key, "turn-1", "U1", store.VerdictUseful, t0.Add(time.Hour))
		again.Agent, again.Alert = "kagent/other", "Other" // what a later vote says of the attribution is not taken
		prev, err := s.RecordVote(ctx, again)
		noErr(t, err, "RecordVote of the same verdict")
		if prev != store.VerdictUseful {
			t.Errorf("the previous verdict = %q; want %q, which says nothing changed", prev, store.VerdictUseful)
		}
		if got, err := s.ListVotes(ctx, key); err != nil || len(got) != 1 || !sameVote(got[0], first, t0) {
			t.Fatalf("ListVotes = %+v, %v; want the first vote untouched: %+v", got, err, first)
		}
	})

	su.run(t, "a person has one vote on each message of each session, and people vote apart", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		other := keyOf("2.2")
		votes := []store.FeedbackVote{
			vote(key, "turn-1", "U1", store.VerdictUseful, t0),
			vote(key, "turn-1", "U2", store.VerdictNotUseful, t0),   // another person on the same message
			vote(key, "turn-2", "U1", store.VerdictNotUseful, t0),   // another message of the same session
			vote(other, "turn-1", "U1", store.VerdictNotUseful, t0), // another session
		}
		for _, v := range votes {
			prev, err := s.RecordVote(ctx, v)
			noErr(t, err, "RecordVote")
			if prev != "" {
				t.Errorf("RecordVote(%s, %s, %s) returned the previous verdict %q; want none: it is a vote of its own", v.Key.Thread, v.Turn, v.User, prev)
			}
		}
		if got, err := s.ListVotes(ctx, key); err != nil || len(got) != 3 {
			t.Errorf("ListVotes of the session = %+v, %v; want its 3 votes", got, err)
		}
		if got, err := s.ListVotes(ctx, other); err != nil || len(got) != 1 {
			t.Errorf("ListVotes of the other session = %+v, %v; want its 1 vote", got, err)
		}
	})

	su.run(t, "a vote that lacks what identifies it, or is of no known verdict or kind, is refused", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		good := vote(key, "turn-1", "U1", store.VerdictUseful, t0)
		for name, mutate := range map[string]func(*store.FeedbackVote){
			"no session":  func(v *store.FeedbackVote) { v.Key.Thread = "" },
			"no turn":     func(v *store.FeedbackVote) { v.Turn = "" },
			"no user":     func(v *store.FeedbackVote) { v.User = "" },
			"no Binding":  func(v *store.FeedbackVote) { v.Binding = "" },
			"no agent":    func(v *store.FeedbackVote) { v.Agent = "" },
			"no verdict":  func(v *store.FeedbackVote) { v.Verdict = "" },
			"a verdict":   func(v *store.FeedbackVote) { v.Verdict = "meh" },
			"no kind":     func(v *store.FeedbackVote) { v.Kind = "" },
			"a kind":      func(v *store.FeedbackVote) { v.Kind = "digest" },
			"no time":     func(v *store.FeedbackVote) { v.At = time.Time{} },
			"a time of 0": func(v *store.FeedbackVote) { v.At = time.Unix(0, 0) },
		} {
			v := good
			mutate(&v)
			if _, err := s.RecordVote(ctx, v); !isInvalid(err) {
				t.Errorf("RecordVote with %s = %v; want ErrInvalid", name, err)
			}
		}
		if got, err := s.ListVotes(ctx, key); err != nil || len(got) != 0 {
			t.Errorf("ListVotes after the refused votes = %+v, %v; want none", got, err)
		}
		_, err := s.ListVotes(ctx, store.ConversationKey{})
		isErr(t, err, store.ErrInvalid, "ListVotes of an incomplete key")
	})

	su.run(t, "votes of one person recorded at once leave one vote, and one of them is the first", func(t *testing.T, s store.Store) {
		ctx := t.Context()
		var firsts atomic.Int32
		parallel(8, func(i int) {
			v := vote(key, "turn-1", "U1", store.VerdictUseful, t0.Add(time.Duration(i)*time.Second))
			if i%2 == 1 {
				v.Verdict = store.VerdictNotUseful
			}
			prev, err := s.RecordVote(ctx, v)
			if err != nil {
				t.Errorf("RecordVote %d: %v", i, err)
			} else if prev == "" {
				firsts.Add(1)
			}
		})
		if n := firsts.Load(); n != 1 {
			t.Errorf("%d of 8 concurrent votes reported no previous verdict; want exactly 1", n)
		}
		if got, err := s.ListVotes(ctx, key); err != nil || len(got) != 1 {
			t.Fatalf("ListVotes = %+v, %v; want the one vote", got, err)
		}
	})

	su.runReopen(t, "votes survive a restart", func(t *testing.T, s store.Store, reopen func() store.Store) {
		ctx := t.Context()
		v := vote(key, "turn-1", "U1", store.VerdictNotUseful, t0)
		_, err := s.RecordVote(ctx, v)
		noErr(t, err, "RecordVote")
		s = reopen()
		if got, err := s.ListVotes(ctx, key); err != nil || len(got) != 1 || !sameVote(got[0], v, t0) {
			t.Fatalf("ListVotes after a restart = %+v, %v; want %+v", got, err, v)
		}
		// A vote after the restart is a change of the stored one, not a second vote.
		changed := vote(key, "turn-1", "U1", store.VerdictUseful, t0.Add(time.Minute))
		if prev, err := s.RecordVote(ctx, changed); err != nil || prev != store.VerdictNotUseful {
			t.Fatalf("RecordVote after a restart = %q, %v; want the stored verdict %q", prev, err, store.VerdictNotUseful)
		}
	})
}

func isInvalid(err error) bool { return errors.Is(err, store.ErrInvalid) }
