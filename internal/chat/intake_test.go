package chat_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
)

const conn = "courier-system/slack-main"

// The session manager is what Submit goes to.
var _ chat.Submitter = (*session.Manager)(nil)

// submitter records what the session manager is asked to submit. fn, if set, decides the
// answer; by default a turn is Accepted.
type submitter struct {
	mu  sync.Mutex
	got []session.Inbound
	fn  func(session.Inbound) (session.Result, error)
}

func (s *submitter) Submit(_ context.Context, in session.Inbound) (session.Result, error) {
	s.mu.Lock()
	s.got = append(s.got, in)
	fn := s.fn
	s.mu.Unlock() // not held while fn runs: a fn that waits must not stop submitted() from answering
	if fn != nil {
		return fn(in)
	}
	return resultOf(session.Accepted), nil
}

// resultOf is what the session manager answers a reply that it gave the disposition d: the pairing of Disposition and
// Admission it makes, the notice of BindingGone being the one a reply that is taken gets.
func resultOf(d session.Disposition) session.Result {
	admission := map[session.Disposition]session.Admission{
		session.Created:     {Kind: session.AdmitTurn},
		session.Accepted:    {Kind: session.AdmitTurn},
		session.NotOwned:    {Kind: session.AdmitIgnored, Ignored: session.IgnoredNotOwned},
		session.Unmentioned: {Kind: session.AdmitIgnored, Ignored: session.IgnoredUnmentioned},
		session.Expired:     {Kind: session.AdmitNotice, Notice: session.NoticeExpired},
		session.BindingGone: {Kind: session.AdmitNotice, Notice: session.NoticeBindingGone},
	}[d]
	return session.Result{Disposition: d, Admission: admission}
}

