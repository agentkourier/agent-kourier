package audit

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// memLog is a store.Audit that keeps what the store would: the entry as Redacted returns it, refused when its outcome is
// longer than its kind allows. It lets a test write many rows without a database.
type memLog struct {
	store.Audit
	rows []store.AuditEntry
}

func (m *memLog) AppendAudit(_ context.Context, e store.AuditEntry) error {
	e = e.Redacted()
	if len(e.Outcome) > e.Kind.MaxOutcome() {
		return fmt.Errorf("outcome of %d bytes: %w", len(e.Outcome), store.ErrInvalid)
	}
	m.rows = append(m.rows, e)
	return nil
}

func memWriter(t *testing.T) (*Writer, *memLog) {
	t.Helper()
	m := &memLog{}
	return writerOver(t, m, Options{Now: func() time.Time { return t0 }}), m
}

// builtRow is the row Decision builds for ds and names, before the writer and the store redact it again.
func builtRow(ds []dialect.Decision, names []string) string {
	approved := 0
	for _, x := range ds {
		if x.Approved {
			approved++
		}
	}
	head := fmt.Sprintf("answered approved=%d denied=%d", approved, len(ds)-approved)
	return head + toolLists(len(head), names, ds)
}

// write writes the row of an answered approval and returns the row as stored. It fails the test unless that is the row
// that was built, byte for byte: what is written must never be rewritten by the redaction of the whole row, or cut.
func write(t *testing.T, w *Writer, m *memLog, ds []dialect.Decision, names []string) string {
	t.Helper()
	d := Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionAnswered, Answer: &dialect.Answer{Decisions: ds}, ToolNames: names}
	if err := w.Decision(context.Background(), who, d); err != nil {
		t.Fatalf("Decision: %v", err)
	}
	got := m.rows[len(m.rows)-1].Outcome
	if want := builtRow(ds, names); got != want {
		t.Fatalf("the stored row (%d bytes) is not the built row (%d bytes):\nstored %.300q\nbuilt  %.300q", len(got), len(want), got, want)
	}
	return got
}

// decide writes the row of an answered approval of the given tools ("!" denies one) and returns it.
func decide(t *testing.T, w *Writer, m *memLog, tools []string) string {
	t.Helper()
	ds, ns := decisions(tools...)
	return write(t, w, m, ds, ns)
}

// listed is one tool list of a decision row, read back: the names, in order, and the K of a trailing +K (0 when none).
type listed struct {
	names []string
	left  int
}

// row is a decision row read back by a strict reader.
type row struct {
	approved, denied int
	approvedList     *listed
	deniedList       *listed
}

// parseRow reads `answered approved=N denied=M[ approved_tools=[...]][ denied_tools=[...]]` and nothing else. A name is a
// Go-quoted string, read with strconv.QuotedPrefix, so that what a name holds cannot end it early or start another.
func parseRow(s string) (r row, err error) {
	rest, ok := strings.CutPrefix(s, "answered approved=")
	if !ok {
		return r, errors.New("no counts")
	}
	a, rest, ok := strings.Cut(rest, " denied=")
	if !ok {
		return r, errors.New("no denied count")
	}
	d, rest, _ := strings.Cut(rest, " ")
	if rest != "" {
		rest = " " + rest
	}
	if r.approved, err = strconv.Atoi(a); err != nil {
		return r, err
	}
	if r.denied, err = strconv.Atoi(d); err != nil {
		return r, err
	}
	for _, label := range []string{"approved_tools", "denied_tools"} {
		after, ok := strings.CutPrefix(rest, " "+label+"=[")
		if !ok {
			continue
		}
		var l listed
		if l, rest, err = parseList(after); err != nil {
			return r, fmt.Errorf("%s: %w", label, err)
		}
		if label == "approved_tools" {
			r.approvedList = &l
		} else {
			r.deniedList = &l
		}
	}
	if rest != "" {
		return r, fmt.Errorf("trailing text %q", rest)
	}
	return r, nil
}

