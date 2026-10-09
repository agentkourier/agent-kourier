package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/config"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The owner's channel and the bot of the webhook that posts into it: Alertmanager's alerts, Grafana's and the heartbeat.
const (
	alertChannel = "C0EXAMPLE01"
	webhookBot   = "B0EXAMPLE01"
	// dryRunConfig holds the Bindings that hack/kind/sandbox/README.md has the owner run, so the tests prove those.
	dryRunConfig = "../../hack/kind/sandbox/dryrun"

	kindLink = "http://alertmanager.kind.local/#/alerts?receiver=slack-critical-kind"
	prodLink = "https://alertmanager.example.com/#/alerts?receiver=slack-critical"
	beatLink = "https://alertmanager.example.com/#/alerts?receiver=slack-heartbeat"
)

// The messages of the channel, in the shapes the owner's webhook posts them.
var (
	kindFiring = fakeslack.Attachment{Title: "[kind] FIRING (1) - KubePodCrashLooping", TitleLink: kindLink,
		Fallback: "[FIRING:1] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical) | " + kindLink,
		Text:     "*KubePodCrashLooping* - critical\nPod is crash looping.\n"}
	kindResolved = fakeslack.Attachment{Title: "[kind] RESOLVED - KubePodCrashLooping", TitleLink: kindLink,
		Fallback: "[RESOLVED] KubePodCrashLooping shop (checkout-7d9f8b6c5-x2x4k critical) | " + kindLink, Text: "*KubePodCrashLooping* - critical\n"}
	prodFiring = fakeslack.Attachment{Title: "FIRING (1) - KubeAPIErrorBudgetBurn", TitleLink: prodLink,
		Fallback: "[FIRING:1] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | " + prodLink,
		Text:     "*KubeAPIErrorBudgetBurn* - critical\nThe API server is burning too much error budget.\n"}
	prodResolved = fakeslack.Attachment{Title: "RESOLVED - KubeAPIErrorBudgetBurn", TitleLink: prodLink,
		Fallback: "[RESOLVED] KubeAPIErrorBudgetBurn kube-system (long 1h critical apiserver) | " + prodLink}
	heartbeat = fakeslack.Attachment{Title: "Prometheus heartbeat", TitleLink: beatLink,
		Fallback: "[FIRING:1] Watchdog  (none prometheus) | " + beatLink, Text: "Watchdog firing — Prometheus + Alertmanager are alive."}
	grafana = fakeslack.Attachment{Title: "FIRING: HighRequestLatency (payments)", TitleLink: "https://grafana.example.com/alerting/list",
		Fallback: "FIRING: HighRequestLatency (payments)", Text: "**Firing**\n\nValue: A=0.92\n"}
)

// webhook is a post of the incoming webhook's bot, or of bot, with the attachment.
func webhook(bot string, a fakeslack.Attachment) fakeslack.WebhookPost {
	return fakeslack.WebhookPost{Channel: alertChannel, BotID: bot, Username: "Alertmanager", Attachments: []fakeslack.Attachment{a}}
}

// connect runs a client against the fake, which it needs for the fake to accept a post, and ends it with the test.
func connect(t *testing.T, fake *fakeslack.Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	c := slack.New(slack.Config{BotToken: secret.New("xoxb-FAKE-0123456789"), AppToken: secret.New("xapp-FAKE-0123456789"), APIURL: fake.URL() + "/",
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	done := make(chan struct{})
	go func() { defer close(done); _ = c.Run(ctx, quietSink{}) }()
	t.Cleanup(func() { cancel(); <-done })
	for deadline := time.Now().Add(5 * time.Second); fake.Connections() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the client did not connect")
		}
	}
}

// quietSink takes everything and does nothing with it.
type quietSink struct{}

func (quietSink) Message(context.Context, slack.Message) error         { return nil }
func (quietSink) Interaction(context.Context, slack.Interaction) error { return nil }