func (s *submitter) submitted() []session.Inbound {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

// allKeys is a SecretResolver whose Secrets have every key.
type allKeys struct{}

func (allKeys) Keys(context.Context, string, string) ([]string, error) {
	return []string{"botToken", "appToken", "token"}, nil
}

// inbound is a message of a platform named "slack" in the thread (empty for none), which is in channel at id.
func inbound(channel, id, thread string, mentioned bool) chat.Inbound {
	return chat.Inbound{At: chat.Ref{Platform: "slack", Conn: conn, Channel: channel, ID: id, Thread: thread}, User: "U1", Text: "hi", Mentioned: mentioned}
}

// newIntake returns an Intake for the connection, over a config that binds C1 to sre/sre-alerts
// and the private channel G1 to payments/pay.
func newIntake(t *testing.T) (*chat.Intake, *submitter) { return newIntakeNotifying(t, nil) }

// newIntakeNotifying is newIntake with the func that takes a notice for a thread courier cannot continue.
func newIntakeNotifying(t *testing.T, notice func(context.Context, session.Key, session.Notice) error) (*chat.Intake, *submitter) {
	t.Helper()
	return newIntakeModes(t, notice, "", "")
}

// newIntakeModes is newIntakeNotifying with the chat.threadReplies of the Binding of C1 and of G1.
func newIntakeModes(t *testing.T, notice func(context.Context, session.Key, session.Notice) error, c1, g1 v1alpha1.ThreadReplies) (*chat.Intake, *submitter) {
	t.Helper()
	meta := func(ns, name string) metav1.ObjectMeta { return metav1.ObjectMeta{Namespace: ns, Name: name} }
	bind := func(ns, name, channel string, mode v1alpha1.ThreadReplies) v1alpha1.Binding {
		return v1alpha1.Binding{ObjectMeta: meta(ns, name), Spec: v1alpha1.BindingSpec{
			Agent: v1alpha1.AgentRef{BackendRef: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "kagent"}},
			Chat:  v1alpha1.ChatBinding{ConnectionRef: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"}, Channel: channel, ThreadReplies: mode}}}
	}
	allow := []string{"sre", "payments"}
	snap, err := config.Resolve(t.Context(), &config.Set{
		AgentBackends:   []v1alpha1.AgentBackend{{ObjectMeta: meta("courier-system", "kagent"), Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectKagentV1, AllowedNamespaces: allow}}},
		ChatConnections: []v1alpha1.ChatConnection{{ObjectMeta: meta("courier-system", "slack-main"), Spec: v1alpha1.ChatConnectionSpec{AllowedNamespaces: allow}}},
		Bindings:        []v1alpha1.Binding{bind("sre", "sre-alerts", "C1", c1), bind("payments", "pay", "G1", g1)},
	}, allKeys{})
	if err != nil {
		t.Fatal(err)
	}
	sub := &submitter{}
	return chat.NewIntake(chat.IntakeConfig{
		Connection: v1alpha1.ObjectRef{Namespace: "courier-system", Name: "slack-main"},
		Snapshot:   func() *config.Snapshot { return snap }, Submitter: sub, Notice: notice,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}), sub
}

// TestTriggers checks spec 8.2's table: what each message is submitted as, if at all.
func TestTriggers(t *testing.T) {
	tests := []struct {
		name    string
		m       chat.Inbound
		submit  bool
		create  bool
		binding string
		thread  string
	}{
		{"mention outside a thread starts one", inbound("C1", "5.5", "", true), true, true, "sre/sre-alerts", "5.5"},
		{"mention in a thread, owned or not", inbound("C1", "5.5", "4.4", true), true, true, "sre/sre-alerts", "4.4"},
		{"reply in a thread", inbound("C1", "5.5", "4.4", false), true, false, "", "4.4"},
		{"mention in a private channel", inbound("G1", "5.5", "", true), true, true, "payments/pay", "5.5"},
		{"mention in an unbound channel's thread", inbound("C9", "5.5", "4.4", true), true, false, "", "4.4"},
		{"mention in an unbound channel", inbound("C9", "5.5", "", true), false, false, "", ""},
		{"message outside any thread", inbound("C1", "5.5", "", false), false, false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, sub := newIntake(t)
			if err := in.Message(t.Context(), tt.m); err != nil {
				t.Fatal(err)
			}
			var want []session.Inbound
			if tt.submit {
				want = []session.Inbound{{Key: session.Key{Connection: conn, Channel: tt.m.At.Channel, Thread: tt.thread}, Binding: tt.binding,
					MessageID: chat.MessageID(chat.Ref{Platform: "slack", Conn: conn, Channel: tt.m.At.Channel, ID: "5.5"}), MessageRef: "5.5", UserID: "U1", Text: "hi", Mentioned: tt.m.Mentioned, Create: tt.create}}
			}
			if got := sub.submitted(); !slices.Equal(got, want) {
				t.Fatalf("submitted\n%+v\nwant\n%+v", got, want)
			}
		})
	}
}

// The message's ID is the string the store already holds for Slack, "slack:<conn>:<channel>:<ts>", whatever
// the connection the Ref says: the Intake's own connection scopes it.
func TestTheMessageIDIsTheStringTheStoreAlreadyHolds(t *testing.T) {
	in, sub := newIntake(t)
	m := inbound("C1", "5.5", "", true)
	m.At.Conn = "somewhere/else"
	if err := in.Message(t.Context(), m); err != nil {
		t.Fatal(err)
	}
	if got := sub.submitted(); len(got) != 1 || got[0].MessageID != "slack:"+conn+":C1:5.5" {
		t.Fatalf("submitted %+v; want the message ID slack:%s:C1:5.5", got, conn)
	}
}

// TestSubmitOutcomes delivers one message twice to see what Submit's answer does: whether
// the message is acknowledged (a nil error) and whether it is remembered. A message is
// remembered only once a turn is queued, so a redelivery is not submitted again; any other
// answer leaves the next event of the message free to try.
func TestSubmitOutcomes(t *testing.T) {
	disposition := func(d session.Disposition) (session.Result, error) { return resultOf(d), nil }
	tests := []struct {
		name    string
		answer  func() (session.Result, error)
		wantErr bool
		again   bool // the second delivery is submitted
	}{
		{"created", func() (session.Result, error) { return disposition(session.Created) }, false, false},
		{"accepted", func() (session.Result, error) { return disposition(session.Accepted) }, false, false},
		{"an answer to the agent's question", func() (session.Result, error) {
			return session.Result{Disposition: session.Accepted, Admission: session.Admission{Kind: session.AdmitAnswer, Question: "q1"}}, nil
		}, false, false},
		{"not owned", func() (session.Result, error) { return disposition(session.NotOwned) }, false, true},
		{"expired", func() (session.Result, error) { return disposition(session.Expired) }, false, true},
		{"binding gone", func() (session.Result, error) { return disposition(session.BindingGone) }, false, true},
		{"invalid", func() (session.Result, error) { return session.Result{}, fmt.Errorf("x: %w", store.ErrInvalid) }, false, true},
		{"store failure", func() (session.Result, error) { return session.Result{}, errors.New("disk full") }, true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in, sub := newIntake(t)
			sub.fn = func(session.Inbound) (session.Result, error) { return tt.answer() }
			m := inbound("C1", "5.5", "4.4", true)
			if err := in.Message(t.Context(), m); (err != nil) != tt.wantErr {
				t.Fatalf("Message = %v; want error: %v", err, tt.wantErr)
			}
			_ = in.Message(t.Context(), m)
			if n := len(sub.submitted()); (n == 2) != tt.again {
				t.Fatalf("Submit called %d times for two deliveries; want a second call: %v", n, tt.again)
			}
		})
	}
}

