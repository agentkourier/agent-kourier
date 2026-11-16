package chattest

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/web"
)

// suiteSecret is the test profile's secret: a fake constant.
const suiteSecret = "fake-suite-secret"

// profileLeaks are the ways a profile can break what the profile suite holds it to.
type profileLeaks struct {
	verifiesNothing bool // every request passes Verify
	ignoresTime     bool // a signature of any time passes
	lax             bool // an unknown field is taken
	noCap           bool // a body of any size is taken
	constantID      bool // every event has the ID "1"
	freshID         bool // every delivery of an event has an ID of its own
	wrongMention    bool // an event's Mentioned is the opposite of what the sender said
	unsignedReplies bool // the replies carry no signature
	dropsKey        bool // the replies carry no idempotency key
	constantKey     bool // every reply has the one idempotency key
	mangledID       bool // a reply carries an ID of its own, not the one it was given
	offShape        bool // the events are in a channel whose ID has no digit, which no kourier-v1 Binding can name
}

// hmacProfile is a profile of the suite's tests: HMAC-SHA256 of the timestamp and the body, in two headers, over a small JSON schema.
type hmacProfile struct {
	leaks   profileLeaks
	replies bool
	n       atomic.Int64
}

type hmacEvent struct {
	Channel string `json:"channel"`
	Thread  string `json:"thread,omitempty"`
	ID      string `json:"id"`
	User    string `json:"user"`
	Text    string `json:"text"`
	Mention bool   `json:"mention,omitempty"`
	Ping    bool   `json:"ping,omitempty"`
}

const (
	hmacMaxBody = 1024
	hmacSkew    = time.Minute
)

func hmacSign(secret string, body []byte, at time.Time) http.Header {
	ts := strconv.FormatInt(at.Unix(), 10)
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + ":"))
	m.Write(body)
	h := http.Header{}
	h.Set("X-Test-Timestamp", ts)
	h.Set("X-Test-Signature", hex.EncodeToString(m.Sum(nil)))
	return h
}

func (p *hmacProfile) Name() v1alpha1.WebProfile { return v1alpha1.WebProfileKourierV1 }

func (p *hmacProfile) Verify(h http.Header, body []byte, now time.Time) error {
	if p.leaks.verifiesNothing {
		return nil
	}
	sec, err := strconv.ParseInt(h.Get("X-Test-Timestamp"), 10, 64)
	if err != nil {
		return errors.New("no timestamp")
	}
	if d := now.Sub(time.Unix(sec, 0)); !p.leaks.ignoresTime && (d > hmacSkew || d < -hmacSkew) {
		return errors.New("stale")
	}
	want := hmacSign(suiteSecret, body, time.Unix(sec, 0)).Get("X-Test-Signature")
	if !hmac.Equal([]byte(h.Get("X-Test-Signature")), []byte(want)) {
		return errors.New("bad signature")
	}
	return nil
}

