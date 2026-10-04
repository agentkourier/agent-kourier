package slack

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	slackgo "github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// textAPI is the API of a Platform that keeps the text of every call that carries any, and fails the next ones with err.
type textAPI struct {
	API
	sent []string
	err  error
	// failIn, if set, is the error of a call in the mode it is given, or nil for one that works; it is asked when err is nil.
	failIn func(StreamMode) error
}

func (a *textAPI) take(md string, mode StreamMode) error {
	if a.err != nil {
		return a.err
	}
	if a.failIn != nil {
		if err := a.failIn(mode); err != nil {
			return err
		}
	}
	a.sent = append(a.sent, md)
	return nil
}

func (a *textAPI) StartStream(_ context.Context, _, _, _, md string, mode StreamMode) (string, error) {
	return "1.1", a.take(md, mode)
}

func (a *textAPI) AppendStream(_ context.Context, _, _, md string, mode StreamMode) error {
	return a.take(md, mode)
}

func (a *textAPI) StopStream(_ context.Context, _, _, md string, _ json.RawMessage, _ string, mode StreamMode) error {
	return a.take(md, mode)
}

func newTextStream(t *testing.T, first string) (*textAPI, chat.Stream) {
	t.Helper()
	api := &textAPI{}
	st, err := NewPlatform(api, "t/s").Stream(t.Context(), chat.Target{Channel: "C1", Thread: "1.0"}, "U1", first)
	if err != nil {
		t.Fatal(err)
	}
	return api, st
}

// Slack reads the texts of a stream as one document, so a block that is opened by one is closed by another, and
// what is raw in the one is raw in the other.
func TestStreamIsReadAsOneDocumentAcrossItsCalls(t *testing.T) {
	api, st := newTextStream(t, "Run it:\n``")
	ctx := t.Context()
	for _, md := range []string{"`bash\nkubectl get <pod> -p='[{}]'\n<@U1", "> x\n``"} {
		if err := st.Append(ctx, md); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Stop(ctx, "`\nthen `kubectl logs <pod>` or [x](y)", chat.Message{}, "tag"); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"Run it:\n",
		"```\nkubectl get <pod> -p='[{}]'\n&lt;@U1",
		"> x\n",
		"```\nthen `kubectl logs <pod>` or \\[x\\](y)",
	}
	if !slices.Equal(api.sent, want) {
		t.Fatalf("sent %q\nwant %q", api.sent, want)
	}
}

func TestStreamAppendSendsNothingWhileAllOfItIsHeldBack(t *testing.T) {
	api, st := newTextStream(t, "``")
	if err := st.Append(t.Context(), "`"); err != nil {
		t.Fatal(err)
	}
	if want := []string{""}; !slices.Equal(api.sent, want) {
		t.Fatalf("sent %q; want only the start, with no text: %q", api.sent, want)
	}
	if err := st.Stop(t.Context(), "bash\nx\n```", chat.Message{}, "tag"); err != nil {
		t.Fatal(err)
	}
	if got := api.sent[len(api.sent)-1]; got != "```\nx\n```" {
		t.Fatalf("Stop sent %q", got)
	}
}

// A stream of an earlier run may be in a block that courier cannot see, so it escapes everything, for good.
func TestStreamAtEscapesEverything(t *testing.T) {
	api := &textAPI{}
	st := NewPlatform(api, "t/s").StreamAt(chat.Ref{Platform: PlatformName, Channel: "C1", ID: "1.1"})
	if err := st.Append(t.Context(), "```bash\nmodel = [] <@U1> `x`\n```\n"); err != nil {
		t.Fatal(err)
	}
	if err := st.Stop(t.Context(), "[x](y)", chat.Message{}, "tag"); err != nil {
		t.Fatal(err)
	}
	want := []string{"```bash\nmodel = \\[\\] &lt;@U1&gt; `x`\n```\n", "\\[x\\](y)"}
	if !slices.Equal(api.sent, want) {
		t.Fatalf("sent %q\nwant %q", api.sent, want)
	}
}

