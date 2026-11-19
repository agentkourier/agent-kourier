package webhook_test

import (
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/webhook"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// outcome serves req on p, which runs on a sink that takes everything, and returns the one series it counted, the answer's status,
// and what the sink got.
func outcome(t *testing.T, cfg webhook.Config, req *http.Request) (series string, code int, got []chat.Inbound) {
	t.Helper()
	rec := &recorder{}
	cfg.Recorder = rec
	p := newPlatform(t, cfg)
	sk := &sink{}
	running(t, p, sk)
	code = serve(t, p, req).Code
	for k, n := range rec.snapshot() {
		if n > 0 {
			if series != "" {
				t.Fatalf("counted %q and %q, want one series", series, k)
			}
			series = k
		}
	}
	return series, code, sk.messages()
}

// The steps run in their order, and none after the one that ends a request: each case is a request that two steps would end, and the
// earlier one must be what it ends with.
func TestTheStepsRunInOrder(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		signed     bool
		want       string
		code       int
	}{
		{"verify before parse: an unsigned body that is no JSON", `{"a":`, false, "unverified unsigned", http.StatusUnauthorized},
		{"parse before the channel: a signed body that is no JSON", `{"repository":`, true, "malformed schema", http.StatusBadRequest},
		{"the channel before the actor: the agent's event with no channel", `{"sender":{"login":"agent[bot]"}}`, true,
			"unmapped no_channel", http.StatusOK},
		{"the actor before the thread: the agent's event with no thread", `{"repository":{"full_name":"a/b"},"sender":{"login":"agent[bot]"}}`,
			true, "self none", http.StatusOK},
		{"the actor before the thread: no actor and no thread", `{"repository":{"full_name":"a/b"}}`, true,
			"ignored no_actor", http.StatusOK},
		{"the thread before the Sink: no thread", `{"repository":{"full_name":"a/b"},"sender":{"login":"octocat"}}`, true,
			"unmapped no_thread", http.StatusOK},
		{"the thread before the Sink: a bad thread", `{"repository":{"full_name":"a/b"},"sender":{"login":"octocat"},"issue":{"number":"1 2"}}`,
			true, "unmapped bad_id", http.StatusOK},
		{"the Sink last", `{"repository":{"full_name":"a/b"},"sender":{"login":"octocat"},"issue":{"number":12}}`, true,
			"accepted none", http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series, code, got := outcome(t, config(nil, nil), request(tc.body, tc.signed))
			if series != tc.want || code != tc.code {
				t.Errorf("counted %q and answered %d, want %q and %d", series, code, tc.want, tc.code)
			}
			if wantTaken := tc.want == "accepted none"; (len(got) == 1) != wantTaken {
				t.Errorf("the sink got %d events", len(got))
			}
		})
	}
}

