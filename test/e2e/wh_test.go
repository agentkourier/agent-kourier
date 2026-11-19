//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakevendors"
)

// The WH_ scenarios drive the fast tier's webhook connections, the four recipes of examples/recipes/webhook-*, from outside the broker: the fake
// vendors (test/fakevendors) sign a delivery as the vendor does and POST it to the chat listener's Service at the route of the vendor's connection,
// and are the vendors' APIs that the stub agent's webhook agents post on, each post sent back as the vendor's delivery with the agent's own account
// as its actor. They run in the fast tier on custom resources (E2E_CONFIG_SOURCE=crds make e2e-fast-up), whose bring-up admits the recipes'
// namespace (hack/kind/webhook-values.yaml), and skip anywhere else: the files route serves exactly one ChatConnection, the broker's own Slack one.
// The recipes on the files route are internal/broker's in-process tests. Each scenario uses pull requests and issues of numbers of its own, so
// that a thread, and so a session, is never one of an earlier run: the stub, the fake and the store outlive a run.

// whQuiet is how long a scenario watches for what must not happen (a post, a turn) once what must has.
const whQuiet = 5 * time.Second

// The values the scenarios' deliveries carry beside their numbers.
const (
	whRepo    = "acme/widgets"
	whHeadSHA = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	whBaseSHA = "0123456789abcdef0123456789abcdef01234567"
	whCommit  = "a1b2c3d4e5f6"
	whRename  = "Rename codename FALCON to OSPREY"
	// whAnswered is the message of the INFO line the webhook platform logs for each request it verified.
	whAnswered = "webhook: delivery answered"
)

// whScenario is a scenario with the fake vendors.
type whScenario struct {
	*scenario
	vendors *Vendors
}

