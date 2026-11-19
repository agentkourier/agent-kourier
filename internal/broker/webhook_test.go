package broker

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/yaml"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/fakevendors"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// The four webhook recipes (examples/recipes/webhook-*) end to end in one process, each as the broker's own connection on the files
// route: a vendor (test/fakevendors) signs and sends its deliveries to the connection's route on the chat listener; the stub agent reads
// the prompt and posts on the vendor's API, which records the post and sends the delivery of it back with the agent as the actor, which
// the broker drops as the agent's own. Each recipe runs on a broker of its own, since the files route serves one ChatConnection.

// The recipes' objects, as their files name them.
const (
	whNamespace = "acme-webhooks"
	whRecipes   = "../../examples/recipes"
	// whBackendURL is the placeholder URL of each recipe's AgentBackend, which a test puts the stub agent's in place of.
	whBackendURL = "http://REPLACE-ME.agents.svc.cluster.local:8080"
	// whSigningSecret is the webhook secret of every recipe's Secret here: a fake value.
	whSigningSecret = "fake-webhook-signing-secret"
	whGate          = "webhook-held"
)

// The agents' identities, as the recipes' ChatConnections name them.
const (
	whGitHubAgent    = "acme-review-agent[bot]"
	whBitbucketAgent = "712020:00000000-0000-0000-0000-000000000001"
	whJiraAgent      = "712020:00000000-0000-0000-0000-000000000000"
	whLinearAgent    = "00000000-0000-0000-0000-000000000002"
	whHeadSHA        = "9f8e7d6c5b4a39281706f5e4d3c2b1a098765432"
	whBaseSHA        = "0123456789abcdef0123456789abcdef01234567"
)

// whRecipe is one recipe: its vendor, which is the name of its directory's suffix and of its ChatConnection, its Binding, and the
// agent's identity on the vendor.
type whRecipe struct {
	vendor, binding, identity string
}

func (rc whRecipe) connection() string { return whNamespace + "/" + rc.vendor }
func (rc whRecipe) route() string      { return "/chat/webhook/" + whNamespace + "/" + rc.vendor }
func (rc whRecipe) dir() string        { return filepath.Join(whRecipes, "webhook-"+rc.vendor, "kourier") }

// whStep is one delivery a vendor sends, and what follows it: a turn of the agent whose prompt holds prompt, and a post on the vendor's
// API whose body holds post; or, with no post, nothing (the next step's turn number shows that no turn ran).
type whStep struct {
	send   fakevendors.Delivery
	prompt []string
	post   []string
	kind   string
	target string
}

// whFlow is what a test of a recipe sends: steps, in order, each after the one before has settled; start, a delivery that starts an
// investigation in a thread of its own for each n, by the recipe's first trigger; and stale, if the recipe checks a timestamp, a
// delivery signed right whose time is too old.
type whFlow struct {
	steps []whStep
	start func(n int) fakevendors.Delivery
	stale *fakevendors.Delivery
}

func TestWebhookRecipeGitHubReviewsAPullRequestAndRepliesToAMention(t *testing.T) {
	t.Parallel()
	pr := func(n int) fakevendors.Delivery {
		return fakevendors.GitHubPR{Action: "opened", Repo: "acme/widgets", Number: n, Title: "Bound the retry", Body: "It retried forever.",
			HeadSHA: whHeadSHA, BaseSHA: whBaseSHA, HeadRef: "fix/retry", Sender: "octocat"}.Delivery()
	}
	runWebhookRecipe(t, whRecipe{vendor: fakevendors.GitHub, binding: "github-reviews", identity: whGitHubAgent}, whFlow{
		start: func(n int) fakevendors.Delivery { return pr(100 + n) },
		steps: []whStep{
			{send: pr(42),
				prompt: []string{"GitHub pull request #42 in repository acme/widgets needs a review.",
					"Head commit " + whHeadSHA + ", base commit " + whBaseSHA + ".",
					"The webhook event that triggered this", "octocat", "Bound the retry", "fix/retry"},
				post: []string{"Review of acme/widgets#42 at " + whHeadSHA + " (turn 1)"}, kind: "review", target: "acme/widgets#42"},
			// A comment that does not mention the agent starts no turn: the next is turn 2.
			{send: fakevendors.GitHubComment{Repo: "acme/widgets", Number: 42, CommentID: 900, Text: "LGTM", Author: "hubot"}.Delivery()},
			{send: fakevendors.GitHubComment{Repo: "acme/widgets", Number: 42, CommentID: 901, Text: "@acme-review-agent is the retry bounded now?",
				Author: "octocat"}.Delivery(),
				prompt: []string{"A comment on GitHub pull request #42 in repository acme/widgets mentions you;", "@acme-review-agent is the retry bounded now?"},
				post:   []string{"Reply on acme/widgets#42 (turn 2)"}, kind: "comment", target: "acme/widgets#42"},
		},
	})
}