// Verify checks the raw bytes, against the secret or the token after the prefix, and then the body's time.
func TestVerify(t *testing.T) {
	ms := func(d time.Duration) string { return strconv.FormatInt(epoch.Add(d).UnixMilli(), 10) }
	withTS := func(c *webhook.Config) {
		c.Verify.Timestamp = &webhook.Timestamp{From: "/webhookTimestamp", Unit: time.Millisecond, Window: time.Minute}
	}
	tsBody := func(ts string) string { return `{"webhookTimestamp":` + ts + `}` }
	for _, tc := range []struct {
		name   string
		cfg    func(*webhook.Config)
		body   string
		header func(h http.Header, body string)
		want   string // "" for verified
	}{
		{"signed", nil, event, nil, ""},
		{"signed in upper-case hex", nil, event, func(h http.Header, b string) {
			h.Set(sigHeader, "sha256="+strings.ToUpper(strings.TrimPrefix(sign(fakeSecret, b), "sha256=")))
		}, ""},
		{"unsigned", nil, event, func(h http.Header, _ string) { h.Del(sigHeader) }, "unsigned"},
		{"an empty header", nil, event, func(h http.Header, _ string) { h.Set(sigHeader, "") }, "unsigned"},
		{"without the prefix", nil, event, func(h http.Header, b string) { h.Set(sigHeader, strings.TrimPrefix(sign(fakeSecret, b), "sha256=")) }, "bad_signature"},
		{"under another secret", nil, event, func(h http.Header, b string) { h.Set(sigHeader, sign("another", b)) }, "bad_signature"},
		{"of another body", nil, event, func(h http.Header, _ string) { h.Set(sigHeader, sign(fakeSecret, event+" ")) }, "bad_signature"},
		{"not hex", nil, event, func(h http.Header, _ string) { h.Set(sigHeader, "sha256=zz") }, "bad_signature"},
		{"under the secret with a newline", nil, event, func(h http.Header, b string) { h.Set(sigHeader, sign(fakeSecret+"\n", b)) }, "bad_signature"},
		{"a timestamp now", withTS, tsBody(ms(0)), nil, ""},
		{"a timestamp 59 s ago", withTS, tsBody(ms(-59 * time.Second)), nil, ""},
		{"a timestamp 61 s ago", withTS, tsBody(ms(-61 * time.Second)), nil, "stale"},
		{"a timestamp 61 s ahead", withTS, tsBody(ms(61 * time.Second)), nil, "stale"},
		{"a timestamp in seconds where ms are wanted", withTS, tsBody(strconv.FormatInt(epoch.Unix(), 10)), nil, "stale"},
		{"a timestamp that is a string", withTS, tsBody(`"` + ms(0) + `"`), nil, "stale"},
		{"a timestamp that is not an integer", withTS, tsBody(ms(0) + ".5"), nil, "stale"},
		{"no timestamp", withTS, `{"type":"Issue"}`, nil, "stale"},
		{"a signed body that is no JSON, with a timestamp to read", withTS, `{"webhookTimestamp":`, nil, "stale"},
		{"a bad signature is checked before the timestamp", withTS, tsBody(ms(time.Hour)), func(h http.Header, b string) { h.Set(sigHeader, sign("x", b)) }, "bad_signature"},
		{"a timestamp in seconds", func(c *webhook.Config) {
			c.Verify.Timestamp = &webhook.Timestamp{From: "/t", Unit: time.Second, Window: time.Minute}
		}, `{"t":` + strconv.FormatInt(epoch.Add(-30*time.Second).Unix(), 10) + `}`, nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(nil, nil)
			if tc.cfg != nil {
				tc.cfg(&cfg)
			}
			req := request(tc.body, true)
			if tc.header != nil {
				tc.header(req.Header, tc.body)
			}
			series, _, _ := outcome(t, cfg, req)
			if verified := !strings.HasPrefix(series, "unverified "); verified != (tc.want == "") || (tc.want != "" && series != "unverified "+tc.want) {
				t.Errorf("counted %q, want unverified %q (empty: verified)", series, tc.want)
			}
		})
	}
}

// The token scheme compares a fixed token after the prefix, as Jira Automation's "Send web request" sends one.
func TestTheTokenScheme(t *testing.T) {
	cfg := config(nil, nil)
	cfg.Verify = webhook.Verify{Scheme: webhook.SchemeToken, Header: "Authorization", Prefix: "Bearer ",
		Secret: webhook.Credential{Secret: "ns/jira", Key: "token", Value: secret.New(fakeToken)}}
	for _, tc := range []struct {
		name, value, want string
	}{
		{"the token", "Bearer " + fakeToken, ""},
		{"another token", "Bearer " + strings.Replace(fakeToken, "f", "e", 1), "bad_signature"},
		{"the token without the prefix", fakeToken, "bad_signature"},
		{"a prefix of the token", "Bearer " + fakeToken[:len(fakeToken)-1], "bad_signature"},
		{"the token and more", "Bearer " + fakeToken + "x", "bad_signature"},
		{"nothing", "", "unsigned"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request(event, false)
			if tc.value != "" {
				req.Header.Set("Authorization", tc.value)
			}
			series, _, _ := outcome(t, cfg, req)
			if verified := !strings.HasPrefix(series, "unverified "); verified != (tc.want == "") || (tc.want != "" && series != "unverified "+tc.want) {
				t.Errorf("counted %q, want unverified %q (empty: verified)", series, tc.want)
			}
		})
	}
}

