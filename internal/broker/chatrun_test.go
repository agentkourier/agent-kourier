package broker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
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

func policy(c *clock) RunPolicy {
	return RunPolicy{Now: c.Now, After: c.After, Logger: slog.New(slog.DiscardHandler)}
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
			fails(fmt.Errorf("slack auth.test: %w", slackgo.SlackErrorResponse{Err: code})),
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
		fails(fmt.Errorf("slack auth.test: %w", slackgo.SlackErrorResponse{Err: "internal_error"})),
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

	got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), r)

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

// versioned is a reader whose Secret has a version, and which can rotate the Secret as the first value is read: an update that lands
// between the read of the version and the read of the values.
type versioned struct {
	*reader
	version     atomic.Int64
	rotateOnGet bool
	versionErr  error
}

func (v *versioned) Value(ctx context.Context, ns, name, key string) (secret.Value, error) {
	if v.rotateOnGet {
		v.version.Add(1)
	}
	return v.reader.Value(ctx, ns, name, key)
}

func (v *versioned) SecretVersion(context.Context, string, string) (string, error) {
	if v.versionErr != nil {
		return "", v.versionErr
	}
	return strconv.FormatInt(v.version.Load(), 10), nil
}

// ChooseConnection says which Secret the tokens came from and the version it had, so that a later change of it is told against the
// version the tokens were read under. A reader that keeps no versions, or cannot give one now, leaves the version empty and the
// connection is chosen all the same.
func TestChooseConnectionRecordsTheVersionOfTheCredentialsSecret(t *testing.T) {
	vals := func() *reader {
		return &reader{vals: map[string]string{"platform/creds-slack/botToken": "xoxb-fake-bot", "platform/creds-slack/appToken": "xapp-fake-app"}}
	}
	t.Run("a reader with versions", func(t *testing.T) {
		v := &versioned{reader: vals()}
		v.version.Store(7)
		got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), v)
		if err != nil {
			t.Fatal(err)
		}
		if want := (SecretVersion{Namespace: "platform", Name: "creds-slack", Version: "7"}); got.CredentialsSecret != want {
			t.Errorf("CredentialsSecret = %+v; want %+v", got.CredentialsSecret, want)
		}
	})
	t.Run("a rotation while the values are read is not hidden", func(t *testing.T) {
		v := &versioned{reader: vals(), rotateOnGet: true}
		v.version.Store(7)
		got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), v)
		if err != nil {
			t.Fatal(err)
		}
		if got.CredentialsSecret.Version != "7" {
			t.Fatalf("baseline version = %q; want the version before the values were read, 7", got.CredentialsSecret.Version)
		}
		watch := credentialsWatch{baseline: got.CredentialsSecret, versions: v}
		if !watch.changed(t.Context()) {
			t.Error("the Secret rotated while the tokens were read, and the watch does not see it")
		}
	})
	t.Run("a reader without versions", func(t *testing.T) {
		got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), vals())
		if err != nil {
			t.Fatal(err)
		}
		if want := (SecretVersion{Namespace: "platform", Name: "creds-slack"}); got.CredentialsSecret != want {
			t.Errorf("CredentialsSecret = %+v; want %+v, with no version", got.CredentialsSecret, want)
		}
	})
	t.Run("a version that cannot be read", func(t *testing.T) {
		got, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), &versioned{reader: vals(), versionErr: errors.New("cache not synced")})
		if err != nil {
			t.Fatalf("ChooseConnection = %v; a version that cannot be read must not fail the choice", err)
		}
		if got.CredentialsSecret.Version != "" {
			t.Errorf("version = %q; want none", got.CredentialsSecret.Version)
		}
	})
}

func TestChooseConnectionNeedsExactlyOne(t *testing.T) {
	r := &reader{vals: map[string]string{}}
	for name, snap := range map[string]*config.Snapshot{
		"none": slackConnections(t), "two": slackConnections(t, "platform/a", "platform/b"),
	} {
		_, err := ChooseConnection(t.Context(), snap, r)
		if !errors.Is(err, ErrWiring) {
			t.Errorf("%s: ChooseConnection = %v, want ErrWiring", name, err)
		}
	}
	if _, err := ChooseConnection(t.Context(), slackConnections(t, "platform/a", "platform/b"), r); !strings.Contains(err.Error(), "platform/a") || !strings.Contains(err.Error(), "platform/b") {
		t.Errorf("the error for two connections should name both: %v", err)
	}
}

func TestChooseConnectionRefusesWhatV1CannotRun(t *testing.T) {
	r := &reader{vals: map[string]string{"platform/creds-a/botToken": "xoxb-fake", "platform/creds-a/appToken": "xapp-fake"}}
	snap := slackConnections(t, "platform/a")
	conn := snap.ChatConnections()[0]
	conn.Spec.Platform = "teams"
	if _, err := ChooseConnection(t.Context(), snap, r); !errors.Is(err, ErrWiring) {
		t.Errorf("a connection that is not Slack's: %v, want ErrWiring", err)
	}
	if _, err := ChooseConnection(t.Context(), nil, r); err == nil || errors.Is(err, ErrWiring) {
		t.Errorf("before any config: %v, want an error that is not a wiring error", err)
	}
}

func TestChooseConnectionNamesAMissingKeyNotItsValue(t *testing.T) {
	r := &reader{vals: map[string]string{"platform/creds-slack/botToken": "xoxb-fake-bot"}} // no appToken: the fake reads it as empty
	_, err := ChooseConnection(t.Context(), slackConnections(t, "platform/slack"), r)
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
		_, err := ChooseConnection(t.Context(), snap, failing{tc.err})
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
