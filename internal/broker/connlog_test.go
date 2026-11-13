package broker

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// logCapture is a slog.Handler that keeps every record it is given, with the attributes of its logger (With) and then of the call, in
// order and each as given, so that a key given twice is seen twice.
type logCapture struct {
	mu   *sync.Mutex
	recs *[]capturedRecord
	with []slog.Attr
}

type capturedRecord struct {
	level slog.Level
	msg   string
	attrs []slog.Attr
}

// count is how many attributes of r have the key.
func (r capturedRecord) count(key string) int {
	n := 0
	for _, a := range r.attrs {
		if a.Key == key {
			n++
		}
	}
	return n
}

// get is the text of the first attribute of r with the key, or "".
func (r capturedRecord) get(key string) string {
	for _, a := range r.attrs {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

func newLogCapture() *logCapture {
	return &logCapture{mu: new(sync.Mutex), recs: new([]capturedRecord)}
}

func (h *logCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *logCapture) Handle(_ context.Context, r slog.Record) error {
	rec := capturedRecord{level: r.Level, msg: r.Message, attrs: append([]slog.Attr(nil), h.with...)}
	r.Attrs(func(a slog.Attr) bool { rec.attrs = append(rec.attrs, a); return true })
	h.mu.Lock()
	*h.recs = append(*h.recs, rec)
	h.mu.Unlock()
	return nil
}

func (h *logCapture) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &logCapture{mu: h.mu, recs: h.recs, with: append(append([]slog.Attr(nil), h.with...), attrs...)}
}

func (h *logCapture) WithGroup(string) slog.Handler { return h }

// records returns the records so far.
func (h *logCapture) records() []capturedRecord {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]capturedRecord(nil), *h.recs...)
}

// seen reports whether a record with msg has been logged that names connection.
func (h *logCapture) seen(msg, connection string) bool {
	for _, r := range h.records() {
		if r.msg == msg && r.get("connection") == connection {
			return true
		}
	}
	return false
}

// The lines of the Socket Mode connection (connecting, connected, reconnecting) and the WARN of a connection that ended name the connection they
// are about, with the key the other lines of a connection use, so that two connections' lines can be told apart. It runs two connections of the
// real Slack adapter against the fake Slack, and drops their sockets while Slack is up (they reconnect by themselves) and while it is down (the
// run ends and RunPlatform connects again).
func TestSocketModeLogLinesNameTheirConnection(t *testing.T) {
	world := newSocketWorld(t, testApp(1), testApp(2))
	logs := newLogCapture()
	log := slog.New(logs)
	cfg := RunConfig{newSlack: func(sc slack.Config) slackAPI { sc.APIURL = world.srv.URL + "/"; return slack.New(sc) }}
	leader := &leaderHalf{cfg: cfg, log: log}
	host := &runtimeHost{cfg: cfg, log: log, metrics: newTestMetrics(t), table: newRuntimeTable(), open: leader.openPlatform, parent: t.Context()}
	t.Cleanup(func() {
		host.haltAll(context.Background())
		host.closeAll()
	})

	names := []string{"team-a/slack", "team-b/slack"}
	for i, name := range names {
		ns, n, _ := strings.Cut(name, "/")
		app := testApp(i + 1)
		conn := Connection{Ref: v1alpha1.ObjectRef{Namespace: ns, Name: n}, Platform: v1alpha1.PlatformSlack,
			Credentials: map[string]secret.Value{"botToken": secret.New(app.BotToken), "appToken": secret.New(app.AppToken)}}
		rt, err := host.build(conn)
		if err != nil {
			t.Fatal(err)
		}
		host.run(rt, newRecordedSink(), nil)
		world.waitConnections(t, app, 1)
	}
	for _, name := range names {
		testutil.Eventually(t, name+" to log that it connected", func() bool { return logs.seen(slack.ConnectedMessage, name) }, testutil.Within(10*time.Second))
	}

	// Slack is up: a dropped socket is opened again by the adapter, which says it is reconnecting.
	for i := range names {
		if _, err := world.fake.DropConnections(testApp(i + 1).AppID); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		testutil.Eventually(t, name+" to log that it is reconnecting", func() bool { return logs.seen("slack: socket mode reconnecting", name) }, testutil.Within(10*time.Second))
	}
	for i := range names {
		world.waitConnections(t, testApp(i+1), 1)
	}

	// Slack is down: the run ends, and RunPlatform says it connects again.
	world.gate.down.Store(true)
	for i := range names {
		if _, err := world.fake.DropConnections(testApp(i + 1).AppID); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		testutil.Eventually(t, name+" to log that its connection ended", func() bool { return logs.seen("slack connection ended; connecting again", name) }, testutil.Within(10*time.Second))
	}

	// Every line of the Socket Mode connection names its connection, and the lines that were asked for were all seen, for both.
	saw := map[string]map[string]bool{}
	for _, r := range logs.records() {
		if !strings.HasPrefix(r.msg, "slack: socket mode") && r.msg != "slack connection ended; connecting again" {
			continue
		}
		if n := r.count("connection"); n != 1 {
			t.Errorf("the line %q has %d connection attributes; want one", r.msg, n)
		}
		c := r.get("connection")
		if c != names[0] && c != names[1] {
			t.Errorf("the line %q has connection %q; want %v", r.msg, c, names)
			continue
		}
		if saw[c] == nil {
			saw[c] = map[string]bool{}
		}
		saw[c][r.msg] = true
	}
	for _, name := range names {
		for _, msg := range []string{"slack: socket mode connecting", slack.ConnectedMessage, "slack: socket mode reconnecting", "slack connection ended; connecting again"} {
			if !saw[name][msg] {
				t.Errorf("connection %s logged no line %q that names it", name, msg)
			}
		}
	}
}
