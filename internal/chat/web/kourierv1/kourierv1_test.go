package kourierv1_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/kourierv1"
	"github.com/agentkourier/agent-kourier/internal/limitsdoc"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// fakeKey is the test's signing secret: a fake constant.
const fakeKey = "fake-kourier-signing-secret"

var epoch = time.Unix(1_800_000_000, 0)

func newProfile(t *testing.T) *kourierv1.Profile {
	t.Helper()
	p, err := kourierv1.New(secret.New(fakeKey))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestNewRefusesNoSecret(t *testing.T) {
	if _, err := kourierv1.New(secret.Value{}); err == nil {
		t.Error("a profile with no signing secret was made")
	}
}

func TestVerifySignatureTakesItsOwnSignatureAndNothingElse(t *testing.T) {
	body := []byte(`{"a":1}`)
	ts, sig := kourierv1.Sign([]byte(fakeKey), epoch, body)
	if err := kourierv1.VerifySignature([]byte(fakeKey), epoch, ts, sig, body); err != nil {
		t.Fatalf("its own signature: %v", err)
	}
	for _, tc := range []struct {
		name         string
		key, ts, sig string
		body         []byte
		now          time.Time
	}{
		{"another key", "other-fake-secret", ts, sig, body, epoch},
		{"another body", fakeKey, ts, sig, []byte(`{"a":2}`), epoch},
		{"another timestamp", fakeKey, "1800000001", sig, body, epoch},
		{"no signature", fakeKey, ts, "", body, epoch},
		{"no timestamp", fakeKey, "", sig, body, epoch},
		{"a timestamp that is no number", fakeKey, "soon", sig, body, epoch},
		{"no version prefix", fakeKey, ts, strings.TrimPrefix(sig, "v1="), body, epoch},
		{"another version", fakeKey, ts, "v0=" + strings.TrimPrefix(sig, "v1="), body, epoch},
		{"not hex", fakeKey, ts, "v1=zz", body, epoch},
		{"stale", fakeKey, ts, sig, body, epoch.Add(kourierv1.MaxSkew + time.Second)},
		{"from the future", fakeKey, ts, sig, body, epoch.Add(-kourierv1.MaxSkew - time.Second)},
	} {
		if err := kourierv1.VerifySignature([]byte(tc.key), tc.now, tc.ts, tc.sig, tc.body); err == nil {
			t.Errorf("%s: verified", tc.name)
		}
	}
	for _, edge := range []time.Duration{kourierv1.MaxSkew, -kourierv1.MaxSkew} {
		if err := kourierv1.VerifySignature([]byte(fakeKey), epoch.Add(edge), ts, sig, body); err != nil {
			t.Errorf("at the edge of the skew (%s): %v", edge, err)
		}
	}
}

// The signature is the hex HMAC-SHA256 of "v1:", the timestamp, ":" and the body, under the signing secret, as the wire contract has it.
func TestTheSignatureIsTheDocumentedHMAC(t *testing.T) {
	ts, sig := kourierv1.Sign([]byte("key"), time.Unix(1_700_000_000, 0), []byte("body"))
	// HMAC-SHA256 under "key" of "v1:1700000000:body", computed outside Go: printf 'v1:1700000000:body' | openssl dgst -sha256 -hmac key
	const want = "v1=7b51d71fdbcb3856f97c81ab6db93404979061627c704b839a9d98b4936e2df5"
	if ts != "1700000000" || sig != want {
		t.Errorf("Sign gave %q, %q; want 1700000000, %q", ts, sig, want)
	}
}

func TestDecodeReadsTheSchema(t *testing.T) {
	p := newProfile(t)
	for _, tc := range []struct {
		name, body string
		want       web.Event
	}{
		{"a message that opens a thread", `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hello"}`,
			web.Event{Channel: "ch-1", ID: "m-1", User: "u-1", Text: "hello", Mentioned: true}},
		{"a reply in a thread", `{"v":1,"channel":"ch-1","thread":"m-1","id":"m-2","user":"u-2","text":"and"}`,
			web.Event{Channel: "ch-1", Thread: "m-1", ID: "m-2", User: "u-2", Text: "and"}},
		{"a reply that mentions Agent Kourier", `{"v":1,"channel":"ch-1","thread":"m-1","id":"m-3","user":"u-2","text":"and","mention":true}`,
			web.Event{Channel: "ch-1", Thread: "m-1", ID: "m-3", User: "u-2", Text: "and", Mentioned: true}},
		{"a UUID", `{"v":1,"channel":"0f8fad5b-d9cb-469f-a165-70867728950e","id":"7c9e6679-7425-40de-944b-e07fc1f90ae7","user":"user.name_1","text":""}`,
			web.Event{Channel: "0f8fad5b-d9cb-469f-a165-70867728950e", ID: "7c9e6679-7425-40de-944b-e07fc1f90ae7", User: "user.name_1", Mentioned: true}},
		{"trailing white space", "{\"v\":1,\"channel\":\"ch-1\",\"id\":\"m-1\",\"user\":\"u-1\",\"text\":\"hi\"}\n  ",
			web.Event{Channel: "ch-1", ID: "m-1", User: "u-1", Text: "hi", Mentioned: true}},
	} {
		ev, ok, err := p.Decode(nil, []byte(tc.body))
		if err != nil || !ok || ev != tc.want {
			t.Errorf("%s: %+v, %t, %v; want %+v", tc.name, ev, ok, err, tc.want)
		}
	}
}

func TestDecodeRefusesWhatIsNotTheSchema(t *testing.T) {
	p := newProfile(t)
	long := strings.Repeat("a", 65)
	for name, body := range map[string]string{
		"not JSON":                    `hello`,
		"null":                        `null`,
		"an array":                    `[]`,
		"an unknown field":            `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hi","url":"http://evil.example/"}`,
		"another version":             `{"v":2,"channel":"ch-1","id":"m-1","user":"u-1","text":"hi"}`,
		"no version":                  `{"channel":"ch-1","id":"m-1","user":"u-1","text":"hi"}`,
		"no channel":                  `{"v":1,"id":"m-1","user":"u-1","text":"hi"}`,
		"no id":                       `{"v":1,"channel":"ch-1","user":"u-1","text":"hi"}`,
		"no user":                     `{"v":1,"channel":"ch-1","id":"m-1","text":"hi"}`,
		"a channel with no digit":     `{"v":1,"channel":"general","id":"m-1","user":"u-1","text":"hi"}`,
		"a repository's name":         `{"v":1,"channel":"octo/repo1","id":"m-1","user":"u-1","text":"hi"}`,
		"a colon in the id":           `{"v":1,"channel":"ch-1","id":"a:b","user":"u-1","text":"hi"}`,
		"an id too long":              `{"v":1,"channel":"ch-1","id":"` + long + `","user":"u-1","text":"hi"}`,
		"a channel too long":          `{"v":1,"channel":"1` + long + `","id":"m-1","user":"u-1","text":"hi"}`,
		"a thread of another form":    `{"v":1,"channel":"ch-1","thread":"a b","id":"m-1","user":"u-1","text":"hi"}`,
		"a thread that is its own id": `{"v":1,"channel":"ch-1","thread":"m-1","id":"m-1","user":"u-1","text":"hi"}`,
		"a user with a space":         `{"v":1,"channel":"ch-1","id":"m-1","user":"u 1","text":"hi"}`,
		"text that is no string":      `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":1}`,
		"two documents":               `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hi"}{"v":1}`,
		"a stray bracket after":       `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hi"}]`,
	} {
		_, _, err := p.Decode(nil, []byte(body))
		if err == nil || errors.Is(err, web.ErrTooLarge) {
			t.Errorf("%s: %v, want an error of the schema", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "evil") {
			t.Errorf("%s: the error quotes the body: %v", name, err)
		}
	}
}

func TestDecodeBoundsTheBody(t *testing.T) {
	p := newProfile(t)
	body := `{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"`
	pad := func(n int) []byte { return []byte(body + strings.Repeat("a", n-len(body)-2) + `"}`) }
	if _, ok, err := p.Decode(nil, pad(kourierv1.MaxBody)); err != nil || !ok {
		t.Errorf("a body of the bound: %t, %v", ok, err)
	}
	if _, _, err := p.Decode(nil, pad(kourierv1.MaxBody+1)); !errors.Is(err, web.ErrTooLarge) {
		t.Errorf("a body past the bound: %v, want web.ErrTooLarge", err)
	}
}

func TestTheProfilePassesTheProfileSuite(t *testing.T) {
	sign := func(key string, body []byte, at time.Time) http.Header {
		ts, sig := kourierv1.Sign([]byte(key), at, body)
		h := http.Header{}
		h.Set(kourierv1.HeaderTimestamp, ts)
		h.Set(kourierv1.HeaderSignature, sig)
		return h
	}
	chattest.ProfileConformance(t, chattest.ProfileSubject{
		Profile: newProfile(t),
		Secret:  fakeKey,
		Sign:    sign,
		MaxSkew: kourierv1.MaxSkew,
		MaxBody: kourierv1.MaxBody,
		Events: []chattest.ProfileEvent{
			{Name: "a message that opens a thread", Body: []byte(`{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hello <!channel>"}`),
				Want: &web.Event{Channel: "ch-1", ID: "m-1", User: "u-1", Text: "hello <!channel>", Mentioned: true}},
			{Name: "a reply in a thread", Body: []byte(`{"v":1,"channel":"ch-1","thread":"m-1","id":"m-2","user":"u-2","text":"and"}`),
				Want: &web.Event{Channel: "ch-1", Thread: "m-1", ID: "m-2", User: "u-2", Text: "and"}},
			{Name: "a reply that mentions Agent Kourier", Body: []byte(`{"v":1,"channel":"ch-1","thread":"m-1","id":"m-3","user":"u-2","text":"and","mention":true}`),
				Want: &web.Event{Channel: "ch-1", Thread: "m-1", ID: "m-3", User: "u-2", Text: "and", Mentioned: true}},
		},
		Malformed: []chattest.ProfileCase{
			{Name: "an unknown field", Body: []byte(`{"v":1,"channel":"ch-1","id":"m-1","user":"u-1","text":"hi","url":"http://evil.example/"}`)},
			{Name: "another version", Body: []byte(`{"v":2,"channel":"ch-1","id":"m-1","user":"u-1","text":"hi"}`)},
			{Name: "a channel of another form", Body: []byte(`{"v":1,"channel":"general","id":"m-1","user":"u-1","text":"hi"}`)},
			{Name: "a colon in the id", Body: []byte(`{"v":1,"channel":"ch-1","id":"a:b","user":"u-1","text":"hi"}`)},
		},
		CheckReply: func(want web.Reply, h http.Header, body []byte) (string, error) {
			if err := kourierv1.VerifySignature([]byte(fakeKey), epoch, h.Get(kourierv1.HeaderTimestamp), h.Get(kourierv1.HeaderSignature), body); err != nil {
				return "", err
			}
			var env kourierv1.Outbound
			if err := json.Unmarshal(body, &env); err != nil {
				return "", fmt.Errorf("not the envelope: %w", err)
			}
			wantEnv := kourierv1.Outbound{V: kourierv1.OutboundVersion, Kind: want.Kind, Connection: want.Connection, Channel: want.Channel,
				Thread: want.Thread, ID: want.ID, Message: want.Message}
			if !reflect.DeepEqual(env, wantEnv) {
				return "", fmt.Errorf("the envelope is %+v, want %+v", env, wantEnv)
			}
			key := h.Get(kourierv1.HeaderIdempotencyKey)
			if want.Key != "" && key != want.Key {
				return "", fmt.Errorf("the idempotency key is %q, want the outbox's tag %q", key, want.Key)
			}
			return key, nil
		},
	})
}

var reply = web.Reply{Kind: web.ReplyPost, Connection: "ns/web", Channel: "ch-1", Thread: "m-1", ID: "outbox-42", Key: "outbox-42",
	Message: chat.Say(chat.Message{Fallback: "done", Parts: []chat.Part{chat.Header{Text: "Result"}, chat.Body{Markdown: "**ok**"}}})}

func TestEncodeSignsTheEnvelope(t *testing.T) {
	p := newProfile(t)
	req, err := p.Encode(reply, epoch)
	if err != nil {
		t.Fatal(err)
	}
	h := req.Header
	if err := kourierv1.VerifySignature([]byte(fakeKey), epoch, h.Get(kourierv1.HeaderTimestamp), h.Get(kourierv1.HeaderSignature), req.Body); err != nil {
		t.Errorf("the reply's signature: %v", err)
	}
	if h.Get(kourierv1.HeaderIdempotencyKey) != "outbox-42" || h.Get("Content-Type") != "application/json" {
		t.Errorf("the headers are %v, want the key and JSON", h)
	}
	var env kourierv1.Outbound
	if err := json.Unmarshal(req.Body, &env); err != nil {
		t.Fatal(err)
	}
	want := kourierv1.Outbound{V: kourierv1.OutboundVersion, Kind: "post", Connection: "ns/web", Channel: "ch-1", Thread: "m-1", ID: "outbox-42", Message: reply.Message}
	if !reflect.DeepEqual(env, want) {
		t.Errorf("the envelope is %+v, want %+v", env, want)
	}
	if strings.Contains(string(req.Body), fakeKey) {
		t.Error("the body holds the secret")
	}
}

// Every text the agent or a person wrote reaches the receiver inert: a literal field is plain text, cleaned, on one line where its
// place is one, and cut to the limit; the body's markdown is CommonMark in which nothing links, mentions or is HTML.
func TestEncodeMakesTheTextInert(t *testing.T) {
	p := newProfile(t)
	in := chat.Say(chat.Message{Fallback: "done\u200b <!channel>", Parts: []chat.Part{
		chat.Header{Text: "a\nb\u202e"},
		chat.Body{Markdown: `[x](http://evil.example) ![i](http://evil.example/i.png) <b>hi</b> <!channel> \[ok]`},
		chat.Context{Text: "c\td"},
		chat.ToolLine{Name: "rm\n-rf", Summary: "/"},
		chat.Mention{User: "<@U1>", Text: "answered"},
		chat.Mention{User: "u-7", Text: "answered"},
		chat.Buttons{Buttons: []chat.Button{{ID: "b", Label: "go\nnow", Value: "v"}}},
		chat.Steps{Steps: []chat.Step{{ID: "s", Title: "a step"}}},
	}})
	before, _ := json.Marshal(in)
	r := reply
	r.Message = in
	req, err := p.Encode(r, epoch)
	if err != nil {
		t.Fatal(err)
	}
	var env kourierv1.Outbound
	if err := json.Unmarshal(req.Body, &env); err != nil {
		t.Fatal(err)
	}
	want := chat.Message{Fallback: "done <!channel>", Parts: []chat.Part{
		chat.Header{Text: "a b"},
		chat.Body{Markdown: `\[x\](http://evil.example) !\[i\](http://evil.example/i.png) &lt;b>hi&lt;/b> &lt;!channel> \\\[ok\]`},
		chat.Context{Text: "c d"},
		chat.ToolLine{Name: "rm -rf", Summary: "/"},
		chat.Mention{Text: "answered"},
		chat.Mention{User: "u-7", Text: "answered"},
		chat.Buttons{Buttons: []chat.Button{{ID: "b", Label: "go now", Value: "v"}}},
	}}
	if !reflect.DeepEqual(*env.Message.Message, want) {
		t.Errorf("the message sent is\n%+v\nwant\n%+v", *env.Message.Message, want)
	}
	if after, _ := json.Marshal(in); string(after) != string(before) {
		t.Error("Encode changed the caller's message")
	}
}

func TestEncodeCutsATextToTheLimit(t *testing.T) {
	p := newProfile(t)
	r := reply
	r.Message = chat.Say(chat.Message{Fallback: strings.Repeat("f", kourierv1.MaxText+10), Parts: []chat.Part{chat.Body{Markdown: strings.Repeat("[", kourierv1.MaxText+10)}}})
	req, err := p.Encode(r, epoch)
	if err != nil {
		t.Fatal(err)
	}
	var env kourierv1.Outbound
	if err := json.Unmarshal(req.Body, &env); err != nil {
		t.Fatal(err)
	}
	m := env.Message.Message
	if n := chat.Units(m.Fallback); n != kourierv1.MaxText || !strings.HasSuffix(m.Fallback, chat.Truncated) {
		t.Errorf("the fallback is %d units, want %d ending in the marker", n, kourierv1.MaxText)
	}
	// The body is counted as it is sent, its escapes included: each "[" is sent as two units, "\[", and is not split.
	body := m.Parts[0].(chat.Body).Markdown
	if n := chat.Units(body); n > kourierv1.MaxText || n < kourierv1.MaxText-1 || !strings.HasSuffix(body, chat.Truncated) {
		t.Errorf("the body is %d units, want %d or one fewer, ending in the marker", n, kourierv1.MaxText)
	}
}

// The code in a body reaches the receiver as the agent wrote it, and a cut in a block closes it before the marker, which the
// receiver shows as text.
func TestEncodeSendsCodeAsWritten(t *testing.T) {
	p := newProfile(t)
	for _, tc := range []struct{ name, md, want string }{
		{"a block and a span", "Pods <x>:\n```\nweb-0   <none>   [ready]\n```\nRun `C:\\x\\[a]`.",
			"Pods &lt;x>:\n```\nweb-0   <none>   [ready]\n```\nRun `C:\\x\\[a]`."},
		{"a block cut", "```\n" + strings.Repeat("<", kourierv1.MaxText) + "\n```",
			"```\n" + strings.Repeat("<", kourierv1.MaxText-4-5-chat.Units(chat.Truncated)) + "\n```\n" + chat.Truncated},
	} {
		r := reply
		r.Message = chat.Say(chat.Message{Fallback: "f", Parts: []chat.Part{chat.Body{Markdown: tc.md}}})
		req, err := p.Encode(r, epoch)
		if err != nil {
			t.Fatal(err)
		}
		var env kourierv1.Outbound
		if err := json.Unmarshal(req.Body, &env); err != nil {
			t.Fatal(err)
		}
		if got := env.Message.Message.Parts[0].(chat.Body).Markdown; got != tc.want {
			t.Errorf("%s: the body sent is %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestEncodeChecksTheUsersOfAnEnding(t *testing.T) {
	p := newProfile(t)
	for by, want := range map[string]string{"u-7": "u-7", "<@U1>": "", "a b": ""} {
		r := reply
		r.Message = chat.End(chat.Ending{Outcome: chat.Answered, By: by, Prompt: chat.Prompt{ID: "ix", Hint: "h\nint", Questions: []chat.Question{{Text: "which\n", Choices: []string{"a\nb"}}}}, Answers: [][]string{{"a\nb"}}})
		req, err := p.Encode(r, epoch)
		if err != nil {
			t.Fatal(err)
		}
		var env kourierv1.Outbound
		if err := json.Unmarshal(req.Body, &env); err != nil {
			t.Fatal(err)
		}
		e := env.Message.Ending
		if e.By != want || e.Prompt.Hint != "h int" || e.Prompt.Questions[0].Text != "which" || e.Prompt.Questions[0].Choices[0] != "a b" || e.Answers[0][0] != "a b" {
			t.Errorf("%q: the ending sent is %+v, want By %q and every text cleaned", by, e, want)
		}
	}
}

// A reply whose envelope is over the bound is refused as too large: Fits says so of a prompt before it is stored.
func TestEncodeRefusesAReplyOverTheBound(t *testing.T) {
	p := newProfile(t)
	r := reply
	r.Message = chat.Ask(tooLarge())
	if _, err := p.Encode(r, epoch); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("a prompt of %d questions: %v, want chat.ErrTooLarge", len(r.Message.Prompt.Questions), err)
	}
}

// tooLarge is a prompt whose envelope is over the bound, each of its questions within its limit.
func tooLarge() chat.Prompt {
	q := chat.Question{Text: strings.Repeat("x", kourierv1.MaxText)}
	n := kourierv1.MaxReplyBody/kourierv1.MaxText + 1
	p := chat.Prompt{ID: "ix-big"}
	for range n {
		p.Questions = append(p.Questions, q)
	}
	return p
}

// TestLimitsPage holds docs/reference/limits.md to the bounds of the profile.
func TestLimitsPage(t *testing.T) {
	limitsdoc.Duration(t, "webSignatureAge", kourierv1.MaxSkew)
	limitsdoc.Bytes(t, "webInboundBody", kourierv1.MaxBody)
	limitsdoc.Count(t, "webTextUnits", kourierv1.MaxText)
	limitsdoc.Bytes(t, "webReplyBody", kourierv1.MaxReplyBody)
}
