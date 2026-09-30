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

func TestAnonymousRequestsShareOneOwner(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next()
	agent.Next()
	agent.Next()
	_, sent := c.call("SendMessage", msg("m1", "hi", ""))
	ctxID := task(t, sent)["contextId"].(string)

	_, notJWT := c.as("fake-not-a-jwt").call("SendMessage", msg("m2", "hi", ctxID))
	_, again := c.call("SendMessage", msg("m3", "hi", ctxID))
	_, named := c.as(unsigned(t, obj{"email": "a@example.test"})).call("SendMessage", msg("m4", "hi", ctxID))

	if dig(notJWT, "error") != nil || dig(again, "error") != nil || dig(named, "error", "code") != float64(-31403) {
		t.Errorf("no token and a token with no claims share an owner, a named email does not: %v, %v, %v", notJWT, again, named)
	}
}

func TestRequiredAuthRefusesRequestsWithoutABearerToken(t *testing.T) {
	s, agent, c := newAgent(t, stubagent.WithRequiredAuth())
	agent.Next()
	cardURL := s.AgentURL("kagent", "test") + "/.well-known/agent-card.json"

	cardStatus, _ := do(t, http.MethodGet, cardURL, nil, http.Header{})
	rpcStatus, _ := do(t, http.MethodPost, s.AgentURL("kagent", "test"), obj{"jsonrpc": "2.0", "id": "1", "method": "GetTask"}, http.Header{})
	withToken := http.Header{"Authorization": {"Bearer fake-token"}}
	cardOK, _ := do(t, http.MethodGet, cardURL, nil, withToken)
	_, sent := c.as("fake-token").call("SendMessage", msg("m1", "hi", ""))

	if cardStatus != http.StatusUnauthorized || rpcStatus != http.StatusUnauthorized || cardOK != http.StatusOK || dig(sent, "error") != nil {
		t.Errorf("without a token: card HTTP %d, RPC HTTP %d; with one: card HTTP %d, send %v; want 401, 401, 200 and a send that works",
			cardStatus, rpcStatus, cardOK, sent)
	}
}
