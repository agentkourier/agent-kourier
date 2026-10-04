// Package chattest holds what a chat adapter is held to, and a platform to test against. Conformance is
// the safety gate of every adapter: it sends the hostile text of Corpus through every place text can go
// (chat.Where) and fails the adapter if any of it comes out as anything but the characters it is made of.
// FakePlatform is an in-memory chat.Platform for tests of the code that uses one.
package chattest

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// Subject is an adapter under test, and how to see what it sent. An adapter is tested against its
// platform's own reading of what it sent, which only its author can write: Sent is that reading.
type Subject struct {
	Platform chat.Platform
	// Sent returns, and forgets, every piece of text the platform received since the last call, each as
	// a person sees it with the constructs the platform acted on in it (Scan reads text on the wire in a
	// Slack-and-CommonMark style). An adapter that sends its text with markup in it must say what the
	// markup does here, and an adapter that makes all of it literal reports none.
	Sent func() []Shown
	// Inert returns an error if any request since the last call was not inert: it had the platform parse
	// the text (link names, mention parsing), unfurl a link, or follow one. It returns nil when every
	// request carried the platform's inert options.
	Inert func() error

	// StyleInLiteral says that the platform cannot show a literal field's text without style: a "*" in
	// a context line is bold there. It is a concession and not a default: the adapter says so once, and a
	// reviewer sees it.
	StyleInLiteral bool
	// AllowAutolink says that the platform makes a bare URL into a link that shows its own address, and
	// no escape stops it. The link goes nowhere but where the text says, and the request is inert (no
	// unfurl).
	AllowAutolink bool
	// Target is where the checks post, and the zero Target a default. A platform that needs the thread to
	// exist (Slack does) gives one that does.
	Target chat.Target
	// Concessions are the constructs the platform acts on in a place that no escape stops, each with the
	// reason. They are narrower than StyleInLiteral and AllowAutolink, which excuse a kind in every place, and
	// an adapter lists every one it needs: a reviewer reads this list, and a concession without a reason is a
	// setup error.
	Concessions []Concession
	// TooLarge, if set, is a prompt too large for the platform: Fits must refuse it with chat.ErrTooLarge.
	TooLarge *chat.Prompt
	// MentionUser is a user ID the platform shows as a mention, in a chat.Mention part, or "" if the platform has
	// none: then a Mention shows its user as nothing but text. The checks send it, and require exactly one mention
	// of it and nothing else the platform acts on; every other user ID they send is forged, and must never act.
	MentionUser string
}

// Concession is a construct of the kinds Kinds that the platform acts on in the text of the place Where
// whatever the adapter does, and why. It excuses those kinds in that place and nothing else.
type Concession struct {
	Where chat.Where
	Kinds []Kind
	// Why says what the platform does, and why it is harmless there. It is required.
	Why string
}

// StyleKinds are the cosmetic kinds, for a Concession of style.
func StyleKinds() []Kind { return []Kind{KindEmphasis, KindCode, KindHeading, KindQuote, KindList} }

func (c *checker) conceded(w chat.Where, k Kind) bool {
	for _, cc := range c.s.Concessions {
		if cc.Where == w && slices.Contains(cc.Kinds, k) {
			return true
		}
	}
	return false
}

// Rule names what a violation broke.
type Rule string

// The rules.
const (
	RuleSetup     Rule = "setup"     // the Subject is incomplete
	RuleRejected  Rule = "rejected"  // the adapter returned an error for text it should show
	RuleDropped   Rule = "dropped"   // the text is shown nowhere
	RuleActive    Rule = "active"    // text was made into a construct the platform acts on
	RuleInvisible Rule = "invisible" // a control, format or hidden character is shown
	RuleNewline   Rule = "newline"   // a one-line place shows a line break
	RuleLimit     Rule = "limit"     // more than the platform's limit for the place
	RuleTruncated Rule = "truncated" // text over the limit is neither cut nor marked
	RuleFidelity  Rule = "fidelity"  // the text shown is not the text given, cleaned
	RuleInert     Rule = "inert"     // a request was not inert
	RuleContract  Rule = "contract"  // another part of the Platform contract
)