// parseList reads names up to the closing bracket and returns what follows it.
func parseList(s string) (l listed, rest string, err error) {
	for {
		switch {
		case strings.HasPrefix(s, `"`):
			q, err := strconv.QuotedPrefix(s)
			if err != nil {
				return l, "", fmt.Errorf("a name: %w", err)
			}
			name, err := strconv.Unquote(q)
			if err != nil {
				return l, "", err
			}
			l.names, s = append(l.names, name), s[len(q):]
		case strings.HasPrefix(s, "+"):
			digits := strings.TrimLeft(s[1:], "0123456789")
			if l.left, err = strconv.Atoi(s[1 : len(s)-len(digits)]); err != nil {
				return l, "", err
			}
			s = digits
		default:
			return l, "", fmt.Errorf("unexpected %.20q", s)
		}
		switch {
		case strings.HasPrefix(s, ","):
			s = s[1:]
		case strings.HasPrefix(s, "]"):
			return l, s[1:], nil
		default:
			return l, "", fmt.Errorf("expected , or ] before %.20q", s)
		}
	}
}

// markOf is the mark a cut name ends in: three dots and the first 16 hex digits of the SHA-256 of the whole cleaned name.
func markOf(cleaned string) string {
	sum := sha256.Sum256([]byte(cleaned))
	return "...#" + hex.EncodeToString(sum[:8])
}

// checkName reports why shown is not what a name written as raw must read as: its cleaned text when that fits whole and the
// next redaction would leave it alone (see stableName), and otherwise a prefix of it that leaves room for the mark, then the
// mark (the prefix is the longest that fits unless the end of it makes a token shape with the mark).
func checkName(raw, shown string) error {
	cleaned := redact.IdentifierText(raw)
	if q := strconv.Quote(cleaned); len(q) <= maxToolName && stableName(q) {
		if shown != cleaned {
			return fmt.Errorf("%q reads %q; want it whole", raw, shown)
		}
		return nil
	}
	mark := markOf(cleaned)
	prefix, ok := strings.CutSuffix(shown, mark)
	if !ok || !strings.HasPrefix(cleaned, prefix) {
		return fmt.Errorf("%.40q reads %.60q; want a prefix of the cleaned name and %s", raw, shown, mark)
	}
	if n := len(strconv.Quote(shown)); n > maxToolName {
		return fmt.Errorf("%.40q reads as %d bytes quoted; want at most %d", raw, n, maxToolName)
	}
	return nil
}

// A name of 300 ASCII bytes is written whole; one past the cap is cut to it and ends in a mark that holds a hash of the whole
// cleaned name, so two names that share their first characters do not read alike.
func TestDecisionOfAnApprovalMarksACutNameWithAHashOfTheWholeName(t *testing.T) {
	prefix := strings.Repeat("n", 400)
	a, b := prefix+"a", prefix+"b"
	w, m := memWriter(t)
	rowA, rowB := decide(t, w, m, []string{a}), decide(t, w, m, []string{b})
	if rowA == rowB {
		t.Fatalf("two different names read alike: %q", rowA)
	}
	for _, tc := range []struct{ raw, row string }{{a, rowA}, {b, rowB}} {
		r, err := parseRow(tc.row)
		if err != nil || r.approvedList == nil || len(r.approvedList.names) != 1 {
			t.Fatalf("row %q: %v", tc.row, err)
		}
		if err := checkName(tc.raw, r.approvedList.names[0]); err != nil {
			t.Error(err)
		}
		if shown := r.approvedList.names[0]; !strings.HasSuffix(shown, markOf(tc.raw)) || len(strconv.Quote(shown)) != maxToolName {
			t.Errorf("row (%d bytes) = %.80q...; want the name cut to exactly the cap and ended in %s", len(tc.row), tc.row, markOf(tc.raw))
		}
	}
	if whole := decide(t, w, m, []string{strings.Repeat("n", 300)}); strings.Contains(whole, "...") {
		t.Errorf("a name of 300 ASCII bytes was cut: %.80q", whole)
	}
}

