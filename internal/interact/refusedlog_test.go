package interact_test

import (
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/interact"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// The line that says the dialect refused an answer names the interaction and why, in words, and never the value the
// person typed: the answer is what they said, and a person may paste a secret into the wrong box. The person is told
// the same as before, and the line's message and keys are the ones an operator greps.
func TestARefusedAnswerIsLoggedWithoutItsValue(t *testing.T) {
	const typed = "xoxb-1111-2222-FAKEFAKEFAKE password=hunter2-FAKE and a long private note about a customer"
	r := newDecideRig(t)
	var out logBuf
	d, err := interact.NewDecider(decideCfg(interact.DecideConfig{
		Store: r.st, Auditor: r.aud, Ephemerals: r.eph, Session: r.sess, Sessions: r.st, Replacer: r.rep, Outbox: r.out,
		Logger: slog.New(telemetry.NewJSONHandler(&out, slog.LevelDebug)),
	}))
	if err != nil {
		t.Fatal(err)
	}
	r.pending(t)
	pd := r.press(t, "UALICE1", d0.Add(time.Minute), "req-1")
	pd.Answer = dialect.Answer{RequestID: "req-1", Answers: [][]string{{typed}}}
	if err := d.Decide(t.Context(), pd); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("%d log lines; want the one refusal:\n%s", len(lines), out.String())
	}
	if strings.Contains(out.String(), "hunter2") || strings.Contains(out.String(), "xoxb") || strings.Contains(out.String(), "customer") {
		t.Errorf("the log holds the person's answer:\n%s", out.String())
	}
	var line map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &line); err != nil {
		t.Fatal(err)
	}
	if line["msg"] != "interact: the dialect refused an answer" || line["level"] != "INFO" || line["interaction"] != "ix-1" ||
		line["err"] != "question 1: answer 1 is not one of the choices" {
		t.Errorf("line = %v; want the message, the interaction and the reason, in the keys operators grep", line)
	}
	if got := r.eph.list(); len(got) != 1 || !strings.Contains(got[0], "could not be used") {
		t.Errorf("ephemerals %q; want the person told the answer could not be used", got)
	}
}
