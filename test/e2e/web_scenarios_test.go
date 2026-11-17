//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeweb"
)

// The WEB_ scenarios drive the web platform's two profiles from outside the broker: the fake sender (test/fakeweb) signs a request as a
// kourier-v1 client or as GitHub's webhook would and POSTs it to the chat listener's Service, and the fake receiver is the kourier-v1
// connection's reply URL. They run in the fast tier on custom resources (E2E_CONFIG_SOURCE=crds make e2e-fast-up), whose bring-up admits
// the namespace of the two web ChatConnections (hack/kind/web-values.yaml), and skip anywhere else: the files route serves exactly one
// ChatConnection, the broker's own Slack one, and a web connection is a team's, in a namespace of its own. The failover case needs two
// replicas on Postgres too (E2E_STORE=postgres E2E_REPLICAS=2 E2E_CONFIG_SOURCE=crds make e2e-fast-up).

// quiet is how long a scenario watches for what must not happen (a second turn, a second reply, a post) once what must has.
const quiet = 5 * time.Second

// webScenario is a scenario with the fake web sender and receiver.
type webScenario struct {
	*scenario
	web *Web
}

// requireWeb skips the test unless the run is in the fast tier with the web connections served: on custom resources, with their
// namespace admitted. A selector or namespaces it cannot read fail the test (Env.Admits).
func requireWeb(t *testing.T) {
	t.Helper()
	env := NewEnv(os.Getenv)
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if env.Tier != "fast" {
		t.Skipf("the web connections are the fast tier's: their doubles, the fake web sender and receiver, run there, not in the %s tier", env.Tier)
	}
	admits, err := env.Admits(WebNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if !admits {
		t.Skipf("the broker does not serve the namespace %s (%s): the web ChatConnections are custom resources of a team's namespace, which the files route, serving its one connection, never reads; bring the tier up with E2E_CONFIG_SOURCE=crds and without E2E_TWO_APPS", WebNamespace, env.Admission())
	}
}

// newWebScenario returns a webScenario once the broker is ready and both web connections run, or skips (requireWeb).
func newWebScenario(t *testing.T) *webScenario {
	t.Helper()
	requireWeb(t)
	return webOn(newScenario(t))
}

// webOn adds the fake web sender and receiver to s, and waits for both web connections to run.
func webOn(s *scenario) *webScenario {
	w := &webScenario{scenario: s, web: NewWeb(s.env.WebURL)}
	w.webConnected()
	return w
}

// webConnected waits for the leader's two web connections to run (agentkourier_chat_connected), which they do once the leader has
// started them: until then their routes answer 503. Call it again after anything that restarts Agent Kourier.
func (w *webScenario) webConnected() {
	w.t.Helper()
	for _, conn := range []string{ConnWebKourier, ConnWebGitHub} {
		err := Wait(w.ctx, 2*time.Minute, "the web connection "+conn+" to run", func(ctx context.Context) (bool, error) {
			up, err := w.leaderCounter(ctx, "agentkourier_chat_connected", map[string]string{"connection": conn})
			if errors.Is(err, ErrNoSample) {
				return false, nil
			}
			return up == 1, err
		})
		if err != nil {
			w.t.Fatal(err)
		}
	}
}

// sum is the sum of the leader's samples of name that have labels, 0 when it has none.
func (w *webScenario) sum(name string, labels map[string]string) float64 {
	w.t.Helper()
	var (
		v   float64
		err error
	)
	many, err := w.replicated(w.ctx)
	switch {
	case err != nil:
	case many:
		var leader kube.Replica
		if leader, err = w.reps.WaitLeader(w.ctx, 2*time.Minute); err == nil {
			v, err = w.reps.Sum(w.ctx, leader, name, labels)
		}
	default:
		v, err = w.agentKourier.Sum(w.ctx, name, labels)
	}
	if errors.Is(err, ErrNoSample) {
		return 0
	}
	if err != nil {
		w.t.Fatal(err)
	}
	return v
}

// requests is the leader's agentkourier_web_requests_total of the connection conn for outcome.
func (w *webScenario) requests(conn, outcome string) float64 {
	w.t.Helper()
	return w.counter("agentkourier_web_requests_total", map[string]string{"connection": conn, "outcome": outcome})
}

// token is a word that only this run of the test holds, for its IDs and its text: the stub, the fake and the store outlive a run.
func (w *webScenario) token(name string) string {
	return w.stub.Gate("web-" + name)
}

// send has the fake send in once and returns how the broker answered. An error of the fake's own fails the test.
func (w *webScenario) send(in fakeweb.Send) fakeweb.Sent {
	w.t.Helper()
	sent, err := w.web.Send(w.ctx, in)
	if err != nil {
		w.t.Fatal(err)
	}
	return sent
}

// sendStatus sends in once and fails the test unless the broker answered want.
func (w *webScenario) sendStatus(in fakeweb.Send, want int, desc string) fakeweb.Sent {
	w.t.Helper()
	sent := w.send(in)
	if sent.Status != want {
		w.t.Fatalf("%s: answered %d (%s); want %d", desc, sent.Status, sent.Error, want)
	}
	return sent
}

// sendUntilTaken sends in until the broker takes it (202), as a sender that follows the contract does: a 503 is sent again after its
// Retry-After, and a request that got no answer (the chat Service has no endpoint while the pod restarts) after a second. Any other
// answer fails the test. It returns the answer that took it and the answers before it.
func (w *webScenario) sendUntilTaken(in fakeweb.Send, desc string) (fakeweb.Sent, []fakeweb.Sent) {
	w.t.Helper()
	var before []fakeweb.Sent
	deadline := time.Now().Add(2 * time.Minute)
	for {
		sent := w.send(in)
		switch sent.Status {
		case http.StatusAccepted:
			return sent, before
		case http.StatusServiceUnavailable, 0:
		default:
			w.t.Fatalf("%s: answered %d (%s); want 202, or 503 or no answer while the broker is not ready", desc, sent.Status, sent.Error)
		}
		before = append(before, sent)
		if time.Now().After(deadline) {
			w.t.Fatalf("%s: not taken in 2m; the last answer: %d %s", desc, sent.Status, sent.Error)
		}
		wait := time.Second
		if secs, err := strconv.Atoi(sent.RetryAfter); err == nil && secs > 0 {
			wait = time.Duration(secs) * time.Second
		}
		select {
		case <-w.ctx.Done():
			w.t.Fatal(w.ctx.Err())
		case <-time.After(wait):
		}
	}
}

// kourierSend is a kourier-v1 Send of m to its connection's route, signed now.
func kourierSend(m fakeweb.Message) fakeweb.Send {
	return fakeweb.Send{Profile: fakeweb.ProfileKourierV1, Path: RouteWebKourier, Body: m.Body()}
}

// githubSend is a github Send of the event's body to its connection's route, signed.
func githubSend(event, body string) fakeweb.Send {
	return fakeweb.Send{Profile: fakeweb.ProfileGitHub, Path: RouteWebGitHub, Body: body, Event: event}
}

// thread waits for the kourier-v1 thread of the message id (its replies, ThreadReplies) to hold n posts (Posts), and returns its
// replies. Every reply the receiver was sent in the thread must keep the contract: a reply the receiver refused, unsigned, badly signed
// or without its Idempotency-Key, fails the test.
func (w *webScenario) thread(id string, n int, desc string) []fakeweb.Reply {
	w.t.Helper()
	got, err := w.web.WaitThread(w.ctx, stepTimeout, desc, ConnWebKourier, ChannelWebKourier, id, func(rs []fakeweb.Reply) bool { return len(Posts(rs)) >= n })
	if err != nil {
		w.t.Fatalf("%s: %v\nthe thread: %s", desc, err, describeReplies(got))
	}
	w.kept(got)
	if posts := Posts(got); len(posts) != n {
		w.t.Fatalf("%s: the thread holds %d posts; want %d\nthe thread: %s", desc, len(posts), n, describeReplies(got))
	}
	return got
}

// kept fails the test for each reply the receiver refused.
func (w *webScenario) kept(rs []fakeweb.Reply) {
	w.t.Helper()
	for _, r := range rs {
		if r.Problem != "" {
			w.t.Errorf("the receiver refused a reply of the thread %s: %s", r.Thread, r.Problem)
		}
	}
}

// stillPosts checks that the thread of id holds n posts for quiet: no turn starts after the ones it waited for.
func (w *webScenario) stillPosts(id string, n int, desc string) {
	w.t.Helper()
	var got []fakeweb.Reply
	err := Consistently(w.ctx, quiet, desc, func(ctx context.Context) (bool, error) {
		var err error
		got, err = w.web.ThreadReplies(ctx, ConnWebKourier, ChannelWebKourier, id)
		return len(Posts(got)) == n, err
	})
	if err != nil {
		w.t.Errorf("%v\nthe thread: %s", err, describeReplies(got))
	}
	w.kept(got)
}

// describeReplies is a thread of replies for a failure message.
func describeReplies(rs []fakeweb.Reply) string {
	var b strings.Builder
	for _, r := range rs {
		fmt.Fprintf(&b, "\n  %s %s key %q repeat %v problem %q: %s", r.Kind, r.ID, r.Key, r.Repeat, r.Problem, strings.Join(r.Markdown, " | "))
	}
	return b.String()
}

// tasks are the tasks of the stub's agent that the web Bindings' token owns and whose prompt holds tok.
func (w *webScenario) tasks(agent, tok string) []AgentTask {
	w.t.Helper()
	all, err := w.stub.Tasks(w.ctx, "kagent", agent, WebAgentToken())
	if err != nil {
		w.t.Fatal(err)
	}
	var out []AgentTask
	for _, task := range all {
		if strings.Contains(task.Prompt, tok) {
			out = append(out, task)
		}
	}
	return out
}

// waitTasks waits for n tasks of agent whose prompt holds tok, and returns them.
func (w *webScenario) waitTasks(agent, tok string, n int, desc string) []AgentTask {
	w.t.Helper()
	var got []AgentTask
	err := Wait(w.ctx, stepTimeout, desc, func(context.Context) (bool, error) {
		got = w.tasks(agent, tok)
		return len(got) >= n, nil
	})
	if err != nil {
		w.t.Fatal(err)
	}
	if len(got) != n {
		w.t.Fatalf("%s: %d tasks; want %d", desc, len(got), n)
	}
	return got
}

// stillTasks checks that agent has n tasks whose prompt holds tok for quiet.
func (w *webScenario) stillTasks(agent, tok string, n int, desc string) {
	w.t.Helper()
	err := Consistently(w.ctx, quiet, desc, func(context.Context) (bool, error) {
		return len(w.tasks(agent, tok)) == n, nil
	})
	if err != nil {
		w.t.Error(err)
	}
}

// markdown is the Markdown of a post, its body parts joined.
func markdown(r fakeweb.Reply) string { return strings.Join(r.Markdown, "\n") }

// webText is a message that only a test's run holds, with what CommonMark would read as a link, an HTML tag and code: the agent's
// answer quotes it (scenario-memory), and the reply must reach the receiver inert.
func webText(tok string) string {
	return "about " + tok + ": see [this](http://evil.test) <b>now</b>"
}

// The neutralised forms of webText's link and tag, as the kourier-v1 profile writes a reply: '[' and ']' escaped, '<' an entity.
const (
	inertLink = `\[this\](http://evil.test)`
	inertTag  = `&lt;b>now&lt;/b>`
)

// TestWEB_KourierV1TurnRepliesSignedKeyedAndInert: a kourier-v1 message that opens a thread starts a turn, and its reply reaches the
// receiver as a post of the thread, signed with the connection's signingSecret (the receiver verifies it), with an Idempotency-Key that
// is the post's tag; a reply in the thread is the session's second turn, and its answer, quoting the first message, has that message's
// link and tag made inert.
func TestWEB_KourierV1TurnRepliesSignedKeyedAndInert(t *testing.T) {
	w := newWebScenario(t)
	tok := w.token("turn")
	first := webText(tok)
	open := fakeweb.Message{Channel: ChannelWebKourier, ID: tok + "-1", User: "web-user-a", Text: first}
	w.sendUntilTaken(kourierSend(open), "the message that opens the thread")
	th := w.thread(open.ID, 1, "the first turn's reply")
	post := Posts(th)[0]
	if post.Key == "" || post.Key != post.ID {
		t.Errorf("the post's Idempotency-Key is %q and its id %q; want the key to be the post's tag", post.Key, post.ID)
	}
	if got := markdown(post); !strings.Contains(got, "Message 1.") {
		t.Errorf("the first reply is %q; want the agent's answer to the first turn", got)
	}

	reply := fakeweb.Message{Channel: ChannelWebKourier, Thread: open.ID, ID: tok + "-2", User: "web-user-b", Text: "and then " + tok}
	w.sendUntilTaken(kourierSend(reply), "the reply in the thread")
	th = w.thread(open.ID, 2, "the second turn's reply")
	second := Posts(th)[1]
	got := markdown(second)
	if !strings.Contains(got, "Message 2.") || !strings.Contains(got, tok) {
		t.Errorf("the second reply is %q; want turn 2 of the session, quoting the first message", got)
	}
	if !strings.Contains(got, inertLink) || !strings.Contains(got, inertTag) || strings.Contains(got, "<b>") || strings.Contains(got, "[this](") {
		t.Errorf("the second reply is %q; want the first message's link as %s and its tag as %s, inert", got, inertLink, inertTag)
	}
	if second.Key == post.Key {
		t.Errorf("both posts have the Idempotency-Key %q; want one per post", post.Key)
	}
	for _, r := range th {
		if r.Kind == "post" && r.Repeat {
			t.Logf("the receiver was sent the post %s again (key %q): a retry the key makes harmless", r.ID, r.Key)
		}
	}
	w.stillPosts(open.ID, 2, "the thread holds the two turns' posts and no more")
}

// TestWEB_KourierV1RepeatStartsNoTurn: a message sent again, as the same signed request and re-signed later, is taken (202, counted
// accepted) and dropped: no second turn at the agent, no second reply. Requests unsigned or badly signed are answered 401 and counted
// unverified, and start nothing.
func TestWEB_KourierV1RepeatStartsNoTurn(t *testing.T) {
	w := newWebScenario(t)
	tok := w.token("repeat")
	m := fakeweb.Message{Channel: ChannelWebKourier, ID: tok, User: "web-user-a", Text: "once " + tok}
	sent, _ := w.sendUntilTaken(kourierSend(m), "the message")
	w.thread(m.ID, 1, "the reply")
	w.waitTasks(AgentWebKourier, tok, 1, "the message's one task")

	accepted, unverified := w.requests(ConnWebKourier, "accepted"), w.requests(ConnWebKourier, "unverified")
	duplicates := w.counter("agentkourier_intake_total", map[string]string{"source": "message", "disposition": "duplicate", "connection": ConnWebKourier})
	same := kourierSend(m)
	same.Timestamp = sent.Timestamp
	w.sendStatus(same, http.StatusAccepted, "the same signed request again")
	w.sendStatus(kourierSend(m), http.StatusAccepted, "the message re-signed")
	for _, signing := range []string{fakeweb.SignNone, fakeweb.SignBad} {
		in := kourierSend(fakeweb.Message{Channel: ChannelWebKourier, ID: tok + "-" + signing, User: "web-user-a", Text: "forged " + tok})
		in.Signing = signing
		w.sendStatus(in, http.StatusUnauthorized, "a request signed "+signing)
	}

	w.stillPosts(m.ID, 1, "the thread holds the one reply")
	w.stillTasks(AgentWebKourier, tok, 1, "the agent has the one task of the message")
	if got := w.requests(ConnWebKourier, "accepted") - accepted; got != 2 {
		t.Errorf("agentkourier_web_requests_total{outcome=accepted} rose by %v; want 2, the two repeats", got)
	}
	if got := w.requests(ConnWebKourier, "unverified") - unverified; got != 2 {
		t.Errorf("agentkourier_web_requests_total{outcome=unverified} rose by %v; want 2", got)
	}
	if got := w.counter("agentkourier_intake_total", map[string]string{"source": "message", "disposition": "duplicate", "connection": ConnWebKourier}) - duplicates; got != 2 {
		t.Errorf("the intake counted %v duplicates of the connection; want 2", got)
	}
}

// TestWEB_KourierV1RestartKeepsTheSession: across a restart of Agent Kourier (its Deployment rolled out), a message sent again is
// dropped by the store, not by the memory the restart lost, and a reply in the thread continues the session the first message opened.
func TestWEB_KourierV1RestartKeepsTheSession(t *testing.T) {
	w := newWebScenario(t)
	tok := w.token("restart")
	open := fakeweb.Message{Channel: ChannelWebKourier, ID: tok + "-1", User: "web-user-a", Text: webText(tok)}
	sent, _ := w.sendUntilTaken(kourierSend(open), "the message that opens the thread")
	w.thread(open.ID, 1, "the first turn's reply")

	restart, cancel := context.WithTimeout(w.ctx, 3*time.Minute) // a stuck rollout fails here, with its reason
	defer cancel()
	if err := w.pods.RolloutRestart(restart, kube.AgentKourierNamespace, kube.AgentKourierDeployment); err != nil {
		t.Fatal(err)
	}
	w.connected()
	w.webConnected()

	again := kourierSend(open)
	again.Timestamp = sent.Timestamp // the same signed request, within the skew it is taken in
	w.sendUntilTaken(again, "the first message again, after the restart")
	reply := fakeweb.Message{Channel: ChannelWebKourier, Thread: open.ID, ID: tok + "-2", User: "web-user-b", Text: "after the restart " + tok}
	w.sendUntilTaken(kourierSend(reply), "the reply after the restart")
	th := w.thread(open.ID, 2, "the reply after the restart")
	if got := markdown(Posts(th)[1]); !strings.Contains(got, "Message 2.") || !strings.Contains(got, inertLink) {
		t.Errorf("the reply after the restart is %q; want turn 2 of the session, quoting the first message: the thread kept its session", got)
	}
	w.stillPosts(open.ID, 2, "the first message sent again started no turn")
	tasks := w.waitTasks(AgentWebKourier, tok, 2, "the session's two tasks")
	if tasks[0].ContextID != tasks[1].ContextID {
		t.Errorf("the turns before and after the restart are in the contexts %q and %q; want one", tasks[0].ContextID, tasks[1].ContextID)
	}
}

// TestWEB_FailoverTakesTheResend: on two replicas, with the leader killed, a request to the chat listener's Service finds no pod (no
// answer) or a 503 with a Retry-After until a replica takes over, and the same request sent again is then taken, once: one turn, one
// reply.
func TestWEB_FailoverTakesTheResend(t *testing.T) {
	requireWeb(t)
	f := newFailover(t)
	w := webOn(f.scenario)
	old := f.leader()
	before := f.restarts()
	tok := w.token("failover")
	m := fakeweb.Message{Channel: ChannelWebKourier, ID: tok, User: "web-user-a", Text: "across the failover " + tok}

	if err := f.sig.Kill(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	_, gap := w.sendUntilTaken(kourierSend(m), "the message sent while nobody leads, until it is taken")
	if len(gap) == 0 {
		t.Errorf("the message was taken at once after the leader was killed; want the gap's answers (no answer, or 503) first")
	}
	for _, a := range gap {
		if a.Status == http.StatusServiceUnavailable && a.RetryAfter == "" {
			t.Errorf("a 503 in the gap has no Retry-After")
		}
	}
	t.Logf("%d answers in the gap before the message was taken", len(gap))
	next := f.takeover(old)
	if up, err := f.reps.Counter(f.ctx, next, "agentkourier_chat_connected", map[string]string{"connection": ConnWebKourier}); err != nil || up != 1 {
		t.Errorf("the new leader's web connection %s: connected %v (%v); want 1", ConnWebKourier, up, err)
	}
	w.thread(m.ID, 1, "the reply, after the takeover")
	w.stillPosts(m.ID, 1, "the thread holds the one reply")
	w.stillTasks(AgentWebKourier, tok, 1, "the agent has the one task of the message")
	f.settle()
	f.onlyEnded(before, old.Name)
}

// TestWEB_GitHubPullRequestStartsATurnAndPostsNothing: a signed pull_request opened starts a turn of the github Binding, whose agent is
// prompted with the pull request; output none queues no row and posts nothing. The same body delivered again is dropped; an event the
// connection's own login sent, and a ping, start nothing (ignored); a comment that mentions the login joins the pull request's session;
// requests unsigned or badly signed are answered 401 (unverified).
func TestWEB_GitHubPullRequestStartsATurnAndPostsNothing(t *testing.T) {
	w := newWebScenario(t)
	tok := w.token("pr")
	number := int(time.Now().Unix() % 1_000_000_000) // a pull request, and so a thread, of this run alone
	const binding = WebNamespace + "/github"
	completed := func() float64 {
		return w.counter("agentkourier_turn_outcomes_total", map[string]string{"binding": binding, "outcome": "completed"})
	}
	abandoned, turns := w.sum("agentkourier_outbox_abandoned_total", nil), completed()
	accepted, ignored, unverified := w.requests(ConnWebGitHub, "accepted"), w.requests(ConnWebGitHub, "ignored"), w.requests(ConnWebGitHub, "unverified")

	title := "Fix the parser for " + tok
	opened := fakeweb.PullRequest{Action: "opened", Repo: GitHubRepo, Number: number, Title: title, Sender: "octo-dev"}.Body()
	w.sendUntilTaken(githubSend("pull_request", opened), "the pull request opened")
	first := w.waitTasks(AgentWebGitHub, tok, 1, "the pull request's turn")[0]
	for _, want := range []string{"Repository: " + GitHubRepo, "Pull request: #" + strconv.Itoa(number), "Title: " + title} {
		if !strings.Contains(first.Prompt, want) {
			t.Errorf("the agent was prompted with %q; want it to hold %q", first.Prompt, want)
		}
	}
	if err := Wait(w.ctx, stepTimeout, "the turn to complete", func(context.Context) (bool, error) { return completed()-turns == 1, nil }); err != nil {
		t.Error(err)
	}

	w.sendStatus(githubSend("pull_request", opened), http.StatusAccepted, "the same body, delivered again")
	selfTok := w.token("pr-self")
	for _, login := range []string{GitHubLogin, strings.ToUpper(GitHubLogin)} {
		push := fakeweb.PullRequest{Action: "synchronize", Repo: GitHubRepo, Number: number, Title: title + " " + selfTok, Sender: login}.Body()
		w.sendStatus(githubSend("pull_request", push), http.StatusAccepted, "a push the connection's login made, as "+login)
	}
	w.sendStatus(githubSend("ping", fakeweb.Ping(GitHubRepo)), http.StatusAccepted, "a ping")
	for _, signing := range []string{fakeweb.SignNone, fakeweb.SignBad} {
		in := githubSend("pull_request", fakeweb.PullRequest{Action: "opened", Repo: GitHubRepo, Number: number + 1, Title: "forged " + tok, Sender: "octo-dev"}.Body())
		in.Signing = signing
		w.sendStatus(in, http.StatusUnauthorized, "a delivery signed "+signing)
	}
	w.stillTasks(AgentWebGitHub, tok, 1, "the pull request's one turn: the delivery again, the login's pushes and the forged ones start none")
	if got := w.tasks(AgentWebGitHub, selfTok); len(got) != 0 {
		t.Errorf("the login's own push started %d turns; want none", len(got))
	}

	commentTok := w.token("pr-comment")
	comment := fakeweb.IssueComment{Repo: GitHubRepo, Number: number, Title: title, Sender: "octo-dev", Text: "@" + GitHubLogin + " please look again " + commentTok, CommentID: number}.Body()
	w.sendUntilTaken(githubSend("issue_comment", comment), "a comment that mentions the login")
	second := w.waitTasks(AgentWebGitHub, commentTok, 1, "the comment's turn")[0]
	if second.ContextID != first.ContextID {
		t.Errorf("the comment's turn is in the context %q and the pull request's in %q; want one: the comment joins the pull request's session", second.ContextID, first.ContextID)
	}
	if err := Wait(w.ctx, stepTimeout, "the comment's turn to complete", func(context.Context) (bool, error) { return completed()-turns == 2, nil }); err != nil {
		t.Error(err)
	}

	err := Consistently(w.ctx, quiet, "the github connection has no outbound row and posts nothing", func(ctx context.Context) (bool, error) {
		if pending := w.sum("agentkourier_outbox_pending", map[string]string{"connection": ConnWebGitHub}); pending != 0 {
			return false, fmt.Errorf("agentkourier_outbox_pending of %s is %v", ConnWebGitHub, pending)
		}
		all, err := w.web.Replies(ctx)
		for _, r := range all {
			if r.Connection == ConnWebGitHub {
				return false, fmt.Errorf("the receiver was sent a reply of %s: %s %s", ConnWebGitHub, r.Kind, r.ID)
			}
		}
		return err == nil, err
	})
	if err != nil {
		t.Error(err)
	}
	if got := w.sum("agentkourier_outbox_abandoned_total", nil) - abandoned; got != 0 {
		t.Errorf("agentkourier_outbox_abandoned_total rose by %v; want 0: output none makes no row to abandon", got)
	}
	if got := w.requests(ConnWebGitHub, "accepted") - accepted; got != 3 {
		t.Errorf("agentkourier_web_requests_total{outcome=accepted} rose by %v; want 3: the pull request, its repeat and the comment", got)
	}
	if got := w.requests(ConnWebGitHub, "ignored") - ignored; got != 3 {
		t.Errorf("agentkourier_web_requests_total{outcome=ignored} rose by %v; want 3: the login's two pushes and the ping", got)
	}
	if got := w.requests(ConnWebGitHub, "unverified") - unverified; got != 2 {
		t.Errorf("agentkourier_web_requests_total{outcome=unverified} rose by %v; want 2", got)
	}
}
