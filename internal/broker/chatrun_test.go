package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	slackgo "github.com/slack-go/slack"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
)

// runner is a PlatformRunner that plays one scripted step for each Run.
type runner struct {
	steps []func(call int) error
	calls int
}

func (r *runner) Classify(op chat.Op, err error) chat.Verdict {
	return slack.NewPlatform(nil, "ns/conn").Classify(op, err)
}

func (r *runner) Run(context.Context, chat.Sink) error {
	r.calls++
	return r.steps[min(r.calls, len(r.steps))-1](r.calls)
}

// clock is a fake clock whose After records each wait and returns at once, or never.
type clock struct {
	now   time.Time
	waits []time.Duration
	hang  bool // After never fires: the wait ends only with the context
}

func (c *clock) Now() time.Time { return c.now }

func (c *clock) After(d time.Duration) <-chan time.Time {
	c.waits = append(c.waits, d)
	ch := make(chan time.Time, 1)
	if !c.hang {
		ch <- c.now
	}
	return ch
}

// policy is the clock's policy with no jitter (Random says 0, which cuts nothing off the pause), so a test reads the pause itself.
func policy(c *clock) RunPolicy {
	return RunPolicy{Now: c.Now, After: c.After, Random: func() float64 { return 0 }, Logger: slog.New(slog.DiscardHandler)}
}

func fails(err error) func(int) error { return func(int) error { return err } }

func secs(n ...int) []time.Duration {
	var out []time.Duration
	for _, s := range n {
		out = append(out, time.Duration(s)*time.Second)
	}
	return out
}

// stopAfter ends the context during the call, as a shutdown signal would, and returns what Run does then.
func stopAfter(cancel context.CancelFunc) func(int) error {
	return func(int) error { cancel(); return context.Canceled }
}

func TestRunPlatformBacksOffFromOneSecondToAMinute(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{}
	steps := make([]func(int) error, 0, 10)
	for range 9 {
		steps = append(steps, fails(errors.New("connection reset")))
	}
	steps = append(steps, stopAfter(cancel))

	err := RunPlatform(ctx, &runner{steps: steps}, nil, policy(c))

	if !errors.Is(err, context.Canceled) || errors.Is(err, ErrSlackAuth) {
		t.Errorf("RunPlatform = %v, want the context's end", err)
	}
	if want := secs(1, 2, 4, 8, 16, 32, 60, 60, 60); !slices.Equal(c.waits, want) {
		t.Errorf("waits = %v, want %v: doubling from 1 s, capped at 60 s", c.waits, want)
	}
}

func TestRunPlatformStopsAtOnceOnAFatalAuthError(t *testing.T) {
	for _, code := range []string{"invalid_auth", "token_revoked", "account_inactive", "not_authed"} {
		ctx, cancel := context.WithCancel(t.Context())
		c := &clock{}
		r := &runner{steps: []func(int) error{
			fails(fmt.Errorf("slack auth.test call: %w", slackgo.SlackErrorResponse{Err: code})),
			stopAfter(cancel), // a second run only happens if the error was retried
		}}

		err := RunPlatform(ctx, r, nil, policy(c))
		cancel()

		if !errors.Is(err, ErrSlackAuth) || !strings.Contains(err.Error(), code) {
			t.Errorf("%s: RunPlatform = %v, want ErrSlackAuth naming the code", code, err)
		}
		if r.calls != 1 || len(c.waits) != 0 {
			t.Errorf("%s: %d runs and %d waits, want one run and no retry", code, r.calls, len(c.waits))
		}
	}
}

func TestRunPlatformRetriesWhatIsNotFatal(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{}
	r := &runner{steps: []func(int) error{
		fails(fmt.Errorf("slack socket mode: %w", slackgo.SlackErrorResponse{Err: "ratelimited"})),
		fails(fmt.Errorf("slack auth.test call: %w", slackgo.SlackErrorResponse{Err: "internal_error"})),
		fails(nil), // Run ends only on error, but a nil is no reason to exit either
		stopAfter(cancel),
	}}

	if err := RunPlatform(ctx, r, nil, policy(c)); errors.Is(err, ErrSlackAuth) || r.calls != 4 {
		t.Errorf("RunPlatform = %v after %d runs, want a retry of each of the first three", err, r.calls)
	}
}