// ownersChannel is a fake Slack whose channel holds, oldest first: the kind alert firing and resolving three times, the production
// alert firing and resolving, the heartbeat, Grafana's alert, a person's message, Agent Kourier's own post and an alert of another bot.
// The posts' ts are returned in the same order.
func ownersChannel(t *testing.T) (*fakeslack.Server, []string) {
	t.Helper()
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	connect(t, fake)
	var ts []string
	post := func(p fakeslack.WebhookPost) {
		t.Helper()
		got, err := fake.Webhook(p)
		if err != nil {
			t.Fatal(err)
		}
		ts = append(ts, got.TS)
	}
	for range 3 {
		post(webhook(webhookBot, kindFiring))
		post(webhook(webhookBot, kindResolved))
	}
	post(webhook(webhookBot, prodFiring))
	post(webhook(webhookBot, prodResolved))
	post(webhook(webhookBot, heartbeat))
	post(webhook(webhookBot, grafana))
	person, err := fake.Mention(alertChannel, "U1", "<@UFAKEBOT> is this real?")
	if err != nil {
		t.Fatal(err)
	}
	ts = append(ts, person.TS)
	post(webhook("BFAKEBOT", kindFiring)) // Agent Kourier's own bot
	post(webhook("B0OTHERBOT1", kindFiring))
	return fake, ts
}

// dry runs `agent-kourier dry-run args` against the fake, with the canary as the bot token in the environment, a page of
// three messages so that the history takes several, and returns what it printed and its exit code.
func dry(t *testing.T, fake *fakeslack.Server, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	getenv := env{"SLACK_BOT_TOKEN": canaryToken}.get
	opts, err := parseDryRun(args, getenv)
	if err != nil {
		return "", errb.String() + err.Error(), exitUsage
	}
	opts.apiURL, opts.pageLimit = fake.URL()+"/", 3
	code = execDryRun(t.Context(), opts, getenv, &out, &errb)
	return out.String(), errb.String(), code
}

// replay is the report of `agent-kourier dry-run --json` for the Binding.
func replay(t *testing.T, fake *fakeslack.Server, binding string, extra ...string) dryRunReport {
	t.Helper()
	stdout, stderr, code := dry(t, fake, append([]string{"--config", dryRunConfig, "--binding", "agent-kourier/" + binding, "--json"}, extra...)...)
	if code != exitOK {
		t.Fatalf("exit code %d; stderr %q", code, stderr)
	}
	var r dryRunReport
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatalf("the output is no JSON report: %v\n%s", err, stdout)
	}
	return r
}

func intp(i int) *int { return &i }

