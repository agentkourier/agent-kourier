package session

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
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

// Queued reads back what Classify decided and Manager.carryOut wrote: a reply is an answer to the question it was
// enqueued pinned to, and any other is a turn.
func TestQueued(t *testing.T) {
	tests := []struct {
		name string
		r    store.Reply
		want Admission
	}{
		{"enqueued by a mention", store.Reply{ID: "r1"}, admTurn},
		{"enqueued pinned to a question", store.Reply{ID: "r1", InteractionID: "q1"}, admAnswerQ1},
		{"claimed for a question by the pipeline", store.Reply{ID: "r1", InteractionID: "q1", UserID: "U1", Text: "prod"}, admAnswerQ1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Queued(tt.r); got != tt.want {
				t.Errorf("Queued(%+v) = %+v; want %+v", tt.r, got, tt.want)
			}
		})
	}
}

// Only a turn and an answer leave a row in the queue: a person's reply that is any other kind is not in the thread's
// history of what the agent was sent.
func TestAdmissionQueues(t *testing.T) {
	for _, tt := range []struct {
		adm  Admission
		want bool
	}{
		{admTurn, true}, {admAnswerQ1, true},
		{admNotOwned, false}, {admUnmentioned, false}, {admGoneQuietly, false}, {admExpired, false}, {admGone, false},
		{Admission{}, false},
	} {
		if got := tt.adm.Queues(); got != tt.want {
			t.Errorf("%+v.Queues() = %v; want %v", tt.adm, got, tt.want)
		}
	}
}

// What the intake writes is what Queued and WasAdmitted read, for every kind of admission: a reply is in the queue
// exactly when it was admitted as a turn or an answer, and it is read back as the same kind, pinned to the same question.
func TestSubmitWritesWhatQueuedAndWasAdmittedRead(t *testing.T) {
	const live, expired = 0 * time.Hour, 2 * time.Hour
	tests := []struct {
		name      string
		mode      v1alpha1.ThreadReplies
		age       time.Duration
		mentioned bool
		create    bool
		owned     bool
		gone      bool
		want      Admission
	}{
		{"not owned", modeMention, live, false, false, false, false, admNotOwned},
		{"a mention creates the session", modeMention, live, true, true, false, false, admTurn},
		{"a mention in a live thread", modeMention, live, true, false, true, false, admTurn},
		{"a plain reply in mode all", modeAll, live, false, false, true, false, admTurn},
		{"a plain reply in mode mention, answering the pending question", modeMention, live, false, false, true, false, admAnswerQ1},
		{"an expired thread", modeAll, expired, true, false, true, false, admExpired},
		{"a gone Binding", modeAll, live, true, false, true, true, admGone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFixture(t)
			f.threadReplies, f.textReplies = tt.mode, true
			if tt.owned {
				name := "ns/b1"
				if tt.gone {
					name = "ns/gone"
				}
				f.seed(name, tt.age)
				f.question("q1", true)
			}
			in := reply("1700.02", tt.mentioned)
			in.Create = tt.create
			res, err := f.submitAny(in)
			if err != nil || res.Admission != tt.want {
				t.Fatalf("Submit = %+v, %v; want %+v", res, err, tt.want)
			}
			admitted, err := WasAdmitted(t.Context(), f.raw, in.MessageID)
			if err != nil || admitted != tt.want.Queues() {
				t.Fatalf("WasAdmitted = %v, %v; want %v", admitted, err, tt.want.Queues())
			}
			rows := f.drain()
			if len(rows) != map[bool]int{true: 1, false: 0}[tt.want.Queues()] {
				t.Fatalf("%d rows queued; want one if and only if the reply was admitted", len(rows))
			}
			if len(rows) == 1 && Queued(rows[0]) != tt.want {
				t.Errorf("Queued(%+v) = %+v; want %+v: the row does not say what the admission decided", rows[0], Queued(rows[0]), tt.want)
			}
		})
	}
}

type failingRecord struct{ err error }

func (r failingRecord) HasReply(context.Context, string) (bool, error) { return false, r.err }

// A queue that cannot say is an error, never "not admitted": the reply would be shown to the agent a second time.
func TestWasAdmittedPassesAStoreErrorOn(t *testing.T) {
	boom := errors.New("database is locked")
	if admitted, err := WasAdmitted(t.Context(), failingRecord{boom}, "slack:slack:C1:1"); admitted || !errors.Is(err, boom) {
		t.Errorf("WasAdmitted = %v, %v; want false and the store's error", admitted, err)
	}
}

