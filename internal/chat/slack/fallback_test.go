package slack_test

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// fallbackWorld is a Platform over a fake Slack that refuses the chunks mode with a code, with a log that can be read.
func fallbackWorld(t *testing.T, code string) (*slack.Platform, chat.Target, *counter, *bytes.Buffer, func() int) {
	t.Helper()
	c, fake, calls, root := updateClient(t)
	fake.RefuseChunkMode(code)
	logs := &bytes.Buffer{}
	p := slack.NewPlatform(c, "agent-kourier-system/slack-main").WithLogger(slog.New(slog.NewTextHandler(logs, nil)))
	warns := func() int { return strings.Count(logs.String(), "level=WARN") }
	return p, chat.Target{Channel: "C1", Thread: root}, calls, logs, warns
}

// TestASlackThatRefusesTheChunksModeStillStreamsInMarkdownMode: the chunks mode is a guess, and a Slack that refuses it
// must not cost the reply. The start is made once more in markdown_text mode, the stream carries the text through
// the stop in that mode, it shows no steps, and the platform is in that mode from then on: the next stream starts there,
// with no failed call first, and the refusal is logged once, at WARN.
func TestASlackThatRefusesTheChunksModeStillStreamsInMarkdownMode(t *testing.T) {
	for _, code := range []string{"invalid_chunks", "streaming_mode_mismatch", "cannot_provide_both_markdown_text_and_chunks", "invalid_arguments"} {
		t.Run(code, func(t *testing.T) {
			p, target, calls, logs, warns := fallbackWorld(t, code)
			ctx := t.Context()
			st, err := p.Stream(ctx, target, "U1", "Hel")
			if err != nil {
				t.Fatalf("Stream = %v; want a stream, started in the other mode", err)
			}
			if err := st.Append(ctx, "lo"); err != nil {
				t.Fatal(err)
			}
			if err := st.Step(ctx, chat.Step{ID: "s1", Title: "search"}); !errors.Is(err, chat.ErrNoSteps) {
				t.Errorf("Step on a stream in markdown_text mode = %v; want ErrNoSteps", err)
			}
			if err := st.Stop(ctx, "!", chat.Message{}, "ob-1"); err != nil {
				t.Fatal(err)
			}
			starts := calls.sent("/chat.startStream")
			if len(starts) != 2 || !starts[0].Has("chunks") || starts[1].Has("chunks") || starts[1].Get("markdown_text") != "Hel" {
				t.Fatalf("starts %v; want the one in chunks mode that was refused, and the one in markdown_text mode", starts)
			}
			for _, path := range []string{"/chat.appendStream", "/chat.stopStream"} {
				if f := calls.sent(path)[0]; f.Has("chunks") || f.Get("markdown_text") == "" {
					t.Errorf("%s sent %v; want the text in markdown_text, the mode the stream is in", path, f)
				}
			}
			second, err := p.Stream(ctx, target, "U1", "Again")
			if err != nil {
				t.Fatal(err)
			}
			if n := len(calls.sent("/chat.startStream")); n != 3 {
				t.Errorf("%d starts in all; want one more for the second stream, with no refused call first", n)
			}
			if err := second.Stop(ctx, "", chat.Message{}, "ob-2"); err != nil {
				t.Fatal(err)
			}
			if warns() != 1 || !strings.Contains(logs.String(), code) {
				t.Errorf("%d warnings, log %q; want one, naming Slack's answer", warns(), logs)
			}
		})
	}
}

// A start with no text carries no mode, so it is not refused for one. The first text the stream is sent may be what
// Slack refuses, and is sent again in the other mode, and a step before it, which Slack refuses too, is the
// renderer's to end the steps for, and no reason to leave the mode.
func TestAStreamStartedWithNoTextFallsBackAtItsFirstText(t *testing.T) {
	p, target, calls, _, warns := fallbackWorld(t, "invalid_chunks")
	ctx := t.Context()
	st, err := p.Stream(ctx, target, "U1", "")
	if err != nil {
		t.Fatalf("Stream = %v; want a stream: a start with no text is no chunks request", err)
	}
	if err := st.Step(ctx, chat.Step{ID: "s1", Title: "search"}); err == nil {
		t.Fatal("a step was taken by a Slack that refuses chunks")
	}
	if warns() != 0 {
		t.Fatal("a refused step moved the stream to the other mode: a step is no text")
	}
	if err := st.Append(ctx, "Hello"); err != nil {
		t.Fatalf("Append = %v; want the text, sent again in markdown_text", err)
	}
	if err := st.Stop(ctx, "", chat.Message{}, "ob-1"); err != nil {
		t.Fatal(err)
	}
	if n := len(calls.sent("/chat.appendStream")); n != 3 { // the step, the text in chunks, and the text again
		t.Errorf("%d appends, want 3", n)
	}
	if warns() != 1 {
		t.Errorf("%d warnings; want one", warns())
	}
}