func TestWebhookRecipeBitbucketReviewsAPullRequestAndRepliesToAMention(t *testing.T) {
	t.Parallel()
	pr := func(id int) fakevendors.Delivery {
		return fakevendors.BitbucketPR{Repo: "acme/widgets", ID: id, Title: "Bound the retry", Commit: "a1b2c3d4e5f6", Actor: "712020:sam"}.Delivery()
	}
	runWebhookRecipe(t, whRecipe{vendor: fakevendors.Bitbucket, binding: "bitbucket-reviews", identity: whBitbucketAgent}, whFlow{
		start: func(n int) fakevendors.Delivery { return pr(100 + n) },
		steps: []whStep{
			{send: pr(7),
				prompt: []string{"Bitbucket pull request #7 of acme/widgets needs a review at commit a1b2c3d4e5f6.", "712020:sam", "Bound the retry"},
				post:   []string{"Review of acme/widgets#7 at a1b2c3d4e5f6 (turn 1)"}, kind: "comment", target: "acme/widgets#7"},
			{send: fakevendors.BitbucketComment{Repo: "acme/widgets", ID: 7, CommentID: 50, Text: "@acme-agent and the backoff?", Actor: "712020:sam"}.Delivery(),
				prompt: []string{"A comment on Bitbucket pull request #7 of acme/widgets mentions you;", "@acme-agent and the backoff?"},
				post:   []string{"Reply on acme/widgets#7 (turn 2)"}, kind: "comment", target: "acme/widgets#7"},
		},
	})
}

func TestWebhookRecipeJiraPlansAnIssueAndRevisesThePlanWhenItsSummaryChanges(t *testing.T) {
	t.Parallel()
	now := time.Now().UnixMilli()
	runWebhookRecipe(t, whRecipe{vendor: fakevendors.Jira, binding: "jira-plans", identity: whJiraAgent}, whFlow{
		start: func(n int) fakevendors.Delivery {
			return fakevendors.JiraIssue{Event: "jira:issue_created", Key: fmt.Sprintf("OPS-%d", 100+n), Project: "OPS", Summary: "Tidy the docs",
				Actor: "712020:sam", Timestamp: now}.Delivery()
		},
		steps: []whStep{
			{send: fakevendors.JiraIssue{Event: "jira:issue_created", Key: "OPS-7", Project: "OPS", Summary: "Rename codename FALCON to OSPREY",
				Description: "In the code, the configs and the docs.", Actor: "712020:sam", Timestamp: now}.Delivery(),
				prompt: []string{"Jira issue OPS-7 was created in project OPS.", "Rename codename FALCON to OSPREY"},
				post:   []string{"Plan for OPS-7 (turn 1): rename codename FALCON to OSPREY"}, kind: "comment", target: "OPS-7"},
			// An update of another field starts no turn.
			{send: fakevendors.JiraIssue{Event: "jira:issue_updated", Key: "OPS-7", Project: "OPS", Summary: "Rename codename FALCON to OSPREY",
				Actor: "712020:sam", Timestamp: now + 1, ChangedField: "status"}.Delivery()},
			{send: fakevendors.JiraIssue{Event: "jira:issue_updated", Key: "OPS-7", Project: "OPS", Summary: "Rename codename FALCON to KESTREL",
				Actor: "712020:sam", Timestamp: now + 2, ChangedField: "summary", FromSummary: "Rename codename FALCON to OSPREY"}.Delivery(),
				prompt: []string{"Jira issue OPS-7 of project OPS was updated (its summary changed).", "Rename codename FALCON to KESTREL"},
				post:   []string{"Plan for OPS-7 (turn 2): rename codename FALCON to KESTREL"}, kind: "comment", target: "OPS-7"},
		},
	})
}