// Violation is one way an adapter failed.
type Violation struct {
	Rule   Rule
	Where  chat.Where
	Method string // the Platform method called
	Case   string // the Corpus case, or the check
	Detail string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s: %s, in %s via %s: %s", v.Rule, v.Case, v.Where, v.Method, v.Detail)
}

// Options adjust a check.
type Options struct {
	// Short runs each case once, with the text alone, and not three times with a marker around it.
	Short bool
	// cases, if set, are the cases to run in place of Corpus. It is for a test of the suite itself, which shows that a
	// deliberately unsafe adapter is found and needs only the cases that its leak touches. It is not exported, so that
	// an adapter's own tests, which are held to the whole Corpus, cannot set it.
	cases []Case
}

// Conformance fails t for every way the adapter breaks the contract of package chat. An adapter's tests
// call it with the adapter built on a fake of its platform, and every adapter must.
func Conformance(t *testing.T, s Subject) {
	t.Helper()
	vs := CheckWith(t.Context(), s, Options{Short: testing.Short()})
	for i, v := range vs {
		if i == 40 {
			t.Errorf("... and %d more", len(vs)-i)
			break
		}
		t.Error(v)
	}
}

// Check runs the whole of Conformance and returns what it found, without failing anything.
func Check(ctx context.Context, s Subject) []Violation { return CheckWith(ctx, s, Options{}) }

// CheckWith is Check with options.
func CheckWith(ctx context.Context, s Subject, opts Options) []Violation {
	return checkShard(ctx, s, opts, placements(s.MentionUser), true)
}

// ConformanceSharded is Conformance for an adapter whose checks are slow, such as one that is tested over HTTP
// against a fake of its platform: it runs the places in parallel, each group on a Subject of its own, which
// newSubject makes. Every group is checked as Conformance checks, and the contract once.
func ConformanceSharded(t *testing.T, shards int, newSubject func(t *testing.T) Subject) {
	t.Helper()
	results := make([][]Violation, shards)
	subjects := make([]Subject, shards) // made one at a time, before any runs, as a test's setup is
	for i := range subjects {
		subjects[i] = newSubject(t)
	}
	all := placements(subjects[0].MentionUser)
	var wg sync.WaitGroup
	for i := range shards {
		var mine []placement
		for j := i; j < len(all); j += shards {
			mine = append(mine, all[j])
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = checkShard(t.Context(), subjects[i], Options{Short: testing.Short()}, mine, i == 0)
		}()
	}
	wg.Wait()
	n := 0
	for _, vs := range results {
		for _, v := range vs {
			if n++; n > 40 {
				continue
			}
			t.Error(v)
		}
	}
	if n > 40 {
		t.Errorf("... and %d more", n-40)
	}
}

// checkShard runs the checks of some places, and the contract if asked.
func checkShard(ctx context.Context, s Subject, opts Options, places []placement, contract bool) []Violation {
	if s.Platform == nil || s.Sent == nil || s.Inert == nil {
		return []Violation{{Rule: RuleSetup, Case: "subject", Detail: "Platform, Sent and Inert are all required"}}
	}
	for _, cc := range s.Concessions {
		if cc.Why == "" || len(cc.Kinds) == 0 {
			return []Violation{{Rule: RuleSetup, Where: cc.Where, Case: "concession", Detail: "a concession needs kinds and a reason"}}
		}
	}
	c := &checker{ctx: ctx, s: s, caps: s.Platform.Capabilities(), opts: opts, target: cmp.Or(s.Target, defaultTarget)}
	if contract {
		c.contract()
		c.mention()
	}
	cases := Corpus
	if len(opts.cases) > 0 {
		cases = opts.cases
	}
	for _, p := range places {
		for _, cs := range cases {
			for _, text := range c.variants(cs.Text) {
				c.run(p, cs.Name, text) // by every method and every variant: no two are assumed to share a renderer
			}
		}
		c.oversize(p)
	}
	return c.found
}

// canary is a marker with nothing in it for markup to act on, put around hostile text so that the pieces
// of what the platform received that came from it can be told from the pieces the adapter wrote.
const canary = "zq7kx3"

var defaultTarget = chat.Target{Conn: "conformance/chat", Channel: "C0CONFORM", Thread: "1.1"}

type checker struct {
	target chat.Target
	ctx    context.Context
	s      Subject
	caps   chat.Caps
	opts   Options
	found  []Violation
}