// field is the state of one field of an event.
func field(name, value string, id, present, invalid bool) chat.WebhookField {
	return chat.WebhookField{Name: name, Value: value, ID: id, Present: present, Invalid: invalid}
}

// The fields of an event: the first value of each that resolves and, for an id field, passes the id check. An id field moves past a
// value that fails it; one whose values all fail it is Invalid, not absent; and only an Invalid thread field unmaps the event.
func TestTheFieldsOfAnEvent(t *testing.T) {
	const repo = `"repository":{"full_name":"acme/widgets"},"sender":{"login":"octocat"}`
	long := strings.Repeat("a", webhook.MaxIDValue)
	for _, tc := range []struct {
		name   string
		body   string
		header http.Header
		series string
		thread string
		fields []chat.WebhookField // the fields after event, number
	}{
		{"every field from its first pointer", `{` + repo + `,"pull_request":{"number":7,"head":{"sha":"abc"},"body":"hi"}}`, nil,
			"accepted none", "pr-7", []chat.WebhookField{field("sha", "abc", true, true, false), field("body", "hi", false, true, false)}},
		{"an id field moves past a bad value to the next pointer", `{` + repo + `,"issue":{"number":7},"pull_request":{"head":{"sha":"a b"}},"after":"def"}`, nil,
			"accepted none", "pr-7", []chat.WebhookField{field("sha", "def", true, true, false), field("body", "", false, false, false)}},
		{"an id field of bad values alone is Invalid, and the event is taken", `{` + repo + `,"issue":{"number":7},"pull_request":{"head":{"sha":"a b"}},"after":"c;d"}`, nil,
			"accepted none", "pr-7", []chat.WebhookField{field("sha", "", true, false, true), field("body", "", false, false, false)}},
		{"an id field longer than the bound is Invalid", `{` + repo + `,"issue":{"number":7},"after":"` + long + `a"}`, nil,
			"accepted none", "pr-7", []chat.WebhookField{field("sha", "", true, false, true), field("body", "", false, false, false)}},
		{"an id field as long as the bound is taken", `{` + repo + `,"issue":{"number":7},"after":"` + long + `"}`, nil,
			"accepted none", "pr-7", []chat.WebhookField{field("sha", long, true, true, false), field("body", "", false, false, false)}},
		{"a text field takes any value", `{` + repo + `,"issue":{"number":7},"comment":{"body":"a b; <c>"}}`, nil,
			"accepted none", "pr-7", []chat.WebhookField{field("sha", "", true, false, false), field("body", "a b; <c>", false, true, false)}},
		{"a number and a boolean resolve as written", `{` + repo + `,"issue":{"number":7},"after":12.50,"comment":{"body":true}}`, nil,
			"accepted none", "pr-7", []chat.WebhookField{field("sha", "12.50", true, true, false), field("body", "true", false, true, false)}},
		{"the thread moves past a bad value", `{` + repo + `,"pull_request":{"number":"7 7"},"issue":{"number":8}}`, nil,
			"accepted none", "pr-8", []chat.WebhookField{field("sha", "", true, false, false), field("body", "", false, false, false)}},
		{"a thread of bad values alone unmaps", `{` + repo + `,"pull_request":{"number":"7 7"},"issue":{"number":"8;"}}`, nil,
			"unmapped bad_id", "", nil},
		{"a thread with a colon unmaps", `{` + repo + `,"issue":{"number":"a:b"}}`, nil, "unmapped bad_id", "", nil},
		{"an empty thread unmaps", `{` + repo + `,"issue":{"number":""}}`, nil, "unmapped no_thread", "", nil},
		{"a thread of null does not resolve", `{` + repo + `,"issue":{"number":null}}`, nil, "unmapped no_thread", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			series, _, got := outcome(t, config(nil, nil), request(tc.body, true))
			if series != tc.series {
				t.Fatalf("counted %q, want %q", series, tc.series)
			}
			if tc.series != "accepted none" {
				return
			}
			if len(got) != 1 {
				t.Fatalf("the sink got %d events, want 1", len(got))
			}
			if got[0].At.Thread != tc.thread {
				t.Errorf("the thread is %q, want %q", got[0].At.Thread, tc.thread)
			}
			if fs := got[0].Webhook.Fields; len(fs) != 4 || !slices.Equal(fs[2:], tc.fields) {
				t.Errorf("the fields are %+v, want event, number, then %+v", fs, tc.fields)
			}
			for _, f := range got[0].Webhook.Fields {
				if f.Present && f.Invalid {
					t.Errorf("the field %s is both present and invalid", f.Name)
				}
			}
		})
	}
}