func (p *hmacProfile) Decode(_ http.Header, body []byte) (web.Event, bool, error) {
	if len(body) > hmacMaxBody && !p.leaks.noCap {
		return web.Event{}, false, web.ErrTooLarge
	}
	var e hmacEvent
	dec := json.NewDecoder(bytes.NewReader(body))
	if !p.leaks.lax {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(&e); err != nil || dec.More() {
		return web.Event{}, false, errors.New("not the schema")
	}
	if e.Ping {
		return web.Event{}, false, nil
	}
	if e.Channel == "" || e.ID == "" || e.User == "" {
		return web.Event{}, false, errors.New("a field is missing")
	}
	ev := web.Event{Channel: e.Channel, Thread: e.Thread, ID: e.ID, User: e.User, Text: e.Text, Mentioned: e.Thread == "" || e.Mention}
	switch {
	case p.leaks.constantID:
		ev.ID = "1"
	case p.leaks.freshID:
		ev.ID += "-" + strconv.FormatInt(p.n.Add(1), 10)
	}
	if p.leaks.wrongMention {
		ev.Mentioned = !ev.Mentioned
	}
	return ev, true, nil
}

func (p *hmacProfile) Caps() chat.Caps { return chat.Caps{MaxText: 1000} }

func (p *hmacProfile) Encoder() web.Encoder {
	if !p.replies {
		return nil
	}
	return p
}

// hmacKeyHeader carries a reply's idempotency key.
const hmacKeyHeader = "X-Test-Key"

func (p *hmacProfile) Encode(r web.Reply, now time.Time) (web.Request, error) {
	if p.leaks.mangledID {
		r.ID = "mangled"
	}
	body, err := json.Marshal(r)
	if err != nil {
		return web.Request{}, err
	}
	h := hmacSign(suiteSecret, body, now)
	if p.leaks.unsignedReplies {
		h = http.Header{}
	}
	switch {
	case p.leaks.constantKey:
		h.Set(hmacKeyHeader, "one-key")
	case !p.leaks.dropsKey:
		h.Set(hmacKeyHeader, r.Key)
	}
	return web.Request{Header: h, Body: body}, nil
}

func hmacSubject(p *hmacProfile) ProfileSubject {
	s := ProfileSubject{
		Profile: p,
		Secret:  suiteSecret,
		Sign:    hmacSign,
		MaxSkew: hmacSkew,
		MaxBody: hmacMaxBody,
		Events: []ProfileEvent{
			{Name: "a message that opens a thread", Body: []byte(`{"channel":"ch-1","id":"m-1","user":"u-1","text":"hello"}`),
				Want: &web.Event{Channel: "ch-1", ID: "m-1", User: "u-1", Text: "hello", Mentioned: true}},
			{Name: "a reply in a thread", Body: []byte(`{"channel":"ch-1","thread":"m-1","id":"m-2","user":"u-2","text":"and"}`),
				Want: &web.Event{Channel: "ch-1", Thread: "m-1", ID: "m-2", User: "u-2", Text: "and"}},
			{Name: "a ping", Body: []byte(`{"ping":true}`)},
		},
		Malformed: []ProfileCase{
			{Name: "an unknown field", Body: []byte(`{"channel":"ch-1","id":"m-1","user":"u-1","text":"hi","url":"http://evil.example/"}`)},
			{Name: "no user", Body: []byte(`{"channel":"ch-1","id":"m-1","text":"hi"}`)},
		},
	}
	if p.replies {
		s.CheckReply = func(want web.Reply, h http.Header, body []byte) (string, error) {
			sec, err := strconv.ParseInt(h.Get("X-Test-Timestamp"), 10, 64)
			if err != nil {
				return "", errors.New("no timestamp")
			}
			if hmacSign(suiteSecret, body, time.Unix(sec, 0)).Get("X-Test-Signature") != h.Get("X-Test-Signature") {
				return "", errors.New("bad signature")
			}
			var got web.Reply
			if err := json.Unmarshal(body, &got); err != nil {
				return "", err
			}
			got.Key, want.Key = "", ""
			if !reflect.DeepEqual(got, want) {
				return "", fmt.Errorf("the reply is %+v, want %+v", got, want)
			}
			return h.Get(hmacKeyHeader), nil
		}
	}
	if p.leaks.offShape {
		for i, e := range s.Events {
			s.Events[i].Body = bytes.ReplaceAll(e.Body, []byte(`"ch-1"`), []byte(`"general"`))
			if e.Want != nil {
				w := *e.Want
				w.Channel = "general"
				s.Events[i].Want = &w
			}
		}
	}
	return s
}

func TestAProfileThatKeepsTheContractPasses(t *testing.T) {
	t.Parallel()
	for _, replies := range []bool{false, true} {
		ProfileConformance(t, hmacSubject(&hmacProfile{replies: replies}))
	}
}

func TestEveryWayAProfileCanFailIsCaught(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		leaks   profileLeaks
		replies bool
		want    []string // a case each, among the violations found
	}{
		"verifies nothing":  {leaks: profileLeaks{verifiesNothing: true}, want: []string{"unsigned", "another secret", "another body"}},
		"ignores the time":  {leaks: profileLeaks{ignoresTime: true}, want: []string{"stale", "from the future"}},
		"takes any field":   {leaks: profileLeaks{lax: true}, want: []string{"malformed: an unknown field"}},
		"takes any size":    {leaks: profileLeaks{noCap: true}, want: []string{"over the bound"}},
		"one ID for all":    {leaks: profileLeaks{constantID: true}, want: []string{"distinct IDs"}},
		"an ID per try":     {leaks: profileLeaks{freshID: true}, want: []string{"repeat: a message that opens a thread", "repeat: a reply in a thread"}},
		"the wrong mention": {leaks: profileLeaks{wrongMention: true}, want: []string{"a message that opens a thread", "a reply in a thread"}},
		"unsigned replies":  {leaks: profileLeaks{unsignedReplies: true}, replies: true, want: []string{"reply"}},
		"no reply key":      {leaks: profileLeaks{dropsKey: true}, replies: true, want: []string{"reply", "update"}},
		"one reply key":     {leaks: profileLeaks{constantKey: true}, replies: true, want: []string{"another reply", "update again"}},
		"a mangled reply":   {leaks: profileLeaks{mangledID: true}, replies: true, want: []string{"reply", "update"}},
		"an off-shape ID":   {leaks: profileLeaks{offShape: true}, want: []string{"shape: a message that opens a thread", "shape: a reply in a thread"}},
	} {
		t.Run(name, func(t *testing.T) {
			vs := CheckProfile(t.Context(), hmacSubject(&hmacProfile{leaks: tc.leaks, replies: tc.replies}))
			var found []string
			for _, v := range vs {
				if v.Rule != RuleContract {
					t.Errorf("violation %v, want the contract rule", v)
				}
				found = append(found, v.Case)
			}
			for _, w := range tc.want {
				if !slices.Contains(found, w) {
					t.Errorf("the violations %q do not include %q", found, w)
				}
			}
		})
	}
}