// A connection that stayed up for ResetAfter was healthy, so the next failure starts the backoff over.
func TestRunPlatformResetsTheBackoffAfterALongRun(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{}
	short := fails(errors.New("reset"))
	r := &runner{steps: []func(int) error{
		short, short, short, // waits 1, 2, 4
		func(int) error { c.now = c.now.Add(130 * time.Second); return errors.New("reset") }, // up for 130 s: wait 1
		short, // wait 2
		func(int) error { c.now = c.now.Add(119 * time.Second); return errors.New("reset") }, // up for 119 s: not enough, wait 4
		stopAfter(cancel),
	}}

	_ = RunPlatform(ctx, r, nil, policy(c))

	if want := secs(1, 2, 4, 1, 2, 4); !slices.Equal(c.waits, want) {
		t.Errorf("waits = %v, want %v", c.waits, want)
	}
}

func TestRunPlatformStopsWhenTheContextEndsDuringABackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{hang: true}
	r := &runner{steps: []func(int) error{func(int) error { cancel(); return errors.New("reset") }}}

	if err := RunPlatform(ctx, r, nil, policy(c)); !errors.Is(err, context.Canceled) {
		t.Errorf("RunPlatform = %v, want the context's end", err)
	}
	if r.calls != 1 {
		t.Errorf("%d runs, want the loop to stop", r.calls)
	}
}

// failsThenStops is n steps that each fail, then one that ends the context.
func failsThenStops(n int, cancel context.CancelFunc) []func(int) error {
	steps := make([]func(int) error, 0, n+1)
	for range n {
		steps = append(steps, fails(errors.New("connection reset")))
	}
	return append(steps, stopAfter(cancel))
}

// The pause is cut by a random share of its upper half, so a wait is never longer than the pause (the cap is the
// longest wait) and never shorter than half of it. The pause itself keeps doubling from what it was, not from what was waited.
func TestRunPlatformCutsEachPauseByUpToHalf(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{}
	cuts := []float64{0, 0.5, 0.999999, 0.25, 0, 0.75, 0.5, 0.999999}
	next := 0
	p := policy(c)
	p.Random = func() float64 { next++; return cuts[(next-1)%len(cuts)] }

	_ = RunPlatform(ctx, &runner{steps: failsThenStops(8, cancel)}, nil, p)

	pauses := secs(1, 2, 4, 8, 16, 32, 60, 60)
	if len(c.waits) != len(pauses) {
		t.Fatalf("waits = %v; want %d of them", c.waits, len(pauses))
	}
	for i, pause := range pauses {
		want := pause - time.Duration(cuts[i]*float64(pause/2))
		if c.waits[i] != want || c.waits[i] > pause || c.waits[i] < pause/2 {
			t.Errorf("wait %d = %v after a cut of %v; want %v, between %v and %v", i, c.waits[i], cuts[i], want, pause/2, pause)
		}
	}
}

// A random source that strays outside [0, 1) cannot make a wait longer than the pause or shorter than half of it.
func TestRunPlatformClampsAStrayRandomNumber(t *testing.T) {
	for _, tc := range []struct {
		name string
		r    float64
		want time.Duration
	}{{"negative", -3, time.Second}, {"one", 1, 500 * time.Millisecond}, {"far above one", 40, 500 * time.Millisecond}} {
		ctx, cancel := context.WithCancel(t.Context())
		c := &clock{}
		p := policy(c)
		p.Random = func() float64 { return tc.r }

		_ = RunPlatform(ctx, &runner{steps: failsThenStops(1, cancel)}, nil, p)

		if len(c.waits) != 1 || c.waits[0] != tc.want {
			t.Errorf("%s: waits = %v; want [%v]", tc.name, c.waits, tc.want)
		}
	}
}