func (c *checker) add(rule Rule, w chat.Where, method, cs, format string, args ...any) {
	c.found = append(c.found, Violation{Rule: rule, Where: w, Method: method, Case: cs, Detail: fmt.Sprintf(format, args...)})
}

func (c *checker) variants(text string) []string {
	if c.opts.Short {
		return []string{canary + text}
	}
	return []string{canary + text, text + canary, canary + "\n" + text + "\n" + canary}
}

// contract checks what is not about text: the Platform's other promises.
func (c *checker) contract() {
	p := c.s.Platform
	if c.caps.MaxText <= len(chat.Truncated) {
		c.add(RuleContract, 0, "Capabilities", "limits", "MaxText is %d, which cannot hold even the truncation marker", c.caps.MaxText)
	}
	for w, n := range c.caps.Limits {
		if n <= len(chat.Truncated) {
			c.add(RuleContract, w, "Capabilities", "limits", "the limit is %d, which cannot hold even the truncation marker", n)
		}
	}
	if _, err := p.Post(c.ctx, c.target, chat.Outgoing{}, "tag"); !errors.Is(err, chat.ErrInvalid) {
		c.add(RuleContract, 0, "Post", "malformed", "a zero Outgoing gave %v, want an error wrapping chat.ErrInvalid", err)
	}
	small := chat.Prompt{ID: "ix-conformance", Questions: []chat.Question{{Text: "q"}}}
	if err := p.Fits(small); err != nil {
		c.add(RuleContract, 0, "Fits", "small prompt", "a one-question prompt does not fit: %v", err)
	}
	if c.s.TooLarge != nil {
		if err := p.Fits(*c.s.TooLarge); !errors.Is(err, chat.ErrTooLarge) {
			c.add(RuleContract, 0, "Fits", "too large", "the prompt given as too large gave %v, want chat.ErrTooLarge", err)
		}
	}
	for _, err := range []error{errors.New("boom"), context.Canceled, context.DeadlineExceeded} {
		for _, op := range []chat.Op{chat.OpPost, chat.OpReact, chat.OpOther} {
			if v := p.Classify(op, err); v.Class != chat.Transient {
				c.add(RuleContract, 0, "Classify", "unknown error", "%v for op %d is %+v, want Transient: an error nobody knows is retried", err, op, v)
			}
		}
	}
	if !c.caps.Streams {
		if _, err := p.Stream(c.ctx, c.target, "U1", "x"); !errors.Is(err, chat.ErrNoStreams) {
			c.add(RuleContract, chat.WhereStream, "Stream", "no streams", "a platform with Streams false gave %v, want chat.ErrNoStreams", err)
		}
	}
	if c.caps.Steps && !c.caps.Streams {
		c.add(RuleContract, chat.WhereStepTitle, "Capabilities", "steps", "Steps is true and Streams is false: a step is shown in a stream")
	}
	if c.caps.Streams {
		c.zeroStop()
		c.stepContract()
	}
	c.s.Sent() // the contract checks are not text checks
	_ = c.s.Inert()
}

// zeroStop checks that a stream ends without showing anything more when it is stopped with no markdown and the zero
// Message. What was streamed stays as the message's content, and a stop adds to it (chat.Stream.Stop): a platform
// that shows more here repeats the streamed text, which is how a reply came to be posted twice.
func (c *checker) zeroStop() {
	st, err := c.s.Platform.Stream(c.ctx, c.target, "U1", "start")
	if err != nil || st == nil {
		c.add(RuleContract, chat.WhereStream, "Stream", "zero stop", "a stream of plain text gave %v, %v", st, err)
		return
	}
	c.s.Sent() // what the stream began with is not what the stop shows
	_ = c.s.Inert()
	if err := st.Stop(c.ctx, "", chat.Message{}, "tag-zero"); err != nil {
		c.add(RuleContract, chat.WhereStream, "Stop", "zero stop", "a stop with no markdown and the zero Message was refused: %v", err)
		return
	}
	n := 0
	for _, s := range c.s.Sent() {
		if s.Displayed != "" { // the zero Message has an empty fallback, which is not text
			n++
		}
	}
	if n > 0 {
		c.add(RuleContract, chat.WhereStream, "Stop", "zero stop", "a stop with no markdown and the zero Message showed %d texts, want none: it adds to the streamed text, and adds nothing", n)
	}
}