func TestTheProfileSuiteRefusesAnIncompleteSubject(t *testing.T) {
	t.Parallel()
	for name, mod := range map[string]func(*ProfileSubject){
		"no profile":                func(s *ProfileSubject) { s.Profile = nil },
		"no Sign":                   func(s *ProfileSubject) { s.Sign = nil },
		"no secret":                 func(s *ProfileSubject) { s.Secret = "" },
		"no bound":                  func(s *ProfileSubject) { s.MaxBody = 0 },
		"no event to take":          func(s *ProfileSubject) { s.Events = s.Events[2:] },
		"replies with no check":     func(s *ProfileSubject) { s.Profile = &hmacProfile{replies: true} },
		"an event with no name":     func(s *ProfileSubject) { s.Events[0].Name = "" },
		"a malformed case unnamed":  func(s *ProfileSubject) { s.Malformed[0].Name = "" },
		"a first event over bounds": func(s *ProfileSubject) { s.MaxBody = 10 },
	} {
		t.Run(name, func(t *testing.T) {
			s := hmacSubject(&hmacProfile{})
			mod(&s)
			vs := CheckProfile(t.Context(), s)
			if len(vs) != 1 || vs[0].Rule != RuleSetup {
				t.Errorf("violations %v, want one setup violation", vs)
			}
		})
	}
}

// The suite says what it found in words of its own: no secret, and no signature.
func TestTheProfileSuiteQuotesNoSecret(t *testing.T) {
	t.Parallel()
	vs := CheckProfile(t.Context(), hmacSubject(&hmacProfile{leaks: profileLeaks{verifiesNothing: true, ignoresTime: true}}))
	if len(vs) == 0 {
		t.Fatal("no violations: the test proves nothing")
	}
	for _, v := range vs {
		if strings.Contains(v.String(), suiteSecret) {
			t.Errorf("the violation %v holds the secret", v)
		}
	}
}