func TestWebhookRecipeLinearPlansAnIssueAndAnswersItsDescriptionChangeAndAMention(t *testing.T) {
	t.Parallel()
	// issue is Linear issue ENG-n, created now: the recipe checks the time of a delivery.
	issue := func(n int, title string) fakevendors.LinearIssue {
		return fakevendors.LinearIssue{Action: "create", IssueID: fmt.Sprintf("00000000-0000-4000-8000-%012d", n), Ident: fmt.Sprintf("ENG-%d", n),
			Team: "ENG", Title: title, Actor: "user-sam", Timestamp: time.Now().UnixMilli()}
	}
	stale := issue(99, "Too late")
	stale.Timestamp -= (2 * time.Minute).Milliseconds() // the recipe's window is 1m
	staleDelivery := stale.Delivery()
	retitled, described := issue(7, "Rename codename FALCON to OSPREY, everywhere"), issue(7, "Rename codename FALCON to KESTREL")
	retitled.Action, retitled.FromTitle = "update", "Rename codename FALCON to OSPREY"
	described.Action, described.FromDescription = "update", "Rename it."
	runWebhookRecipe(t, whRecipe{vendor: fakevendors.Linear, binding: "linear-plans", identity: whLinearAgent}, whFlow{
		start: func(n int) fakevendors.Delivery { return issue(100+n, "Tidy the docs").Delivery() },
		stale: &staleDelivery,
		steps: []whStep{
			{send: issue(7, "Rename codename FALCON to OSPREY").Delivery(),
				prompt: []string{"Linear issue ENG-7 was created in team ENG.", "Rename codename FALCON to OSPREY"},
				post:   []string{"Plan for ENG-7 (turn 1): rename codename FALCON to OSPREY"}, kind: "comment", target: "ENG-7"},
			// An update of another field (the title alone) starts no turn.
			{send: retitled.Delivery()},
			{send: described.Delivery(),
				prompt: []string{"The description of Linear issue ENG-7 of team ENG changed.", "Rename it."},
				post:   []string{"Plan for ENG-7 (turn 2): rename codename FALCON to KESTREL"}, kind: "comment", target: "ENG-7"},
			{send: fakevendors.LinearComment{CommentID: "c-1", IssueID: described.IssueID, Ident: "ENG-7", Team: "ENG", Text: "@acme-agent which files?",
				Actor: "user-sam", Timestamp: time.Now().UnixMilli()}.Delivery(),
				prompt: []string{"A comment on Linear issue ENG-7 of team ENG mentions you;", "@acme-agent which files?"},
				post:   []string{"Triage of ENG-7 (turn 3)"}, kind: "comment", target: "ENG-7"},
		},
	})
}

// whRun is one recipe's broker, vendor and agent while a test runs.
type whRun struct {
	t       *testing.T
	rc      whRecipe
	r       *running
	stub    *stubagent.Server
	vendors *fakevendors.Vendors
	logs    *syncBuffer

	mu       sync.Mutex
	prompts  []string // guarded by mu
	accepted int      // the deliveries answered 200 by the test's own sends
}