// TestDecisionOfAnApprovalNamesAllTwentyToolsAtTheCap pins the worst case of the approval path: 20 tools, every name at
// the per-name cap. Every name is listed whole, there is no +K, the row fits the decision limit, and its length is the
// one MaxDecisionOutcome is sized for.
func TestDecisionOfAnApprovalNamesAllTwentyToolsAtTheCap(t *testing.T) {
	full := strings.Repeat("x", maxToolUnits)
	for _, tc := range []struct {
		name    string
		tools   []string
		wantLen int
	}{
		{"20 approved", repeat(20, full), len(`answered approved=20 denied=0 approved_tools=[]`) + 20*maxToolName + 19},
		{"20 denied", repeat(20, "!"+full), len(`answered approved=0 denied=20 denied_tools=[]`) + 20*maxToolName + 19},
		{"10 and 10", append(repeat(10, full), repeat(10, "!"+full)...), len(`answered approved=10 denied=10 approved_tools=[] denied_tools=[]`) + 20*maxToolName + 18},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := memWriter(t)
			got := decide(t, w, m, tc.tools)
			if len(got) != tc.wantLen || len(got) > store.MaxDecisionOutcome {
				t.Errorf("row is %d bytes; want %d, within MaxDecisionOutcome %d", len(got), tc.wantLen, store.MaxDecisionOutcome)
			}
			r, err := parseRow(got)
			if err != nil {
				t.Fatalf("row %.100q: %v", got, err)
			}
			if n := len(r.approvedList.namesOrNil()) + len(r.deniedList.namesOrNil()); n != 20 || r.approvedList.leftOrZero()+r.deniedList.leftOrZero() != 0 || strings.Contains(got, ",+") {
				t.Errorf("row names %d of 20 tools (%d marked left out); want all 20 and no +K", n, r.approvedList.leftOrZero()+r.deniedList.leftOrZero())
			}
			for _, name := range append(r.approvedList.namesOrNil(), r.deniedList.namesOrNil()...) {
				if name != full {
					t.Fatalf("a name reads %.40q (%d bytes); want the full name", name, len(name))
				}
			}
		})
	}
}

func (l *listed) namesOrNil() []string {
	if l == nil {
		return nil
	}
	return l.names
}

func (l *listed) leftOrZero() int {
	if l == nil {
		return 0
	}
	return l.left
}

// The longest row the approval path can produce is the 10/10 split of names at the cap, and MaxDecisionOutcome holds it. A name
// is at most maxToolName bytes as written, however many bytes a unit of it quotes to (a backslash, a quote or an unassigned
// character quote to two to six), so no name makes a longer row.
func TestDecisionOfAnApprovalWorstRowFitsTheDecisionLimit(t *testing.T) {
	const worst = 6122 // the 10/10 split, 20 names of maxToolName bytes
	if len(`answered approved=10 denied=10 approved_tools=[] denied_tools=[]`)+20*maxToolName+18 != worst || worst > store.MaxDecisionOutcome {
		t.Fatalf("the worst row is %d bytes by this arithmetic; MaxDecisionOutcome is %d", worst, store.MaxDecisionOutcome)
	}
	for _, unit := range []string{"x", `\`, `"`, "\u0378", "\U0001F600", "\u00e9", "\u3042"} {
		hostile := strings.Repeat(unit, maxToolUnits)
		w, m := memWriter(t)
		got := decide(t, w, m, append(repeat(10, hostile), repeat(10, "!"+hostile+"2")...))
		if len(got) > worst {
			t.Errorf("20 names of %U x %d make a row of %d bytes; want at most %d", []rune(unit)[0], maxToolUnits, len(got), worst)
		}
		r, err := parseRow(got)
		if err != nil || len(r.approvedList.names) != 10 || len(r.deniedList.names) != 10 || r.approvedList.left != 0 || r.deniedList.left != 0 {
			t.Errorf("20 names of %U: row %.80q... = %+v, %v; want all 20 and no +K", []rune(unit)[0], got, r, err)
		}
	}
}

// Past the approval path (an approval holds 20 tools) the lists still end in +K, the number of names left out, and the
// counts are exact, so the row accounts for every tool.
func TestDecisionOfAnApprovalPastTwentyToolsEndsInAMarker(t *testing.T) {
	w, m := memWriter(t)
	got := decide(t, w, m, repeat(40, strings.Repeat("x", maxToolUnits)))
	if len(got) > store.MaxDecisionOutcome || !strings.HasPrefix(got, "answered approved=40 denied=0 approved_tools=[") {
		t.Fatalf("row of %d bytes = %.80q; want the exact counts first and at most %d bytes", len(got), got, store.MaxDecisionOutcome)
	}
	r, err := parseRow(got)
	if err != nil {
		t.Fatalf("row %.100q: %v", got, err)
	}
	if n := len(r.approvedList.names); n == 0 || r.approvedList.left == 0 || n+r.approvedList.left != 40 {
		t.Errorf("%d names listed and %d left out; want them to add up to 40", len(r.approvedList.names), r.approvedList.left)
	}
}

