package fakeslack_test

import (
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// Shared helpers for the content tests.
func chars(n int) string { return strings.Repeat("x", n) }

func mrkdwn(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject("mrkdwn", s, false, false)
}

func plain(s string) *slack.TextBlockObject {
	return slack.NewTextBlockObject("plain_text", s, false, false)
}

func dividers(n int) slack.MsgOption {
	blocks := make([]slack.Block, n)
	for i := range blocks {
		blocks[i] = slack.NewDividerBlock()
	}
	return slack.MsgOptionBlocks(blocks...)
}

// expectContent sends opts through chat.postMessage and chat.update and checks
// both return the error code want, or succeed when want is empty.
func expectContent(t *testing.T, opts []slack.MsgOption, want string) {
	t.Helper()
	_, api := newFake(t)
	ts := post(t, api, text("seed"))
	_, _, postErr := api.PostMessage("C1", opts...)
	_, _, _, updateErr := api.UpdateMessage("C1", ts, opts...)
	for method, err := range map[string]error{"chat.postMessage": postErr, "chat.update": updateErr} {
		if (err == nil) != (want == "") || (err != nil && err.Error() != want) {
			t.Errorf("%s = %v; want %q", method, err, want)
		}
	}
}

// newStream starts a stream in a thread and returns its ts.
func newStream(t *testing.T) (*fakeslack.Server, *slack.Client, string) {
	t.Helper()
	fake, api := newFake(t)
	ts, err := startIn(api, "C1", post(t, api, text("q")), recipients()...)
	if err != nil {
		t.Fatalf("StartStream: %v", err)
	}
	return fake, api, ts
}

// Each rule is checked at its boundary, through slack-go, on every method that
// writes message content.
func TestContentLimitsMatchSlack(t *testing.T) {
	section := func(s string) slack.MsgOption {
		return slack.MsgOptionBlocks(slack.NewSectionBlock(mrkdwn(s), nil, nil))
	}
	field := func(s string) slack.MsgOption {
		return slack.MsgOptionBlocks(slack.NewSectionBlock(nil, []*slack.TextBlockObject{mrkdwn(s)}, nil))
	}
	header := func(s string) slack.MsgOption { return slack.MsgOptionBlocks(slack.NewHeaderBlock(plain(s))) }

	for name, tc := range map[string]struct {
		opts []slack.MsgOption
		want string
	}{
		"50 blocks":                {[]slack.MsgOption{dividers(50)}, ""},
		"51 blocks":                {[]slack.MsgOption{dividers(51)}, "invalid_blocks"},
		"section text at limit":    {[]slack.MsgOption{section(chars(3000))}, ""},
		"section text over limit":  {[]slack.MsgOption{section(chars(3001))}, "invalid_blocks"},
		"section field at limit":   {[]slack.MsgOption{field(chars(2000))}, ""},
		"section field over limit": {[]slack.MsgOption{field(chars(2001))}, "invalid_blocks"},
		"header at limit":          {[]slack.MsgOption{header(chars(150))}, ""},
		"header one character":     {[]slack.MsgOption{header("h")}, ""},
		"header empty":             {[]slack.MsgOption{header("")}, "invalid_blocks"},
		"header over limit":        {[]slack.MsgOption{header(chars(151))}, "invalid_blocks"},
		"text at limit":            {[]slack.MsgOption{text(chars(40000))}, ""},
		"text over limit":          {[]slack.MsgOption{text(chars(40001))}, "msg_too_long"},
		"no text, no blocks":       {nil, "no_text"},
		"empty blocks, no text":    {[]slack.MsgOption{slack.MsgOptionBlocks()}, "no_text"},
		"empty blocks with text":   {[]slack.MsgOption{slack.MsgOptionBlocks(), text("hi")}, ""},
	} {
		t.Run(name, func(t *testing.T) { expectContent(t, tc.opts, tc.want) })
	}

	t.Run("chat.stopStream", func(t *testing.T) {
		_, api, ts := newStream(t)
		if _, _, err := api.StopStream("C1", ts, dividers(51)); err == nil || err.Error() != "invalid_blocks" {
			t.Fatalf("StopStream with 51 blocks = %v; want invalid_blocks", err)
		}
	})
}

func TestServerIsAnHTTPHandler(t *testing.T) {
	h := fakeslack.NewHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	api := slack.New(testToken, slack.OptionAPIURL(srv.URL+"/"))

	post(t, api, text("via handler"))
	if msgs := h.Messages("C1"); len(msgs) != 1 || msgs[0].Text != "via handler" {
		t.Fatalf("messages = %+v", msgs)
	}
	if h.URL() != "" {
		t.Fatalf("a handler-only Server has URL %q; want none", h.URL())
	}
	h.Close() // a no-op, not a panic
}

func TestTimestampsComeFromTheClock(t *testing.T) {
	var micros atomic.Int64 // read by the server's goroutines
	micros.Store(time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC).UnixMicro())
	_, api := newFake(t, fakeslack.WithClock(func() time.Time { return time.UnixMicro(micros.Load()) }))
	first := post(t, api, text("a"))
	second := post(t, api, text("b")) // the clock has not moved, so the ts steps by 1µs
	if first != "1790683200.000000" || second != "1790683200.000001" {
		t.Fatalf("ts = %s, %s; want 1790683200.000000, 1790683200.000001", first, second)
	}
	micros.Add(time.Hour.Microseconds())
	third := post(t, api, text("c"))
	if third != "1790686800.000000" {
		t.Fatalf("ts after the clock moved = %s; want 1790686800.000000", third)
	}

	// Reconciliation reads "recent history" by wall clock (spec 8.6), so a
	// window measured from the clock must find the recent posts and no others.
	window := func(oldest time.Duration) []slack.Message {
		now := time.UnixMicro(micros.Load())
		return history(t, api, slack.GetConversationHistoryParameters{
			Oldest: fmtTS(now.Add(-oldest)), Latest: fmtTS(now.Add(time.Minute))}).Messages
	}
	wantTS(t, "last 2h", window(2*time.Hour), third, second, first)
	wantTS(t, "last 5m", window(5*time.Minute), third)
}