// requireWebhooks returns when the run is in the fast tier with the webhook connections served: on custom resources, with their namespace
// admitted. It skips in another tier, on the files route, which serves the one ChatConnection of its directory, and on the admission of the
// tier with two Slack apps, which admits team-a's namespace in place of the recipes' (the MC_ tests hold it to that). Any other admission on
// custom resources fails the test: up-fast.sh admits the namespace there (webhook-values.yaml), and a bring-up that lost the overlay must not
// pass as a skip. A selector or namespaces it cannot read fail the test too (Env.Admits).
func requireWebhooks(t *testing.T) {
	t.Helper()
	env := NewEnv(os.Getenv)
	if err := CheckTier(env.Tier); err != nil {
		t.Fatal(err)
	}
	if env.Tier != "fast" {
		t.Skipf("the webhook connections are the fast tier's: their doubles, the fake vendors and the stub's webhook agents, run there, not in the %s tier", env.Tier)
	}
	if env.ConfigSource != "crds" {
		t.Skipf("the broker reads its config from %s (%s): the webhook connections are custom resources of a namespace of their own, which the files route, serving its one connection, never reads; bring the tier up with E2E_CONFIG_SOURCE=crds", env.ConfigSource, env.Admission())
	}
	admitted, err := env.Admits(WebhookNamespace)
	if err == nil && admitted {
		return
	}
	if teams, terr := env.Admits(kube.MultiTeamNamespace); terr == nil && teams {
		t.Skipf("the broker admits the namespaces of the tier with two Slack apps (%s), which leaves %s out: bring the tier up without E2E_TWO_APPS", env.Admission(), WebhookNamespace)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("on custom resources the fast tier admits %s (hack/kind/webhook-values.yaml, which up-fast.sh gives the route upgrade), and this broker does not (%s): the bring-up lost the overlay", WebhookNamespace, env.Admission())
}

// newWebhookScenario returns a whScenario once the broker is ready and the four webhook connections run, or skips (requireWebhooks).
func newWebhookScenario(t *testing.T) *whScenario {
	t.Helper()
	requireWebhooks(t)
	w := &whScenario{scenario: newScenario(t)}
	w.vendors = NewVendors(w.env.VendorsURL)
	for _, vendor := range WebhookVendors {
		conn := WebhookConnection(vendor)
		err := Wait(w.ctx, 2*time.Minute, "the webhook connection "+conn+" to run", func(ctx context.Context) (bool, error) {
			up, err := w.leaderCounter(ctx, "agentkourier_chat_connected", map[string]string{"connection": conn})
			if errors.Is(err, ErrNoSample) {
				return false, nil
			}
			return up == 1, err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return w
}

// sum is the sum of the leader's samples of name that have labels, 0 when it has none.
func (w *whScenario) sum(name string, labels map[string]string) float64 {
	w.t.Helper()
	var v float64
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

// requests is the leader's agentkourier_webhook_requests_total of vendor's connection with outcome and reason.
func (w *whScenario) requests(vendor, outcome, reason string) float64 {
	w.t.Helper()
	return w.counter("agentkourier_webhook_requests_total", map[string]string{"connection": WebhookConnection(vendor), "outcome": outcome, "reason": reason})
}

// served is the leader's agentkourier_chat_listener_requests_total of the requests to vendor's connection that its handler was given.
func (w *whScenario) served(vendor string) float64 {
	w.t.Helper()
	return w.counter("agentkourier_chat_listener_requests_total", map[string]string{"outcome": "served", "platform": "webhook", "connection": WebhookConnection(vendor)})
}

// limited is the leader's count of the starts of vendor's Binding that a limit stopped, by every limit and trigger.
func (w *whScenario) limited(vendor string) float64 {
	w.t.Helper()
	return w.sum("agentkourier_trigger_limited_total", map[string]string{"binding": WebhookBinding(vendor)})
}

// whSent is how the broker answered the sends of one delivery: the last answer, and every one before it.
type whSent struct {
	last   fakevendors.Sent
	before []fakevendors.Sent
}

// handled is how many of the sends the chat listener gave to the connection's handler, as far as the answers tell: every one but those that got no
// answer, when no 503 came, which the listener answers too, for a connection that does not run on this replica.
func (s whSent) handled() (n int, exact bool) {
	exact = true
	for _, a := range append(append([]fakevendors.Sent(nil), s.before...), s.last) {
		switch a.Status {
		case 0:
		case http.StatusServiceUnavailable:
			exact = false
		default:
			n++
		}
	}
	return n, exact
}

// sendUntilAnswered has the fake send d until the broker answers it with other than a 503 or no answer at all, as a vendor sends again: a 503
// after its Retry-After, and a request that got no answer (the chat Service has no endpoint while the pod restarts) after a second. It fails the
// test after two minutes, or on an error of the fake's own.
func (w *whScenario) sendUntilAnswered(d fakevendors.Delivery, desc string) whSent {
	w.t.Helper()
	var out whSent
	deadline := time.Now().Add(2 * time.Minute)
	for {
		sent, err := w.vendors.Send(w.ctx, d)
		if err != nil {
			w.t.Fatal(err)
		}
		if sent.Status != 0 && sent.Status != http.StatusServiceUnavailable {
			out.last = sent
			return out
		}
		out.before = append(out.before, sent)
		if time.Now().After(deadline) {
			w.t.Fatalf("%s: not answered in 2m; the last answer: %d %s", desc, sent.Status, sent.Error)
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

// sendStatus sends d (sendUntilAnswered) and fails the test unless the broker answered want.
func (w *whScenario) sendStatus(d fakevendors.Delivery, want int, desc string) whSent {
	w.t.Helper()
	s := w.sendUntilAnswered(d, desc)
	if s.last.Status != want {
		w.t.Fatalf("%s: answered %d (%s); want %d", desc, s.last.Status, s.last.Error, want)
	}
	return s
}

// start sends d, a delivery that starts a turn of vendor's Binding, until the broker takes it (200) and no limit stops it: a turn of an earlier
// scenario may still be ending, and the recipes' triggers run one turn at a time (maxConcurrent 1). A limited event is answered 200 and not
// remembered, so a vendor that sends it again has it decided again. It fails the test after a minute of limits, which is the recipe's rate limit
// (maxRuns an hour) spent by earlier runs on a tier that outlives them.
func (w *whScenario) start(vendor string, d fakevendors.Delivery, desc string) whSent {
	w.t.Helper()
	var answers []fakevendors.Sent
	deadline := time.Now().Add(time.Minute)
	for {
		limited := w.limited(vendor)
		s := w.sendStatus(d, http.StatusOK, desc)
		answers = append(append(answers, s.before...), s.last)
		// The trigger decides after the answer: a start a limit stops shows on the counter at once after it, or not at all.
		if err := Consistently(w.ctx, time.Second, "no limit stops "+desc, func(context.Context) (bool, error) {
			return w.limited(vendor) == limited, nil
		}); err == nil {
			return whSent{last: answers[len(answers)-1], before: answers[:len(answers)-1]}
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("%s: a limit of %s stopped it for a minute (agentkourier_trigger_limited_total %v); the recipe allows 10 starts an hour: the tier's runs may have spent them", desc, WebhookBinding(vendor), w.limited(vendor))
		}
	}
}

// postsOn are the posts on vendor's API on target, oldest first.
func (w *whScenario) postsOn(ctx context.Context, vendor, target string) ([]fakevendors.Post, error) {
	all, err := w.vendors.Posts(ctx)
	var out []fakevendors.Post
	for _, p := range all {
		if p.Vendor == vendor && p.Target == target {
			out = append(out, p)
		}
	}
	return out, err
}

// waitPosts waits for n posts of the agent on vendor's API on target, and returns them.
func (w *whScenario) waitPosts(vendor, target string, n int, desc string) []fakevendors.Post {
	w.t.Helper()
	var got []fakevendors.Post
	err := Wait(w.ctx, stepTimeout, desc, func(ctx context.Context) (bool, error) {
		var err error
		got, err = w.postsOn(ctx, vendor, target)
		return len(got) >= n, err
	})
	if err != nil {
		w.t.Fatalf("%s: %v\nthe posts on %s: %+v; starts a limit stopped: %v", desc, err, target, got, w.limited(vendor))
	}
	if len(got) != n {
		w.t.Fatalf("%s: %d posts on %s; want %d: %+v", desc, len(got), target, n, got)
	}
	return got
}

// stillPosts checks that vendor's API holds n posts on target for whQuiet: no turn starts after the ones the scenario waited for.
func (w *whScenario) stillPosts(vendor, target string, n int, desc string) {
	w.t.Helper()
	var got []fakevendors.Post
	err := Consistently(w.ctx, whQuiet, desc, func(ctx context.Context) (bool, error) {
		var err error
		got, err = w.postsOn(ctx, vendor, target)
		return len(got) == n, err
	})
	if err != nil {
		w.t.Errorf("%v\nthe posts on %s: %+v", err, target, got)
	}
}

// answered are the lines of the INFO "webhook: delivery answered" that name the delivery id, in the log of every Agent Kourier pod.
func (w *whScenario) answered(id string) []map[string]any {
	w.t.Helper()
	text, err := w.agentKourierLogs(w.ctx)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []map[string]any
	for _, line := range strings.Split(text, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == whAnswered && rec["delivery"] == id {
			out = append(out, rec)
		}
	}
	return out
}

// waitAnswered waits for n INFO lines of the delivery id (answered) and checks that each is of vendor's connection with outcome.
func (w *whScenario) waitAnswered(vendor, id, outcome string, n int) {
	w.t.Helper()
	var got []map[string]any
	err := Wait(w.ctx, 30*time.Second, "the INFO line of the delivery "+id, func(context.Context) (bool, error) {
		got = w.answered(id)
		return len(got) >= n, nil
	})
	if err != nil || len(got) != n {
		w.t.Fatalf("%d %q lines name the delivery %s (%v); want %d", len(got), whAnswered, id, err, n)
	}
	for _, rec := range got {
		if rec["level"] != "INFO" || rec["connection"] != WebhookConnection(vendor) || rec["outcome"] != outcome || rec["reason"] != "none" {
			w.t.Errorf("the line of the delivery %s is %v; want INFO, of %s, outcome %s, reason none", id, rec, WebhookConnection(vendor), outcome)
		}
	}
}

// whNumber is a pull request's or an issue's number that only this run of a scenario uses.
func whNumber() int { return 1_000_000 + rand.IntN(9_000_000) }

// whStart is the first delivery of vendor's recipe for its pull request or issue n, and what the agent posts for it: a post of kind on target
// whose body holds text.
type whStart struct {
	d                  fakevendors.Delivery
	kind, target, text string
}

// startOf is whStart of vendor's recipe for n.
func startOf(t *testing.T, vendor string, n int) whStart {
	t.Helper()
	now := time.Now().UnixMilli()
	switch vendor {
	case fakevendors.GitHub:
		return whStart{fakevendors.GitHubPR{Action: "opened", Repo: whRepo, Number: n, Title: "Bound the retry", Body: "It retried forever.",
			HeadSHA: whHeadSHA, BaseSHA: whBaseSHA, HeadRef: "fix/retry", Sender: "octocat"}.Delivery(),
			"review", fmt.Sprintf("%s#%d", whRepo, n), fmt.Sprintf("Review of %s#%d at %s (turn 1)", whRepo, n, whHeadSHA)}
	case fakevendors.Bitbucket:
		return whStart{fakevendors.BitbucketPR{Repo: whRepo, ID: n, Title: "Bound the retry", Commit: whCommit, Actor: "712020:sam"}.Delivery(),
			"comment", fmt.Sprintf("%s#%d", whRepo, n), fmt.Sprintf("Review of %s#%d at %s (turn 1)", whRepo, n, whCommit)}
	case fakevendors.Jira:
		key := fmt.Sprintf("OPS-%d", n)
		return whStart{fakevendors.JiraIssue{Event: "jira:issue_created", Key: key, Project: "OPS", Summary: whRename,
			Description: "In the code, the configs and the docs.", Actor: "712020:sam", Timestamp: now}.Delivery(),
			"comment", key, "Plan for " + key + " (turn 1): rename codename FALCON to OSPREY"}
	case fakevendors.Linear:
		key := fmt.Sprintf("ENG-%d", n)
		return whStart{linearIssue(n, now).Delivery(), "comment", key, "Plan for " + key + " (turn 1): rename codename FALCON to OSPREY"}
	}
	t.Fatalf("no recipe of the vendor %q", vendor)
	return whStart{}
}

// linearIssue is Linear issue ENG-n, created at the time now (in ms).
func linearIssue(n int, now int64) fakevendors.LinearIssue {
	return fakevendors.LinearIssue{Action: "create", IssueID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), Ident: fmt.Sprintf("ENG-%d", n),
		Team: "ENG", Title: whRename, Actor: "user-sam", Timestamp: now}
}

// TestWH_EachRecipeTakesASignedDeliveryAndDropsTheAgentsOwnPost: for each recipe, a delivery signed as the vendor signs it is taken (200,
// counted accepted, and logged at INFO with its delivery ID) and starts a turn of the recipe's agent, which reads the recipe's prompt and posts
// on the vendor's API; the vendor's delivery of that post, with the agent's own account as its actor, is answered 200 and dropped (counted self),
// and starts no turn. The chat listener counts each request under the webhook platform and the connection.
func TestWH_EachRecipeTakesASignedDeliveryAndDropsTheAgentsOwnPost(t *testing.T) {
	w := newWebhookScenario(t)
	for _, vendor := range WebhookVendors {
		t.Run(vendor, func(t *testing.T) {
			sc := *w.scenario
			sc.t, sc.ctx = t, t.Context()
			w := &whScenario{scenario: &sc, vendors: w.vendors}
			st := startOf(t, vendor, whNumber())
			accepted, self, served := w.requests(vendor, "accepted", "none"), w.requests(vendor, "self", "none"), w.served(vendor)

			sent := w.start(vendor, st.d, "the "+vendor+" delivery")
			p := w.waitPosts(vendor, st.target, 1, "the agent's post on "+st.target)[0]
			if p.Kind != st.kind || !strings.Contains(p.Body, st.text) {
				t.Errorf("the post is %+v; want a %s on %s that holds %q", p, st.kind, st.target, st.text)
			}
			if p.SelfStatus != http.StatusOK {
				t.Errorf("the vendor's delivery of the agent's own post was answered %d; want 200, dropped as the agent's own", p.SelfStatus)
			}
			taken, exact := sent.handled()
			if got := w.requests(vendor, "accepted", "none") - accepted; got != float64(taken) {
				t.Errorf("agentkourier_webhook_requests_total{outcome=accepted} of %s rose by %v; want %d, the sends answered 200", WebhookConnection(vendor), got, taken)
			}
			if got := w.requests(vendor, "self", "none") - self; got != 1 {
				t.Errorf("agentkourier_webhook_requests_total{outcome=self} of %s rose by %v; want 1, the agent's own post", WebhookConnection(vendor), got)
			}
			switch got := w.served(vendor) - served; {
			case exact && got != float64(taken+1):
				t.Errorf("agentkourier_chat_listener_requests_total{outcome=served,platform=webhook} of %s rose by %v; want %d: the deliveries and the agent's own", WebhookConnection(vendor), got, taken+1)
			case got < float64(taken+1):
				t.Errorf("agentkourier_chat_listener_requests_total{outcome=served,platform=webhook} of %s rose by %v; want at least %d", WebhookConnection(vendor), got, taken+1)
			}
			w.waitAnswered(vendor, st.d.ID(), "accepted", taken)
			w.stillPosts(vendor, st.target, 1, "the agent's own post starts no turn")
		})
	}
}

// TestWH_UnverifiedDeliveriesAreRefusedAndCountedByReason: a delivery with no signature, one signed with another secret, and a Linear delivery
// signed right whose time is past the recipe's window are each answered 401, counted unverified with their reason (unsigned, bad_signature,
// stale), and start nothing. The chat listener gave each to the connection's handler; the platform logs none of them at INFO.
func TestWH_UnverifiedDeliveriesAreRefusedAndCountedByReason(t *testing.T) {
	w := newWebhookScenario(t)
	gh := startOf(t, fakevendors.GitHub, whNumber())
	unsigned, forged := gh.d, gh.d
	unsigned.Signing, forged.Signing = fakevendors.SignNone, fakevendors.SignBad
	n := whNumber()
	staleIssue := linearIssue(n, time.Now().Add(-2*time.Minute).UnixMilli()) // the recipe's window is 1m
	stale := staleIssue.Delivery()

	for _, tc := range []struct {
		vendor, reason string
		d              fakevendors.Delivery
	}{
		{fakevendors.GitHub, "unsigned", unsigned},
		{fakevendors.GitHub, "bad_signature", forged},
		{fakevendors.Linear, "stale", stale},
	} {
		before, served := w.requests(tc.vendor, "unverified", tc.reason), w.served(tc.vendor)
		sent := w.sendStatus(tc.d, http.StatusUnauthorized, "a "+tc.vendor+" delivery that is "+tc.reason)
		if got := w.requests(tc.vendor, "unverified", tc.reason) - before; got != 1 {
			t.Errorf("agentkourier_webhook_requests_total{outcome=unverified,reason=%s} of %s rose by %v; want 1", tc.reason, WebhookConnection(tc.vendor), got)
		}
		handled, exact := sent.handled()
		if got := w.served(tc.vendor) - served; (exact && got != float64(handled)) || got < float64(handled) {
			t.Errorf("agentkourier_chat_listener_requests_total{outcome=served,platform=webhook} of %s rose by %v after a delivery that is %s; want %d", WebhookConnection(tc.vendor), got, tc.reason, handled)
		}
	}
	w.stillPosts(fakevendors.GitHub, gh.target, 0, "no turn of the unverified GitHub deliveries")
	w.stillPosts(fakevendors.Linear, staleIssue.Ident, 0, "no turn of the stale Linear delivery")
	for _, id := range []string{unsigned.ID(), stale.ID()} {
		if lines := w.answered(id); len(lines) != 0 {
			t.Errorf("the unverified delivery %s is logged at INFO: %v; want it at DEBUG alone", id, lines)
		}
	}
}

// TestWH_ARedeliveryIsTakenOnceAndStartsNoSecondTurn: the same GitHub delivery sent again, as GitHub's redelivery is, is answered 200 and
// counted accepted, logged at INFO with the same delivery ID, and decided a duplicate by the trigger: no second turn, no second review. A comment
// that mentions the agent in the pull request then starts the session's second turn, not its third.
func TestWH_ARedeliveryIsTakenOnceAndStartsNoSecondTurn(t *testing.T) {
	w := newWebhookScenario(t)
	const vendor = fakevendors.GitHub
	n := whNumber()
	st := startOf(t, vendor, n)
	sent := w.start(vendor, st.d, "the pull request")
	w.waitPosts(vendor, st.target, 1, "the review")
	taken, _ := sent.handled()

	dedup := map[string]string{"binding": WebhookBinding(vendor), "decision": "dedup"}
	before, accepted := w.counter("agentkourier_trigger_decisions_total", dedup), w.requests(vendor, "accepted", "none")
	w.sendStatus(st.d, http.StatusOK, "the same delivery again")
	if err := Wait(w.ctx, 30*time.Second, "the trigger to decide the redelivery a duplicate", func(context.Context) (bool, error) {
		return w.counter("agentkourier_trigger_decisions_total", dedup) == before+1, nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := w.requests(vendor, "accepted", "none") - accepted; got != 1 {
		t.Errorf("agentkourier_webhook_requests_total{outcome=accepted} rose by %v after the redelivery; want 1: the platform takes it, and the trigger drops it", got)
	}
	w.waitAnswered(vendor, st.d.ID(), "accepted", taken+1)
	w.stillPosts(vendor, st.target, 1, "the redelivery starts no turn")

	mention := fakevendors.GitHubComment{Repo: whRepo, Number: n, CommentID: n, Text: "@acme-review-agent is the retry bounded now?", Author: "octocat"}.Delivery()
	w.start(vendor, mention, "the comment that mentions the agent")
	reply := w.waitPosts(vendor, st.target, 2, "the reply to the mention")[1]
	if want := fmt.Sprintf("Reply on %s (turn 2)", st.target); reply.Kind != "comment" || !strings.Contains(reply.Body, want) {
		t.Errorf("the reply is %+v; want a comment that holds %q: the redelivery ran no turn of the session", reply, want)
	}
}