// hostilePieces are what the names of the property test are made of: the shapes that could end a quoted name, start another
// list, pass for a count or a marker, hide a secret, break the line or split a character.
var hostilePieces = []string{
	`"`, `\`, `","`, `"]`, `]`, `[`, `,`, `+`, `+3`, ` `, "\n", "\t", "\u200b", "\u0378", "\ue000", "\U0001F600", "\u00e9", "\u3042", "\xff", "\xc3", "\x00",
	`"] denied_tools=["x`, `] denied_tools=["`, `", "`, `"],["`, ` approved=0 denied=20`, `...`, `...#0123456789abcdef`, `answered approved=1`,
	"xoxb-1234-abcd", "Bearer abcdef", "Basic dXNlcjpwYXNz", "ghp_abcdefghijklmnopqrstuvwxyz0123456789", "eyJhbGciOiJub25lIn0.eyJjYW5hcnkiOjF9.sig",
	"Bearer:", "Bearer%20", "id_token=", "access_token:", "\u0378=", "id_token\u0378=x,", "Bearer\u0378abcdefg", "eyJ\u0378.a.b", "Basic\u0378dXNlcjpwYXNz",
	"password=hunter2", "--token", "kubectl_get", "delete_pod", "a", "b", "0123456789", strings.Repeat("x", 60), strings.Repeat("ab", 110),
}

// TestDecisionOfAnApprovalRowIsParseableAndUnforgeable is the property test: for random approvals of up to 20 tools with
// hostile names, the row, as the store keeps it, reads back with a strict parser to exactly the tools that were decided: the
// counts, the lists and their order, and each name as it must read (whole, or cut and marked), with no name left out and
// none invented. The names are short and long; a few are the longest a 300-unit name can quote to.
func TestDecisionOfAnApprovalRowIsParseableAndUnforgeable(t *testing.T) {
	// A row costs about a second under the race detector (about 20 ms without it) (most hostile names walk back to a stable cut, and each is redacted
	// many times), so the default is a few rows; the deterministic tables pin the behaviour. AUDIT_PROPERTY_ROWS runs a deeper
	// search: 2000 rows passed without the race detector, in about 40 seconds.
	rows := 20
	if v, err := strconv.Atoi(os.Getenv("AUDIT_PROPERTY_ROWS")); err == nil && v > 0 {
		rows = v
	} else if testing.Short() {
		rows = 10
	}
	rng := rand.New(rand.NewSource(20261011))
	w, m := memWriter(t)
	for i := 0; i < rows; i++ {
		n := 1 + rng.Intn(20)
		var ds []dialect.Decision
		var names, wantApproved, wantDenied []string
		for j := 0; j < n; j++ {
			var b strings.Builder
			for k := rng.Intn(12); k >= 0; k-- {
				piece := hostilePieces[rng.Intn(len(hostilePieces))]
				if rng.Intn(10) == 0 {
					piece = strings.Repeat(piece, 1+rng.Intn(400/len(piece)+1))
				}
				b.WriteString(piece)
			}
			name, approved := b.String(), rng.Intn(2) == 0
			ds, names = append(ds, dialect.Decision{ToolID: fmt.Sprintf("id-%d", j), Approved: approved}), append(names, name)
			if approved {
				wantApproved = append(wantApproved, name)
			} else {
				wantDenied = append(wantDenied, name)
			}
		}
		d := Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionAnswered, Answer: &dialect.Answer{Decisions: ds}, ToolNames: names}
		if err := w.Decision(context.Background(), who, d); err != nil {
			t.Fatalf("row %d: Decision: %v", i, err)
		}
		got := m.rows[len(m.rows)-1].Outcome // the tables compare it with the built row, byte for byte; here it is parsed
		r, err := parseRow(got)
		if err != nil {
			t.Fatalf("row %d = %.200q: %v", i, got, err)
		}
		if r.approved != len(wantApproved) || r.denied != len(wantDenied) {
			t.Fatalf("row %d: counts %d/%d; want %d/%d", i, r.approved, r.denied, len(wantApproved), len(wantDenied))
		}
		for _, side := range []struct {
			label string
			l     *listed
			want  []string
		}{{"approved", r.approvedList, wantApproved}, {"denied", r.deniedList, wantDenied}} {
			if (side.l == nil) != (len(side.want) == 0) {
				t.Fatalf("row %d: the %s list is %v for %d tools", i, side.label, side.l, len(side.want))
			}
			if side.l == nil {
				continue
			}
			if side.l.left != 0 || len(side.l.names) != len(side.want) {
				t.Fatalf("row %d: %d %s names and %d left out; want all %d", i, len(side.l.names), side.label, side.l.left, len(side.want))
			}
			for j, raw := range side.want {
				if err := checkName(raw, side.l.names[j]); err != nil {
					t.Fatalf("row %d, %s tool %d: %v\nraw %q\ncleaned %q\nrow %q", i, side.label, j, err, raw, redact.IdentifierText(raw), got)
				}
			}
		}
		if len(got) > store.MaxDecisionOutcome {
			t.Fatalf("row %d is %d bytes", i, len(got))
		}
	}
}