// The owner's channel replayed through the trial trigger (the kind receiver) and through production's: each message ends
// in the outcome the intake would give it, oldest first, with the trigger and what was extracted; the same alert is a
// match under one and unmatched under the other. Of the thirteen messages only three fit a page, so this reads them all
// through several.
func TestDryRunReplaysTheOwnersChannelThroughTheTrialAndProduction(t *testing.T) {
	fake, ts := ownersChannel(t)
	kind := func(state string) map[string]string {
		return map[string]string{"state": state, "alertname": "KubePodCrashLooping", "key": "KubePodCrashLooping shop"}
	}
	prod := func(state string) map[string]string {
		return map[string]string{"state": state, "alertname": "KubeAPIErrorBudgetBurn", "key": "KubeAPIErrorBudgetBurn kube-system"}
	}
	type row struct {
		sender  dryRunSender
		outcome string
		trigger *int
		fields  map[string]string
		why     string
	}
	bot := dryRunSender{Kind: "bot", ID: webhookBot}
	for _, tc := range []struct {
		binding string
		rows    []row
		summary dryRunSummary
	}{
		{"trial", []row{
			{bot, "matched", intp(1), kind("FIRING"), ""}, {bot, "matched", intp(1), kind("RESOLVED"), ""},
			{bot, "matched", intp(1), kind("FIRING"), ""}, {bot, "matched", intp(1), kind("RESOLVED"), ""},
			{bot, "matched", intp(1), kind("FIRING"), ""}, {bot, "matched", intp(1), kind("RESOLVED"), ""},
			{bot, "unmatched", nil, nil, "trigger 1: match titleLink"}, {bot, "unmatched", nil, nil, "trigger 1: match titleLink"},
			{bot, "unmatched", nil, nil, "trigger 1: match title"}, {bot, "unmatched", nil, nil, "trigger 1: match fallback"},
			{dryRunSender{Kind: "person", ID: "U1"}, "person", nil, nil, ""},
			{dryRunSender{Kind: "agent-kourier"}, "own", nil, nil, ""},
			{dryRunSender{Kind: "bot", ID: "B0OTHERBOT1"}, "other_bot", nil, nil, ""},
		}, dryRunSummary{Messages: 13,
			Outcomes: map[string]int{"matched": 6, "unmatched": 4, "person": 1, "own": 1, "other_bot": 1},
			Triggers: map[string]int{"1": 6},
			Keys:     []dryRunKey{{"KubePodCrashLooping shop", 6}},
			Flapping: []dryRunFlap{{Key: "KubePodCrashLooping shop", Cycles: 3, States: map[string]int{"FIRING": 3, "RESOLVED": 3}}}}},
		{"production", []row{
			{bot, "unmatched", nil, nil, "trigger 1: match titleLink"}, {bot, "unmatched", nil, nil, "trigger 1: match titleLink"},
			{bot, "unmatched", nil, nil, "trigger 1: match titleLink"}, {bot, "unmatched", nil, nil, "trigger 1: match titleLink"},
			{bot, "unmatched", nil, nil, "trigger 1: match titleLink"}, {bot, "unmatched", nil, nil, "trigger 1: match titleLink"},
			{bot, "matched", intp(1), prod("FIRING"), ""}, {bot, "matched", intp(1), prod("RESOLVED"), ""},
			{bot, "unmatched", nil, nil, "trigger 1: match title"}, {bot, "unmatched", nil, nil, "trigger 1: match fallback"},
			{dryRunSender{Kind: "person", ID: "U1"}, "person", nil, nil, ""},
			{dryRunSender{Kind: "agent-kourier"}, "own", nil, nil, ""},
			{dryRunSender{Kind: "bot", ID: "B0OTHERBOT1"}, "other_bot", nil, nil, ""},
		}, dryRunSummary{Messages: 13,
			Outcomes: map[string]int{"matched": 2, "unmatched": 8, "person": 1, "own": 1, "other_bot": 1},
			Triggers: map[string]int{"1": 2},
			Keys:     []dryRunKey{{"KubeAPIErrorBudgetBurn kube-system", 2}},
			Flapping: nil}},
	} {
		t.Run(tc.binding, func(t *testing.T) {
			r := replay(t, fake, tc.binding)
			if r.Binding != "agent-kourier/"+tc.binding || r.Channel != alertChannel {
				t.Errorf("report is of %s in %s; want agent-kourier/%s in %s", r.Binding, r.Channel, tc.binding, alertChannel)
			}
			if len(r.Messages) != len(tc.rows) {
				t.Fatalf("%d messages; want %d", len(r.Messages), len(tc.rows))
			}
			for i, want := range tc.rows {
				got := r.Messages[i]
				if got.TS != ts[i] {
					t.Errorf("message %d is %s; want %s: oldest first", i, got.TS, ts[i])
				}
				if got.Sender != want.sender || got.Outcome != want.outcome || got.Why != want.why || !maps.Equal(got.Fields, want.fields) ||
					(got.Trigger == nil) != (want.trigger == nil) || (want.trigger != nil && *got.Trigger != *want.trigger) {
					t.Errorf("message %d = %+v;\nwant %+v", i, got, want)
				}
			}
			if got := r.Summary; !summaryEqual(got, tc.summary) {
				t.Errorf("summary = %+v;\nwant %+v", got, tc.summary)
			}
		})
	}
}

