// Package chattest holds what a chat adapter is held to, and a platform to test against. Conformance is
// the safety gate of every adapter: it sends the hostile text of Corpus through every place text can go
// (chat.Where) and fails the adapter if any of it comes out as anything but the characters it is made of.
// FakePlatform is an in-memory chat.Platform for tests of the code that uses one.
package chattest

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
	// TooLarge, if set, is a prompt too large for the platform: Fits must refuse it with chat.ErrTooLarge.
	TooLarge *chat.Prompt
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
	if s.Platform == nil || s.Sent == nil || s.Inert == nil {
		return []Violation{{Rule: RuleSetup, Case: "subject", Detail: "Platform, Sent and Inert are all required"}}
	}
	c := &checker{ctx: ctx, s: s, caps: s.Platform.Capabilities(), opts: opts}
	c.contract()
	for _, p := range placements() {
		for _, cs := range Corpus {
			vs := c.variants(cs.Text)
			for i, text := range vs {
				c.run(p, cs.Name, text, i == len(vs)-1) // Update and Ephemeral take the last variant only: they share the rendering of Post
			}
		}
		c.oversize(p)
	}
	return c.found
}

// canary is a marker with nothing in it for markup to act on, put around hostile text so that the pieces
// of what the platform received that came from it can be told from the pieces the adapter wrote.
const canary = "zq7kx3"

var target = chat.Target{Conn: "conformance/chat", Channel: "C0CONFORM", Thread: "1.1"}

type checker struct {
	ctx   context.Context
	s     Subject
	caps  chat.Caps
	opts  Options
	found []Violation
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
	if _, err := p.Post(c.ctx, target, chat.Outgoing{}, "tag"); !errors.Is(err, chat.ErrInvalid) {
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
		if _, err := p.Stream(c.ctx, target, "U1", "x"); !errors.Is(err, chat.ErrNoStreams) {
			c.add(RuleContract, chat.WhereStream, "Stream", "no streams", "a platform with Streams false gave %v, want chat.ErrNoStreams", err)
		}
	}
	c.s.Sent() // the contract checks are not text checks
	_ = c.s.Inert()
}

// call is one thing to send: a message, a prompt or an ending (Post, Update, and Ephemeral for a message),
// or markdown for a stream.
type call struct {
	out chat.Outgoing
	md  string // for a stream
}

