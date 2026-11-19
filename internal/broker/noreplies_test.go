package broker

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/outbox"
	"github.com/agentkourier/agent-kourier/internal/session"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// spySink records what reaches the renderer's sink: each turn, notice and queued notice, by its connection. It is safe for
// concurrent use.
type spySink struct {
	mu  sync.Mutex
	got []string
}

func (s *spySink) add(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, what)
}

func (s *spySink) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.got)
}

func (s *spySink) Turn(_ context.Context, t session.TurnRef) agent.Handler {
	s.add("turn " + t.Key.Connection)
	return func(agent.Event) {}
}

func (s *spySink) Notice(_ context.Context, key session.Key, _ session.Notice) {
	s.add("notice " + key.Connection)
}

func (s *spySink) QueueNotice(_ context.Context, key session.Key, _ session.Notice) error {
	s.add("queue " + key.Connection)
	return nil
}

// keysOfEveryPlatform says every Secret has every key of every platform, as the snapshot builder only checks that they exist.
type keysOfEveryPlatform struct{}

func (keysOfEveryPlatform) Keys(context.Context, string, string) ([]string, error) {
	return []string{"botToken", "appToken", "signingSecret", v1alpha1.WebhookSecretKey, v1alpha1.WebhookTokenKey}, nil
}

// webhookConnectionSpec is a webhook ChatConnection that Validate accepts.
func webhookConnectionSpec() v1alpha1.ChatConnectionSpec {
	return v1alpha1.ChatConnectionSpec{
		Platform: v1alpha1.PlatformWebhook,
		Webhook: &v1alpha1.WebhookSpec{
			Identity: "pr-reviewer[bot]",
			Verify:   v1alpha1.WebhookVerify{Scheme: v1alpha1.WebhookHMACSHA256, Header: "X-Hub-Signature-256", Prefix: "sha256="},
			Channel:  v1alpha1.WebhookChannel{From: []string{"/repository/full_name"}},
			Actor:    v1alpha1.WebhookActor{From: []string{"/sender/login"}},
		},
	}
}

// A connection whose platform has no replies (webhook) has nothing of Agent Kourier's to post, so nothing is rendered or queued for
// it: its turns get no handler and its notices are dropped, and QueueNotice says they were written, so that no event is sent again
// for one. A connection on Slack, which posts replies, and one the config does not have, are passed on as they were.
func TestAConnectionWithNoRepliesHasNothingRenderedOrQueued(t *testing.T) {
	conn := func(name string, spec v1alpha1.ChatConnectionSpec) v1alpha1.ChatConnection {
		spec.CredentialsSecretRef, spec.AllowedNamespaces = v1alpha1.SecretRef{Name: "creds"}, []string{"sre"}
		return v1alpha1.ChatConnection{ObjectMeta: metav1.ObjectMeta{Namespace: "platform", Name: name}, Spec: spec}
	}
	set := &config.Set{ChatConnections: []v1alpha1.ChatConnection{
		conn("slack", v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformSlack}),
		conn("github", webhookConnectionSpec()),
	}}
	snap, err := config.Resolve(t.Context(), set, keysOfEveryPlatform{})
	if err != nil {
		t.Fatal(err)
	}
	inner := &spySink{}
	s := quietSink{renderSink: inner, quiet: noRepliesIn(func() *config.Snapshot { return snap })}

	for _, c := range []string{"platform/slack", "platform/github", "platform/gone"} {
		key := session.Key{Connection: c, Channel: "ch-1", Thread: "t-1"}
		h := s.Turn(t.Context(), session.TurnRef{Key: key})
		if (h == nil) != (c == "platform/github") {
			t.Errorf("Turn on %s gave a handler = %v; want none on the connection with no replies alone", c, h != nil)
		}
		s.Notice(t.Context(), key, session.Notice{ID: "n-1", Kind: session.NoticeExpired})
		if err := s.QueueNotice(t.Context(), key, session.Notice{ID: "n-2", Kind: session.NoticeQueued}); err != nil {
			t.Errorf("QueueNotice on %s = %v; want nil", c, err)
		}
	}
	want := []string{
		"turn platform/slack", "notice platform/slack", "queue platform/slack",
		"turn platform/gone", "notice platform/gone", "queue platform/gone",
	}
	if got := inner.seen(); !slices.Equal(got, want) {
		t.Errorf("the renderer's sink was given %q; want %q: nothing of the webhook connection's", got, want)
	}

	if noRepliesIn(func() *config.Snapshot { return nil })("platform/github") {
		t.Error("with no config yet, a connection has no replies; want it to have them, and its rows to go to the outbox")
	}
}