// Connections that fail at the same moment, as every one does when a network blips, wait different times, none past the
// pause: they do not come back together. Each is a RunPlatform with a random source of its own.
func TestRunPlatformSpreadsConnectionsThatFailTogether(t *testing.T) {
	const n = 16
	waited := map[time.Duration]bool{}
	for i := range n {
		ctx, cancel := context.WithCancel(t.Context())
		c := &clock{}
		p := policy(c)
		p.Random = rand.New(rand.NewPCG(uint64(i), 7)).Float64

		_ = RunPlatform(ctx, &runner{steps: failsThenStops(1, cancel)}, nil, p)

		if len(c.waits) != 1 || c.waits[0] > time.Second || c.waits[0] < 500*time.Millisecond {
			t.Fatalf("connection %d: waits = %v; want one between 500ms and 1s", i, c.waits)
		}
		waited[c.waits[0]] = true
	}
	if len(waited) < n*3/4 {
		t.Errorf("%d connections that failed together waited %d different times; want them spread", n, len(waited))
	}
}

// With no random source of its own RunPlatform draws one, so a broker's connections differ without a test's help.
func TestRunPlatformJittersByDefault(t *testing.T) {
	waited := map[time.Duration]bool{}
	for range 20 {
		ctx, cancel := context.WithCancel(t.Context())
		c := &clock{}
		p := policy(c)
		p.Random = nil

		_ = RunPlatform(ctx, &runner{steps: failsThenStops(1, cancel)}, nil, p)

		if len(c.waits) != 1 || c.waits[0] > time.Second || c.waits[0] < 500*time.Millisecond {
			t.Fatalf("waits = %v; want one between 500ms and 1s", c.waits)
		}
		waited[c.waits[0]] = true
	}
	if len(waited) < 2 {
		t.Errorf("20 runs waited %d different times; want a random cut", len(waited))
	}
}

// A platform that was told to wait (Slack's Retry-After) is waited for if that is longer than the pause, as slack-go
// did for the reconnect, and the pause does not wait on it when it is shorter.
func TestRunPlatformWaitsForWhatSlackAskedFor(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{}
	limited := func(d time.Duration) func(int) error {
		return fails(fmt.Errorf("slack socket mode connection attempt failed: %w", &slackgo.RateLimitedError{RetryAfter: d}))
	}
	r := &runner{steps: []func(int) error{limited(90 * time.Second), limited(time.Second), limited(0), limited(time.Duration(1<<62) / 2), limited(24 * time.Hour), stopAfter(cancel)}}

	_ = RunPlatform(ctx, r, nil, policy(c))

	// The pauses are 1 s, 2 s, 4 s, 8 s and 16 s. A Retry-After of 90 s wins over the first; 1 s loses to the second; a limit that
	// names no wait is one second (Classify), which loses to the third; one of any size, a day included, is cut to retryAfterMax.
	if want := secs(90, 2, 4, 300, 300); !slices.Equal(c.waits, want) {
		t.Errorf("waits = %v; want %v", c.waits, want)
	}
}

// slackConnections resolves a snapshot of ChatConnections named by namespace/name.
func slackConnections(t *testing.T, names ...string) *config.Snapshot {
	t.Helper()
	set := &config.Set{}
	for _, n := range names {
		ns, name, _ := strings.Cut(n, "/")
		set.ChatConnections = append(set.ChatConnections, v1alpha1.ChatConnection{
			ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
			Spec:       v1alpha1.ChatConnectionSpec{Platform: v1alpha1.PlatformSlack, CredentialsSecretRef: v1alpha1.SecretRef{Name: "creds-" + name}},
		})
	}
	snap, err := config.Resolve(t.Context(), set, keys{})
	if err != nil {
		t.Fatal(err)
	}
	return snap
}

func TestChooseConnectionReadsTheTokensOfTheOneConnection(t *testing.T) {
	r := &reader{vals: map[string]string{"platform/creds-slack/botToken": "xoxb-fake-bot", "platform/creds-slack/appToken": "xapp-fake-app"}}

	got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), v1alpha1.ObjectRef{}, r)

	if err != nil {
		t.Fatal(err)
	}
	if got.Ref.Namespace != "platform" || got.Ref.Name != "slack" || got.Credentials["botToken"].Reveal() != "xoxb-fake-bot" || got.Credentials["appToken"].Reveal() != "xapp-fake-app" {
		t.Errorf("ChooseConnection = %+v, want platform/slack with the bot and app tokens of its credentials Secret", got.Ref)
	}
	if strings.Contains(fmt.Sprintf("%v %+v", got, got), "xoxb-fake-bot") {
		t.Error("the connection prints a token")
	}
}

