package session

import (
	"testing"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
)

const (
	modeAll       = v1alpha1.ThreadRepliesAll
	modeMention   = v1alpha1.ThreadRepliesMention
	dialectA2A    = string(v1alpha1.DialectA2A)
	dialectKagent = string(v1alpha1.DialectKagentV1)
)

// liveThread is a reply in a thread whose session is live, in a mention-only Binding of the a2a dialect, with a
// posted text question of the session's own task pending. Each row of the table changes what it needs.
func liveThread() ReplyFacts {
	return ReplyFacts{Owned: true, Mode: modeMention, Dialect: dialectA2A, TaskID: "t1", Question: &PendingQuestion{ID: "q1", TaskID: "t1", Posted: true}}
}

var (
	admTurn        = Admission{Kind: AdmitTurn}
	admAnswerQ1    = Admission{Kind: AdmitAnswer, Question: "q1"}
	admNotOwned    = Admission{Kind: AdmitIgnored, Ignored: IgnoredNotOwned}
	admUnmentioned = Admission{Kind: AdmitIgnored, Ignored: IgnoredUnmentioned}
	admGoneQuietly = Admission{Kind: AdmitIgnored, Ignored: IgnoredBindingGone}
	admExpired     = Admission{Kind: AdmitNotice, Notice: NoticeExpired}
	admGone        = Admission{Kind: AdmitNotice, Notice: NoticeBindingGone}
	withMention    = func(f *ReplyFacts) { f.Mentioned = true }
)