// stepContract checks what a stream promises of its steps. A platform that shows none says so with ErrNoSteps and shows
// nothing; one that shows them refuses a step of a stream that ended with ErrNotStreaming, and accepts a step it has seen
// again, which is how one is finished.
func (c *checker) stepContract() {
	st, err := c.s.Platform.Stream(c.ctx, c.target, "U1", "start")
	if err != nil || st == nil {
		c.add(RuleContract, chat.WhereStream, "Stream", "step", "a stream of plain text gave %v, %v", st, err)
		return
	}
	step := chat.Step{ID: "s1", Title: canary + " tool", Detail: "k=v"}
	c.s.Sent()
	_ = c.s.Inert()
	err = st.Step(c.ctx, step)
	shown := c.s.Sent()
	if !c.caps.Steps {
		if !errors.Is(err, chat.ErrNoSteps) {
			c.add(RuleContract, chat.WhereStepTitle, "Step", "no steps", "a platform with Steps false gave %v, want chat.ErrNoSteps", err)
		}
		if len(shown) > 0 {
			c.add(RuleContract, chat.WhereStepTitle, "Step", "no steps", "a platform with Steps false showed %d texts for a step", len(shown))
		}
		return
	}
	if err != nil {
		c.add(RuleContract, chat.WhereStepTitle, "Step", "begin", "a step was refused: %v", err)
		return
	}
	step.State = chat.StepDone
	if err := st.Step(c.ctx, step); err != nil {
		c.add(RuleContract, chat.WhereStepTitle, "Step", "finish", "a step it had begun was refused when it was finished: %v", err)
	}
	if err := st.Stop(c.ctx, "", chat.Message{}, "tag-step"); err != nil {
		c.add(RuleContract, chat.WhereStream, "Stop", "step", "a stream with a step was not stopped: %v", err)
		return
	}
	if err := st.Step(c.ctx, step); !errors.Is(err, chat.ErrNotStreaming) {
		c.add(RuleContract, chat.WhereStepTitle, "Step", "after stop", "a step of a stopped stream gave %v, want an error wrapping chat.ErrNotStreaming", err)
	}
}

// mention checks a Mention of a valid user, in every method that takes a message: the platform shows exactly one
// mention of that user and nothing else that acts (a platform with no mentions shows none), and the words that
// follow it are shown whole. The forged IDs are the placement of the Mention's user, with the corpus.
func (c *checker) mention() {
	user := cmp.Or(c.s.MentionUser, "U0123456789")
	words := canary + " answered this question first"
	out := chat.Say(chat.Message{Fallback: "fallback", Parts: []chat.Part{chat.Mention{User: user, Text: words}}})
	calls := []struct {
		name string
		prep func() error
		do   func() error
	}{{name: "Post", do: func() error { _, err := c.s.Platform.Post(c.ctx, c.target, out, "tag-m1"); return err }}}
	var ref chat.Ref
	calls = append(calls, struct {
		name string
		prep func() error
		do   func() error
	}{name: "Update",
		prep: func() (err error) {
			ref, err = c.s.Platform.Post(c.ctx, c.target, chat.Say(chat.Message{Fallback: "seed"}), "tag-m2")
			return err
		},
		do: func() error { return c.s.Platform.Update(c.ctx, ref, out) }})
	if c.caps.Ephemeral {
		calls = append(calls, struct {
			name string
			prep func() error
			do   func() error
		}{name: "Ephemeral", do: func() error { return c.s.Platform.Ephemeral(c.ctx, c.target, "U1", *out.Message) }})
	}
	if c.caps.Streams {
		var st chat.Stream
		calls = append(calls, struct {
			name string
			prep func() error
			do   func() error
		}{name: "Stop",
			prep: func() (err error) { st, err = c.s.Platform.Stream(c.ctx, c.target, "U1", "start"); return err },
			do:   func() error { return st.Stop(c.ctx, "", *out.Message, "tag-m3") }})
	}
	for _, m := range calls {
		if m.prep != nil {
			if err := m.prep(); err != nil {
				c.add(RuleRejected, chat.WhereUser, m.name, "valid mention", "a plain message was refused while preparing: %v", err)
				continue
			}
		}
		c.s.Sent()
		_ = c.s.Inert()
		err := m.do()
		shown := c.s.Sent()
		if err := c.s.Inert(); err != nil {
			c.add(RuleInert, chat.WhereUser, m.name, "valid mention", "%v", err)
		}
		if err != nil {
			c.add(RuleRejected, chat.WhereUser, m.name, "valid mention", "%v", err)
			continue
		}
		mentions, whole := 0, false
		for _, s := range shown {
			for _, a := range s.Active {
				switch {
				case a.Kind == KindMention && c.s.MentionUser != "" && strings.Contains(a.Text, user):
					mentions++
				default:
					c.add(RuleActive, chat.WhereUser, m.name, "valid mention", "a mention of a valid user became a %s: %q", a.Kind, a.Text)
				}
			}
			whole = whole || strings.Contains(s.Displayed, words)
		}
		if c.s.MentionUser != "" && mentions != 1 {
			c.add(RuleActive, chat.WhereUser, m.name, "valid mention", "the valid user became a mention %d times, want exactly one", mentions)
		}
		if !whole {
			c.add(RuleFidelity, chat.WhereUser, m.name, "valid mention", "the words after the mention are not shown whole (shown: %q)", displayed(shown))
		}
	}
}

