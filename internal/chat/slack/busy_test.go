package slack_test

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// statusSlack answers assistant.threads.setStatus with the body it is given, in turn (the last one repeats), and keeps the
// forms it was sent.
type statusSlack struct {
	mu      sync.Mutex
	answers []string
	forms   []url.Values
	paths   []string
}

func (s *statusSlack) RoundTrip(r *http.Request) (*http.Response, error) {
	raw, _ := io.ReadAll(r.Body)
	_ = r.Body.Close()
	form, _ := url.ParseQuery(string(raw))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paths = append(s.paths, r.URL.Path)
	s.forms = append(s.forms, form)
	body := `{"ok":true}`
	if len(s.answers) > 0 {
		body = s.answers[0]
		if len(s.answers) > 1 {
			s.answers = s.answers[1:]
		}
	}
	res := &http.Response{StatusCode: 200, Status: "200 OK", Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}
	if strings.HasPrefix(body, "429") {
		res.StatusCode, res.Status, res.Body = 429, "429 Too Many Requests", io.NopCloser(strings.NewReader(""))
		res.Header = http.Header{"Retry-After": {"7"}}
	}
	return res, nil
}

func (s *statusSlack) calls() []url.Values {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]url.Values(nil), s.forms...)
}

func busyPlatform(t *testing.T, answers ...string) (*slack.Platform, *statusSlack, *bytes.Buffer) {
	t.Helper()
	rt := &statusSlack{answers: answers}
	logs := &bytes.Buffer{}
	c := slack.New(slack.Config{BotToken: secret.New("xoxb-fake-token"), AppToken: secret.New("xapp-fake-token"), APIURL: "http://slack.test/",
		HTTPClient: &http.Client{Transport: rt}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	return slack.NewPlatform(c, "ns/conn").WithLogger(slog.New(slog.NewTextHandler(logs, nil))), rt, logs
}

var busyThread = chat.Target{Conn: "ns/conn", Channel: "C1", Thread: "1700000000.000100"}

func TestSlackShowsAThreadAsBusyWithTheAssistantStatus(t *testing.T) {
	p, rt, _ := busyPlatform(t)
	if !p.Capabilities().Busy {
		t.Fatal("Capabilities().Busy is false, want Slack to say it shows a thread as busy")
	}
	var _ chat.BusyMarker = p
	if err := p.MarkBusy(t.Context(), busyThread, true); err != nil {
		t.Fatalf("MarkBusy(true) = %v", err)
	}
	if err := p.MarkBusy(t.Context(), busyThread, false); err != nil {
		t.Fatalf("MarkBusy(false) = %v", err)
	}
	got := rt.calls()
	if len(got) != 2 || rt.paths[0] != "/assistant.threads.setStatus" {
		t.Fatalf("requests = %v %v, want two to assistant.threads.setStatus", rt.paths, got)
	}
	for i, want := range []string{"is working…", ""} {
		f := got[i]
		if f.Get("channel_id") != "C1" || f.Get("thread_ts") != busyThread.Thread || !f.Has("status") || f.Get("status") != want || f.Has("loading_messages") {
			t.Errorf("request %d = %v, want channel_id, thread_ts and status %q (sent even when empty), with no loading_messages", i, f, want)
		}
	}
}

func TestSlackMarksNoThreadlessTarget(t *testing.T) {
	p, rt, _ := busyPlatform(t)
	if err := p.MarkBusy(t.Context(), chat.Target{Channel: "C1"}, true); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("MarkBusy of no thread = %v, want ErrInvalid", err)
	}
	if len(rt.calls()) != 0 {
		t.Errorf("requests = %v, want none", rt.paths)
	}
}

// A refusal that says the app or workspace cannot show the status turns it off for the connection for the process's life:
// no more calls, a log of it once, and the error the renderer reads as "stop asking".
func TestARefusalTurnsTheStatusOffOnceAndForGood(t *testing.T) {
	for _, code := range []string{"missing_scope", "not_allowed_token_type", "not_allowed", "access_denied", "feature_not_enabled", "no_permission", "unknown_method"} {
		t.Run(code, func(t *testing.T) {
			p, rt, logs := busyPlatform(t, `{"ok":false,"error":"`+code+`"}`)
			for i, busy := range []bool{true, true, false, true} {
				if err := p.MarkBusy(t.Context(), busyThread, busy); !errors.Is(err, chat.ErrNoBusy) {
					t.Fatalf("MarkBusy #%d = %v, want ErrNoBusy", i+1, err)
				}
			}
			if n := len(rt.calls()); n != 1 {
				t.Errorf("requests = %d, want the one that was refused", n)
			}
			if n := strings.Count(logs.String(), "\n"); n != 1 || !strings.Contains(logs.String(), code) || !strings.Contains(logs.String(), "assistant:write") {
				t.Errorf("log = %q, want one line that names the code and the scope", logs.String())
			}
			if strings.Contains(logs.String(), "xoxb-fake-token") {
				t.Error("the log holds the token")
			}
		})
	}
}

// Anything else may pass on another try and turns nothing off: the next call goes to Slack.
func TestOtherAnswersTurnNothingOff(t *testing.T) {
	for name, answer := range map[string]string{
		"a server error":         `{"ok":false,"error":"internal_error"}`,
		"a thread that is gone":  `{"ok":false,"error":"thread_not_found"}`,
		"a channel that is gone": `{"ok":false,"error":"channel_not_found"}`,
		"a rate limit":           "429",
	} {
		t.Run(name, func(t *testing.T) {
			p, rt, logs := busyPlatform(t, answer, `{"ok":true}`)
			err := p.MarkBusy(t.Context(), busyThread, true)
			if err == nil || errors.Is(err, chat.ErrNoBusy) {
				t.Fatalf("MarkBusy = %v, want the error of the call, not ErrNoBusy", err)
			}
			if strings.Contains(err.Error(), "xoxb-fake-token") {
				t.Errorf("the error holds the token: %v", err)
			}
			if err := p.MarkBusy(t.Context(), busyThread, true); err != nil {
				t.Errorf("the next MarkBusy = %v, want it to go to Slack and succeed", err)
			}
			if n := len(rt.calls()); n != 2 || logs.Len() != 0 {
				t.Errorf("requests = %d, log %q; want two calls and nothing logged", n, logs.String())
			}
		})
	}
}

func TestARateLimitIsAClassifiedWait(t *testing.T) {
	p, _, _ := busyPlatform(t, "429")
	err := p.MarkBusy(t.Context(), busyThread, true)
	var limited *slackgo.RateLimitedError
	if !errors.As(err, &limited) || p.Classify(chat.OpOther, err).RetryAfter == 0 {
		t.Errorf("MarkBusy = %v, want a rate limit that Classify reads as a wait", err)
	}
}

// A client that cannot make the call (a double of the API) is the same as a refusal, with no call and nothing to log.
func TestAnAPIWithoutTheCallCannotShowBusy(t *testing.T) {
	p := slack.NewPlatform(struct{ slack.API }{}, "ns/conn")
	if err := p.MarkBusy(t.Context(), busyThread, true); !errors.Is(err, chat.ErrNoBusy) {
		t.Errorf("MarkBusy = %v, want ErrNoBusy", err)
	}
}