func TestChooseConnectionNeedsExactlyOne(t *testing.T) {
	r := &reader{vals: map[string]string{}}
	for name, snap := range map[string]*config.Snapshot{
		"none": slackConnections(t), "two": slackConnections(t, "platform/a", "platform/b"),
	} {
		_, err := ChooseConnection(t.Context(), snap, v1alpha1.ObjectRef{}, r)
		if !errors.Is(err, ErrWiring) {
			t.Errorf("%s: ChooseConnection = %v, want ErrWiring", name, err)
		}
	}
	if _, err := ChooseConnection(t.Context(), slackConnections(t, "platform/a", "platform/b"), v1alpha1.ObjectRef{}, r); !strings.Contains(err.Error(), "platform/a") || !strings.Contains(err.Error(), "platform/b") {
		t.Errorf("the error for two connections should name both: %v", err)
	}
}

// With the custom resources the snapshot may hold the ChatConnections of the admitted namespaces besides the broker's own: the
// broker's own is the one it runs (the others are not run yet), found by its reference and not by being the only one. The files
// route has no reference and keeps exactly one.
func TestChooseConnectionPicksTheOwnConnectionAmongSeveral(t *testing.T) {
	r := &reader{vals: map[string]string{
		"platform/creds-slack/botToken": "xoxb-fake-own", "platform/creds-slack/appToken": "xapp-fake-own",
		"team-a/creds-slack/botToken": "xoxb-fake-a", "team-a/creds-slack/appToken": "xapp-fake-a",
	}}
	own := v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}
	// The own connection is neither the first nor the last of the snapshot, which is sorted by namespace and name.
	snap := slackConnections(t, "team-a/slack", "platform/slack", "zeta/slack")

	got, err := ChooseConnection(t.Context(), snap, own, r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ref != own || got.Credentials["botToken"].Reveal() != "xoxb-fake-own" || got.Credentials["appToken"].Reveal() != "xapp-fake-own" {
		t.Errorf("ChooseConnection = %+v, want platform/slack with its own tokens, not another connection's", got.Ref)
	}
	// One connection that is the own is chosen as before.
	if got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), own, r); err != nil || got.Ref != own {
		t.Errorf("ChooseConnection over the own alone = %+v, %v; want it", got.Ref, err)
	}
	// The files route, which names none, still refuses several.
	if _, err := ChooseConnection(t.Context(), snap, v1alpha1.ObjectRef{}, r); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("several connections and no own: ChooseConnection = %v; want ErrWiring saying exactly one", err)
	}
}

// An own connection that the snapshot lacks is the operator's, whatever else the snapshot holds. And a team connection never
// decides the outcome: its Secret is not read here, so one that is missing or unsynced cannot fail a takeover.
func TestChooseConnectionNeedsTheOwnConnectionToBeThere(t *testing.T) {
	r := &reader{vals: map[string]string{"team-a/creds-slack/botToken": "xoxb-fake-a", "team-a/creds-slack/appToken": "xapp-fake-a"}}
	own := v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}
	_, err := ChooseConnection(t.Context(), slackConnections(t, "team-a/slack"), own, r)
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "platform/slack") {
		t.Errorf("ChooseConnection = %v; want an ErrWiring naming platform/slack", err)
	}

	own = v1alpha1.ObjectRef{Namespace: "platform", Name: "slack"}
	r = &reader{vals: map[string]string{"platform/creds-slack/botToken": "xoxb-fake-own", "platform/creds-slack/appToken": "xapp-fake-own"}} // team-a's Secret is not there
	if got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack", "team-a/slack"), own, r); err != nil || got.Ref != own {
		t.Errorf("ChooseConnection with a team Secret missing = %+v, %v; want the own connection", got.Ref, err)
	}
}