func summaryEqual(a, b dryRunSummary) bool {
	return a.Messages == b.Messages && maps.Equal(a.Outcomes, b.Outcomes) && maps.Equal(a.Triggers, b.Triggers) &&
		slices.Equal(a.Keys, b.Keys) && slices.EqualFunc(a.Flapping, b.Flapping, func(x, y dryRunFlap) bool {
		return x.Key == y.Key && x.Cycles == y.Cycles && maps.Equal(x.States, y.States)
	})
}

// A Binding's exclude turns away a message that passes its match, and the report says so.
func TestDryRunShowsWhatAnExcludeTurnedAway(t *testing.T) {
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	connect(t, fake)
	inhibitor := kindFiring
	inhibitor.Title = "[kind] FIRING (1) - InfoInhibitor"
	for _, a := range []fakeslack.Attachment{kindFiring, inhibitor} {
		if _, err := fake.Webhook(webhook(webhookBot, a)); err != nil {
			t.Fatal(err)
		}
	}
	trial, err := os.ReadFile(dryRunConfig + "/trial.yaml")
	if err != nil {
		t.Fatal(err)
	}
	const was = `exclude: {title: '^Prometheus heartbeat$'}`
	if !bytes.Contains(trial, []byte(was)) {
		t.Fatal("trial.yaml no longer has the exclude this test rewrites")
	}
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/binding.yaml", bytes.Replace(trial, []byte(was), []byte(`exclude: {title: '- InfoInhibitor$'}`), 1), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := dry(t, fake, "--config", dir, "--binding", "agent-kourier/trial", "--json")
	if code != exitOK {
		t.Fatalf("exit code %d; stderr %q", code, stderr)
	}
	var r dryRunReport
	if err := json.Unmarshal([]byte(stdout), &r); err != nil {
		t.Fatal(err)
	}
	if len(r.Messages) != 2 || r.Messages[0].Outcome != "matched" || r.Messages[1].Outcome != "excluded" || r.Messages[1].Why != "trigger 1: exclude title" {
		t.Errorf("messages = %+v; want the alert matched and InfoInhibitor excluded by its title", r.Messages)
	}
	if r.Summary.Outcomes["excluded"] != 1 {
		t.Errorf("outcomes = %v; want one excluded", r.Summary.Outcomes)
	}
}

// The text report lists the messages oldest first, one line each, with the outcome, the trigger, what was extracted and the
// title; and then the counts per outcome and trigger, the keys and the keys that flap.
func TestDryRunTextReport(t *testing.T) {
	fake, ts := ownersChannel(t)
	stdout, stderr, code := dry(t, fake, "--config", dryRunConfig, "--binding", "agent-kourier/trial")
	if code != exitOK || stderr != "" {
		t.Fatalf("exit code %d; stderr %q", code, stderr)
	}
	lines := strings.Split(stdout, "\n")
	var rows []string
	for _, l := range lines {
		if regexp.MustCompile(`^\d+\.\d+\s`).MatchString(l) {
			rows = append(rows, l)
		}
	}
	if len(rows) != len(ts) {
		t.Fatalf("%d message lines; want %d:\n%s", len(rows), len(ts), stdout)
	}
	for i, l := range rows {
		if !strings.HasPrefix(l, ts[i]+" ") {
			t.Errorf("line %d starts %q; want ts %s: oldest first", i, strings.Fields(l)[0], ts[i])
		}
	}
	for i, want := range map[int][]string{
		0:  {webhookBot, "matched", "FIRING", "KubePodCrashLooping", "KubePodCrashLooping shop", "[kind] FIRING (1) - KubePodCrashLooping"},
		1:  {webhookBot, "matched", "RESOLVED", "KubePodCrashLooping", "[kind] RESOLVED - KubePodCrashLooping"},
		6:  {webhookBot, "unmatched", "FIRING (1) - KubeAPIErrorBudgetBurn", "trigger 1: match titleLink"},
		8:  {"unmatched", "Prometheus heartbeat", "trigger 1: match title"},
		10: {"person:U1", "person", "is this real?"},
		11: {"agent-kourier", "own"},
		12: {"B0OTHERBOT1", "other_bot"},
	} {
		for _, s := range want {
			if !strings.Contains(rows[i], s) {
				t.Errorf("line %d = %q; want it to hold %q", i, rows[i], s)
			}
		}
	}
	for _, want := range []string{
		"13 messages",
		"matched", "unmatched", "other_bot",
		"trigger 1", // the matches per trigger
		"KubePodCrashLooping shop",
		"flapping", "3 cycles", "3 FIRING", "3 RESOLVED",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the output does not hold %q:\n%s", want, stdout)
		}
	}
	if strings.Contains(stdout, "KubeAPIErrorBudgetBurn kube-system") {
		t.Errorf("the trial's report lists a key of production's alert:\n%s", stdout)
	}
}