// call is one thing to send: a message, a prompt or an ending (Post, Update, and Ephemeral for a message),
// or markdown for a stream.
type call struct {
	out  chat.Outgoing
	md   string     // for a stream
	step *chat.Step // for a step of a stream
}

// placement is a place text can go, and how to put some there.
type placement struct {
	where    chat.Where
	name     string
	build    func(text string) call
	optional bool // the adapter may show nothing of it (an Ending.By that is no user ID is "someone")
	// mention says the message has a mention of Subject.MentionUser beside the text, which is allowed exactly
	// once, and that nothing else it shows may act.
	mention bool
}

func msg(parts ...chat.Part) call {
	return call{out: chat.Say(chat.Message{Fallback: "fallback", Parts: parts})}
}

func prompt(mod func(*chat.Prompt)) call {
	p := chat.Prompt{ID: "ix-conformance", Questions: []chat.Question{{Text: "which"}}}
	mod(&p)
	return call{out: chat.Ask(p)}
}

func ending(mod func(*chat.Ending)) call {
	e := chat.Ending{Outcome: chat.Answered, By: "U0123456789", At: time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC),
		Prompt: chat.Prompt{ID: "ix-conformance", Questions: []chat.Question{{Text: "which"}}}, Answers: [][]string{{"prod"}}}
	mod(&e)
	return call{out: chat.End(e)}
}

