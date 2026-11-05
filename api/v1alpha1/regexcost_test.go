package v1alpha1

import (
	"fmt"
	"regexp/syntax"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// instructions is what the limits count: the length of the program that regexp compiles expr to.
func instructions(t *testing.T, expr string) int {
	t.Helper()
	re, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		t.Fatalf("compile %q: %v", expr, err)
	}
	return len(prog.Inst)
}

// The regexes the tests below put at and over a limit are held to the counts they are named for, so that a change
// of Go's compiler moves them here, in one place, and not as a puzzling failure in the cases that use them.
func TestRegexInstructionCountsOfTheFixtures(t *testing.T) {
	for _, tc := range []struct {
		expr string
		want int
	}{
		{`.{0,999}`, maxRegexInstructions},
		{`.{0,999}a`, maxRegexInstructions + 1},
		{`.{0,1000}`, maxRegexInstructions + 2},
		{`(?P<state>.{0,998})`, maxRegexInstructions},
		{`(?P<state>.{0,998})a`, maxRegexInstructions + 1},
		{`a`, 3},
		{`.{0,900}`, 1802}, // measured with Go 1.27: the number the docs use of a regex under the limit
	} {
		if got := instructions(t, tc.expr); got != tc.want {
			t.Errorf("%q compiles to %d instructions, the tests assume %d", tc.expr, got, tc.want)
		}
	}
}

func TestRegexInstructionLimit(t *testing.T) {
	const at = "spec.chat.triggers[1]"
	const (
		atLimit   = `.{0,999}`
		overLimit = `.{0,999}a`
	)
	over := func(path string, n int) string {
		return fmt.Sprintf("%s: regular expression compiles to %d instructions, more than the %d instruction limit; the regular expressions after it are not checked", path, n, maxRegexInstructions)
	}
	runMutations[Binding](t, "binding.yaml", []mutation[Binding]{
		{"a match regex at the limit", func(o *Binding) { bot(o).Match[FieldTitle] = atLimit }, ""},
		{"a match regex one instruction over", func(o *Binding) { bot(o).Match[FieldTitle] = overLimit },
			over(at+".match.title", maxRegexInstructions+1)},
		{"an exclude regex at the limit", func(o *Binding) { bot(o).Exclude[FieldTitle] = atLimit }, ""},
		{"an exclude regex one instruction over", func(o *Binding) { bot(o).Exclude[FieldTitle] = overLimit },
			over(at+".exclude.title", maxRegexInstructions+1)},
		{"an extract regex at the limit", func(o *Binding) {
			bot(o).Extract["state"] = Extract{Field: FieldTitle, Regex: `(?P<state>.{0,998})`}
		}, ""},
		{"an extract regex one instruction over", func(o *Binding) {
			bot(o).Extract["state"] = Extract{Field: FieldTitle, Regex: `(?P<state>.{0,998})a`}
		}, over(at+".extract.state.regex", maxRegexInstructions+1)},
		// Within the 512 byte cap and far over the limit: the shape of the cost that the byte cap does not bound.
		{"the regex that the byte cap lets through", func(o *Binding) { bot(o).Match[FieldTitle] = strings.Repeat(`.{0,1000}`, 56) },
			over(at+".match.title", 112002)},
		// Nothing after the refusal is compiled: a regex that is invalid is not reported until the one before it is fixed.
		{"a refusal stops the check of the regexes after it", func(o *Binding) {
			bot(o).Match[FieldBody] = `.{0,1000}`
			bot(o).Match[FieldTitle] = "("
		}, over(at+".match.body", maxRegexInstructions+2)},
	})
}

// regexTrigger is a botMessage trigger of the sender B0000000000+n whose regexes are given: match and exclude by field,
// and no extract, thread rule or anything else that would make the trigger's own cost matter.
func regexTrigger(o *Binding, n int, match, exclude map[MessageField]string) Trigger {
	tr := *bot(o)
	tr.From = &BotSource{ID: fmt.Sprintf("B%010d", n), Kind: SenderKindBot}
	tr.Match, tr.Exclude, tr.Extract, tr.Thread = match, exclude, nil, TriggerThread{}
	tr.PromptTemplate = "x"
	return tr
}

func fields(n int, expr string) map[MessageField]string {
	m := map[MessageField]string{}
	for _, f := range messageFields()[:n] {
		m[f] = expr
	}
	return m
}