// Alert text is untrusted: a title that holds terminal escapes, a newline or a hundred characters is shown on one line,
// cut to about eighty, with no control character; a key that is long is shortened and told apart by a hash; and JSON keeps
// the title whole.
func TestDryRunShowsHostileAndLongTextSafely(t *testing.T) {
	fake := fakeslack.New()
	t.Cleanup(fake.Close)
	connect(t, fake)
	hostile := kindFiring
	hostile.Title = "[kind] FIRING (1) - \x1b[31mRed\x1b[0m\nSECOND LINE \u202eevil " + strings.Repeat("long ", 30)
	longKey := kindFiring
	longKey.Fallback = "[FIRING:1] " + strings.Repeat("a-very-long-group-label ", 6) + "(critical) | " + kindLink
	for _, a := range []fakeslack.Attachment{hostile, longKey} {
		if _, err := fake.Webhook(webhook(webhookBot, a)); err != nil {
			t.Fatal(err)
		}
	}
	stdout, _, code := dry(t, fake, "--config", dryRunConfig, "--binding", "agent-kourier/trial")
	if code != exitOK {
		t.Fatalf("exit code %d", code)
	}
	for _, r := range stdout {
		if r == '\x1b' || r == '\u202e' || (r < ' ' && r != '\n' && r != '\t') {
			t.Fatalf("the output holds the control character %q:\n%q", r, stdout)
		}
	}
	for _, l := range strings.Split(stdout, "\n") {
		if strings.Contains(l, "SECOND LINE") && !strings.Contains(l, "Red") {
			t.Errorf("a title ran over two lines: %q", l)
		}
		if i := strings.Index(l, "[kind] FIRING (1) - "); i >= 0 && strings.Contains(l, "SECOND LINE") {
			// The title is the cell that ends in the ellipsis; the cells after it are the reason.
			end := strings.Index(l[i:], "…")
			if end < 0 || utf8.RuneCountInString(l[i:i+end+len("…")]) > 80 {
				t.Errorf("a title longer than eighty characters was not cut to eighty: %q", l)
			}
		}
	}
	if !regexp.MustCompile(`…#[0-9a-f]{6}`).MatchString(stdout) || strings.Contains(stdout, strings.Repeat("a-very-long-group-label ", 3)) {
		t.Errorf("the long key is not shortened with a hash:\n%s", stdout)
	}
	var r dryRunReport
	jsonOut, _, _ := dry(t, fake, "--config", dryRunConfig, "--binding", "agent-kourier/trial", "--json")
	if err := json.Unmarshal([]byte(jsonOut), &r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.Messages[0].Title, "\x1b[31mRed") || !strings.Contains(r.Messages[1].Fields["key"], strings.Repeat("a-very-long-group-label ", 5)) {
		t.Errorf("JSON should keep the title and the key whole: %+v", r.Messages)
	}
}