// The longest row an approval makes is stored by the real store and read back whole.
func TestDecisionOfAnApprovalWorstRowIsStoredAndReadBack(t *testing.T) {
	w, s := newWriter(t)
	full := strings.Repeat("x", maxToolUnits)
	ds, ns := decisions(append(repeat(10, full), repeat(10, "!"+full)...)...)
	if err := w.Decision(context.Background(), who, Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionAnswered, Answer: &dialect.Answer{Decisions: ds}, ToolNames: ns}); err != nil {
		t.Fatalf("Decision: %v", err)
	}
	got := only(t, s).Outcome
	want := "answered approved=10 denied=10 approved_tools=[" + strings.TrimSuffix(strings.Repeat(`"`+full+`",`, 10), ",") + "] denied_tools=[" + strings.TrimSuffix(strings.Repeat(`"`+full+`",`, 10), ",") + "]"
	if got != want || len(got) != 6122 {
		t.Errorf("stored row is %d bytes; want the %d-byte row of 20 whole names", len(got), len(want))
	}
}

// A name that the redaction of the whole row would rewrite is never written as it is: it is written cut and marked, in the
// form the second pass leaves alone. Each case here was stored wrong before: the closing `"]` eaten, the name replaced by
// "[redacted]" (so that different tools read alike), or the row cut by the cap after the second pass made it longer.
func TestDecisionOfAnApprovalNeverStoresADifferentRowThanItBuilt(t *testing.T) {
	legit := strings.Repeat("n", 300)
	for _, tc := range []struct {
		name  string
		tools []string
	}{
		{"Bearer: alone", []string{"Bearer:"}},
		{"Bearer: last of a list", []string{"a", "Bearer:", "!x"}},
		{"Bearer: in the middle", []string{"Bearer:", "b"}},
		{"Bearer%20 last", []string{"get_Bearer%20"}},
		{"id_token= last", []string{"a", "id_token="}},
		{"access_token: last", []string{"get_access_token:"}},
		{"client_secret= in the middle", []string{"client_secret=", "b"}},
		{"Bearer and an unassigned character", []string{"Bearer͸abcdefg"}},
		{"JWT and an unassigned character", []string{"eyJ͸.a.b"}},
		{"Basic and an unassigned character", []string{"Basic͸dXNlcjpwYXNz"}},
		{"an unassigned character and an equals sign", []string{"id_token͸=x"}},
		{"one name after another that read alike without the hash", []string{"Bearer͸abcdefg", "Bearer͸hijklmn"}},
		{"17 pairs beside 19 names of 300 bytes, approved", append([]string{strings.Repeat("id_token͸=x,", 17)}, repeat(19, legit)...)},
		{"17 pairs beside 19 names of 300 bytes, split", append([]string{"!" + strings.Repeat("id_token͸=x,", 17)}, repeat(19, legit)...)},
		{"19 names of 300 bytes beside 17 pairs", append(repeat(19, legit), "!"+strings.Repeat("id_token͸=x,", 17))},
		{"the cut falls after a Slack token prefix", []string{strings.Repeat("n", 275) + "xoxb-" + ", and more text that makes the name longer than the cap"}},
		{"a whole name that ends as a mark does", []string{"kubectl_get...#0123456789abcdef"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w, m := memWriter(t)
			got := decide(t, w, m, append([]string(nil), tc.tools...)) // fails unless the stored row is the built row
			r, err := parseRow(got)
			if err != nil || r.approvedList.leftOrZero()+r.deniedList.leftOrZero() != 0 {
				t.Fatalf("row %.200q: %v; want it to parse with no name left out", got, err)
			}
			names := append(r.approvedList.namesOrNil(), r.deniedList.namesOrNil()...)
			if len(names) != len(tc.tools) {
				t.Fatalf("row %.200q names %d of %d tools", got, len(names), len(tc.tools))
			}
			ds, raws := decisions(tc.tools...)
			var approved, denied []string
			for i, x := range ds {
				if x.Approved {
					approved = append(approved, raws[i])
				} else {
					denied = append(denied, raws[i])
				}
			}
			for i, raw := range append(approved, denied...) {
				if err := checkName(raw, names[i]); err != nil {
					t.Error(err)
				}
			}
			// The real store, which redacts once more, keeps the same row.
			w2, s := newWriter(t)
			ds, ns := decisions(tc.tools...)
			if err := w2.Decision(context.Background(), who, Decision{InteractionID: "i1", TaskID: "t1", Result: DecisionAnswered, Answer: &dialect.Answer{Decisions: ds}, ToolNames: ns}); err != nil {
				t.Fatalf("Decision: %v", err)
			}
			if stored := only(t, s).Outcome; stored != got {
				t.Errorf("the SQLite store keeps %.200q; want %.200q", stored, got)
			}
		})
	}
}