func placements(mentionUser string) []placement {
	out := []placement{
		{chat.WhereFallback, "fallback", func(s string) call { return call{out: chat.Say(chat.Message{Fallback: s})} }, false, false},
		{chat.WhereHeader, "header", func(s string) call { return msg(chat.Header{Text: s}) }, false, false},
		{chat.WhereBody, "body", func(s string) call { return msg(chat.Body{Markdown: s}) }, false, false},
		{chat.WhereContext, "context", func(s string) call { return msg(chat.Context{Text: s}) }, false, false},
		{chat.WhereNote, "note", func(s string) call { return msg(chat.Note{Text: s}) }, false, false},
		{chat.WhereToolName, "tool name", func(s string) call { return msg(chat.ToolLine{Name: s}) }, false, false},
		{chat.WhereToolArg, "tool summary", func(s string) call { return msg(chat.ToolLine{Name: "tool", Summary: s}) }, false, false},
		{chat.WhereButton, "button label", func(s string) call {
			return msg(chat.Buttons{Buttons: []chat.Button{{ID: "go", Label: s, Value: "ix-conformance"}}})
		}, false, false},
		{chat.WhereHint, "prompt hint", func(s string) call { return prompt(func(p *chat.Prompt) { p.Hint = s }) }, false, false},
		{chat.WhereSubagent, "prompt sub-agent", func(s string) call { return prompt(func(p *chat.Prompt) { p.Subagent = s }) }, false, false},
		{chat.WhereQuestion, "question", func(s string) call {
			return prompt(func(p *chat.Prompt) { p.Questions = []chat.Question{{Text: s}} })
		}, false, false},
		{chat.WhereChoice, "choice", func(s string) call {
			return prompt(func(p *chat.Prompt) { p.Questions = []chat.Question{{Text: "which", Choices: []string{s, "other"}}} })
		}, false, false},
		{chat.WhereChoice, "choice, multiple", func(s string) call {
			return prompt(func(p *chat.Prompt) {
				p.Questions = []chat.Question{{Text: "which", Choices: []string{s, "other"}, Multiple: true}}
			})
		}, false, false},
		{chat.WhereHint, "ending hint", func(s string) call { return ending(func(e *chat.Ending) { e.Prompt.Hint = s }) }, false, false},
		{chat.WhereSubagent, "ending sub-agent", func(s string) call { return ending(func(e *chat.Ending) { e.Prompt.Subagent = s }) }, false, false},
		{chat.WhereQuestion, "ending question", func(s string) call {
			return ending(func(e *chat.Ending) { e.Prompt.Questions = []chat.Question{{Text: s}} })
		}, false, false},
		{chat.WhereAnswer, "ending answer", func(s string) call { return ending(func(e *chat.Ending) { e.Answers = [][]string{{s}} }) }, false, false},
		{chat.WhereUser, "ending decider", func(s string) call { return ending(func(e *chat.Ending) { e.By = s }) }, true, false},
		{chat.WhereUser, "mention user", func(s string) call { return msg(chat.Mention{User: s, Text: "answered first"}) }, true, false},
		{chat.WhereStream, "stream", func(s string) call { return call{md: s} }, false, false},
		{chat.WhereStepTitle, "step title", func(s string) call { return call{step: &chat.Step{ID: "s1", Title: s}} }, false, false},
		{chat.WhereStepDetail, "step detail", func(s string) call { return call{step: &chat.Step{ID: "s1", Title: "tool", Detail: s}} }, false, false},
	}
	// The words of a Mention, beside a user the platform shows as a mention, and beside one it does not: the words
	// of the first must stay text next to a live mention, which is where a renderer that builds one text of the
	// two lets them act.
	words := func(user string) func(string) call {
		return func(s string) call { return msg(chat.Mention{User: user, Text: s}) }
	}
	if mentionUser != "" {
		out = append(out, placement{chat.WhereContext, "mention text", words(mentionUser), false, true})
		return append(out, placement{chat.WhereContext, "mention text, invalid user", words("someone"), false, false})
	}
	return append(out, placement{chat.WhereContext, "mention text", words("someone"), false, false})
}

// run sends text in the place p, by every method that takes it, and checks what the platform received.
func (c *checker) run(p placement, name, text string) {
	if p.where == chat.WhereStream {
		c.stream(p, name, text)
		return
	}
	if st := p.build(text).step; st != nil {
		c.step(p, name, text, *st)
		return
	}
	var ref chat.Ref // the message of a plain post, which Update replaces
	methods := []struct {
		name string
		prep func() error // before the text is sent: not checked
		do   func() error
	}{
		{name: "Post", do: func() error { _, err := c.s.Platform.Post(c.ctx, c.target, p.build(text).out, "tag-1"); return err }},
		{name: "Update",
			prep: func() (err error) {
				ref, err = c.s.Platform.Post(c.ctx, c.target, chat.Say(chat.Message{Fallback: "seed"}), "tag-seed")
				return err
			},
			do: func() error { return c.s.Platform.Update(c.ctx, ref, p.build(text).out) }},
	}
	if m := p.build(text).out.Message; m != nil {
		if c.caps.Ephemeral {
			methods = append(methods, struct {
				name string
				prep func() error
				do   func() error
			}{name: "Ephemeral", do: func() error { return c.s.Platform.Ephemeral(c.ctx, c.target, "U1", *m) }})
		}
		if c.caps.Streams { // a stream ends with a message of its own, which is rendered apart from Post's
			var st chat.Stream
			methods = append(methods, struct {
				name string
				prep func() error
				do   func() error
			}{name: "Stop",
				prep: func() (err error) { st, err = c.s.Platform.Stream(c.ctx, c.target, "U1", "start"); return err },
				do:   func() error { return st.Stop(c.ctx, "", *m, "tag-stop") }})
		}
	}
	for _, m := range methods {
		if m.prep != nil {
			if err := m.prep(); err != nil {
				c.add(RuleRejected, p.where, m.name, name, "a plain message was refused while preparing: %v", err)
				continue
			}
		}
		c.s.Sent()
		_ = c.s.Inert()
		err := m.do()
		c.verify(p, m.name, name, text, err)
	}
}