// A header field reads the header's values in order, as a first-of list; an id field moves past one that fails its check.
func TestAHeaderField(t *testing.T) {
	cfg := config(nil, nil)
	cfg.Fields = append(cfg.Fields, webhook.Field{Name: "hook", Header: "X-Hook-ID", ID: true})
	for _, tc := range []struct {
		name   string
		values []string
		want   chat.WebhookField
	}{
		{"none", nil, field("hook", "", true, false, false)},
		{"one", []string{"h-1"}, field("hook", "h-1", true, true, false)},
		{"a bad one, then a good one", []string{"h 1", "h-2"}, field("hook", "h-2", true, true, false)},
		{"bad ones alone", []string{"h 1", "h;2"}, field("hook", "", true, false, true)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := request(event, true)
			req.Header.Set("X-GitHub-Event", "pull_request")
			for _, v := range tc.values {
				req.Header.Add("X-Hook-ID", v)
			}
			_, _, got := outcome(t, cfg, req)
			if len(got) != 1 {
				t.Fatalf("the sink got %d events, want 1", len(got))
			}
			fs := got[0].Webhook.Fields
			if fs[len(fs)-1] != tc.want {
				t.Errorf("the field is %+v, want %+v", fs[len(fs)-1], tc.want)
			}
			if fs[0] != field("event", "pull_request", false, true, false) {
				t.Errorf("the event field is %+v", fs[0])
			}
		})
	}
}

// The delivery ID is the first value of the delivery header when it passes the id check, and empty otherwise.
func TestTheDeliveryID(t *testing.T) {
	for _, tc := range []struct {
		name, header, value, want string
	}{
		{"a UUID", "X-GitHub-Delivery", "72d3162e-cc78-11e3-81ab-4c9367dc0958", "72d3162e-cc78-11e3-81ab-4c9367dc0958"},
		{"none", "X-GitHub-Delivery", "", ""},
		{"not an identifier", "X-GitHub-Delivery", "a b", ""},
		{"longer than the bound", "X-GitHub-Delivery", strings.Repeat("d", webhook.MaxIDValue+1), ""},
		{"as long as the bound", "X-GitHub-Delivery", strings.Repeat("d", webhook.MaxIDValue), strings.Repeat("d", webhook.MaxIDValue)},
		{"a connection with no delivery header", "", "72d3162e", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(nil, nil)
			cfg.DeliveryHeader = tc.header
			req := request(event, true)
			if tc.value != "" {
				req.Header.Set("X-GitHub-Delivery", tc.value)
			}
			_, _, got := outcome(t, cfg, req)
			if len(got) != 1 || got[0].Webhook.Delivery != tc.want {
				t.Errorf("the sink got %+v, want one event with the delivery %q", got, tc.want)
			}
		})
	}
}

