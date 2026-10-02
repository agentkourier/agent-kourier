package chattest

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

func TestFakePlatformPassesConformance(t *testing.T) {
	t.Parallel()
	Conformance(t, NewFake().Subject())
}

func TestConformanceRefusesAnIncompleteSubject(t *testing.T) {
	vs := Check(t.Context(), Subject{Platform: NewFake()})
	if len(vs) != 1 || vs[0].Rule != RuleSetup {
		t.Errorf("Check of a subject with no Sent and no Inert = %v, want one setup violation", vs)
	}
}

// dialectKind is how the test platform's wire format is read: the reader that its Sent uses, which is the
// reader of the platform it stands for.
type dialectKind int

const (
	slackPlainText dialectKind = iota // Slack plain_text: only &amp; &lt; &gt;, no escape, tokens are tokens
	commonMark                        // CommonMark: backslash escapes, entities, links by reference
)

// leaks are the ways markupPlatform can be made unsafe, one at a time. Each is a way a real adapter fails.
type leaks struct {
	noEscape, noClean, keepNewlines, noTruncate bool
	drop                                        chat.Where // shows nothing of this place, if dropping
	dropping                                    bool
	mangle, notInert                            bool
	acceptsMalformed                            bool
	noStreamError                               bool
	tooLargeFits                                bool

	// Escaping that does not work for the platform's dialect, the way the suite once passed them.
	backslashAll      bool // a backslash before every ASCII punctuation: Slack honours none, and the "<" is a token
	backslashLt       bool // a backslash before "<" and "&" only
	backslashMarkdown bool // a CommonMark-minded escaper (backslashes and no entities) for a platform that has none
	refLinks          bool // entities and backslashes, except "[", "]" and ":": a CommonMark reference link survives
	updateGap         bool // the Update path leaves a field that starts with "<" as it is
	stopGap           bool // the Stop path does not escape
	emphasised        bool // the rich places hold the raw text inside emphasis: a reader once saw nothing there

	// A platform with mentions shows a valid user ID as one, and the leaks are how its check of the ID can fail.
	mentions    bool // the platform has mentions: a valid user ID of a Mention is shown as one
	rawUser     bool // the user of a Mention or an Ending is put on the wire as it is
	twoMentions bool // a valid user is shown as two mentions
	looseID     bool // the shape of an ID is looked for anywhere in the text, and what holds it is taken for one
	// wordsBesideMention puts the words of a Mention on the wire as they are, in the same text as the mention of a
	// valid user, so that what the words hold acts: "answered <!here>" broadcasts.
	wordsBesideMention bool
}

// markupPlatform is a platform whose wire format is text with markup: an adapter of it has to escape, which
// is what a safe one does, and what each leak leaves out. It stores its messages in a FakePlatform and
// reports what it sent by the reader of its dialect, which is what a Subject has to say for such a platform.
type markupPlatform struct {
	*FakePlatform
	dialect dialectKind
	leaks   leaks
	mode    string // the method being recorded, for the leaks that are of one method only
	sent    []Shown
}

// validUser is the shape of the user IDs of the platform the markup platform models.
var validUser = regexp.MustCompile(`^[UW][A-Z0-9]{2,20}$`)

// looseUser is a check of the shape that is not anchored: the ID is found anywhere in the text.
var looseUser = regexp.MustCompile(`[UW][A-Z0-9]{2,20}`)

func newMarkup(d dialectKind, l leaks) *markupPlatform {
	f := NewFake()
	// Small limits, so that the oversize checks send a few hundred characters and not twelve thousand.
	f.SetCaps(chat.Caps{
		Streams: true, Ephemeral: true, Edits: true, Inputs: true, MaxText: 300,
		Limits: map[chat.Where]int{chat.WhereHeader: 100, chat.WhereButton: 60, chat.WhereChoice: 80, chat.WhereHint: 100, chat.WhereBody: 600, chat.WhereStream: 600},
	})
	return &markupPlatform{FakePlatform: f, dialect: d, leaks: l}
}

func (m *markupPlatform) read(wire string) Shown {
	if m.dialect == commonMark {
		return ScanCommonMark(wire)
	}
	return ScanSlackPlain(wire)
}