// runWebhookRecipe runs rc's recipe on a broker of its own and sends flow: deliveries that are not verified first, then the steps, the
// first step's delivery again, and last two starts while a turn is held, the second of which a limit stops.
func runWebhookRecipe(t *testing.T, rc whRecipe, flow whFlow) {
	t.Helper()
	w := startWebhookRecipe(t, rc)

	// Nothing that is not verified reaches the agent: each is answered 401 and counted with its reason.
	unsigned, forged := flow.steps[0].send, flow.steps[0].send
	unsigned.Signing, forged.Signing = fakevendors.SignNone, fakevendors.SignBad
	type refused struct {
		reason string
		d      fakevendors.Delivery
	}
	unverified := []refused{{"unsigned", unsigned}, {"bad_signature", forged}}
	if flow.stale != nil {
		unverified = append(unverified, refused{"stale", *flow.stale})
	}
	for _, u := range unverified {
		if got := w.send(u.d); got != http.StatusUnauthorized {
			t.Fatalf("a delivery that is %s was answered %d; want 401", u.reason, got)
		}
		w.r.waitMetric(w.series("unverified", u.reason) + " 1")
	}

	turns := 0
	for i, st := range flow.steps {
		turns = w.step(i, st, turns)
		if i == 0 {
			// The same body again, as a vendor's redelivery is, is taken once: no turn runs, and the next step's turn number shows it.
			if got := w.send(st.send); got != http.StatusOK {
				t.Fatalf("the first step's delivery, sent again, was answered %d; want 200", got)
			}
			w.r.waitMetric(fmt.Sprintf(`agentkourier_trigger_decisions_total{binding=%q,decision="dedup"} 1`, whNamespace+"/"+rc.binding))
		}
	}
	posts := w.vendors.Posts()

	// While a turn of the first trigger runs, a start of it in another thread is stopped by maxConcurrent: answered 200, and logged at
	// WARN with its delivery ID, by which it can be found in the vendor's log and sent again. The first start is sent until no limit
	// stops it, since a turn of the steps may still be ending: a limited event is decided again when it is sent again.
	limits := fmt.Sprintf(`agentkourier_trigger_limited_total{binding=%q,limit="concurrent",trigger="0"}`, whNamespace+"/"+rc.binding)
	w.stub.Agent("agents", rc.vendor).Next(stubagent.Hold(whGate), stubagent.Text("held"))
	held, stopped := flow.start(1), w.value(limits)
	testutil.Eventually(t, "a start of the first trigger that no limit stops", func() bool {
		if got := w.send(held); got != http.StatusOK {
			t.Fatalf("a start was answered %d; want 200", got)
		}
		if n := w.value(limits); n != stopped {
			stopped = n
			return false
		}
		return true
	}, testutil.Within(15*time.Second), testutil.Every(50*time.Millisecond))
	limited := flow.start(2)
	if got := w.send(limited); got != http.StatusOK {
		t.Fatalf("a start that a limit stops was answered %d; want 200", got)
	}
	if got := w.value(limits); got != stopped+1 {
		t.Errorf("%s = %v after a start while a turn of the first trigger is held; want %v", limits, got, stopped+1)
	}
	if id := limited.ID(); id == "" || !w.loggedLimited(id) {
		t.Errorf("no WARN of a webhook event a limit stopped names its delivery %q:\n%s", id, w.logs.String())
	}
	w.stub.Release(whGate)

	// The agent's own events were answered 200 and dropped, each counted as self; every other delivery the test sent and was answered
	// 200 was accepted.
	w.r.waitMetric(w.series("self", "none") + fmt.Sprintf(" %d", len(posts)))
	w.r.waitMetric(w.series("accepted", "none") + fmt.Sprintf(" %d", w.accepted))
	w.r.cancel()
	if err := w.r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended; want nil", err)
	}
}