// TestDedupKey checks that the two events of one message are one submit however they differ, and that the same ID
// in another channel is another message.
func TestDedupKey(t *testing.T) {
	in, sub := newIntake(t)
	m := inbound("C1", "5.5", "", true)
	again := m
	again.Direct = false
	other := inbound("G1", "5.5", "", true)
	for _, msg := range []chat.Inbound{m, again, other} {
		if err := in.Message(t.Context(), msg); err != nil {
			t.Fatal(err)
		}
	}
	if got := sub.submitted(); len(got) != 2 || got[0].Key.Channel != "C1" || got[1].Key.Channel != "G1" {
		t.Fatalf("submitted %+v; want one turn in C1 and one in G1", got)
	}
}

// TestIntakePassesInteractionsOn: a press goes to the configured handler, and the handler's
// error is the Intake's, so the event is left unacknowledged.
func TestIntakePassesInteractionsOn(t *testing.T) {
	var got []chat.Interaction
	fail := errors.New("busy")
	in := chat.NewIntake(chat.IntakeConfig{Interactions: func(_ context.Context, i chat.Interaction) error {
		got = append(got, i)
		return fail
	}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	press := chat.Interaction{User: "U1", At: chat.Ref{Channel: "C1", ID: "5.5"}, PromptID: "ix-1"}
	if err := in.Interaction(t.Context(), press); !errors.Is(err, fail) || len(got) != 1 || got[0].PromptID != "ix-1" {
		t.Fatalf("Interaction = %v, handler saw %+v; want its error, and the press", err, got)
	}
	// With no handler the press is dropped, and acknowledged.
	if err := chat.NewIntake(chat.IntakeConfig{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}).Interaction(t.Context(), press); err != nil {
		t.Fatalf("Interaction with no handler = %v; want nil", err)
	}
}

// notices records what the intake asks to tell a thread.
type notices struct {
	mu   sync.Mutex
	got  []session.Notice
	key  []session.Key
	fail []error // the error of each next call, in order; none left means success
}

func (n *notices) take(_ context.Context, key session.Key, notice session.Notice) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.got, n.key = append(n.got, notice), append(n.key, key)
	if len(n.fail) == 0 {
		return nil
	}
	err := n.fail[0]
	n.fail = n.fail[1:]
	return err
}

// A reply in a thread courier cannot continue, past its threadTTL or with its Binding gone, is
// answered with a notice (spec 8.2). Its ID is built from the message, so the two events of a
// mention are one notice; the thread it goes to is the one the reply is in.
func TestAThreadCourierCannotContinueGetsOneNotice(t *testing.T) {
	for name, tc := range map[string]struct {
		disposition session.Disposition
		kind        session.NoticeKind
		suffix      string
	}{
		"expired":      {session.Expired, session.NoticeExpired, "/expired"},
		"binding gone": {session.BindingGone, session.NoticeBindingGone, "/binding_gone"},
	} {
		t.Run(name, func(t *testing.T) {
			var got notices
			in, sub := newIntakeNotifying(t, got.take)
			sub.fn = func(session.Inbound) (session.Result, error) { return resultOf(tc.disposition), nil }
			// The two events of one mention: the same message, twice.
			m := inbound("C1", "5.5", "4.4", true)
			m.Text = "<!channel> <@U2> [x](https://evil.example) & \u202e"
			for range 2 {
				if err := in.Message(t.Context(), m); err != nil {
					t.Fatal(err)
				}
			}
			want := session.Notice{ID: chat.MessageID(chat.Ref{Platform: "slack", Conn: conn, Channel: "C1", ID: "5.5"}) + tc.suffix, Kind: tc.kind, MessageRef: "5.5"}
			if len(got.got) != 1 || got.got[0] != want || got.key[0] != (session.Key{Connection: conn, Channel: "C1", Thread: "4.4"}) {
				t.Errorf("notices %+v for keys %+v; want exactly %+v in the thread of the reply, with no text of the message in it", got.got, got.key, want)
			}
			// Another message in the thread is its own notice.
			other := m
			other.At.ID = "6.6"
			if err := in.Message(t.Context(), other); err != nil || len(got.got) != 2 || got.got[1].ID == want.ID {
				t.Errorf("a second message: %v, notices %+v; want a notice of its own", err, got.got)
			}
		})
	}
}

// Nothing else is noticed: a turn that was taken, a thread courier does not own, a reply that fails
// to submit. And a thread that cannot be continued is quiet, not a crash, when no one takes notices.
func TestOnlyAThreadCourierCannotContinueIsNoticed(t *testing.T) {
	disposition := func(d session.Disposition) func(session.Inbound) (session.Result, error) {
		return func(session.Inbound) (session.Result, error) { return resultOf(d), nil }
	}
	for name, fn := range map[string]func(session.Inbound) (session.Result, error){
		"created":   disposition(session.Created),
		"accepted":  disposition(session.Accepted),
		"not owned": disposition(session.NotOwned),
		"invalid": func(session.Inbound) (session.Result, error) {
			return session.Result{}, fmt.Errorf("x: %w", store.ErrInvalid)
		},
		"failed": func(session.Inbound) (session.Result, error) { return session.Result{}, errors.New("disk full") },
	} {
		t.Run(name, func(t *testing.T) {
			var got notices
			in, sub := newIntakeNotifying(t, got.take)
			sub.fn = fn
			_ = in.Message(t.Context(), inbound("C1", "5.5", "4.4", false))
			if len(got.got) != 0 {
				t.Errorf("notices %+v; want none", got.got)
			}
		})
	}
	for _, d := range []session.Disposition{session.Expired, session.BindingGone} {
		in, sub := newIntakeNotifying(t, nil)
		sub.fn = func(session.Inbound) (session.Result, error) { return resultOf(d), nil }
		if err := in.Message(t.Context(), inbound("C1", "5.5", "4.4", false)); err != nil {
			t.Errorf("%s with no notice func: %v; want it quiet", d, err)
		}
	}
}

// TestAFailedNoticeLeavesTheEventToBeRedelivered: a notice that could not be written is the event's
// to retry. The error reaches the platform, which leaves the event unacknowledged, and the message is not
// remembered, so the redelivery, and the mention's other event, are tried again. The notice ID is the
// same each time, so a notice written after all is still one row.
func TestAFailedNoticeLeavesTheEventToBeRedelivered(t *testing.T) {
	got := notices{fail: []error{errors.New("disk full")}}
	in, sub := newIntakeNotifying(t, got.take)
	sub.fn = func(session.Inbound) (session.Result, error) {
		return resultOf(session.Expired), nil
	}
	m := inbound("C1", "5.5", "4.4", true)
	if err := in.Message(t.Context(), m); err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Message with a failing notice = %v; want its error, so the event is not acknowledged", err)
	}
	if err := in.Message(t.Context(), m); err != nil { // the redelivery
		t.Fatalf("Message again = %v", err)
	}
	if err := in.Message(t.Context(), m); err != nil { // now remembered: nothing is asked again
		t.Fatalf("Message a third time = %v", err)
	}
	if len(got.got) != 2 || got.got[0] != got.got[1] || len(sub.submitted()) != 2 {
		t.Fatalf("%d notices %+v and %d submits; want the same notice twice, then none, and Submit with it", len(got.got), got.got, len(sub.submitted()))
	}
}