// Two different names that the redaction would make alike do not read alike in the row: each ends in the hash of its own
// cleaned text.
func TestDecisionOfAnApprovalKeepsApartNamesThatRedactionWouldMerge(t *testing.T) {
	a, b := "Bearer͸abcdefg", "Bearer͸hijklmn"
	w, m := memWriter(t)
	if decide(t, w, m, []string{a}) == decide(t, w, m, []string{b}) {
		t.Errorf("the rows of %q and %q read alike", a, b)
	}
}

// A name is whole by its quoted bytes, at most maxToolName with the quotes, not by its units: ASCII 300 whole; a backslash or
// a quote 150; a two-byte character 150; a three-byte one 100; an emoji 75; an unassigned BMP character (\uXXXX, six bytes) 50.
// One more is cut: marked, in the cap, and still the same tool by its hash.
func TestDecisionOfAnApprovalNameIsWholeByQuotedBytes(t *testing.T) {
	for _, tc := range []struct {
		unit  string
		whole int
	}{{"n", 300}, {`\`, 150}, {`"`, 150}, {"é", 150}, {"あ", 100}, {"\U0001F600", 75}, {"͸", 50}} {
		for n, cut := range map[int]bool{tc.whole: false, tc.whole + 1: true} {
			name := strings.Repeat(tc.unit, n)
			w, m := memWriter(t)
			got := decide(t, w, m, []string{name})
			r, err := parseRow(got)
			if err != nil || len(r.approvedList.names) != 1 {
				t.Fatalf("%U x %d: row %.100q: %v", []rune(tc.unit)[0], n, got, err)
			}
			shown := r.approvedList.names[0]
			if cut != strings.HasSuffix(shown, markOf(redact.IdentifierText(name))) || !cut && shown != name {
				t.Errorf("%U x %d: cut = %v, want %v (shown %.40q)", []rune(tc.unit)[0], n, strings.HasSuffix(shown, "..."), cut, shown)
			}
			if err := checkName(name, shown); err != nil {
				t.Error(err)
			}
		}
	}
}

// A row the redaction would change, or that is over the decision limit, is an error and nothing is written: an approval
// whose record cannot be written does not take effect.
func TestCheckDecisionRowFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  string
		ok   bool
	}{
		{"a plain row", `answered approved=1 denied=0 approved_tools=["delete_pod"]`, true},
		{"a name that ends in a credential", `answered approved=1 denied=0 approved_tools=["Bearer:"]`, false},
		{"a token", `answered approved=1 denied=0 approved_tools=["xoxb-1234-abcd"]`, false},
		{"over the limit", "answered " + strings.Repeat("a", store.MaxDecisionOutcome), false},
		{"at the limit", "answered " + strings.Repeat("a", store.MaxDecisionOutcome-len("answered ")), true},
	} {
		if err := checkDecisionRow(tc.row); (err == nil) != tc.ok || err != nil && !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: checkDecisionRow = %v; want ok=%v (ErrInvalid when refused)", tc.name, err, tc.ok)
		}
	}
}