// TestReplyAdmission is the rule of spec 8.2, 8.4 and D38 over the facts of a reply: whether it is a turn, the
// answer to a question, ignored or answered with a notice. Each row names what it changes in liveThread. asks says
// the decision needs the pending question, which admit reads only then.
func TestReplyAdmission(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ReplyFacts)
		want   Admission
		asks   bool
	}{
		// A thread courier does not own.
		{"not owned: a plain reply", func(f *ReplyFacts) { *f = ReplyFacts{Mode: modeMention} }, admNotOwned, false},
		{"not owned: a mentioned reply that may not create", func(f *ReplyFacts) { *f = ReplyFacts{Mode: modeMention, Mentioned: true} }, admNotOwned, false},
		{"not owned: a mention creates the session", func(f *ReplyFacts) { *f = ReplyFacts{Mode: modeMention, Mentioned: true, Create: true} }, admTurn, false},
		{"not owned: a trigger's start creates it, unmentioned", func(f *ReplyFacts) { *f = ReplyFacts{Mode: modeMention, Create: true} }, admTurn, false},

		// The session's Binding is gone: expiry cannot be judged, and the channel's current Binding says whether
		// the reply asked for anything.
		{"gone: no Binding owns the channel, so the default mode is all", func(f *ReplyFacts) { f.BindingGone, f.ChannelMode = true, "" }, admGone, false},
		{"gone: the channel's Binding takes all replies", func(f *ReplyFacts) { f.BindingGone, f.ChannelMode = true, modeAll }, admGone, false},
		{"gone: the channel's Binding takes mentions, and the reply has none, though a question is pending", func(f *ReplyFacts) { f.BindingGone, f.ChannelMode = true, modeMention }, admGoneQuietly, false},
		{"gone: the channel's Binding takes mentions, and the reply mentions courier", func(f *ReplyFacts) { f.BindingGone, f.ChannelMode = true, modeMention; withMention(f) }, admGone, false},
		{"gone: the channel's Binding takes mentions, and the turn may create", func(f *ReplyFacts) { f.BindingGone, f.ChannelMode, f.Create = true, modeMention, true }, admGone, false},
		{"gone: the session's own mode is not asked, all", func(f *ReplyFacts) { f.BindingGone, f.Mode, f.ChannelMode = true, modeAll, modeMention }, admGoneQuietly, false},
		{"gone: the session's own mode is not asked, mention", func(f *ReplyFacts) { f.BindingGone, f.Mode, f.ChannelMode = true, modeMention, modeAll }, admGone, false},
		{"gone: it is gone whether or not the session is old", func(f *ReplyFacts) { f.BindingGone, f.Expired = true, true }, admGone, false},
		{"gone: a quiet reply stays quiet when old", func(f *ReplyFacts) { f.BindingGone, f.Expired, f.ChannelMode = true, true, modeMention }, admGoneQuietly, false},

		// Expiry, with the Binding live: the notice answers a reply that asked for something.
		{"expired, all: a plain reply", func(f *ReplyFacts) { f.Mode, f.Expired = modeAll, true }, admExpired, false},
		{"expired, unset mode is all: a plain reply", func(f *ReplyFacts) { f.Mode, f.Expired = "", true }, admExpired, false},
		{"expired, all: a mentioned reply", func(f *ReplyFacts) { f.Mode, f.Expired = modeAll, true; withMention(f) }, admExpired, false},
		{"expired, mention: a plain reply asked for nothing, and a pending question is an answer to nobody", func(f *ReplyFacts) { f.Expired = true }, admUnmentioned, false},
		{"expired, mention: a mentioned reply", func(f *ReplyFacts) { f.Expired = true; withMention(f) }, admExpired, false},
		{"expired, mention: a turn that may create", func(f *ReplyFacts) { f.Expired, f.Create = true, true }, admExpired, false},

		// The mode, in a live thread.
		{"all: a plain reply is a turn, though a question is pending", func(f *ReplyFacts) { f.Mode = modeAll }, admTurn, false},
		{"unset mode is all: a plain reply is a turn", func(f *ReplyFacts) { f.Mode = "" }, admTurn, false},
		{"mention: a mentioned reply is a turn, never pinned to the question", withMention, admTurn, false},
		{"mention: a turn that may create is no reply and needs no mention", func(f *ReplyFacts) { f.Create = true }, admTurn, false},
		{"live: the channel's mode is not asked, all", func(f *ReplyFacts) { f.ChannelMode, f.Mode = modeMention, modeAll }, admTurn, false},
		{"live: the channel's mode is not asked, mention", func(f *ReplyFacts) { f.ChannelMode, f.Mode, f.Dialect = modeAll, modeMention, dialectKagent }, admUnmentioned, false},

		// A typed answer to a question that takes text replies, in a mention-only Binding.
		{"mention: the answer to the pending text question", func(*ReplyFacts) {}, admAnswerQ1, true},
		{"mention: a dialect with no text questions", func(f *ReplyFacts) { f.Dialect = dialectKagent }, admUnmentioned, false},
		{"mention: no dialect", func(f *ReplyFacts) { f.Dialect = "" }, admUnmentioned, false},
		{"mention: no pending question", func(f *ReplyFacts) { f.Question = nil }, admUnmentioned, true},
		{"mention: the question's prompt is not posted", func(f *ReplyFacts) { f.Question.Posted = false }, admUnmentioned, true},
		{"mention: the question is another task's", func(f *ReplyFacts) { f.Question.TaskID = "t0" }, admUnmentioned, true},
		{"mention: the session has no task", func(f *ReplyFacts) { f.TaskID = "" }, admUnmentioned, true},
		{"mention: the session has another task", func(f *ReplyFacts) { f.TaskID = "t2" }, admUnmentioned, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := liveThread()
			tt.change(&f)
			if got := f.Classify(); got != tt.want {
				t.Errorf("Classify(%+v) = %+v; want %+v", f, got, tt.want)
			}
			if got := f.NeedsQuestion(); got != tt.asks {
				t.Errorf("NeedsQuestion = %v; want %v", got, tt.asks)
			}
		})
	}
}