// The adapter decides what is a mention; the Intake passes it on with a direct message, which needs none, as
// Inbound.Mentioned, for the session manager's gate (D38).
func TestIntakeTellsTheSessionWhetherAReplyMentionsCourier(t *testing.T) {
	for _, tt := range []struct {
		name              string
		mentioned, direct bool
		want              bool
	}{
		{"a plain reply", false, false, false},
		{"a mention", true, false, true},
		{"a direct message", false, true, true},
		{"a mention in a direct message", true, true, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in, sub := newIntake(t)
			m := inbound("C1", "5.5", "4.4", tt.mentioned)
			m.Direct = tt.direct
			if err := in.Message(t.Context(), m); err != nil {
				t.Fatal(err)
			}
			if got := sub.submitted(); len(got) != 1 || got[0].Mentioned != tt.want {
				t.Fatalf("submitted %+v; want one turn with Mentioned=%v", got, tt.want)
			}
		})
	}
}

// An admission the Intake does not know (the zero value, or a kind a newer session manager added) is an error, so that
// the event is delivered again and the mismatch is seen, rather than a turn dropped in silence.
func TestAnAdmissionTheIntakeDoesNotKnowIsAnErrorAndNotRemembered(t *testing.T) {
	for _, kind := range []session.AdmissionKind{"", "reject"} {
		in, sub := newIntake(t)
		sub.fn = func(session.Inbound) (session.Result, error) {
			return session.Result{Disposition: session.Accepted, Admission: session.Admission{Kind: kind}}, nil
		}
		m := inbound("C1", "5.5", "4.4", true)
		for range 2 {
			if err := in.Message(t.Context(), m); err == nil {
				t.Errorf("kind %q: Message = nil; want an error, so the platform delivers the event again", kind)
			}
		}
		if n := len(sub.submitted()); n != 2 {
			t.Errorf("kind %q: Submit called %d times for two deliveries; want 2: the message is not remembered", kind, n)
		}
	}
}