// ToolNameFit is the answer to "does the row write this name whole", by the rule that writes it. Ordinary names in several
// scripts are whole; a name the row would cut for length is NameTooLong at the first byte past the limit of its kind (the
// same limits the row's own test pins); a name the redaction of an identifier or of the whole row would change is
// NameRewritten.
func TestToolNameFitSaysWhatARowWritesWhole(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want NameFit
	}{
		{"empty", "", NameWhole},
		{"ascii", "delete_pod", NameWhole},
		{"with a space", "run command", NameWhole},
		{"with a dot, a dash and a slash", "k8s.io/get-pods", NameWhole},
		{"latin with accents", "créer_dépôt", NameWhole},
		{"cyrillic", "удалить_под", NameWhole},
		{"cjk", "删除容器", NameWhole},
		{"arabic", "مرحبا", NameWhole},
		{"an emoji", "deploy_\U0001F680", NameWhole},
		{"a word that holds a credential word", "BearerTokenValidator", NameRewritten},
		{"a trailing credential scheme", "check_Bearer:", NameRewritten},
		{"a trailing assignment", "check_id_token=", NameRewritten},
		{"a token shape", "xoxb-1234-abcd", NameRewritten},
		{"a zero-width character", "del\u200bete", NameRewritten},
		{"a variation selector", "ok\ufe0f", NameRewritten},
		{"a line break", "a\nb", NameRewritten},
		{"a tab", "a\tb", NameRewritten},
		{"white space at the end", "delete ", NameRewritten},
		{"two spaces", "a  b", NameRewritten},
		{"300 ascii bytes", strings.Repeat("n", 300), NameWhole},
		{"301 ascii bytes", strings.Repeat("n", 301), NameTooLong},
		{"150 two-byte characters", strings.Repeat("é", 150), NameWhole},
		{"151 two-byte characters", strings.Repeat("é", 151), NameTooLong},
		{"100 cjk characters", strings.Repeat("あ", 100), NameWhole},
		{"101 cjk characters", strings.Repeat("あ", 101), NameTooLong},
		{"75 emoji", strings.Repeat("\U0001F600", 75), NameWhole},
		{"76 emoji", strings.Repeat("\U0001F600", 76), NameTooLong},
		{"too long and rewritten", strings.Repeat("n", 301) + "Bearer:", NameTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ToolNameFit(tc.in); got != tc.want {
				t.Errorf("ToolNameFit(%.40q) = %d; want %d", tc.in, got, tc.want)
			}
		})
	}
}

// ToolNameFit and the row cannot drift: for names of every fit, NameWhole is exactly the names the row writes as they are
// (quoted), and a name of any other fit is written as something else.
func TestToolNameFitAgreesWithTheRowThatIsWritten(t *testing.T) {
	names := []string{"", "delete_pod", "BearerTokenValidator", "x Bearer:", "id_token=", "xoxb-1234-abcd", "a\u200bb", "a\nb", " a", "a  b",
		strings.Repeat("n", 300), strings.Repeat("n", 301), strings.Repeat("あ", 100), strings.Repeat("あ", 101), "͸", strings.Repeat("͸", 51)}
	rng := rand.New(rand.NewSource(7))
	alphabet := []rune("aZ_-=: \tBearer\u200b\n字é\U0001F600\\\"")
	for range 300 {
		b := make([]rune, rng.Intn(40))
		for i := range b {
			b[i] = alphabet[rng.Intn(len(alphabet))]
		}
		names = append(names, string(b))
	}
	for _, name := range names {
		written := toolName(name)
		whole := written == strconv.Quote(name)
		if got := ToolNameFit(name); (got == NameWhole) != whole {
			t.Errorf("ToolNameFit(%.40q) = %d, and the row writes %.60q", name, got, written)
		}
	}
}