// Run gives the quiet sink to everything that queues a notice, and the config's connections to the digest loop: on a broker whose
// config has a webhook connection, a reply of that connection waiting for its acknowledgment queues nothing, and the window of its
// stopped starts is marked digested with no line in the outbox, while a connection the config does not have (whose rows the outbox
// abandons) is given both. The broker runs no webhook connection yet, so the rows are the store's from an earlier run.
func TestRunQueuesNothingForAConnectionWithNoReplies(t *testing.T) {
	// The files route serves one ChatConnection, so the webhook connection is a custom resource of an admitted team namespace,
	// beside spec 7's Slack one, in place of the minimal Binding.
	const team, webhook, gone = "team-hooks", "team-hooks/github", "platform/gone"
	c := crStartConfig(t, func(objs []*unstructured.Unstructured) {
		objs[3] = crObject(t, "webhook/connection.yaml")
		objs[3].SetNamespace(team)
	})
	c.ConnectionNamespaces = []string{team}
	c.retentionEvery, c.digestEvery = 10*time.Millisecond, 10*time.Millisecond
	cs := cluster()
	if _, err := cs.CoreV1().Secrets(team).Create(t.Context(), secretOf(team, "github-webhook", v1alpha1.WebhookSecretKey), metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	c.Client = cs
	sink := &spySink{}
	c.sink = sink

	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	for i, conn := range []string{webhook, gone} {
		key := store.ConversationKey{Connection: conn, Channel: "acme/widgets", Thread: "pr-" + strconv.Itoa(i)}
		if _, err := seed.EnqueueReply(t.Context(), store.Reply{ID: "reply-" + strconv.Itoa(i), Key: key, UserID: store.SystemUser, MessageRef: key.Thread, Text: "an event",
			EnqueuedAt: time.Now(), Acknowledgment: "eyes"}); err != nil {
			t.Fatal(err)
		}
		lim := store.TriggerRunLimits{MaxRuns: 1, Per: time.Hour, DigestWindow: 10 * time.Minute}
		for _, thread := range []string{"pr-10", "pr-11"} { // the first goes ahead, the second is stopped
			run := store.TriggerRun{Binding: "sre/b-" + strconv.Itoa(i), At: at, Key: store.ConversationKey{Connection: conn, Channel: "acme/widgets", Thread: thread}}
			if _, err := seed.AdmitTriggerRun(t.Context(), run, lim); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	r := start(t, c)
	r.until("/readyz", 200)
	marks := regexp.MustCompile(`_count\{[^}]*operation="agentkourier\.store\.MarkTriggerDigested"[^}]*\} (\d+)`)
	testutil.Eventually(t, "the retention loop queued no acknowledgment, or the digest loop marked fewer than both windows", func() bool {
		_, body := r.get("/metrics")
		n := 0
		for _, m := range marks.FindAllStringSubmatch(body, -1) {
			k, _ := strconv.Atoi(m[1])
			n += k
		}
		return n >= 2 && slices.Contains(sink.seen(), "queue "+gone)
	}, testutil.Within(10*time.Second), testutil.Every(20*time.Millisecond))
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}

	for _, got := range sink.seen() {
		if strings.HasSuffix(got, " "+webhook) {
			t.Errorf("the renderer's sink was given %q; want nothing of the connection with no replies", got)
		}
	}
	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if left, err := st.ListTriggerDigests(t.Context(), time.Now()); err != nil || len(left) != 0 {
		t.Errorf("windows left to digest %+v, %v; want both marked", left, err)
	}
	for i, conn := range []string{webhook, gone} {
		source := strings.Join([]string{"trigger-digest", conn, "sre/b-" + strconv.Itoa(i), "0", strconv.FormatInt(at.UnixNano(), 10)}, "/")
		_, err := st.GetOutbox(t.Context(), outbox.ID(source, store.OutboxDigest))
		if want := conn == gone; (err == nil) != want || (err != nil && !errors.Is(err, store.ErrNotFound)) {
			t.Errorf("the digest row of %s: %v; want a row %v", conn, err, want)
		}
	}
}
