package kagentv1

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/dialecttest"
)

// A refused answer says why and where, and never what the person typed: the error is logged, and the value is
// what the person said (it may be a password they pasted into the wrong place). The same error says the same
// two things as before, a choice that is not offered and one chosen twice.
func TestRefusedAnswerErrorsCarryNoValue(t *testing.T) {
	const typed = "xoxb-1111-2222-FAKEFAKEFAKE password=hunter2-FAKE"
	d := &Dialect{}
	two := mustParse(t, dialecttest.KagentTask(t, "askuser/21-pause-good-two-answers.response.sse")) // multi-select with choices, then free text
	for _, tc := range []struct {
		name string
		a    dialect.Answer
		want string
	}{
		{"not one of the choices", dialect.Answer{RequestID: two.RequestID, Answers: [][]string{{typed}, {"blue"}}}, "question 1: answer 1 is not one of the choices"},
		{"a later answer not offered", dialect.Answer{RequestID: two.RequestID, Answers: [][]string{{"MySQL", typed}, {"blue"}}}, "question 1: answer 2 is not one of the choices"},
		{"a choice twice", dialect.Answer{RequestID: two.RequestID, Answers: [][]string{{"MySQL", "MySQL"}, {"blue"}}}, "question 1: answer 2 is chosen twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := d.BuildResponse(*two, tc.a)
			if err == nil {
				t.Fatal("built a response from a refused answer")
			}
			if got := err.Error(); got != tc.want || strings.Contains(got, "hunter2") || strings.Contains(got, "xoxb") {
				t.Errorf("error = %q; want %q, and no value", got, tc.want)
			}
		})
	}
}