// With no --since the whole channel is read. Real Slack refuses an oldest that is not after the epoch
// (invalid_ts_oldest, as the fake now does), so the request must carry none.
func TestDryRunWithoutSinceReadsTheWholeChannel(t *testing.T) {
	fake, ts := ownersChannel(t)
	if r := replay(t, fake, "trial"); len(r.Messages) != len(ts) {
		t.Errorf("a dry run with no --since read %d messages; want all %d", len(r.Messages), len(ts))
	}
}

// --limit keeps the newest messages, --since bounds the reading, and the channel defaults to the Binding's and can be
// overridden.
func TestDryRunLimitSinceAndChannel(t *testing.T) {
	fake, ts := ownersChannel(t)
	if r := replay(t, fake, "trial", "--limit", "4"); len(r.Messages) != 4 || r.Messages[0].TS != ts[len(ts)-4] || r.Summary.Messages != 4 {
		t.Errorf("--limit 4 gave %d messages from %s; want the newest four, from %s", len(r.Messages), r.Messages[0].TS, ts[len(ts)-4])
	}
	if r := replay(t, fake, "trial", "--since", "1h"); len(r.Messages) != len(ts) {
		t.Errorf("--since 1h gave %d messages; want all %d", len(r.Messages), len(ts))
	}
	if r := replay(t, fake, "trial", "--channel", "C0OTHERCHAN"); len(r.Messages) != 0 || r.Channel != "C0OTHERCHAN" {
		t.Errorf("--channel C0OTHERCHAN gave %d messages in %s; want none, in that channel", len(r.Messages), r.Channel)
	}
}

// The bot token never shows: not in any output of a good run, in text or JSON, nor in the errors of the runs that
// fail, whether the token is the cause or not.
func TestDryRunNeverPrintsTheToken(t *testing.T) {
	fake, _ := ownersChannel(t)
	tokenFile := writeSecretFile(t, 0o600, "SLACK_BOT_TOKEN="+canaryToken+"\n")
	for _, tc := range []struct {
		name     string
		args     []string
		wantCode int
	}{
		{"text", []string{"--config", dryRunConfig, "--binding", "agent-kourier/trial"}, exitOK},
		{"json", []string{"--config", dryRunConfig, "--binding", "agent-kourier/production", "--json"}, exitOK},
		{"from a file", []string{"--config", dryRunConfig, "--binding", "agent-kourier/trial", "--token-file", tokenFile}, exitOK},
		{"no such Binding", []string{"--config", dryRunConfig, "--binding", "agent-kourier/nope"}, exitUsage},
		{"no config", []string{"--config", t.TempDir() + "/absent", "--binding", "agent-kourier/trial"}, exitUsage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := dry(t, fake, tc.args...)
			if code != tc.wantCode {
				t.Errorf("exit code %d; want %d (stderr %q)", code, tc.wantCode, stderr)
			}
			if strings.Contains(stdout, canaryToken) || strings.Contains(stderr, canaryToken) || strings.Contains(stdout+stderr, "CANARY") {
				t.Errorf("the token is in the output:\nstdout %s\nstderr %s", stdout, stderr)
			}
		})
	}
}