// A stream of another run that started in the mode this process has not latched finds it out at its first text.
func TestAStreamOfAnotherRunInMarkdownModeIsStoppedByItsFirstTextInTheOtherMode(t *testing.T) {
	c, _, _, root := updateClient(t)
	ctx := t.Context()
	ts, err := c.StartStream(ctx, "C1", root, "U1", "Hel", slack.MarkdownMode)
	if err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	p := slack.NewPlatform(c, "agent-kourier-system/slack-main").WithLogger(slog.New(slog.NewTextHandler(logs, nil)))
	at := p.StreamAt(p.RefAt("C1", root, ts))
	if err := at.Stop(ctx, "lo", chat.Message{}, "ob-1"); err != nil {
		t.Fatalf("Stop = %v; want the text, sent in the stream's own mode after the first try was a mismatch", err)
	}
	if strings.Count(logs.String(), "level=WARN") != 1 {
		t.Errorf("log %q; want one warning", logs)
	}
}

// An ordinary failure is no refusal of the mode: a timeout or a rate limit on a start in chunks mode is not tried again in
// markdown_text, and nothing is latched or logged, so the next stream starts in chunks mode as before. The same holds for
// the text of a stream that is already running.
func TestAnOrdinaryFailureLeavesTheChunksModeAlone(t *testing.T) {
	for name, fail := range map[string]func(*counter){
		"a timeout":    func(c *counter) { c.err = errors.New("i/o timeout") },
		"a rate limit": func(c *counter) { c.status, c.body = http.StatusTooManyRequests, "" },
	} {
		heal := func(c *counter) {
			c.mu.Lock()
			c.err, c.status, c.body = nil, 0, ""
			c.mu.Unlock()
		}
		t.Run(name+" on a start", func(t *testing.T) {
			c, _, calls, root := updateClient(t)
			ctx := t.Context()
			if _, err := c.TeamID(ctx); err != nil {
				t.Fatal(err)
			}
			logs := &bytes.Buffer{}
			p := slack.NewPlatform(c, "agent-kourier-system/slack-main").WithLogger(slog.New(slog.NewTextHandler(logs, nil)))
			target := chat.Target{Channel: "C1", Thread: root}
			fail(calls)
			if _, err := p.Stream(ctx, target, "U1", "x"); err == nil {
				t.Fatal("Stream succeeded through a failing transport")
			}
			heal(calls)
			if n := len(calls.sent("/chat.startStream")); n > 1 {
				t.Fatalf("%d start requests; want no second try in the other mode", n)
			}
			if _, err := p.Stream(ctx, target, "U1", "x"); err != nil {
				t.Fatal(err)
			}
			starts := calls.sent("/chat.startStream")
			if last := starts[len(starts)-1]; !last.Has("chunks") || logs.Len() != 0 {
				t.Fatalf("the next start was %v, log %q; want it in chunks mode, and nothing logged", last, logs)
			}
		})
		t.Run(name+" on an append", func(t *testing.T) {
			c, _, calls, root := updateClient(t)
			ctx := t.Context()
			logs := &bytes.Buffer{}
			p := slack.NewPlatform(c, "agent-kourier-system/slack-main").WithLogger(slog.New(slog.NewTextHandler(logs, nil)))
			st, err := p.Stream(ctx, chat.Target{Channel: "C1", Thread: root}, "U1", "Hel")
			if err != nil {
				t.Fatal(err)
			}
			fail(calls)
			if err := st.Append(ctx, "lo"); err == nil {
				t.Fatal("Append succeeded through a failing transport")
			}
			heal(calls)
			if n := len(calls.sent("/chat.appendStream")); n > 1 {
				t.Fatalf("%d append requests; want no second try in the other mode", n)
			}
			if err := st.Append(ctx, "lo"); err != nil {
				t.Fatal(err)
			}
			if appends := calls.sent("/chat.appendStream"); !appends[len(appends)-1].Has("chunks") || logs.Len() != 0 {
				t.Fatalf("the stream left chunks mode, or logged: %q", logs)
			}
		})
	}
}

// A start that is refused in both modes is not the chunks mode's trouble: the platform stays in it, and the error is
// returned, with no latch.
func TestAStartRefusedInBothModesDoesNotLatchTheFallback(t *testing.T) {
	c, _, calls, root := updateClient(t)
	ctx := t.Context()
	if _, err := c.TeamID(ctx); err != nil {
		t.Fatal(err)
	}
	logs := &bytes.Buffer{}
	p := slack.NewPlatform(c, "agent-kourier-system/slack-main").WithLogger(slog.New(slog.NewTextHandler(logs, nil)))
	calls.status, calls.body = http.StatusOK, `{"ok":false,"error":"invalid_arguments"}`
	if _, err := p.Stream(ctx, chat.Target{Channel: "C1", Thread: root}, "U1", "x"); err == nil {
		t.Fatal("Stream succeeded though Slack refused both")
	}
	calls.mu.Lock()
	calls.status, calls.body = 0, ""
	calls.mu.Unlock()
	if _, err := p.Stream(ctx, chat.Target{Channel: "C1", Thread: root}, "U1", "x"); err != nil {
		t.Fatal(err)
	}
	starts := calls.sent("/chat.startStream")
	if len(starts) != 3 || !starts[2].Has("chunks") || logs.Len() != 0 {
		t.Fatalf("%d starts, the last in chunks mode: %v, log %q; want the next stream to start in chunks mode again, and nothing logged", len(starts), len(starts) == 3 && starts[2].Has("chunks"), logs)
	}
}