// An Unmentioned reply is not remembered and not noticed: the message event of a mention may come before its
// app_mention, and the second is the one that counts. The same reply delivered again, still unmentioned, is
// asked about again: the Binding's mode may have been reloaded to all in between.
func TestAnUnmentionedReplyIsNeitherSeenNorNoticed(t *testing.T) {
	var got notices
	in, sub := newIntakeModes(t, got.take, v1alpha1.ThreadRepliesMention, "")
	sub.fn = func(req session.Inbound) (session.Result, error) {
		if req.Mentioned {
			return resultOf(session.Accepted), nil
		}
		return resultOf(session.Unmentioned), nil
	}
	plain := inbound("C1", "5.5", "4.4", false)
	for range 2 {
		if err := in.Message(t.Context(), plain); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(sub.submitted()); n != 2 {
		t.Fatalf("an unmentioned reply delivered twice was submitted %d times; want 2: it is not remembered", n)
	}
	if err := in.Message(t.Context(), inbound("C1", "5.5", "4.4", true)); err != nil { // the app_mention of the same message
		t.Fatal(err)
	}
	if err := in.Message(t.Context(), inbound("C1", "5.5", "4.4", true)); err != nil { // and a duplicate of it
		t.Fatal(err)
	}
	if subs := sub.submitted(); len(subs) != 3 || !subs[2].Mentioned {
		t.Fatalf("submitted %+v; want the mention taken after the unmentioned reply, and once", subs)
	}
	if len(got.got) != 0 {
		t.Errorf("notices %+v; want none", got.got)
	}
}

// A reply in a thread whose Binding left the config is answered with the Binding-gone notice or left alone as the
// session manager decides (reply admission). What this adds is the fact the decision needs and only the channel
// knows: the mode of the Binding that owns the channel now, empty where none does.
func TestIntakeTellsTheSessionTheModeOfTheBindingThatOwnsTheChannel(t *testing.T) {
	for _, tt := range []struct {
		name    string
		channel string // C1 is owned by a Binding in c1's mode; C9 by none
		c1      v1alpha1.ThreadReplies
		want    v1alpha1.ThreadReplies
	}{
		{"the owner takes mentions", "C1", v1alpha1.ThreadRepliesMention, v1alpha1.ThreadRepliesMention},
		{"the owner takes all", "C1", v1alpha1.ThreadRepliesAll, v1alpha1.ThreadRepliesAll},
		{"the owner's mode is unset", "C1", "", ""},
		{"no owner", "C9", v1alpha1.ThreadRepliesMention, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			in, sub := newIntakeModes(t, nil, tt.c1, "")
			if err := in.Message(t.Context(), inbound(tt.channel, "5.5", "4.4", false)); err != nil {
				t.Fatal(err)
			}
			if got := sub.submitted(); len(got) != 1 || got[0].ChannelThreadReplies != tt.want {
				t.Fatalf("submitted %+v; want one reply with ChannelThreadReplies %q", got, tt.want)
			}
		})
	}
}