// startWebhookRecipe starts a broker on rc's recipe, with the stub agent in place of the recipe's agent and a vendor that sends to it,
// and waits until the connection takes requests. Each of opts changes the broker's config before it starts.
func startWebhookRecipe(t *testing.T, rc whRecipe, opts ...func(*RunConfig)) *whRun {
	t.Helper()
	w := &whRun{t: t, rc: rc, stub: stubagent.New(), logs: &syncBuffer{}}
	t.Cleanup(w.stub.Close)
	c := startConfig(t)
	runsAlongside(&c)
	c.ChatListener = someListener(t)
	c.Logger = slog.New(slog.NewTextHandler(w.logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	vendors, err := fakevendors.New(fakevendors.Config{ChatURL: "http://" + c.ChatListener.Addr().String(), Sources: map[string]fakevendors.Source{
		rc.vendor: {Path: rc.route(), Secret: []byte(whSigningSecret), Identity: rc.identity}}})
	if err != nil {
		t.Fatal(err)
	}
	w.vendors = vendors
	api := httptest.NewServer(vendors)
	t.Cleanup(api.Close)
	stubagent.WebhookAgent(w.stub, "agents", rc.vendor, api.URL, func(_ int, prompt string) {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.prompts = append(w.prompts, prompt)
	})
	c.ConfigDir = recipeConfigDir(t, rc, w.stub.AgentURL("agents", rc.vendor))
	c.Client = fake.NewClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: whNamespace, Name: rc.vendor + "-webhook"},
		Data: map[string][]byte{v1alpha1.WebhookSecretKey: []byte(whSigningSecret)}})
	sink := &spySink{}
	c.sink = sink
	for _, o := range opts {
		o(&c)
	}
	t.Cleanup(func() {
		if got := sink.seen(); len(got) != 0 {
			t.Errorf("the renderer's sink was given %q; want nothing of a connection with output: none", got)
		}
	})
	w.r = start(t, c)
	w.r.until("/readyz", http.StatusOK)
	// Every series of the connection exists from when its Run starts.
	w.r.waitMetric(w.series("accepted", "none") + " 0")
	return w
}

