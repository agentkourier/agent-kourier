package chattest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

func TestFakePlatformPassesConformance(t *testing.T) {
	Conformance(t, NewFake().Subject())
}

func TestConformanceRefusesAnIncompleteSubject(t *testing.T) {
	vs := Check(t.Context(), Subject{Platform: NewFake()})
	if len(vs) != 1 || vs[0].Rule != RuleSetup {
		t.Errorf("Check of a subject with no Sent and no Inert = %v, want one setup violation", vs)
	}
}

// leaks are the ways markupPlatform can be made unsafe, one at a time. Each is a way a real adapter fails.
type leaks struct {
	noEscape, noClean, keepNewlines, noTruncate bool
	drop                                        chat.Where // shows nothing of this place, if set (and dropping)
	dropping                                    bool
	mangle, notInert                            bool
	acceptsMalformed                            bool
	noStreamError                               bool
	tooLargeFits                                bool
}

// markupPlatform is a platform whose wire format is text with markup: an adapter of it has to escape, which
// is what a safe one does, and what each leak leaves out. It stores its messages in a FakePlatform and
// reports what it sent by Scan, which is what a Subject has to say for such a platform.
type markupPlatform struct {
	*FakePlatform
	leaks leaks
	sent  []Shown
	wires []string
}

func newMarkup(l leaks) *markupPlatform {
	return &markupPlatform{FakePlatform: NewFake(), leaks: l}
}

// encode is text at place w as it goes on the wire.
func (m *markupPlatform) encode(s string, w chat.Where) string {
	if !m.leaks.noClean {
		s = chat.Clean(s, w.Line() && !m.leaks.keepNewlines)
	}
	if !m.leaks.noTruncate {
		s = chat.Truncate(s, m.caps.Limit(w))
	}
	if m.leaks.mangle {
		s = strings.ReplaceAll(s, " ", "_")
	}
	if m.leaks.noEscape {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r < 0x80 && (unicode.IsPunct(r) || unicode.IsSymbol(r)):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func (m *markupPlatform) record(fields []Field) {
	for _, f := range fields {
		if m.leaks.dropping && f.Where == m.leaks.drop {
			continue
		}
		wire := m.encode(f.Text, f.Where)
		m.wires = append(m.wires, wire)
		m.sent = append(m.sent, Scan(wire))
	}
}

func (m *markupPlatform) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	if m.leaks.acceptsMalformed && o.Validate() != nil {
		return chat.Ref{}, nil
	}
	ref, err := m.FakePlatform.Post(ctx, t, o, tag)
	if err == nil {
		m.record(Fields(o))
	}
	return ref, err
}

func (m *markupPlatform) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	err := m.FakePlatform.Update(ctx, r, o)
	if err == nil {
		m.record(Fields(o))
	}
	return err
}

func (m *markupPlatform) Ephemeral(ctx context.Context, t chat.Target, user string, msg chat.Message) error {
	err := m.FakePlatform.Ephemeral(ctx, t, user, msg)
	if err == nil {
		m.record(MessageFields(msg))
	}
	return err
}

func (m *markupPlatform) Stream(ctx context.Context, t chat.Target, to, md string) (chat.Stream, error) {
	if m.leaks.noStreamError { // says it has no streams, and does not say so when asked
		return nil, nil
	}
	st, err := m.FakePlatform.Stream(ctx, t, to, md)
	if err == nil {
		m.record([]Field{{md, chat.WhereStream}})
		st = &markupStream{Stream: st, m: m}
	}
	return st, err
}

type markupStream struct {
	chat.Stream
	m *markupPlatform
}

func (s *markupStream) Append(ctx context.Context, md string) error {
	err := s.Stream.Append(ctx, md)
	if err == nil {
		s.m.record([]Field{{md, chat.WhereStream}})
	}
	return err
}

func (s *markupStream) Stop(ctx context.Context, final chat.Message, tag string) error {
	err := s.Stream.Stop(ctx, final, tag)
	if err == nil {
		s.m.record(MessageFields(final))
	}
	return err
}

func (m *markupPlatform) Fits(p chat.Prompt) error {
	if m.leaks.tooLargeFits {
		return nil
	}
	return m.FakePlatform.Fits(p)
}

func (m *markupPlatform) subject() Subject {
	s := Subject{
		Platform: m,
		Sent:     func() []Shown { out := m.sent; m.sent = nil; return out },
		Inert: func() error {
			if m.leaks.notInert {
				return errors.New("a request had unfurl_links on")
			}
			return nil
		},
		TooLarge: &chat.Prompt{ID: "ix-big", Questions: make([]chat.Question, 500)},
	}
	if m.leaks.noStreamError {
		m.SetCaps(chat.Caps{MaxText: 4000, Ephemeral: true, Edits: true})
	}
	return s
}

func TestAnEscapingMarkupAdapterPasses(t *testing.T) {
	Conformance(t, newMarkup(leaks{}).subject())
}