// safe is s escaped for the platform's dialect. In CommonMark white space at the start of a line is written as an
// entity, so that four columns of it do not make an indented block, which is code.
func (m *markupPlatform) safe(s string) string {
	var b strings.Builder
	lineStart := true
	for _, r := range s {
		blank := r == ' ' || r == '\t'
		if lineStart && blank && m.dialect == commonMark {
			fmt.Fprintf(&b, "&#%d;", r)
			continue
		}
		lineStart = r == '\n'
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case m.dialect == commonMark && r < 0x80 && (unicode.IsPunct(r) || unicode.IsSymbol(r)):
			b.WriteByte('\\')
			b.WriteRune(r)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
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
	l := m.leaks
	if w == chat.WhereUser {
		switch {
		case l.rawUser:
			return s
		case l.mentions && (validUser.MatchString(s) || (l.looseID && looseUser.MatchString(s))):
			token := "<@" + s + ">"
			if l.twoMentions {
				token += " " + token
			}
			return token
		}
	}
	switch {
	case l.emphasised && w.Rich():
		return "_" + s + "_" // another mark than the "*" that the hostile text of the corpus may hold, so that it nests
	case l.noEscape, l.stopGap && m.mode == "Stop":
		return s
	case l.updateGap && m.mode == "Update" && strings.HasPrefix(s, "<"):
		return s
	case l.backslashAll:
		return strings.NewReplacer(escapes("\\<>&*_~`[]()#-+.!|:/")...).Replace(s)
	case l.backslashLt:
		return strings.NewReplacer("<", `\<`, "&", `\&`).Replace(s)
	case l.backslashMarkdown:
		return strings.NewReplacer(`\`, `\\`, "<", `\<`, "&", `\&`, "*", `\*`, "[", `\[`, "]", `\]`, "(", `\(`, "`", "\\`", "_", `\_`, "~", `\~`).Replace(s)
	case l.refLinks:
		var b strings.Builder
		for _, r := range s {
			switch {
			case r == '&':
				b.WriteString("&amp;")
			case r == '<':
				b.WriteString("&lt;")
			case r == '>':
				b.WriteString("&gt;")
			case r == '[' || r == ']' || r == ':':
				b.WriteRune(r)
			case r < 0x80 && (unicode.IsPunct(r) || unicode.IsSymbol(r)):
				b.WriteByte('\\')
				b.WriteRune(r)
			default:
				b.WriteRune(r)
			}
		}
		return b.String()
	}
	return m.safe(s)
}

// escapes is a replacer's arguments that put a backslash before each character of chars.
func escapes(chars string) []string {
	var out []string
	for _, r := range chars {
		out = append(out, string(r), `\`+string(r))
	}
	return out
}

func (m *markupPlatform) record(mode string, fields []Field) {
	m.mode = mode
	defer func() { m.mode = "" }()
	m.Subject().Sent() // the fake keeps what it shows, which this platform reads for itself: let it go
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if m.leaks.dropping && f.Where == m.leaks.drop {
			continue
		}
		if m.leaks.wordsBesideMention && f.Where == chat.WhereUser && validUser.MatchString(f.Text) && i+1 < len(fields) && fields[i+1].Where == chat.WhereContext {
			m.sent = append(m.sent, m.read("<@"+f.Text+"> "+chat.Clean(fields[i+1].Text, true))) // the Mention's user and its words
			i++
			continue
		}
		m.sent = append(m.sent, m.read(m.encode(f.Text, f.Where)))
	}
}

func (m *markupPlatform) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	if m.leaks.acceptsMalformed && o.Validate() != nil {
		return chat.Ref{}, nil
	}
	ref, err := m.FakePlatform.Post(ctx, t, o, tag)
	if err == nil {
		m.record("Post", Fields(o))
	}
	return ref, err
}

func (m *markupPlatform) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	err := m.FakePlatform.Update(ctx, r, o)
	if err == nil {
		m.record("Update", Fields(o))
	}
	return err
}

func (m *markupPlatform) Ephemeral(ctx context.Context, t chat.Target, user string, msg chat.Message) error {
	err := m.FakePlatform.Ephemeral(ctx, t, user, msg)
	if err == nil {
		m.record("Ephemeral", MessageFields(msg))
	}
	return err
}

func (m *markupPlatform) Stream(ctx context.Context, t chat.Target, to, md string) (chat.Stream, error) {
	if m.leaks.noStreamError { // says it has no streams, and does not say so when asked
		return nil, nil
	}
	st, err := m.FakePlatform.Stream(ctx, t, to, md)
	if err == nil {
		m.record("Stream", []Field{{md, chat.WhereStream}})
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
		s.m.record("Append", []Field{{md, chat.WhereStream}})
	}
	return err
}

func (s *markupStream) Stop(ctx context.Context, markdown string, final chat.Message, tag string) error {
	err := s.Stream.Stop(ctx, markdown, final, tag)
	if err == nil {
		fields := MessageFields(final)
		if markdown != "" {
			fields = append([]Field{{markdown, chat.WhereStream}}, fields...)
		}
		s.m.record("Stop", fields)
	}
	return err
}

func (m *markupPlatform) StreamAt(r chat.Ref) chat.Stream {
	return &markupStream{Stream: m.FakePlatform.StreamAt(r), m: m}
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
	if m.leaks.mentions {
		s.MentionUser = "U0123456789"
	}
	if m.dialect == slackPlainText {
		s.AllowAutolink = true // Slack makes a bare URL a link that shows itself, and no escape stops it
	}
	if m.leaks.noStreamError {
		m.SetCaps(chat.Caps{MaxText: 4000, Ephemeral: true, Edits: true})
	}
	return s
}

func TestAnEscapingMarkupAdapterPassesInBothDialects(t *testing.T) {
	for name, d := range map[string]dialectKind{"slack plain_text": slackPlainText, "commonmark": commonMark} {
		t.Run(name, func(t *testing.T) { t.Parallel(); Conformance(t, newMarkup(d, leaks{}).subject()) })
		t.Run(name+" with mentions", func(t *testing.T) { t.Parallel(); Conformance(t, newMarkup(d, leaks{mentions: true}).subject()) })
	}
}

// The suite is the safety gate, so each way an adapter can fail it must be caught: a deliberately unsafe
// adapter is run for each, and the rule that names its failure must be among the violations. A leak that
// is not caught is a hole in the suite. The last group is the escapers that an earlier suite passed: each
// escapes for a dialect that does not honour the escape.
func TestEveryWayAnAdapterCanBeUnsafeIsCaught(t *testing.T) {
	for _, tc := range []struct {
		name    string
		dialect dialectKind
		leaks   leaks
		rule    Rule
		kind    Kind // the construct found, for RuleActive
	}{
		{"no escaping: markup on the wire", slackPlainText, leaks{noEscape: true}, RuleActive, KindBroadcast},
		{"hidden characters kept", slackPlainText, leaks{noClean: true}, RuleInvisible, ""},
		{"newlines kept in a one-line place", slackPlainText, leaks{keepNewlines: true}, RuleNewline, ""},
		{"text never cut", slackPlainText, leaks{noTruncate: true}, RuleLimit, ""},
		{"a hint dropped", slackPlainText, leaks{dropping: true, drop: chat.WhereHint}, RuleDropped, ""},
		{"a button label dropped", slackPlainText, leaks{dropping: true, drop: chat.WhereButton}, RuleDropped, ""},
		{"text altered", slackPlainText, leaks{mangle: true}, RuleFidelity, ""},
		{"a request that is not inert", slackPlainText, leaks{notInert: true}, RuleInert, ""},
		{"a malformed message accepted", slackPlainText, leaks{acceptsMalformed: true}, RuleContract, ""},
		{"no error from a platform with no streams", slackPlainText, leaks{noStreamError: true}, RuleContract, ""},
		{"a prompt that is too large said to fit", slackPlainText, leaks{tooLargeFits: true}, RuleContract, ""},

		{"Slack: a backslash before every punctuation, which Slack does not honour", slackPlainText, leaks{backslashAll: true}, RuleActive, KindHTML},
		{"Slack: a backslash before < and & only", slackPlainText, leaks{backslashLt: true}, RuleActive, KindMention},
		{"Slack: a CommonMark-minded escaper", slackPlainText, leaks{backslashMarkdown: true}, RuleActive, KindLink},
		{"CommonMark: a reference link survives", commonMark, leaks{refLinks: true}, RuleActive, KindLink},
		{"Slack: raw text inside emphasis in the rich places", slackPlainText, leaks{emphasised: true}, RuleActive, KindMention},
		{"CommonMark: raw text inside emphasis in the rich places", commonMark, leaks{emphasised: true}, RuleActive, KindBroadcast},
		{"Slack: a forged user ID put on the wire raw", slackPlainText, leaks{mentions: true, rawUser: true}, RuleActive, KindBroadcast},
		{"CommonMark: a forged user ID put on the wire raw", commonMark, leaks{mentions: true, rawUser: true}, RuleActive, KindBroadcast},
		{"Slack: an ID is looked for anywhere in the text", slackPlainText, leaks{mentions: true, looseID: true}, RuleActive, KindMention},
		{"CommonMark: an ID is looked for anywhere in the text", commonMark, leaks{mentions: true, looseID: true}, RuleActive, KindMention},
		{"Slack: a valid user shown as two mentions", slackPlainText, leaks{mentions: true, twoMentions: true}, RuleActive, KindMention},
		{"CommonMark: a valid user shown as two mentions", commonMark, leaks{mentions: true, twoMentions: true}, RuleActive, KindMention},
		{"Slack: the words beside a valid mention put on the wire as they are", slackPlainText, leaks{mentions: true, wordsBesideMention: true}, RuleActive, KindBroadcast},
		{"CommonMark: the words beside a valid mention put on the wire as they are", commonMark, leaks{mentions: true, wordsBesideMention: true}, RuleActive, KindBroadcast},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newMarkup(tc.dialect, tc.leaks).subject()
			vs := catch(t.Context(), s) // the gaps that need a variant are in the test below
			var rules []Rule
			found := false
			for _, v := range vs {
				rules = append(rules, v.Rule)
				if v.Rule == tc.rule && (tc.kind == "" || strings.Contains(v.Detail, "became a "+string(tc.kind))) {
					found = true
				}
			}
			if !found {
				t.Errorf("rules found %v, want %s (%s) among them", slices.Compact(slices.Sorted(slices.Values(rules))), tc.rule, tc.kind)
			}
		})
	}
}

// TestAMentionInsideEmphasisIsFoundByName: a mention in emphasis acts in both dialects, and the reader that sees
// nothing inside emphasis is told by the case that is a mention in it. The rows of the table above are met by any case
// of the corpus that the leak turns live, so they do not hold the reader to it.
func TestAMentionInsideEmphasisIsFoundByName(t *testing.T) {
	for name, d := range map[string]dialectKind{"Slack": slackPlainText, "CommonMark": commonMark} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			found := false
			for _, v := range catch(t.Context(), newMarkup(d, leaks{emphasised: true}).subject()) {
				found = found || (v.Rule == RuleActive && v.Case == "emphasis around a mention" && strings.Contains(v.Detail, "became a mention"))
			}
			if !found {
				t.Error("no live mention was found for the case of a mention inside emphasis, which this adapter puts on the wire raw")
			}
		})
	}
}

// TestAValidMentionIsTheOnlyConstructBesideHostileWords: the corpus is sent as the words of a Mention of a valid
// user, and what the platform shows beside the mention must not act, nor may the user be mentioned twice. The
// way a renderer that builds one text of the mention and the words fails is the "words beside" leak: the
// adapter passed the suite once, since its harmless words never held a "<!here>" next to a live mention.
func TestAValidMentionIsTheOnlyConstructBesideHostileWords(t *testing.T) {
	for name, tc := range map[string]struct {
		dialect dialectKind
		leaks   leaks
		kind    Kind
	}{
		"Slack: the words put on the wire as they are":      {slackPlainText, leaks{mentions: true, wordsBesideMention: true}, KindBroadcast},
		"CommonMark: the words put on the wire as they are": {commonMark, leaks{mentions: true, wordsBesideMention: true}, KindBroadcast},
		"Slack: the user shown twice":                       {slackPlainText, leaks{mentions: true, twoMentions: true}, KindMention},
		"CommonMark: the user shown twice":                  {commonMark, leaks{mentions: true, twoMentions: true}, KindMention},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			found := false
			for _, v := range catch(t.Context(), newMarkup(tc.dialect, tc.leaks).subject()) {
				// A violation of the corpus in the place of a Mention's words, and not of the plain Mention check.
				found = found || (v.Where == chat.WhereContext && v.Case != "valid mention" && strings.Contains(v.Detail, "became a "+string(tc.kind)))
			}
			if !found {
				t.Errorf("no violation for hostile words beside a valid mention that became a %s", tc.kind)
			}
		})
	}
}

// Each method is checked with every variant of the text, not one: a renderer that handles a field that starts
// with "<" differently in Update, or in the final message of a stream, is caught for exactly that method.
func TestAGapInOneMethodIsFoundByThatMethodAlone(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		d       dialectKind
		leaks   leaks
		methods []string
	}{
		{commonMark, leaks{updateGap: true}, []string{"Update"}},
		{slackPlainText, leaks{updateGap: true}, []string{"Update"}},
		{commonMark, leaks{stopGap: true}, []string{"Stop", "Stop (last markdown)", "StreamAt Stop"}},
	} {
		// The variants of the text are the point of this test, and each costs a run, so it sends few cases: ones that
		// start or end with a token.
		vs := CheckWith(t.Context(), newMarkup(tc.d, tc.leaks).subject(), Options{cases: pick("channel", "user", "autolink", "markdown link")})
		found, broadcast := map[string]bool{}, false
		for _, v := range vs {
			found[v.Method] = true
			broadcast = broadcast || (v.Rule == RuleActive && strings.Contains(v.Detail, "became a broadcast"))
		}
		only := len(found) == len(tc.methods)
		for _, m := range tc.methods {
			only = only && found[m]
		}
		if !only || !broadcast {
			t.Errorf("a gap in %v was found by %v (a live broadcast seen: %t), want those alone, and the broadcast", tc.methods, found, broadcast)
		}
	}
}

// noEscape is what the suite finds of a CommonMark platform that escapes nothing, found once for the tests
// that read it.
var noEscape = sync.OnceValue(func() []Violation {
	return catchEveryVariant(context.Background(), newMarkup(commonMark, leaks{noEscape: true}).subject())
})

// Dangerous constructs are found in every place where they must not appear, and each kind of dangerous
// text of the corpus is found by name: a platform that lets one through is told which.
func TestNoEscapingIsCaughtForEveryKindOfDangerousText(t *testing.T) {
	t.Parallel()
	vs := noEscape()
	found := map[string]bool{}
	for _, v := range vs {
		if v.Rule == RuleActive {
			found[v.Case] = true
		}
	}
	for _, name := range []string{"channel", "here", "everyone", "user group", "date", "channel link", "user", "user with a name",
		"autolink", "link with a label", "markdown link", "markdown image", "markdown reference link", "markdown reference link, destination on the next line", "bare URL", "script", "bold tag"} {
		if !found[name] {
			t.Errorf("the corpus case %q was not caught as an active construct when nothing is escaped", name)
		}
	}
}

// Style is cosmetic, so a platform that cannot show a literal place unstyled says so, once, and is held
// to everything else; and a bare URL it cannot stop is the same. Nothing else is excused by them.
func TestStyleAndAutolinkConcessionsExcuseOnlyThemselves(t *testing.T) {
	t.Parallel()
	conceding := newMarkup(commonMark, leaks{noEscape: true}).subject()
	conceding.StyleInLiteral, conceding.AllowAutolink = true, true
	var strictKinds, concedingKinds []string
	for _, v := range noEscape() {
		if v.Rule == RuleActive {
			strictKinds = append(strictKinds, v.Detail)
		}
	}
	for _, v := range catch(t.Context(), conceding) {
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

// A per-place concession excuses its kinds in its place and nowhere else, and needs a reason.
func TestAConcessionExcusesItsKindsInItsPlaceOnly(t *testing.T) {
	t.Parallel()
	s := newMarkup(commonMark, leaks{noEscape: true}).subject()
	s.AllowAutolink = true
	s.Concessions = []Concession{{Where: chat.WhereNote, Kinds: StyleKinds(), Why: "a test"}}
	noteStyle, headerStyle := false, false
	for _, v := range catch(t.Context(), s) {
		if v.Rule != RuleActive || !strings.Contains(v.Detail, "became a emphasis") {
			continue
		}
		noteStyle = noteStyle || v.Where == chat.WhereNote
		headerStyle = headerStyle || v.Where == chat.WhereHeader
	}
	if noteStyle {
		t.Error("style in a note was a violation despite the concession")
	}
	if !headerStyle {
		t.Error("a concession for the note excused style in the header")
	}
	s.Concessions = []Concession{{Where: chat.WhereNote, Kinds: StyleKinds()}}
	if vs := Check(t.Context(), s); len(vs) != 1 || vs[0].Rule != RuleSetup {
		t.Errorf("a concession with no reason gave %d violations, want one setup violation", len(vs))
	}
}

// A target that the Subject names is where the checks post.
func TestTheChecksPostWhereTheSubjectSays(t *testing.T) {
	t.Parallel()
	f := NewFake()
	s := f.Subject()
	s.Target = chat.Target{Conn: "ns/conn", Channel: "C9", Thread: "9.9"}
	if vs := CheckWith(t.Context(), s, Options{Short: true}); len(vs) != 0 {
		t.Fatalf("violations: %v", vs)
	}
	for _, m := range f.Messages() {
		if m.Ref.Channel != "C9" || m.Ref.Conn != "ns/conn" {
			t.Fatalf("a check posted in %+v, want only in the Subject's target", m.Ref)
		}
	}
}

// Style in the one rich place is not a violation, and a mention in it is.
func TestTheRichPlaceMayBeStyledButNotNotify(t *testing.T) {
	t.Parallel()
	vs := noEscape()
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
