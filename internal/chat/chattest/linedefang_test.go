package chattest

import (
	"context"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// lineDefangAdapter is an adapter whose escaping of link brackets works a line at a time: it escapes "[" and "]"
// only on a line that holds both, and leaves parentheses alone. Link text that runs over two lines slips
// through it as a live CommonMark link, which is what the scanner once could not see. It sends every text as
// CommonMark.
type lineDefangAdapter struct {
	*FakePlatform
	sent []Shown
	// escape is the adapter's escaping: lineDefang if nil.
	escape func(string) string
}

// lineDefang backslash-escapes ASCII punctuation, but leaves parentheses alone and defangs links the way a
// line-based sanitiser does: it escapes the brackets of a line that holds both a "[" and a "]".
func lineDefang(s string) string {
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		pair := strings.Contains(line, "[") && strings.Contains(line, "]")
		for _, r := range line {
			switch {
			case (r == '[' || r == ']') && !pair, r == '(' || r == ')':
				out.WriteRune(r)
			case r < 128 && isPunct(byte(r)):
				out.WriteByte('\\')
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

func (a *lineDefangAdapter) enc(s string, w chat.Where) Shown {
	s = chat.Truncate(chat.Clean(s, w.Line()), a.caps.Limit(w))
	if a.escape != nil {
		return ScanCommonMark(a.escape(s))
	}
	return ScanCommonMark(lineDefang(s))
}

func (a *lineDefangAdapter) rec(fs []Field) {
	for _, f := range fs {
		a.sent = append(a.sent, a.enc(f.Text, f.Where))
	}
}

func (a *lineDefangAdapter) Post(ctx context.Context, t chat.Target, o chat.Outgoing, tag string) (chat.Ref, error) {
	r, err := a.FakePlatform.Post(ctx, t, o, tag)
	if err == nil {
		a.rec(Fields(o))
	}
	return r, err
}

func (a *lineDefangAdapter) Update(ctx context.Context, r chat.Ref, o chat.Outgoing) error {
	err := a.FakePlatform.Update(ctx, r, o)
	if err == nil {
		a.rec(Fields(o))
	}
	return err
}

func (a *lineDefangAdapter) Ephemeral(ctx context.Context, t chat.Target, u string, m chat.Message) error {
	err := a.FakePlatform.Ephemeral(ctx, t, u, m)
	if err == nil {
		a.rec(MessageFields(m))
	}
	return err
}

func (a *lineDefangAdapter) Stream(ctx context.Context, t chat.Target, to, md string) (chat.Stream, error) {
	st, err := a.FakePlatform.Stream(ctx, t, to, md)
	if err == nil {
		a.rec([]Field{{md, chat.WhereStream}})
		st = &lineDefangStream{st, a}
	}
	return st, err
}

type lineDefangStream struct {
	chat.Stream
	a *lineDefangAdapter
}

func (s *lineDefangStream) Append(ctx context.Context, md string) error {
	err := s.Stream.Append(ctx, md)
	if err == nil {
		s.a.rec([]Field{{md, chat.WhereStream}})
	}
	return err
}

func (s *lineDefangStream) Stop(ctx context.Context, md string, f chat.Message, tag string) error {
	err := s.Stream.Stop(ctx, md, f, tag)
	if err == nil {
		s.a.rec([]Field{{md, chat.WhereStream}})
		s.a.rec(MessageFields(f))
	}
	return err
}

// TestALineBasedLinkDefangerIsCaught: the scanner sees a link whose text runs over two lines, and the
// suite, run on an adapter that defangs links a line at a time, finds links and an image on the wire.
func TestALineBasedLinkDefangerIsCaught(t *testing.T) {
	for _, in := range []string{"[your\nbank](https://evil.test/login)", "[your\nbank](javascript:alert(1))", "![a\nb](https://evil.test/p.png)"} {
		sh := ScanCommonMark(lineDefang(in))
		if len(sh.Active) == 0 {
			t.Errorf("%q goes out as %q, and the scanner finds nothing active in it", in, lineDefang(in))
		}
	}
	a := &lineDefangAdapter{FakePlatform: NewFake()}
	s := Subject{Platform: a, Sent: func() []Shown { o := a.sent; a.sent = nil; return o }, Inert: func() error { return nil }}
	seen := map[string]bool{}
	for _, v := range Check(context.Background(), s) {
		seen[v.Case] = true
	}
	for _, name := range []string{"markdown link, text over two lines", "markdown link, javascript, text over two lines", "markdown image, alt over two lines"} {
		if !seen[name] {
			t.Errorf("the suite found no violation for %q, which this adapter lets through as a live link", name)
		}
	}
}

// colonDefang is lineDefang with the other choices of a careful-looking defanger: it escapes parentheses and leaves ":"
// raw, so that the destination of a link reference definition keeps its colon, and it still escapes brackets only on
// a line that holds both. A definition whose label runs over two lines, "[x\n]: https://evil.test", has no such line,
// and is a live definition for "[x\n]" on the wire.
func colonDefang(s string) string {
	var out strings.Builder
	for i, line := range strings.Split(s, "\n") {
		if i > 0 {
			out.WriteByte('\n')
		}
		pair := strings.Contains(line, "[") && strings.Contains(line, "]")
		for _, r := range line {
			switch {
			case (r == '[' || r == ']') && !pair, r == ':':
				out.WriteRune(r)
			case r < 128 && isPunct(byte(r)):
				out.WriteByte('\\')
				out.WriteRune(r)
			default:
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

// TestALabelOverTwoLinesIsALinkReferenceDefinition: the scanner reads "[x\n]: https://evil.test" as a definition,
// so the link "[x\n]" that it defines is seen, and the suite, run on an adapter whose escaping works a line at a
// time and leaves ":" raw, finds the live link on the wire.
func TestALabelOverTwoLinesIsALinkReferenceDefinition(t *testing.T) {
	const in = "[x\n]\n\n[x\n]: https://evil.test"
	if sh := ScanCommonMark(colonDefang(in)); len(sh.Active) == 0 {
		t.Errorf("%q goes out as %q, and the scanner finds nothing active in it", in, colonDefang(in))
	}
	a := &lineDefangAdapter{FakePlatform: NewFake(), escape: colonDefang}
	s := Subject{Platform: a, Sent: func() []Shown { o := a.sent; a.sent = nil; return o }, Inert: func() error { return nil }}
	seen := map[string]bool{}
	for _, v := range Check(context.Background(), s) {
		seen[v.Case] = true
	}
	if !seen["markdown reference definition, label over two lines"] {
		t.Error("the suite found no violation for a link reference definition whose label runs over two lines, which this adapter lets through live")
	}
}