// slackStub is a Slack that fails auth.test with authError if it is set, and else knows who the bot is and answers every
// history request with historyError. Either may quote the token, as a hostile or broken server could.
func slackStub(t *testing.T, authError, historyError string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/auth.test") {
			if authError != "" {
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": authError})
				return
			}
			_, _ = io.WriteString(w, `{"ok":true,"user_id":"UFAKEBOT","bot_id":"BFAKEBOT"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": historyError})
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/"
}

// A refusal by Slack is reported with the code and what to do about it, with an exit code of its own when the token is the
// cause, and the token is not in it, even if Slack's error quotes it.
func TestDryRunReportsASlackRefusalWithoutTheToken(t *testing.T) {
	for _, tc := range []struct {
		name, authError, slackError, want string
		wantCode                          int
	}{
		{"the bot is not in the channel", "", "not_in_channel", "invite", exitFailure},
		{"the bot lacks the scope", "", "missing_scope", "channels:history", exitFailure},
		{"the token is revoked", "", "token_revoked", "token_revoked", exitSlackAuth},
		{"an error that quotes the token", "", "invalid_auth for " + canaryToken, "invalid_auth", exitSlackAuth},
		{"auth.test refuses the token and quotes it", "invalid_auth for " + canaryToken, "", "invalid_auth", exitSlackAuth},
		{"auth.test says the account is inactive", "account_inactive", "", "account_inactive", exitSlackAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			getenv := env{"SLACK_BOT_TOKEN": canaryToken}.get
			opts, err := parseDryRun([]string{"--config", dryRunConfig, "--binding", "agent-kourier/trial"}, getenv)
			if err != nil {
				t.Fatal(err)
			}
			opts.apiURL = slackStub(t, tc.authError, tc.slackError)
			var out, errb bytes.Buffer
			code := execDryRun(t.Context(), opts, getenv, &out, &errb)
			if code != tc.wantCode || !strings.Contains(errb.String(), tc.want) || out.Len() != 0 {
				t.Errorf("exit code %d, stdout %q, stderr %q; want %d, no stdout and an error that says %q", code, out.String(), errb.String(), tc.wantCode, tc.want)
			}
			if strings.Contains(out.String()+errb.String(), canaryToken) {
				t.Errorf("the token is in the output: %q", errb.String())
			}
		})
	}
}

// The command is reached as `agent-kourier dry-run`, and a wrong command line is a usage error that says what is wrong: a
// token on the command line is no option at all.
func TestDryRunCommandLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no Binding", []string{"dry-run", "--config", dryRunConfig}, "--binding"},
		{"no config", []string{"dry-run", "--binding", "agent-kourier/trial"}, "--config"},
		{"a Binding with no namespace", []string{"dry-run", "--config", dryRunConfig, "--binding", "trial"}, "namespace/name"},
		{"a token as an argument", []string{"dry-run", "--config", dryRunConfig, "--binding", "agent-kourier/trial", "--token", canaryToken}, "not defined"},
		{"a negative limit", []string{"dry-run", "--config", dryRunConfig, "--binding", "agent-kourier/trial", "--limit", "-1"}, "--limit"},
		{"a negative since", []string{"dry-run", "--config", dryRunConfig, "--binding", "agent-kourier/trial", "--since", "-1h"}, "--since"},
		{"a stray argument", []string{"dry-run", "--config", dryRunConfig, "--binding", "agent-kourier/trial", "extra"}, "unexpected argument at position 1"},
		{"a token as a stray argument", []string{"dry-run", "--config", dryRunConfig, "--binding", "agent-kourier/trial", canaryToken}, "unexpected argument at position 1"},
		{"a token as the value of a flag", []string{"dry-run", "--config", dryRunConfig, "--binding", "agent-kourier/trial", "--since", canaryToken}, "-since"},
		{"a token as the Binding", []string{"dry-run", "--config", dryRunConfig, "--binding", canaryToken}, "namespace/name"},
		{"no token", []string{"dry-run", "--config", dryRunConfig, "--binding", "agent-kourier/trial"}, "SLACK_BOT_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(tc.args, "dev", &stdout, &stderr, env{}.get)
			if code != exitUsage || !strings.Contains(stderr.String(), tc.want) || stdout.Len() > 0 {
				t.Errorf("run = %d, stdout %q, stderr %q; want %d and an error that says %q", code, stdout.String(), stderr.String(), exitUsage, tc.want)
			}
			if strings.Contains(stdout.String()+stderr.String(), "CANARY") {
				t.Errorf("the token is in the output: %q", stderr.String())
			}
		})
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"dry-run", "--help"}, "dev", &stdout, &stderr, env{}.get); code != exitOK || !strings.Contains(stderr.String(), "--binding") || strings.Contains(stderr.String(), "xoxb-") {
		t.Errorf("--help = %d, %q; want 0 and the usage", code, stderr.String())
	}
}

// --since is passed on to Slack: a message older than the window is not read, and the same channel read with no --since
// gives all of it.
func TestDryRunSinceReachesSlack(t *testing.T) {
	var clock atomic.Int64 // the fake's idea of now, in seconds
	clock.Store(time.Now().Add(-100 * time.Hour).Unix())
	fake := fakeslack.New(fakeslack.WithClock(func() time.Time { return time.Unix(clock.Load(), 0) }))
	t.Cleanup(fake.Close)
	connect(t, fake)
	post := func(a fakeslack.Attachment) string {
		t.Helper()
		got, err := fake.Webhook(webhook(webhookBot, a))
		if err != nil {
			t.Fatal(err)
		}
		return got.TS
	}
	old := post(kindFiring)
	clock.Store(time.Now().Unix())
	recent := post(kindResolved)
	if r := replay(t, fake, "trial", "--since", "24h"); len(r.Messages) != 1 || r.Messages[0].TS != recent {
		t.Errorf("--since 24h read %+v; want only the message of now, %s", r.Messages, recent)
	}
	if r := replay(t, fake, "trial"); len(r.Messages) != 2 || r.Messages[0].TS != old {
		t.Errorf("no --since read %+v; want both, from %s", r.Messages, old)
	}
}

// flapReport builds the report of the trial over a sequence of states of one alert, F for firing and R for resolved, from
// the loaded Binding as the command builds it.
func flapReport(t *testing.T, states string) dryRunReport {
	t.Helper()
	set, err := config.Load(t.Context(), dryRunConfig)
	if err != nil {
		t.Fatal(err)
	}
	b, err := findBinding(set, "agent-kourier/trial")
	if err != nil {
		t.Fatal(err)
	}
	replay, err := chat.NewReplay(b)
	if err != nil {
		t.Fatal(err)
	}
	var history []chat.HistoryMessage
	for i, st := range states {
		a := kindFiring
		if st == 'R' {
			a = kindResolved
		}
		history = append(history, chat.HistoryMessage{Message: chat.Inbound{
			At:  chat.Ref{Platform: "slack", Channel: alertChannel, ID: strconv.Itoa(1000+i) + ".000000"},
			Bot: &chat.BotMessage{BotID: webhookBot, Title: a.Title, TitleLink: a.TitleLink, Fallback: a.Fallback, AttachmentText: a.Text}}})
	}
	return buildReport("agent-kourier/trial", alertChannel, history, replay, closingStates(b))
}

// A cycle is a closing state after an opening one: a window that starts with a resolve (its firing is before the window)
// and a resolve repeated count for nothing. R,F,F,R,R,F,R is two cycles, short of the three that flag a key, and
// R,F,F,R,R,F,R,F,R is three.
func TestDryRunCountsFlappingCycles(t *testing.T) {
	if r := flapReport(t, "RFFRRFR"); len(r.Summary.Flapping) != 0 {
		t.Errorf("RFFRRFR flagged %+v; want two cycles, which is not flapping", r.Summary.Flapping)
	}
	r := flapReport(t, "RFFRRFRFR")
	if f := r.Summary.Flapping; len(f) != 1 || f[0].Cycles != 3 || f[0].States["FIRING"] != 4 || f[0].States["RESOLVED"] != 5 {
		t.Errorf("RFFRRFRFR flagged %+v; want one key with 3 cycles, 4 FIRING and 5 RESOLVED", f)
	}
	if f := flapReport(t, "FRFRFR").Summary.Flapping; len(f) != 1 || f[0].Cycles != 3 {
		t.Errorf("FRFRFR flagged %+v; want 3 cycles", f)
	}
	if f := flapReport(t, "FFFF").Summary.Flapping; len(f) != 0 {
		t.Errorf("FFFF flagged %+v; want none: nothing closed", f)
	}
}