// The suite is the safety gate, so each way an adapter can fail it must be caught: a deliberately unsafe
// adapter is run for each, and the rule that names its failure must be among the violations. A leak that
// is not caught is a hole in the suite.
func TestEveryWayAnAdapterCanBeUnsafeIsCaught(t *testing.T) {
	for _, tc := range []struct {
		name  string
		leaks leaks
		rule  Rule
	}{
		{"no escaping: markup on the wire", leaks{noEscape: true}, RuleActive},
		{"hidden characters kept", leaks{noClean: true}, RuleInvisible},
		{"newlines kept in a one-line place", leaks{keepNewlines: true}, RuleNewline},
		{"text never cut", leaks{noTruncate: true}, RuleLimit},
		{"a hint dropped", leaks{dropping: true, drop: chat.WhereHint}, RuleDropped},
		{"a button label dropped", leaks{dropping: true, drop: chat.WhereButton}, RuleDropped},
		{"text altered", leaks{mangle: true}, RuleFidelity},
		{"a request that is not inert", leaks{notInert: true}, RuleInert},
		{"a malformed message accepted", leaks{acceptsMalformed: true}, RuleContract},
		{"no error from a platform with no streams", leaks{noStreamError: true}, RuleContract},
		{"a prompt that is too large said to fit", leaks{tooLargeFits: true}, RuleContract},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vs := CheckWith(t.Context(), newMarkup(tc.leaks).subject(), Options{Short: true})
			var rules []Rule
			for _, v := range vs {
				rules = append(rules, v.Rule)
			}
			if !slices.Contains(rules, tc.rule) {
				t.Errorf("rules found %v, want %s among them", slices.Compact(slices.Sorted(slices.Values(rules))), tc.rule)
			}
		})
	}
}

// Dangerous constructs are found in every place where they must not appear, and each kind of dangerous
// text of the corpus is found by name: a platform that lets one through is told which.
func TestNoEscapingIsCaughtForEveryKindOfDangerousText(t *testing.T) {
	vs := Check(t.Context(), newMarkup(leaks{noEscape: true}).subject())
	found := map[string]bool{}
	for _, v := range vs {
		if v.Rule == RuleActive {
			found[v.Case] = true
		}
	}
	for _, name := range []string{"channel", "here", "everyone", "user group", "date", "channel link", "user", "user with a name",
		"autolink", "link with a label", "markdown link", "markdown image", "bare URL", "script", "bold tag"} {
		if !found[name] {
			t.Errorf("the corpus case %q was not caught as an active construct when nothing is escaped", name)
		}
	}
}

// Style is cosmetic, so a platform that cannot show a literal place unstyled says so, once, and is held
// to everything else; and a bare URL it cannot stop is the same. Nothing else is excused by them.
func TestStyleAndAutolinkConcessionsExcuseOnlyThemselves(t *testing.T) {
	strict := newMarkup(leaks{noEscape: true}).subject()
	conceding := newMarkup(leaks{noEscape: true}).subject()
	conceding.StyleInLiteral, conceding.AllowAutolink = true, true
	var strictKinds, concedingKinds []string
	for _, v := range Check(t.Context(), strict) {
		if v.Rule == RuleActive {
			strictKinds = append(strictKinds, v.Detail)
		}
	}
	for _, v := range Check(t.Context(), conceding) {
		if v.Rule == RuleActive {
			concedingKinds = append(concedingKinds, v.Detail)
		}
	}
	has := func(details []string, kind Kind) bool {
		for _, d := range details {
			if strings.Contains(d, "became a "+string(kind)) {
				return true
			}
		}
		return false
	}
	for _, kind := range []Kind{KindEmphasis, KindCode, KindHeading, KindAutolink} {
		if !has(strictKinds, kind) || has(concedingKinds, kind) {
			t.Errorf("%s: caught strictly = %t, caught with the concession = %t; want true and false", kind, has(strictKinds, kind), has(concedingKinds, kind))
		}
	}
	for _, kind := range []Kind{KindMention, KindBroadcast, KindLink, KindImage, KindHTML, KindDate} {
		if !has(concedingKinds, kind) {
			t.Errorf("%s was excused by a concession for style and autolinks", kind)
		}
	}
}

// Style in the one rich place is not a violation, and a mention in it is.
func TestTheRichPlaceMayBeStyledButNotNotify(t *testing.T) {
	vs := Check(t.Context(), newMarkup(leaks{noEscape: true}).subject())
	for _, v := range vs {
		if v.Rule == RuleActive && v.Where == chat.WhereBody && (strings.Contains(v.Detail, "became a emphasis") || strings.Contains(v.Detail, "became a code")) {
			t.Errorf("style in the body was a violation: %v", v)
		}
	}
	notified := false
	for _, v := range vs {
		if v.Rule == RuleActive && v.Where == chat.WhereBody && strings.Contains(v.Detail, "became a mention") {
			notified = true
		}
	}
	if !notified {
		t.Error("a mention in the body was not caught")
	}
}