func TestIsTextQuestion(t *testing.T) {
	ask := func(change func(*dialect.Interaction)) *dialect.Interaction {
		in := &dialect.Interaction{Kind: dialect.AskUser, Questions: []dialect.Question{{Text: "Which service?"}}}
		change(in)
		return in
	}
	tests := []struct {
		name string
		in   *dialect.Interaction
		want bool
	}{
		{"one question with no choices", ask(func(*dialect.Interaction) {}), true},
		{"choices that are an empty list (kagent sends one for free text)", ask(func(in *dialect.Interaction) { in.Questions[0].Choices = []string{} }), true},
		{"no interaction", nil, false},
		{"one question with choices", ask(func(in *dialect.Interaction) { in.Questions[0].Choices = []string{"a", "b"} }), false},
		{"two questions", ask(func(in *dialect.Interaction) { in.Questions = append(in.Questions, dialect.Question{Text: "Why?"}) }), false},
		{"no question", ask(func(in *dialect.Interaction) { in.Questions = nil }), false},
		{"a tool approval", ask(func(in *dialect.Interaction) { in.Kind = dialect.ToolApproval }), false},
		{"a question of no kind", ask(func(in *dialect.Interaction) { in.Kind = "" }), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTextQuestion(tt.in); got != tt.want {
				t.Errorf("IsTextQuestion(%+v) = %v; want %v", tt.in, got, tt.want)
			}
		})
	}
}

// JudgeAnswer is the rule of whether a queued reply answers the question that the paused task waits on.
func TestJudgeAnswer(t *testing.T) {
	asked := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	reply := func(at time.Time, pinned bool) store.Reply {
		r := store.Reply{ID: "r1", EnqueuedAt: at}
		if pinned {
			r.InteractionID = "q1"
		}
		return r
	}
	seen := AskedQuestion{CreatedAt: asked, Posted: true, Open: true}
	tests := []struct {
		name string
		r    store.Reply
		q    AskedQuestion
		want AnswerVerdict
	}{
		// A reply that was queued as a turn.
		{"a turn after the question, which is posted and open", reply(asked.Add(time.Second), false), seen, AnswerDecides},
		{"a turn at the instant the question was asked", reply(asked, false), seen, AnswerDecides},
		{"a turn before the question was asked", reply(asked.Add(-time.Second), false), seen, AnswerNone},
		{"a turn before the question, which is not posted either", reply(asked.Add(-time.Second), false), AskedQuestion{CreatedAt: asked, Open: true}, AnswerNone},
		{"a turn after a question that closed after its prompt was posted", reply(asked.Add(time.Second), false), AskedQuestion{CreatedAt: asked, Posted: true}, AnswerDecides},
		{"a turn after a question whose prompt is not posted yet", reply(asked.Add(time.Second), false), AskedQuestion{CreatedAt: asked, Open: true}, AnswerWaits},
		{"a turn after a question that closed unseen", reply(asked.Add(time.Second), false), AskedQuestion{CreatedAt: asked}, AnswerNone},
		// A reply pinned to its question when it was queued.
		{"pinned, to a posted open question", reply(asked.Add(time.Second), true), seen, AnswerDecides},
		{"pinned, to a question that closed after its prompt was posted", reply(asked.Add(time.Second), true), AskedQuestion{CreatedAt: asked, Posted: true}, AnswerDecides},
		{"pinned, to a question whose prompt is not posted yet (a legacy claim)", reply(asked.Add(time.Second), true), AskedQuestion{CreatedAt: asked, Open: true}, AnswerDecides},
		{"pinned, to a question that closed unseen", reply(asked.Add(time.Second), true), AskedQuestion{CreatedAt: asked}, AnswerConsumed},
		{"pinned from before the question was asked (a legacy claim may predate it)", reply(asked.Add(-time.Second), true), seen, AnswerDecides},
		{"pinned from before the question, which closed unseen", reply(asked.Add(-time.Second), true), AskedQuestion{CreatedAt: asked}, AnswerConsumed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JudgeAnswer(tt.r, tt.q); got != tt.want {
				t.Errorf("JudgeAnswer(%+v, %+v) = %q; want %q", tt.r, tt.q, got, tt.want)
			}
		})
	}
}

// A reply pinned to its question is never a turn, whatever the question's state: it is never judged AnswerNone,
// which would leave it queued to be sent as one. And no verdict but AnswerNone lets a reply that came before the
// question be taken for its answer.
func TestJudgeAnswerInvariants(t *testing.T) {
	asked := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	for _, pinned := range []bool{false, true} {
		for _, at := range []time.Time{asked.Add(-time.Second), asked, asked.Add(time.Second)} {
			for _, posted := range []bool{false, true} {
				for _, open := range []bool{false, true} {
					r := store.Reply{ID: "r1", EnqueuedAt: at}
					if pinned {
						r.InteractionID = "q1"
					}
					q := AskedQuestion{CreatedAt: asked, Posted: posted, Open: open}
					got := JudgeAnswer(r, q)
					switch {
					case pinned && (got == AnswerNone || got == AnswerWaits):
						t.Errorf("pinned %+v, %+v: %q; a pinned reply is decided or consumed, never left queued as a turn", r, q, got)
					case !pinned && got == AnswerConsumed:
						t.Errorf("%+v, %+v: %q; only a pinned reply is consumed", r, q, got)
					case !pinned && at.Before(asked) && got != AnswerNone:
						t.Errorf("%+v, %+v: %q; a reply before the question is no answer", r, q, got)
					case got != AnswerDecides && got != AnswerWaits && got != AnswerNone && got != AnswerConsumed:
						t.Errorf("%+v, %+v: verdict %q is not one of the four", r, q, got)
					}
				}
			}
		}
	}
}