// stream sends text through the stream methods: as the first markdown, as an append, and in the final
// message.
func (c *checker) stream(p placement, name, text string) {
	if !c.caps.Streams {
		return
	}
	do := func(method string, f func() error) {
		c.s.Sent()
		_ = c.s.Inert()
		c.verify(p, method, name, text, f())
	}
	// A platform may hold the end of a stream's text back until it knows what that text is (a code span is known
	// at the end of its line), and writes it when the stream ends, so these two end the stream with no more text.
	end := func(st chat.Stream) error { return st.Stop(c.ctx, "", chat.Message{}, "tag-1") }
	do("Stream", func() error {
		st, err := c.s.Platform.Stream(c.ctx, c.target, "U1", text)
		if err != nil {
			return err
		}
		return end(st)
	})
	// The text that follows starts a line, as it does after a paragraph: a stream is one document, in which a fence
	// that does not start a line is not one, and a fence's info string is not shown.
	open := func() (chat.Stream, bool) {
		st, err := c.s.Platform.Stream(c.ctx, c.target, "U1", "start\n")
		if err != nil {
			c.add(RuleRejected, p.where, "Stream", name, "a stream of plain text was refused: %v", err)
		}
		return st, err == nil
	}
	if st, ok := open(); ok {
		do("Append", func() error {
			if err := st.Append(c.ctx, text); err != nil {
				return err
			}
			return end(st)
		})
	}
	if st, ok := open(); ok {
		do("Stop", func() error {
			return st.Stop(c.ctx, "", chat.Message{Fallback: "done", Parts: []chat.Part{chat.Body{Markdown: text}}}, "tag-2")
		})
	}
	if st, ok := open(); ok { // the last of the text comes with the stop
		do("Stop (last markdown)", func() error {
			return st.Stop(c.ctx, text, chat.Message{Fallback: "done", Parts: []chat.Part{chat.Body{Markdown: "done"}}}, "tag-3")
		})
	}
	if st, ok := open(); ok { // a stream of another run, found again by its Ref
		at := c.s.Platform.StreamAt(st.Ref())
		do("StreamAt Stop", func() error {
			return at.Stop(c.ctx, text, chat.Message{Fallback: "done", Parts: []chat.Part{chat.Body{Markdown: "done"}}}, "tag-4")
		})
	}
}

// step sends text as a step of a stream in each state a step goes through: begun, done, failed, and ended with no
// outcome, which a platform shows with words of its own beside the text.
func (c *checker) step(p placement, name, text string, st chat.Step) {
	if !c.caps.Streams || !c.caps.Steps {
		return
	}
	stream, err := c.s.Platform.Stream(c.ctx, c.target, "U1", "start")
	if err != nil {
		c.add(RuleRejected, p.where, "Stream", name, "a stream of plain text was refused: %v", err)
		return
	}
	for _, state := range []chat.StepState{chat.StepRunning, chat.StepDone, chat.StepFailed, chat.StepEnded} {
		st.State = state
		c.s.Sent()
		_ = c.s.Inert()
		c.verify(p, "Step ("+state.String()+")", name, text, stream.Step(c.ctx, st))
	}
}