// The agent's own events are dropped: any actor that resolves, compared with the identity with ASCII letters folded and nothing else.
// The actor of the event is the first that resolves.
func TestTheActor(t *testing.T) {
	const base = `"repository":{"full_name":"acme/widgets"},"issue":{"number":1}`
	for _, tc := range []struct {
		name, body, series, user string
		edit                     func(*webhook.Config)
	}{
		{"a person", `{` + base + `,"sender":{"login":"octocat"}}`, "accepted none", "octocat", nil},
		{"the agent", `{` + base + `,"sender":{"login":"agent[bot]"}}`, "self none", "", nil},
		{"the agent in another case", `{` + base + `,"sender":{"login":"AGENT[BOT]"}}`, "self none", "", nil},
		{"the agent in the second pointer", `{` + base + `,"sender":{"login":"octocat"},"comment":{"user":{"login":"Agent[bot]"}}}`, "self none", "", nil},
		{"a lookalike of the agent", `{` + base + `,"sender":{"login":"agent[bot]x"}}`, "accepted none", "agent[bot]x", nil},
		{"a lookalike with a Kelvin sign, which Unicode folds to k", `{` + base + `,"sender":{"login":"Kagent"}}`, "accepted none", "Kagent",
			func(c *webhook.Config) { c.Identity = "kagent" }},
		{"no actor", `{` + base + `}`, "ignored no_actor", "", nil},
		{"no actor, kept", `{` + base + `}`, "accepted none", "", func(c *webhook.Config) { c.Actor.KeepMissing = true }},
		{"no self-drop", `{` + base + `,"sender":{"login":"agent[bot]"}}`, "accepted none", "", func(c *webhook.Config) {
			c.Actor, c.Identity = webhook.Actor{None: true}, ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(nil, nil)
			if tc.edit != nil {
				tc.edit(&cfg)
			}
			series, _, got := outcome(t, cfg, request(tc.body, true))
			if series != tc.series {
				t.Fatalf("counted %q, want %q", series, tc.series)
			}
			if tc.series == "accepted none" && (len(got) != 1 || got[0].User != tc.user || got[0].Webhook.Actor != tc.user) {
				t.Errorf("the sink got %+v, want one event of %q", got, tc.user)
			}
		})
	}
}

// A connection with a fixed channel maps every event there; one with pointers takes the first that resolves, and refuses a value
// that is not a channel's shape.
func TestTheChannel(t *testing.T) {
	for _, tc := range []struct {
		name, body, series, channel string
		edit                        func(*webhook.Config)
	}{
		{"a fixed channel", `{"sender":{"login":"o"},"issue":{"number":1}}`, "accepted none", "OPS", func(c *webhook.Config) {
			c.Channel = webhook.Channel{Value: "OPS"}
		}},
		{"the second pointer", `{"project":{"key":"ENG"},"sender":{"login":"o"},"issue":{"number":1}}`, "accepted none", "ENG", func(c *webhook.Config) {
			c.Channel = webhook.Channel{From: []string{"/repository/full_name", "/project/key"}}
		}},
		{"a value with a colon", `{"repository":{"full_name":"a:b"},"sender":{"login":"o"},"issue":{"number":1}}`, "unmapped no_channel", "",
			func(c *webhook.Config) { c.ValidChannel = func(string) bool { return true } }},
		{"a value of another shape", `{"repository":{"full_name":"a b"},"sender":{"login":"o"},"issue":{"number":1}}`, "unmapped no_channel", "", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config(nil, nil)
			if tc.edit != nil {
				tc.edit(&cfg)
			}
			series, _, got := outcome(t, cfg, request(tc.body, true))
			if series != tc.series {
				t.Fatalf("counted %q, want %q", series, tc.series)
			}
			if tc.channel != "" && (len(got) != 1 || got[0].At.Channel != tc.channel) {
				t.Errorf("the sink got %+v, want one event in %q", got, tc.channel)
			}
		})
	}
}

// Without a thread, every event is a thread of its own.
func TestNoThread(t *testing.T) {
	cfg := config(nil, nil)
	cfg.Thread = nil
	series, _, got := outcome(t, cfg, request(`{"repository":{"full_name":"a/b"},"sender":{"login":"o"}}`, true))
	if series != "accepted none" || len(got) != 1 || got[0].At.Thread != "" {
		t.Errorf("counted %q and the sink got %+v; want one event with no thread", series, got)
	}
}