// TestReplyAdmissionInvariants sweeps every combination of the facts and holds the classification to what no
// combination may break: the kinds are closed and carry what they name, an answer is only ever the pending question
// of the session's task in a mention-only live thread, a reply that asked for something is never ignored in a live
// thread, and a question that NeedsQuestion says is not needed changes nothing.
func TestReplyAdmissionInvariants(t *testing.T) {
	flag := func(set func(*ReplyFacts)) []func(*ReplyFacts) { return []func(*ReplyFacts){func(*ReplyFacts) {}, set} }
	modes := func(set func(*ReplyFacts, v1alpha1.ThreadReplies)) (out []func(*ReplyFacts)) {
		for _, m := range []v1alpha1.ThreadReplies{"", modeAll, modeMention} {
			out = append(out, func(f *ReplyFacts) { set(f, m) })
		}
		return out
	}
	dimensions := [][]func(*ReplyFacts){
		flag(func(f *ReplyFacts) { f.Owned = true }),
		flag(func(f *ReplyFacts) { f.Create = true }),
		flag(func(f *ReplyFacts) { f.Mentioned = true }),
		flag(func(f *ReplyFacts) { f.BindingGone = true }),
		flag(func(f *ReplyFacts) { f.Expired = true }),
		modes(func(f *ReplyFacts, m v1alpha1.ThreadReplies) { f.Mode = m }),
		modes(func(f *ReplyFacts, m v1alpha1.ThreadReplies) { f.ChannelMode = m }),
		{func(f *ReplyFacts) { f.Dialect = dialectA2A }, func(f *ReplyFacts) { f.Dialect = dialectKagent }},
		{func(f *ReplyFacts) { f.TaskID = "" }, func(f *ReplyFacts) { f.TaskID = "t1" }},
		{
			func(f *ReplyFacts) { f.Question = nil },
			func(f *ReplyFacts) { f.Question = &PendingQuestion{ID: "q1", TaskID: "t1", Posted: true} },
			func(f *ReplyFacts) { f.Question = &PendingQuestion{ID: "q1", TaskID: "t1"} },
			func(f *ReplyFacts) { f.Question = &PendingQuestion{ID: "q1", TaskID: "t0", Posted: true} },
		},
	}
	swept := 0
	var sweep func(i int, f ReplyFacts)
	sweep = func(i int, f ReplyFacts) {
		if i < len(dimensions) {
			for _, set := range dimensions[i] {
				g := f
				set(&g)
				sweep(i+1, g)
			}
			return
		}
		swept++
		got := f.Classify()
		switch got.Kind {
		case AdmitTurn:
			if got.Question != "" || got.Notice != "" || got.Ignored != "" {
				t.Fatalf("%+v: a turn with more than its kind: %+v", f, got)
			}
		case AdmitAnswer:
			q := f.Question
			if q == nil || got.Question != q.ID || !q.Posted || q.TaskID != f.TaskID || !f.Owned || f.BindingGone || f.Expired || f.Mode != modeMention || f.Mentioned || f.Create || !TakesTextReplies(f.Dialect) {
				t.Fatalf("%+v: answered %+v", f, got)
			}
		case AdmitIgnored:
			if got.Ignored == "" || got.Question != "" || got.Notice != "" {
				t.Fatalf("%+v: ignored with no reason, or with more: %+v", f, got)
			}
			if f.Owned && (f.Mentioned || f.Create) && !f.BindingGone {
				t.Fatalf("%+v: ignored a reply that asked for something: %+v", f, got)
			}
		case AdmitNotice:
			if (got.Notice != NoticeExpired && got.Notice != NoticeBindingGone) || got.Question != "" || got.Ignored != "" || !f.Owned {
				t.Fatalf("%+v: a notice that is neither of the two, or of no session: %+v", f, got)
			}
			if (got.Notice == NoticeBindingGone) != f.BindingGone {
				t.Fatalf("%+v: notice %q; the Binding-gone one is for a gone Binding only", f, got.Notice)
			}
		default:
			t.Fatalf("%+v: kind %q is not one of the four", f, got.Kind)
		}
		if !f.NeedsQuestion() {
			g := f
			g.Question = &PendingQuestion{ID: "other", TaskID: f.TaskID, Posted: true}
			if again := g.Classify(); again != got {
				t.Fatalf("%+v: classified %+v, but %+v with another question, though it said it needs none", f, got, again)
			}
		}
	}
	sweep(0, ReplyFacts{})
	if want := 2 * 2 * 2 * 2 * 2 * 3 * 3 * 2 * 2 * 4; swept != want {
		t.Fatalf("swept %d combinations; want %d", swept, want)
	}
}

func TestTakesTextReplies(t *testing.T) {
	for dialect, want := range map[string]bool{dialectA2A: true, dialectKagent: false, "": false} {
		if got := TakesTextReplies(dialect); got != want {
			t.Errorf("TakesTextReplies(%q) = %v; want %v", dialect, got, want)
		}
	}
}