func TestChooseConnectionRefusesWhatV1CannotRun(t *testing.T) {
	r := &reader{vals: map[string]string{"platform/creds-a/botToken": "xoxb-fake", "platform/creds-a/appToken": "xapp-fake"}}
	snap := slackConnections(t, "platform/a")
	conn := snap.ChatConnections()[0]
	conn.Spec.Platform = "teams"
	if _, err := ChooseConnection(t.Context(), snap, v1alpha1.ObjectRef{}, r); !errors.Is(err, ErrWiring) {
		t.Errorf("a connection that is not Slack's: %v, want ErrWiring", err)
	}
	if _, err := ChooseConnection(t.Context(), nil, v1alpha1.ObjectRef{}, r); err == nil || errors.Is(err, ErrWiring) {
		t.Errorf("before any config: %v, want an error that is not a wiring error", err)
	}
}

func TestChooseConnectionNamesAMissingKeyNotItsValue(t *testing.T) {
	r := &reader{vals: map[string]string{"platform/creds-slack/botToken": "xoxb-fake-bot"}} // no appToken: the fake reads it as empty
	_, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), v1alpha1.ObjectRef{}, r)
	if err == nil || !strings.Contains(err.Error(), "appToken") || strings.Contains(err.Error(), "xoxb-fake-bot") {
		t.Errorf("ChooseConnection = %v, want an error naming appToken and holding no token", err)
	}
	if !errors.Is(err, ErrWiring) {
		t.Errorf("an empty token: %v, want ErrWiring, which exits 2", err)
	}
}

// failing is a reader whose every read fails with err.
type failing struct{ err error }

func (f failing) Value(context.Context, string, string, string) (secret.Value, error) {
	return secret.Value{}, f.err
}

// A Secret or key that is missing is the operator's wiring; a Secret that cannot be read just now is not.
func TestChooseConnectionWiringErrorsAreOnlyTheOperators(t *testing.T) {
	snap := slackConnections(t, "platform/slack")
	for name, tc := range map[string]struct {
		err    error
		wiring bool
	}{
		"key missing":     {fmt.Errorf("read: %w", secret.ErrKeyNotFound), true},
		"Secret missing":  {fmt.Errorf("read: %w", config.ErrSecretNotFound), true},
		"value empty":     {fmt.Errorf("read: %w", secret.ErrEmptyValue), true},
		"cache not ready": {errors.New("secret cache not synced"), false},
	} {
		_, err := ChooseConnection(t.Context(), snap, v1alpha1.ObjectRef{}, failing{tc.err})
		if err == nil || errors.Is(err, ErrWiring) != tc.wiring || !errors.Is(err, tc.err) {
			t.Errorf("%s: ChooseConnection = %v, want wiring=%t and the cause kept", name, err, tc.wiring)
		}
	}
}

// A connection that stayed up for exactly ResetAfter counts as healthy.
func TestRunPlatformResetsAtExactlyTheThreshold(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{}
	short := fails(errors.New("reset"))
	r := &runner{steps: []func(int) error{
		short, short, // waits 1, 2
		func(int) error { c.now = c.now.Add(120 * time.Second); return errors.New("reset") }, // exactly 120 s: wait 1
		stopAfter(cancel),
	}}

	_ = RunPlatform(ctx, r, nil, policy(c))

	if want := secs(1, 2, 1); !slices.Equal(c.waits, want) {
		t.Errorf("waits = %v, want %v", c.waits, want)
	}
}

// A Base above Max still never waits longer than Max.
func TestRunPlatformFirstPauseIsCappedToo(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	c := &clock{}
	p := policy(c)
	p.Base, p.Max = 90*time.Second, 60*time.Second
	r := &runner{steps: []func(int) error{fails(errors.New("reset")), stopAfter(cancel)}}

	_ = RunPlatform(ctx, r, nil, p)

	if want := secs(60); !slices.Equal(c.waits, want) {
		t.Errorf("waits = %v, want %v", c.waits, want)
	}
}
