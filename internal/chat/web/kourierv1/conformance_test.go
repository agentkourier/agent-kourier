package kourierv1_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
	"github.com/agentkourier/agent-kourier/internal/chat/web/kourierv1"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// receiver is the connection's URL as an in-process transport: it checks each reply's signature, keeps its envelope, and answers 200.
// The checks make tens of thousands of replies, which over loopback TCP would cost the sockets and not test anything more.
type receiver struct {
	t   *testing.T
	mu  sync.Mutex
	got []kourierv1.Outbound
}

func (r *receiver) RoundTrip(req *http.Request) (*http.Response, error) {
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return nil, err
	}
	if err := kourierv1.VerifySignature([]byte(fakeKey), epoch, req.Header.Get(kourierv1.HeaderTimestamp), req.Header.Get(kourierv1.HeaderSignature), body); err != nil {
		r.t.Errorf("the receiver got a reply it cannot verify: %v", err)
	}
	var env kourierv1.Outbound
	if err := json.Unmarshal(body, &env); err != nil {
		r.t.Errorf("the receiver got a body that is not the envelope: %v", err)
	}
	r.mu.Lock()
	r.got = append(r.got, env)
	r.mu.Unlock()
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
}

// take returns, and forgets, the envelopes the receiver got.
func (r *receiver) take() []kourierv1.Outbound {
	r.mu.Lock()
	defer r.mu.Unlock()
	got := r.got
	r.got = nil
	return got
}

// subject is the web platform with the kourier-v1 profile, as a receiver that keeps the wire contract shows what it is sent: the body's
// markdown read as CommonMark, by a renderer that may make a bare URL a link (GitHub's flavour does), and every other field shown as
// the text it is. CommonMark shows code as it is written, and so the body is read with code taken for literal
// (ScanCommonMarkLiteralCode): a link, a URL or HTML in a code span or block makes nothing. That reader still reports a Slack token
// ("<!", "<@", "<#") in code, which the profile writes as the agent did; kourier-v1 has no such token, and the corpus puts none in
// code. A case that does needs a concession for the body, whose reason is that a CommonMark receiver has no token to act on.
func subject(t *testing.T) chattest.Subject {
	t.Helper()
	r := &receiver{t: t}
	p, err := web.New(web.Config{Connection: "conformance/web", Profile: newProfile(t), Platform: platform(t), URL: "https://receiver.conformance.test/replies",
		Transport: r, Now: func() time.Time { return epoch }, Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	const why = "the platform has none: "
	return chattest.Subject{
		Platform: p,
		Target:   chat.Target{Conn: "conformance/web", Channel: "ch-1", Thread: "m-1"},
		Sent: func() []chattest.Shown {
			var shown []chattest.Shown
			for _, env := range r.take() {
				for _, f := range chattest.Fields(env.Message) {
					switch {
					case f.Text == "":
					case f.Where.Rich():
						shown = append(shown, chattest.ScanCommonMarkLiteralCode(f.Text))
					default:
						shown = append(shown, chattest.Shown{Displayed: f.Text})
					}
				}
			}
			return shown
		},
		// The wire has no option to parse, unfurl or follow anything: what the receiver does with a URL is the receiver's, and no reply
		// goes anywhere but the connection's URL, whose redirects are not followed (TestARedirectIsNotFollowed in package web).
		Inert: func() error { return nil },
		Channels: func(st chattest.ChannelState) string {
			if st == chattest.ChannelJoined {
				return "ch-1"
			}
			return "#general" // not a kourier-v1 channel ID
		},
		Groups: func(chattest.GroupState) string { return "grp-1" },
		AbsentChannels: map[chattest.ChannelState]string{
			chattest.ChannelNotJoined:    why + "Agent Kourier is in every channel its sender names, since the sender alone says which they are",
			chattest.ChannelArchived:     why + "no channel is archived, since nothing is asked of a channel",
			chattest.ChannelScopeMissing: why + "no permission is read, since nothing is asked of a channel",
		},
		AbsentGroups: map[chattest.GroupState]string{
			chattest.GroupReadable:     why + "kourier-v1 has no user groups (a Binding cannot name one: tool approvals are refused)",
			chattest.GroupScopeMissing: why + "kourier-v1 has no user groups, so no permission to read them",
		},
		// A receiver may render a bare URL as a link that shows its own address (GitHub's CommonMark does), which no escape stops: the
		// link goes where the text says, and Agent Kourier fetches nothing.
		AllowAutolink: true,
		TooLarge:      new(tooLarge()),
	}
}

func TestThePlatformWithKourierV1PassesConformance(t *testing.T) {
	t.Parallel()
	chattest.Conformance(t, subject(t))
}

// A prompt that Fits refuses as too large is one its post would be refused for.
func TestFitsAgreesWithPost(t *testing.T) {
	s := subject(t)
	big := tooLarge()
	if err := s.Platform.Fits(big); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Fits: %v, want chat.ErrTooLarge", err)
	}
	if _, err := s.Platform.Post(t.Context(), s.Target, chat.Ask(big), "t"); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Post: %v, want chat.ErrTooLarge", err)
	}
}