// placement is a place text can go, and how to put some there.
type placement struct {
	where    chat.Where
	name     string
	build    func(text string) call
	optional bool // the adapter may show nothing of it (an Ending.By that is no user ID is "someone")
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

func placements() []placement {
	return []placement{
		{chat.WhereFallback, "fallback", func(s string) call { return call{out: chat.Say(chat.Message{Fallback: s})} }, false},
		{chat.WhereHeader, "header", func(s string) call { return msg(chat.Header{Text: s}) }, false},
		{chat.WhereBody, "body", func(s string) call { return msg(chat.Body{Markdown: s}) }, false},
		{chat.WhereContext, "context", func(s string) call { return msg(chat.Context{Text: s}) }, false},
		{chat.WhereNote, "note", func(s string) call { return msg(chat.Note{Text: s}) }, false},
		{chat.WhereToolName, "tool name", func(s string) call { return msg(chat.ToolLine{Name: s}) }, false},
		{chat.WhereToolArg, "tool argument value", func(s string) call { return msg(chat.ToolLine{Name: "tool", Args: map[string]string{"k": s}}) }, false},
		{chat.WhereToolArg, "tool argument name", func(s string) call { return msg(chat.ToolLine{Name: "tool", Args: map[string]string{s: "v"}}) }, false},
		{chat.WhereButton, "button label", func(s string) call {
			return msg(chat.Buttons{Buttons: []chat.Button{{ID: "go", Label: s, Value: "ix-conformance"}}})
		}, false},
		{chat.WhereHint, "prompt hint", func(s string) call { return prompt(func(p *chat.Prompt) { p.Hint = s }) }, false},
		{chat.WhereSubagent, "prompt sub-agent", func(s string) call { return prompt(func(p *chat.Prompt) { p.Subagent = s }) }, false},
		{chat.WhereQuestion, "question", func(s string) call {
			return prompt(func(p *chat.Prompt) { p.Questions = []chat.Question{{Text: s}} })
		}, false},
		{chat.WhereChoice, "choice", func(s string) call {
			return prompt(func(p *chat.Prompt) { p.Questions = []chat.Question{{Text: "which", Choices: []string{s, "other"}}} })
		}, false},
		{chat.WhereChoice, "choice, multiple", func(s string) call {
			return prompt(func(p *chat.Prompt) {
				p.Questions = []chat.Question{{Text: "which", Choices: []string{s, "other"}, Multiple: true}}
			})
		}, false},
		{chat.WhereHint, "ending hint", func(s string) call { return ending(func(e *chat.Ending) { e.Prompt.Hint = s }) }, false},
		{chat.WhereSubagent, "ending sub-agent", func(s string) call { return ending(func(e *chat.Ending) { e.Prompt.Subagent = s }) }, false},
		{chat.WhereQuestion, "ending question", func(s string) call {
			return ending(func(e *chat.Ending) { e.Prompt.Questions = []chat.Question{{Text: s}} })
		}, false},
		{chat.WhereAnswer, "ending answer", func(s string) call { return ending(func(e *chat.Ending) { e.Answers = [][]string{{s}} }) }, false},
		{chat.WhereUser, "ending decider", func(s string) call { return ending(func(e *chat.Ending) { e.By = s }) }, true},
		{chat.WhereStream, "stream", func(s string) call { return call{md: s} }, false},
	}
}

// run sends text in the place p, by every method that takes it, and checks what the platform received.
func (c *checker) run(p placement, name, text string, full bool) {
	if p.where == chat.WhereStream {
		c.stream(p, name, text, full)
		return
	}
	var ref chat.Ref // the message of a plain post, which Update replaces
	methods := []struct {
		name string
		prep func() error // before the text is sent: not checked
		do   func() error
	}{
		{name: "Post", do: func() error { _, err := c.s.Platform.Post(c.ctx, target, p.build(text).out, "tag-1"); return err }},
		{name: "Update",
			prep: func() (err error) {
				ref, err = c.s.Platform.Post(c.ctx, target, chat.Say(chat.Message{Fallback: "seed"}), "tag-seed")
				return err
			},
			do: func() error { return c.s.Platform.Update(c.ctx, ref, p.build(text).out) }},
	}
	if !full {
		methods = methods[:1]
	}
	if m := p.build(text).out.Message; full && m != nil && c.caps.Ephemeral {
		methods = append(methods, struct {
			name string
			prep func() error
			do   func() error
		}{name: "Ephemeral", do: func() error { return c.s.Platform.Ephemeral(c.ctx, target, "U1", *m) }})
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
func (c *checker) stream(p placement, name, text string, full bool) {
	if !c.caps.Streams {
		return
	}
	do := func(method string, f func() error) {
		c.s.Sent()
		_ = c.s.Inert()
		c.verify(p, method, name, text, f())
	}
	do("Stream", func() error { _, err := c.s.Platform.Stream(c.ctx, target, "U1", text); return err })
	if !full {
		return
	}
	open := func() (chat.Stream, bool) {
		st, err := c.s.Platform.Stream(c.ctx, target, "U1", "start")
		if err != nil {
			c.add(RuleRejected, p.where, "Stream", name, "a stream of plain text was refused: %v", err)
		}
		return st, err == nil
	}
	if st, ok := open(); ok {
		do("Append", func() error { return st.Append(c.ctx, text) })
	}
	if st, ok := open(); ok {
		do("Stop", func() error {
			return st.Stop(c.ctx, chat.Message{Fallback: "done", Parts: []chat.Part{chat.Body{Markdown: text}}}, "tag-2")
		})
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
	if len(carriers) == 0 {
		if !p.optional {
			c.add(RuleDropped, p.where, method, name, "the text %q is shown nowhere (shown: %q)", text, displayed(shown))
		}
		return
	}
	limit := c.caps.Limit(p.where)
	want := chat.Clean(text, p.where.Line())
	faithful := false
	for _, s := range carriers {
		c.checkActive(p, method, name, s)
		if r, ok := invisibleIn(s.Displayed); ok {
			c.add(RuleInvisible, p.where, method, name, "%U is shown in %q", r, s.Displayed)
		}
		if p.where.Line() && strings.ContainsAny(s.Displayed, "\n\r") {
			c.add(RuleNewline, p.where, method, name, "a line break is shown in %q", s.Displayed)
		}
		if n := chat.Units(s.Displayed); n > limit {
			c.add(RuleLimit, p.where, method, name, "%d units are shown, over the limit of %d", n, limit)
		}
		if strings.Contains(s.Displayed, want) {
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
		switch {
		case a.Kind == KindAutolink && c.s.AllowAutolink:
		case a.Kind.Style() && (p.where.Rich() || c.s.StyleInLiteral):
		default:
			c.add(RuleActive, p.where, method, name, "text became a %s: %q", a.Kind, a.Text)
		}
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
	c.run(p, "oversize", canary+strings.Repeat("x", limit+64), true)
	c.run(p, "oversize astral", canary+strings.Repeat("\U0001F389", limit/2+32), false)
}
