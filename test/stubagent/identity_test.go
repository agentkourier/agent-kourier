package stubagent_test

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// unsigned builds an alg:none JWT with the claims, as the spike's identity case does. The stub,
// like kagent's controller, decodes the claims and verifies nothing.
func unsigned(t *testing.T, claims obj) string {
	t.Helper()
	enc := func(v obj) string {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("marshal claims: %v", err)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	return enc(obj{"alg": "none"}) + "." + enc(claims) + "."
}

// as returns a client that sends the bearer token.
func (c *client) as(token string) *client {
	other := *c
	other.hdr = c.hdr.Clone()
	other.hdr.Set("Authorization", "Bearer "+token)
	return &other
}

func TestSessionBelongsToTheEmailClaim(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next() // the owner's send
	agent.Next() // the one send by another identity that is allowed
	owner := c.as(unsigned(t, obj{"email": "a@example.test", "sub": "sub-a"}))
	_, sent := owner.call("SendMessage", msg("m1", "hi", ""))
	created := task(t, sent)
	ctxID, taskID := created["contextId"].(string), created["id"].(string)

	for _, tc := range []struct {
		name   string
		claims obj
		denied bool
	}{
		{"same email, other sub", obj{"email": "a@example.test", "sub": "other-sub"}, false},
		{"other email, same sub", obj{"email": "b@example.test", "sub": "sub-a"}, true},
		{"other email, other sub", obj{"email": "b@example.test", "sub": "other-sub"}, true},
		{"sub only, no email", obj{"sub": "sub-a"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := c.as(unsigned(t, tc.claims))
			other.t = t
			sent := other.open("SendStreamingMessage", msg("x-"+tc.name, "again", ctxID))
			continued := sent.all()
			_, got := other.call("GetTask", obj{"id": taskID})

			if !tc.denied {
				if describe(last(continued)) != "task:COMPLETED" || describe(got) != "task:COMPLETED" {
					t.Errorf("continue = %s, GetTask = %s; want both to work for %v", describe(last(continued)), describe(got), tc.claims)
				}
				return
			}
			rec := fixture(t, "identity/04-continue-other-email-same-sub.response.sse")[0]
			expectLike(t, "continue", continued[0], rec)
			expectLike(t, "GetTask", got, fixture(t, "identity/05-gettask-other-email-same-sub.response.json")[0])
			for label, ev := range map[string]obj{"continue": continued[0], "GetTask": got} {
				info, _ := dig(ev, "error", "data").([]any)
				if dig(ev, "error", "code") != float64(-31403) || dig(ev, "error", "message") != "permission denied" ||
					len(info) != 1 || dig(info[0], "reason") != "UNAUTHORIZED" || !sent.sse || len(continued) != 1 {
					t.Errorf("%s by %v = %v, want -31403 permission denied, UNAUTHORIZED, one SSE event", label, tc.claims, ev)
				}
			}
		})
	}
}

func TestEveryMethodChecksTheOwner(t *testing.T) {
	_, agent, c := newAgent(t)
	slowTurn(agent)
	agent.Next(stubagent.Text("second"))
	owner := c.as(unsigned(t, obj{"email": "a@example.test"}))
	st, taskID, ctxID := startSlow(owner)
	_ = st.resp.Body.Close()
	stranger := c.as(unsigned(t, obj{"email": "b@example.test"}))

	for _, tc := range []struct {
		method string
		params obj
	}{
		{"SendMessage", msg("s1", "hi", ctxID)},
		{"SendMessage", msg("m1", "go slowly", "")}, // a replay of the owner's message
		{"GetTask", obj{"id": taskID}},
		{"SubscribeToTask", obj{"id": taskID}},
		{"CancelTask", obj{"id": taskID}},
	} {
		if _, got := stranger.call(tc.method, tc.params); dig(got, "error", "code") != float64(-31403) {
			t.Errorf("%s by another email = %v, want -31403", tc.method, got)
		}
	}
	// None of that, CancelTask included, touched the task.
	if _, got := owner.call("GetTask", obj{"id": taskID}); describe(got) != "task:WORKING" {
		t.Errorf("after the stranger's calls the task is %s, want task:WORKING", describe(got))
	}
}

// TestStrangerCannotAnswerAPausedTask: UNCONFIRMED: kagent's answer to another identity's reply
// to a HITL pause; the stub refuses it like any other use of the session, and the script never
// runs.
func TestStrangerCannotAnswerAPausedTask(t *testing.T) {
	_, agent, c := newAgent(t)
	called := false
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step { called = true; return nil }))
	owner := withHITL(c.as(unsigned(t, obj{"email": "a@example.test"})))
	_, taskID, ctxID, request := paused(t, owner)
	stranger := withHITL(c.as(unsigned(t, obj{"email": "b@example.test"})))

	_, got := stranger.call("SendStreamingMessage", reply("m2", "x", ctxID, taskID, answers(request["id"].(string), []string{"MySQL"})))

	if dig(got, "error", "code") != float64(-31403) || called {
		t.Errorf("a stranger's answer = %v, script called %v; want -31403 and the script not called", got, called)
	}
}