func TestBindingRegexInstructionBudget(t *testing.T) {
	const atLimit = `.{0,999}` // 2000 instructions; ten of them are the Binding's 20000
	if got := maxBindingRegexInstructions / maxRegexInstructions; got != 10 {
		t.Fatalf("the cases below spend ten regexes of %d instructions, and the budget is %d", maxRegexInstructions, maxBindingRegexInstructions)
	}
	binding := func(o *Binding, ts ...Trigger) {
		o.Spec.Chat.Triggers = append([]Trigger{o.Spec.Chat.Triggers[0]}, ts...)
	}
	runMutations[Binding](t, "binding.yaml", []mutation[Binding]{
		{"ten regexes at the limit, in two triggers, are the budget", func(o *Binding) {
			binding(o, regexTrigger(o, 1, fields(6, atLimit), nil), regexTrigger(o, 2, nil, fields(4, atLimit)))
		}, ""},
		{"a regex of three instructions past the budget", func(o *Binding) {
			// The second trigger's match is spent first, then its excludes in the order of their field names (fallback,
			// link, text, title): the fourth brings the total to 12003 + 6000 + 2000.
			second := regexTrigger(o, 2, map[MessageField]string{FieldTitle: "a"}, fields(4, atLimit))
			binding(o, regexTrigger(o, 1, fields(6, atLimit), nil), second)
		}, "spec.chat.triggers[2].exclude.title: the regular expressions of the Binding compile to 20003 instructions with this one, " +
			"more than the 20000 instruction limit for a Binding; the regular expressions after it are not checked"},
		{"the regexes after the one past the budget are not checked", func(o *Binding) {
			// Ten at the limit are the whole budget; the match of the third trigger is the first past it, and neither its
			// exclude nor the regex of a fourth trigger is checked, though both are invalid.
			third := regexTrigger(o, 3, map[MessageField]string{FieldTitle: "a"}, map[MessageField]string{FieldTitle: "("})
			fourth := regexTrigger(o, 4, map[MessageField]string{FieldTitle: "("}, nil)
			binding(o, regexTrigger(o, 1, fields(6, atLimit), nil), regexTrigger(o, 2, nil, fields(4, atLimit)), third, fourth)
		}, "spec.chat.triggers[3].match.title: the regular expressions of the Binding compile to 20003 instructions with this one, " +
			"more than the 20000 instruction limit for a Binding; the regular expressions after it are not checked"},
		{"the regexes of the Binding's other fields count", func(o *Binding) {
			// Ten regexes at the limit are the whole budget, so the extract's is the first past it.
			tr := regexTrigger(o, 1, fields(6, atLimit), fields(4, atLimit))
			tr.Extract = map[string]Extract{"state": {Field: FieldTitle, Regex: `(?P<state>x)`}}
			binding(o, tr)
		}, "spec.chat.triggers[1].extract.state.regex: the regular expressions of the Binding compile to 20005 instructions with this one, " +
			"more than the 20000 instruction limit for a Binding; the regular expressions after it are not checked"},
	})
}

// hostileBinding is a Binding the schema accepts at every cap: 50 triggers (the mention trigger and 49 botMessage ones), each
// with six match, six exclude and 32 extract regexes. regex gives the text of a regex, with the group an extract needs where
// the group is named.
func hostileBinding(t *testing.T, regex func(group string) string) *Binding {
	t.Helper()
	b := load[Binding](t, "binding.yaml")
	ts := []Trigger{b.Spec.Chat.Triggers[0]}
	zero := int32(0)
	for i := 1; i < maxTriggers; i++ {
		m, x := map[MessageField]string{}, map[MessageField]string{}
		for _, f := range messageFields() {
			m[f], x[f] = regex(""), regex("")
		}
		ex := map[string]Extract{}
		for j := 0; j < maxExtract; j++ {
			name := fmt.Sprintf("e%d", j)
			ex[name] = Extract{Field: FieldText, Regex: regex(name)}
		}
		ts = append(ts, Trigger{
			Type: TriggerBotMessage, From: &BotSource{ID: fmt.Sprintf("B%010d", i), Kind: SenderKindBot}, Match: m, Exclude: x, Extract: ex,
			PromptTemplate: "x",
			Limits:         TriggerLimits{RateLimit: &RateLimit{MaxRuns: 1, Per: metav1.Duration{Duration: time.Minute}, MaxConcurrent: 1}, MaxRunsPerDay: &zero},
		})
	}
	b.Spec.Chat.Triggers = ts
	b.Default()
	return b
}

// regexOf512 makes a regex of exactly 512 bytes: prefix, then literal text, then what ends it, which is a literal that the
// text of the tests lacks, or, for an extract, the named group the extract needs.
func regexOf512(prefix, group string) string {
	tail := "Z"
	if group != "" {
		tail = "(?P<" + group + ">z)"
	}
	return prefix + strings.Repeat("Z", 512-len(prefix)-len(tail)) + tail
}

// Validate of a Binding at every cap stops at the first regex past a budget, so what a hostile Binding costs the broker is
// the cost of one regex over the limit, or of eleven just under it, and not of the 2200 it could hold. The same Binding was
// measured at close to two minutes when every regex was compiled, so a bound of seconds that holds even under the race
// detector is a loose one; the single problem in the result is what shows that the rest was not compiled.
func TestValidateOfAHostileBindingIsCheap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		prefix   string
		contains string
	}{
		// 55 repeats of .{0,1000} are 495 bytes and 110000 instructions, and room is left for the group of an extract.
		{"every regex far over the limit", strings.Repeat(`.{0,1000}`, 55), "regular expression compiles to"},
		// .{0,700} is 1402 instructions and the literal that fills the 512 bytes adds about 500: each regex is under the limit.
		{"every regex just under the limit", `.{0,700}`, "the regular expressions of the Binding compile to"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := hostileBinding(t, func(group string) string { return regexOf512(tc.prefix, group) })
			if n := instructions(t, regexOf512(tc.prefix, "e0")); (n > maxRegexInstructions) != strings.HasPrefix(tc.name, "every regex far") {
				t.Fatalf("a regex of the case compiles to %d instructions: not what its name says", n)
			}
			start := time.Now()
			err := b.Validate()
			elapsed := time.Since(start)
			got := problemsOf(err)
			if len(got) != 1 || !strings.Contains(got[0], tc.contains) {
				t.Fatalf("Validate() problems = %q, want the one that names %q", got, tc.contains)
			}
			t.Logf("Validate of a Binding at every cap, %s: %s (%d regexes of 512 bytes)", tc.name, elapsed, (maxTriggers-1)*(2*len(messageFields())+maxExtract))
			if elapsed > 10*time.Second {
				t.Errorf("Validate took %s; it must stop at the first regex past a budget", elapsed)
			}
		})
	}
}