// A reply the session manager leaves alone in a thread whose Binding is gone is neither noticed nor remembered, so
// the app_mention of the same message that follows is noticed.
func TestAReplyLeftAloneInAThreadWhoseBindingIsGoneIsNeitherNoticedNorSeen(t *testing.T) {
	var got notices
	in, sub := newIntakeNotifying(t, got.take)
	sub.fn = func(req session.Inbound) (session.Result, error) {
		if req.Mentioned {
			return resultOf(session.BindingGone), nil
		}
		return session.Result{Disposition: session.BindingGone, Admission: session.Admission{Kind: session.AdmitIgnored, Ignored: session.IgnoredBindingGone}}, nil
	}
	plain := inbound("C1", "5.5", "4.4", false)
	if err := in.Message(t.Context(), plain); err != nil || len(got.got) != 0 {
		t.Fatalf("the reply: %v, notices %+v; want none", err, got.got)
	}
	if err := in.Message(t.Context(), inbound("C1", "5.5", "4.4", true)); err != nil || len(got.got) != 1 || got.got[0].Kind != session.NoticeBindingGone {
		t.Errorf("the mention of the same message: %v, notices %+v; want the Binding-gone notice", err, got.got)
	}
}

// An expired thread is the session manager's to judge: it answers Expired only for a reply the Binding takes, and the
// Intake notices every one it is told of, as it did.
func TestExpiredIsNoticedInEveryMode(t *testing.T) {
	for _, mode := range []v1alpha1.ThreadReplies{"", v1alpha1.ThreadRepliesAll, v1alpha1.ThreadRepliesMention} {
		var got notices
		in, sub := newIntakeModes(t, got.take, mode, "")
		sub.fn = func(session.Inbound) (session.Result, error) {
			return resultOf(session.Expired), nil
		}
		if err := in.Message(t.Context(), inbound("C1", "5.5", "4.4", true)); err != nil || len(got.got) != 1 || got.got[0].Kind != session.NoticeExpired {
			t.Errorf("mode %q: %v, notices %+v; want one expiry notice", mode, err, got.got)
		}
	}
}