func TestRequestsWithNoTokenShareTheAnonymousOwner(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()
	agent.Next()
	_, sent := c.call("SendMessage", msg("m1", "hi", ""))
	ctxID := task(t, sent)["contextId"].(string)

	_, again := c.call("SendMessage", msg("m2", "hi", ctxID))
	_, named := c.as(unsigned(t, obj{"email": "a@example.test"})).call("SendMessage", msg("m3", "hi", ctxID))

	// UNCONFIRMED: no real kagent mode has an anonymous owner: with OIDC on, a request with no
	// token is refused, and in insecure mode it names its user in X-User-Id.
	if dig(again, "error") != nil || dig(named, "error", "code") != float64(-31403) {
		t.Errorf("no token twice, then a named email: %v, %v; want the first to work and the second denied", again, named)
	}
}

// TestUnreadableTokenIsUnauthorized: a bearer token that yields neither an email nor a sub is
// refused with HTTP 401, with or without WithRequiredAuth, and not treated as anonymous.
// UNCONFIRMED: kagent's answer; the 401 for a missing token was seen with curl (WP-02) and no
// fixture shows an unreadable one.
func TestUnreadableTokenIsUnauthorized(t *testing.T) {
	for _, tc := range []struct{ name, token string }{
		{"not a JWT", "fake-not-a-jwt"},
		{"a payload that is not base64", "a.@@@.b"},
		{"a payload that is not JSON", "a." + base64.RawURLEncoding.EncodeToString([]byte("nope")) + ".b"},
		{"a JWT with no email and no sub", unsigned(t, obj{"iss": "https://issuer.example.test"})},
	} {
		s, agent, c := newAgent(t)
		agent.Next()
		hdr := http.Header{"Authorization": {"Bearer " + tc.token}}

		rpc, _ := do(t, http.MethodPost, s.AgentURL("kagent", "test"), obj{"jsonrpc": "2.0", "id": "1", "method": "GetTask"}, hdr)
		card, _ := do(t, http.MethodGet, s.AgentURL("kagent", "test")+"/.well-known/agent-card.json", nil, hdr)
		_, ok := c.as(unsigned(t, obj{"sub": "s"})).call("SendMessage", msg("m1", "hi", ""))

		if rpc != http.StatusUnauthorized || card != http.StatusUnauthorized || dig(ok, "error") != nil {
			t.Errorf("%s: RPC HTTP %d, card HTTP %d; want 401 and 401 (and a token with a sub alone accepted: %v)", tc.name, rpc, card, ok)
		}
	}
}

// TestOnlyAnAbsentAuthorizationHeaderIsAnonymous: any Authorization header at all must be a
// well-formed Bearer credential (the scheme is case-insensitive, RFC 7235) with a readable JWT
// that has an email or a sub, or the request is refused with HTTP 401, WithRequiredAuth or not.
// UNCONFIRMED: kagent's answer for these; only the missing token was seen (WP-02, curl).
func TestOnlyAnAbsentAuthorizationHeaderIsAnonymous(t *testing.T) {
	jwt := unsigned(t, obj{"email": "a@example.test"})
	for _, tc := range []struct {
		name  string
		value []string // the Authorization header lines; none means absent
		ok    bool
	}{
		{"absent", nil, true},
		{"bearer", []string{"Bearer " + jwt}, true},
		{"lowercase scheme", []string{"bearer " + jwt}, true},
		{"uppercase scheme", []string{"BEARER " + jwt}, true},
		{"empty value", []string{""}, false},
		{"scheme alone", []string{"Bearer"}, false},
		{"scheme and a space", []string{"Bearer "}, false},
		{"another scheme", []string{"Basic YTpi"}, false},
		{"another scheme with a JWT", []string{"Token " + jwt}, false},
		{"the JWT alone", []string{jwt}, false},
		{"two lines", []string{"Bearer " + jwt, "Bearer " + jwt}, false},
	} {
		s, agent, _ := newAgent(t)
		agent.Next()
		hdr := http.Header{"Authorization": tc.value}
		if tc.value == nil {
			hdr = http.Header{}
		}

		rpc, _ := do(t, http.MethodPost, s.AgentURL("kagent", "test"), obj{"jsonrpc": "2.0", "id": "1", "method": "GetTask"}, hdr)
		card, _ := do(t, http.MethodGet, s.AgentURL("kagent", "test")+"/.well-known/agent-card.json", nil, hdr)

		if ok := rpc != http.StatusUnauthorized && card == http.StatusOK; ok != tc.ok {
			t.Errorf("%s: RPC HTTP %d, card HTTP %d; want the request accepted = %v", tc.name, rpc, card, tc.ok)
		}
	}
}

func TestRequiredAuthRefusesRequestsWithoutABearerToken(t *testing.T) {
	s, agent, c := newAgent(t, stubagent.WithRequiredAuth())
	agent.Next()
	cardURL := s.AgentURL("kagent", "test") + "/.well-known/agent-card.json"
	token := unsigned(t, obj{"email": "a@example.test"})

	cardStatus, _ := do(t, http.MethodGet, cardURL, nil, http.Header{})
	rpcStatus, _ := do(t, http.MethodPost, s.AgentURL("kagent", "test"), obj{"jsonrpc": "2.0", "id": "1", "method": "GetTask"}, http.Header{})
	cardOK, _ := do(t, http.MethodGet, cardURL, nil, http.Header{"Authorization": {"Bearer " + token}})
	_, sent := c.as(token).call("SendMessage", msg("m1", "hi", ""))

	if cardStatus != http.StatusUnauthorized || rpcStatus != http.StatusUnauthorized || cardOK != http.StatusOK || dig(sent, "error") != nil {
		t.Errorf("without a token: card HTTP %d, RPC HTTP %d; with one: card HTTP %d, send %v; want 401, 401, 200 and a send that works",
			cardStatus, rpcStatus, cardOK, sent)
	}
}