// verify checks what the platform received after one call that carried text.
func (c *checker) verify(p placement, method, name, text string, callErr error) {
	shown := c.s.Sent()
	if err := c.s.Inert(); err != nil {
		c.add(RuleInert, p.where, method, name, "%v", err)
	}
	if callErr != nil {
		c.add(RuleRejected, p.where, method, name, "%v (text %q)", callErr, text)
		return
	}
	var carriers []Shown
	for _, s := range shown {
		if strings.Contains(s.Displayed, canary) {
			carriers = append(carriers, s)
		}
	}
	if p.where == chat.WhereUser && p.build(text).out.Message != nil {
		// A forged user ID in a Mention: the message holds no mention of anyone, so nothing shown may act, whether or
		// not it holds the text (a platform that shows "someone" shows none of it).
		for _, s := range shown {
			c.checkActive(p, method, name, s)
		}
		return
	}
	if len(carriers) == 0 {
		if !p.optional {
			c.add(RuleDropped, p.where, method, name, "the text %q is shown nowhere (shown: %q)", text, displayed(shown))
		}
		return
	}
	limit := c.caps.Limit(p.where)
	want := chat.Clean(text, p.where.Line())
	faithful := false
	if p.mention {
		c.checkMention(p, method, name, shown)
	}
	for _, s := range carriers {
		if !p.mention { // a message with a mention is checked as a whole above
			c.checkActive(p, method, name, s)
		}
		if r, ok := invisibleIn(s.Displayed); ok {
			c.add(RuleInvisible, p.where, method, name, "%U is shown in %q", r, s.Displayed)
		}
		if p.where.Line() && strings.ContainsAny(s.Displayed, "\n\r") {
			c.add(RuleNewline, p.where, method, name, "a line break is shown in %q", s.Displayed)
		}
		if n := chat.Units(s.Displayed); n > limit {
			c.add(RuleLimit, p.where, method, name, "%d units are shown, over the limit of %d", n, limit)
		}
		if strings.Contains(squash(s.Displayed), squash(want)) {
			faithful = true
		}
	}
	switch {
	case chat.Units(want) > limit && !p.optional:
		cut := false
		for _, s := range carriers {
			cut = cut || strings.Contains(s.Displayed, chat.Truncated)
		}
		if !cut {
			c.add(RuleTruncated, p.where, method, name, "%d units were given for a limit of %d, and the text is not marked as cut", chat.Units(want), limit)
		}
	case !p.where.Rich() && !p.optional && !faithful:
		c.add(RuleFidelity, p.where, method, name, "shown %q, want it to hold %q", displayed(carriers), want)
	}
}

// squash is s with every run of white space one space and none at either end. A platform that reads CommonMark
// shows text that way, and so what it shows has lost no text if it holds the same words in the same order.
func squash(s string) string { return strings.Join(strings.Fields(s), " ") }

func displayed(ss []Shown) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Displayed
	}
	return out
}

// checkActive fails every construct the place does not allow in text that came from outside.
func (c *checker) checkActive(p placement, method, name string, s Shown) {
	for _, a := range s.Active {
		c.checkConstruct(p, method, name, a)
	}
}

// checkConstruct fails one construct unless the place allows it.
func (c *checker) checkConstruct(p placement, method, name string, a Active) {
	switch {
	case a.Kind == KindAutolink && c.s.AllowAutolink:
	case c.conceded(p.where, a.Kind):
	case a.Kind.Style() && (p.where.Rich() || c.s.StyleInLiteral):
	default:
		c.add(RuleActive, p.where, method, name, "text became a %s: %q", a.Kind, a.Text)
	}
}

// checkMention checks all of what the platform showed for a Mention of Subject.MentionUser with hostile words:
// exactly one construct acts as the mention of that user, wherever in the message it is, and nothing else acts,
// so that the words, which sit beside a live mention, are no more able to notify than they are anywhere else.
func (c *checker) checkMention(p placement, method, name string, shown []Shown) {
	mentions := 0
	for _, s := range shown {
		for _, a := range s.Active {
			if a.Kind == KindMention && strings.Contains(a.Text, c.s.MentionUser) && mentions == 0 {
				mentions++
				continue
			}
			c.checkConstruct(p, method, name, a)
		}
	}
	if mentions != 1 {
		c.add(RuleActive, p.where, method, name, "the valid user became a mention %d times beside these words, want exactly one", mentions)
	}
}

// invisibleIn returns a character of s that no one can see, or that is a control.
func invisibleIn(s string) (rune, bool) {
	for _, r := range s {
		if r == '\n' || r == '\t' {
			continue
		}
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs) || r == unicode.ReplacementChar {
			return r, true
		}
	}
	return 0, false
}

// oversize sends text far over the place's limit, once of one-unit characters and once of two-unit ones,
// and verify checks that it is cut within the limit and marked.
func (c *checker) oversize(p placement) {
	if p.optional {
		return
	}
	limit := c.caps.Limit(p.where)
	c.run(p, "oversize", canary+strings.Repeat("x", limit+64))
	c.run(p, "oversize astral", canary+strings.Repeat("\U0001F389", limit/2+32))
}