// A refused call puts back the reading from before it, and for a stream of an earlier run that reading is blind: the stream
// still escapes everything after it.
func TestStreamAtStaysBlindAfterARefusedCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"slack refused it", slackgo.SlackErrorResponse{Err: "internal_error"}},
		{"slack was busy", &slackgo.RateLimitedError{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &textAPI{err: tc.err}
			st := NewPlatform(api, "t/s").StreamAt(chat.Ref{Platform: PlatformName, Channel: "C1", ID: "1.1"})
			if err := st.Append(t.Context(), "x\n"); err == nil {
				t.Fatal("Append succeeded")
			}
			api.err = nil
			if err := st.Append(t.Context(), "```\n<x>\n"); err != nil {
				t.Fatal(err)
			}
			if want := []string{"```\n&lt;x&gt;\n"}; !slices.Equal(api.sent, want) {
				t.Fatalf("sent %q\nwant %q", api.sent, want)
			}
		})
	}
}

// A call that Slack answers with an error was not applied, so the caller's second try is the same text read from the same
// place. One that is lost without an answer may have been applied, and the stream escapes everything from then on.
func TestStreamKeepsItsReadingInStepWithSlack(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"slack refused it", slackgo.SlackErrorResponse{Err: "internal_error"}, []string{"```\n&lt;!here> x\n", "```\nz"}},
		{"slack was busy", &slackgo.RateLimitedError{}, []string{"```\n&lt;!here> x\n", "```\nz"}},
		{"it was lost", errors.New("connection reset"), []string{"```\n&lt;!here&gt; x\n", "```\nz"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api, st := newTextStream(t, "")
			ctx := t.Context()
			api.err = tc.err
			api.sent = nil
			md := "```\n<!here> x\n"
			if err := st.Append(ctx, md); err == nil {
				t.Fatal("Append succeeded")
			}
			api.err = nil
			for _, next := range []string{md, "```\nz"} {
				if err := st.Append(ctx, next); err != nil {
					t.Fatal(err)
				}
			}
			if !slices.Equal(api.sent, tc.want) {
				t.Fatalf("sent %q\nwant %q", api.sent, tc.want)
			}
		})
	}
}

// Chunks mode refused, and the markdown_text try lost with no answer: the first error, which is a refusal, is what the
// caller sees, but the text may be in Slack, and the caller sends it again. The stream must not take that for a text that
// was not applied (it would be in Slack twice and in courier's reading once, and code and prose would be swapped).
func TestStreamIsBlindWhenTheRetryInAnotherModeIsLost(t *testing.T) {
	api, st := newTextStream(t, "")
	ctx := t.Context()
	api.sent = nil
	api.failIn = func(m StreamMode) error {
		if m == ChunksMode {
			return slackgo.SlackErrorResponse{Err: "invalid_chunks"}
		}
		return errors.New("connection reset")
	}
	const md = "```\nx\n"
	if err := st.Append(ctx, md); err == nil {
		t.Fatal("Append succeeded")
	}
	api.failIn = nil
	for _, next := range []string{md, "<https://evil.test|click>\n"} {
		if err := st.Append(ctx, next); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"```\nx\n", "&lt;https://evil.test|click&gt;\n"}
	if !slices.Equal(api.sent, want) {
		t.Fatalf("sent %q\nwant %q", api.sent, want)
	}
}

// Both modes refused by Slack's answer: nothing was applied, and the reading goes back.
func TestStreamKeepsItsReadingWhenBothModesAreRefused(t *testing.T) {
	api, st := newTextStream(t, "")
	api.sent = nil
	api.failIn = func(StreamMode) error { return slackgo.SlackErrorResponse{Err: "invalid_chunks"} }
	if err := st.Append(t.Context(), "```\n<!here>\n"); err == nil {
		t.Fatal("Append succeeded")
	}
	api.failIn = nil
	if err := st.Append(t.Context(), "```\n<!here>\n"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"```\n&lt;!here>\n"}; !slices.Equal(api.sent, want) {
		t.Fatalf("sent %q\nwant %q", api.sent, want)
	}
}

// Text held back when a call is lost is not lost with it: the stream goes blind, and writes it with the next text.
func TestStreamBlindWritesWhatWasHeldBack(t *testing.T) {
	api, st := newTextStream(t, "")
	ctx := t.Context()
	api.sent = nil
	if err := st.Append(ctx, "see `kub"); err != nil {
		t.Fatal(err)
	}
	api.err = errors.New("connection reset")
	if err := st.Append(ctx, "ectl` now\n"); err == nil {
		t.Fatal("Append succeeded")
	}
	api.err = nil
	if err := st.Append(ctx, "ectl` now\n"); err != nil {
		t.Fatal(err)
	}
	if want := []string{"see ", "`kubectl` now\n"}; !slices.Equal(api.sent, want) {
		t.Fatalf("sent %q\nwant %q", api.sent, want)
	}
}