// recipeConfigDir is a config directory with rc's recipe files, its AgentBackend's URL agentURL.
func recipeConfigDir(t *testing.T, rc whRecipe, agentURL string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"chatconnection.yaml", "binding.yaml", "agentbackend.yaml"} {
		b, err := os.ReadFile(filepath.Join(rc.dir(), name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "agentbackend.yaml" {
			if n := strings.Count(string(b), whBackendURL); n != 1 {
				t.Fatalf("%s holds the placeholder URL %d times; want once", name, n)
			}
			b = []byte(strings.Replace(string(b), whBackendURL, agentURL, 1))
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// send has the vendor send d and returns the broker's answer. A 503 asks the vendor to send again, which it does, as a vendor would; any
// other answer is returned.
func (w *whRun) send(d fakevendors.Delivery) int {
	w.t.Helper()
	var sent fakevendors.Sent
	testutil.Eventually(w.t, "a delivery answered other than 503", func() bool {
		sent = w.vendors.Send(d)
		return sent.Status != http.StatusServiceUnavailable
	}, testutil.Within(10*time.Second), testutil.Every(20*time.Millisecond))
	if sent.Status == 0 {
		w.t.Fatalf("the delivery had no answer: %s", sent.Error)
	}
	if sent.Status == http.StatusOK {
		w.mu.Lock()
		w.accepted++
		w.mu.Unlock()
	}
	return sent.Status
}

// step sends st as step i, of a flow that has run turns so far, and checks what follows it. It returns the turns run after it.
func (w *whRun) step(i int, st whStep, turns int) int {
	w.t.Helper()
	before := len(w.vendors.Posts())
	if got := w.send(st.send); got != http.StatusOK {
		w.t.Fatalf("step %d: the broker answered %d; want 200", i, got)
	}
	if st.post == nil {
		return turns
	}
	turns++
	testutil.Eventually(w.t, fmt.Sprintf("step %d: the agent to post", i), func() bool { return len(w.vendors.Posts()) > before },
		testutil.Within(15*time.Second), testutil.Every(20*time.Millisecond))
	posts := w.vendors.Posts()
	if len(posts) != before+1 {
		w.t.Fatalf("step %d: the API has %d posts; want %d", i, len(posts), before+1)
	}
	p := posts[before]
	if p.Kind != st.kind || p.Target != st.target || p.SelfStatus != http.StatusOK {
		w.t.Errorf("step %d: the post is %+v; want a %s on %s, its own delivery answered 200", i, p, st.kind, st.target)
	}
	for _, want := range st.post {
		if !strings.Contains(p.Body, want) {
			w.t.Errorf("step %d: the post %q lacks %q", i, p.Body, want)
		}
	}
	w.mu.Lock()
	got := slices.Clone(w.prompts)
	w.mu.Unlock()
	if len(got) != turns {
		w.t.Fatalf("step %d: the agent was prompted %d times; want %d", i, len(got), turns)
	}
	for _, want := range st.prompt {
		if !strings.Contains(got[turns-1], want) {
			w.t.Errorf("step %d: the prompt lacks %q:\n%s", i, want, got[turns-1])
		}
	}
	return turns
}

// A delivery's span is the parent of the intake's, and the turn it starts is in its trace. The request's span ends with its answer,
// while the turn goes on: the turn's span is restored from the trace context the intake stored with the turn, as a Slack envelope's
// turn is, so that one trace holds the delivery, its intake and the turn it started.
func TestAWebhookDeliverysSpanIsTheParentOfItsIntakeAndTurn(t *testing.T) {
	t.Parallel()
	exporter := tracetest.NewInMemoryExporter()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
	t.Cleanup(func() { _ = provider.Shutdown(context.Background()) })
	rc := whRecipe{vendor: fakevendors.GitHub, binding: "github-reviews", identity: whGitHubAgent}
	w := startWebhookRecipe(t, rc, func(c *RunConfig) { c.TracerProvider = provider })
	d := fakevendors.GitHubPR{Action: "opened", Repo: "acme/widgets", Number: 42, Title: "Bound the retry", Body: "It retried forever.",
		HeadSHA: whHeadSHA, BaseSHA: whBaseSHA, HeadRef: "fix/retry", Sender: "octocat"}.Delivery()
	w.step(0, whStep{send: d, post: []string{"Review of acme/widgets#42"}, kind: "review", target: "acme/widgets#42"}, 0)

	var request tracetest.SpanStub
	var found int
	for _, s := range exporter.GetSpans() {
		attrs := map[attribute.Key]string{}
		for _, a := range s.Attributes {
			attrs[a.Key] = a.Value.String()
		}
		if s.Name == "agentkourier.webhook.request" && attrs["agentkourier.webhook.delivery"] == d.ID() {
			request, found = s, found+1
			if attrs["agentkourier.outcome"] != "accepted" || attrs["agentkourier.connection"] != rc.connection() || s.SpanKind != trace.SpanKindServer {
				t.Errorf("the delivery's span is %s with %v; want an accepted server span of %s", s.SpanKind, attrs, rc.connection())
			}
		}
	}
	if found != 1 {
		t.Fatalf("%d spans of the delivery %s, want 1", found, d.ID())
	}
	under := func(name string, of trace.SpanContext) func() bool {
		return func() bool {
			for _, s := range exporter.GetSpans() {
				if s.Name == name && s.SpanContext.TraceID() == of.TraceID() && (name != "agentkourier.intake" || s.Parent.SpanID() == of.SpanID()) {
					return true
				}
			}
			return false
		}
	}
	if !under("agentkourier.intake", request.SpanContext)() {
		t.Errorf("no intake span is a child of the delivery's span %s", request.SpanContext.SpanID())
	}
	testutil.Eventually(t, "a turn attempt in the delivery's trace", under("agentkourier.turn.attempt", request.SpanContext))
	w.r.cancel()
	if err := w.r.wait(); err != nil {
		t.Fatalf("Run = %v after its context ended; want nil", err)
	}
}

// series is the series of agentkourier_webhook_requests_total of the recipe's connection with outcome and reason, as /metrics writes it.
func (w *whRun) series(outcome, reason string) string {
	return fmt.Sprintf(`agentkourier_webhook_requests_total{connection=%q,outcome=%q,reason=%q}`, w.rc.connection(), outcome, reason)
}

// value is the value of series on /metrics, or 0 when it is not there.
func (w *whRun) value(series string) float64 {
	w.t.Helper()
	_, body := w.r.get("/metrics")
	for line := range strings.Lines(body) {
		if v, ok := strings.CutPrefix(strings.TrimSuffix(line, "\n"), series+" "); ok {
			n, err := strconv.ParseFloat(v, 64)
			if err != nil {
				w.t.Fatalf("the series %q: %v", line, err)
			}
			return n
		}
	}
	return 0
}

// loggedLimited says whether a WARN line says that a webhook event a limit stopped, of the delivery id, is lost unless it is sent again.
func (w *whRun) loggedLimited(id string) bool {
	for line := range strings.Lines(w.logs.String()) {
		if strings.Contains(line, "level=WARN") && strings.Contains(line, "a webhook event was not investigated") &&
			strings.Contains(line, "delivery="+id) && strings.Contains(line, "binding="+whNamespace+"/"+w.rc.binding) {
			return true
		}
	}
	return false
}

// A team's webhook connection on the custom resources runs as the own one does: the GitHub recipe's objects, in the team's namespace,
// take a signed delivery on the chat listener at the team's route and start a turn of the agent, whose own review is dropped.
func TestATeamWebhookConnectionOnTheCustomResourcesStartsATurn(t *testing.T) {
	const team = "team-a"
	stub := stubagent.New()
	t.Cleanup(stub.Close)
	w := newConnWorld(t, nil)
	runsAlongside(&w.c)
	w.c.ChatListener = someListener(t)
	vendors, err := fakevendors.New(fakevendors.Config{ChatURL: "http://" + w.c.ChatListener.Addr().String(), Sources: map[string]fakevendors.Source{
		fakevendors.GitHub: {Path: "/chat/webhook/" + team + "/github", Secret: []byte(whSigningSecret), Identity: whGitHubAgent}}})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(vendors)
	t.Cleanup(api.Close)
	stubagent.WebhookAgent(stub, "agents", "github", api.URL, nil)
	if _, err := w.kube.CoreV1().Secrets(team).Create(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: team, Name: "github-webhook"},
		Data: map[string][]byte{v1alpha1.WebhookSecretKey: []byte(whSigningSecret)}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	rc := whRecipe{vendor: fakevendors.GitHub}
	for _, obj := range recipeObjects(t, rc.dir()) {
		obj.SetNamespace(team)
		obj.SetUID(types.UID("uid-" + team + "-" + obj.GetName()))
		obj.SetGeneration(1)
		gvr := map[string]schema.GroupVersionResource{"ChatConnection": crConnections, "Binding": crBindings, "AgentBackend": crBackends}[obj.GetKind()]
		if obj.GetKind() == "AgentBackend" {
			_ = unstructured.SetNestedField(obj.Object, stub.AgentURL("agents", "github"), "spec", "url")
		}
		if err := w.dyn.Tracker().Create(gvr, obj, team); err != nil {
			t.Fatal(err)
		}
	}
	r := start(t, w.c)
	r.until("/readyz", http.StatusOK)

	// Until the supervisor runs the team's connection its route is not on the listener, or answers 503: the vendor sends again.
	d := fakevendors.GitHubPR{Action: "opened", Repo: "acme/widgets", Number: 42, Title: "Bound the retry", HeadSHA: whHeadSHA, BaseSHA: whBaseSHA,
		HeadRef: "fix/retry", Sender: "octocat"}.Delivery()
	var sent fakevendors.Sent
	w.eventually("the team's webhook connection to take a delivery", func() bool { sent = vendors.Send(d); return sent.Status == http.StatusOK })
	w.eventually("the agent to post its review", func() bool { return len(vendors.Posts()) == 1 })
	if p := vendors.Posts()[0]; p.Kind != "review" || p.Target != "acme/widgets#42" || p.SelfStatus != http.StatusOK ||
		!strings.Contains(p.Body, "Review of acme/widgets#42 at "+whHeadSHA+" (turn 1)") {
		t.Errorf("the post is %+v; want the review of acme/widgets#42 at the head commit, its own delivery answered 200", p)
	}
	r.waitMetric(fmt.Sprintf(`agentkourier_webhook_requests_total{connection=%q,outcome="self",reason="none"} 1`, team+"/github"))
}

// recipeObjects reads the objects of the recipe files in dir.
func recipeObjects(t *testing.T, dir string) []*unstructured.Unstructured {
	t.Helper()
	var objs []*unstructured.Unstructured
	for _, name := range []string{"chatconnection.yaml", "binding.yaml", "agentbackend.yaml"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		js, err := utilyaml.ToJSON(b)
		if err != nil {
			t.Fatal(err)
		}
		u := &unstructured.Unstructured{}
		if err := yaml.Unmarshal(js, &u.Object); err != nil {
			t.Fatal(err)
		}
		objs = append(objs, u)
	}
	return objs
}