// fmtTS renders t as a Slack ts, "seconds.microseconds".
func fmtTS(t time.Time) string {
	return fmt.Sprintf("%d.%06d", t.UnixMicro()/1_000_000, t.UnixMicro()%1_000_000)
}

func TestInspectionReturnsCopies(t *testing.T) {
	fake, api := newFake(t)
	meta := slack.SlackMetadata{EventType: "agentkourier_message", EventPayload: map[string]any{
		"outbox_id": "ob-1", "nested": map[string]any{"k": "v"}}}
	ts := post(t, api, text("x"), slack.MsgOptionMetadata(meta), slack.MsgOptionBlocks(slack.NewDividerBlock()))

	// Tamper with a Messages result and with a Thread result, each in its
	// payload, its nested payload, its text, and a byte of its blocks.
	msgs := fake.Messages("C1")
	thread := fake.Thread("C1", ts)
	for _, got := range []*fakeslack.Message{&msgs[0], &thread[0]} {
		got.Metadata.EventPayload["outbox_id"] = "tampered"
		got.Metadata.EventPayload["nested"].(map[string]any)["k"] = "tampered"
		got.Blocks[0] = 'X'
		got.Text = "tampered"
	}
	for _, again := range []fakeslack.Message{fake.Messages("C1")[0], fake.Thread("C1", ts)[0]} {
		if again.Text != "x" || again.Metadata.EventPayload["outbox_id"] != "ob-1" ||
			again.Metadata.EventPayload["nested"].(map[string]any)["k"] != "v" || again.Blocks[0] != '[' {
			t.Fatalf("mutating a returned message changed the store: %+v", again)
		}
	}
}
