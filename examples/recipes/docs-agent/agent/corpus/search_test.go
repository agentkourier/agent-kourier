package corpus

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func search(t *testing.T, ix *Index, query string, limit int) []Hit {
	t.Helper()
	hits, err := ix.Search(context.Background(), query, limit)
	if err != nil {
		t.Fatalf("Search(%q): %v", query, err)
	}
	return hits
}

func paths(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Path
	}
	return out
}

func TestSearchRanking(t *testing.T) {
	ix := loadFixture(t)
	tests := []struct {
		name, query string
		first       string // path of the best hit
		heading     string // heading of its best section; empty if the case does not pin it
		anchor      string
	}{
		{"a title match beats a body mention", "connect an agent", "how-to/connect-agent.md", "Connect a generic agent", "connect-a-generic-agent"},
		{"an identifier", "maxRunsPerDay", "reference/limits.md", "Limits reference", "limits-reference"},
		{"an identifier split in words", "max runs per day", "reference/limits.md", "Limits reference", "limits-reference"},
		{"a heading match", "credentials rejected", "reference/errors.md", "Credentials rejected", "credentials-rejected"},
		{"a rare word in a body", "storm", "how-to/write-binding.md", "Set limits", "set-limits"},
		{"a front matter title", "threads and sessions", "explanation/threads.md", "", ""},
		{"upper case is ignored", "CREDENTIALS REJECTED", "reference/errors.md", "Credentials rejected", "credentials-rejected"},
		{"plurals and endings are folded", "bindings", "how-to/write-binding.md", "Write a binding", "write-a-binding"},
		{"a word of a fenced block lands in its real section", "looks like a heading", "how-to/connect-agent.md", "Declare the backend", "declare-the-backend"},
		{"punctuation is ignored", "'busy'?! agent... retry", "reference/errors.md", "Agent busy", "agent-busy"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits := search(t, ix, tt.query, 5)
			if len(hits) == 0 {
				t.Fatalf("no hit for %q", tt.query)
			}
			h := hits[0]
			// An empty heading means the case pins the page only: its best section is the text before the first heading.
			if h.Path != tt.first || (tt.heading != "" && (h.Heading != tt.heading || h.Anchor != tt.anchor)) {
				t.Errorf("best hit = %s / %q / %s, want %s / %q / %s\nall: %+v", h.Path, h.Heading, h.Anchor, tt.first, tt.heading, tt.anchor, hits)
			}
			if h.Title == "" || h.Excerpt == "" || h.Score <= 0 {
				t.Errorf("hit lacks a title, an excerpt or a score: %+v", h)
			}
		})
	}
}

func TestSearchOneHitPerPage(t *testing.T) {
	ix := loadFixture(t)
	hits := search(t, ix, "agent", 10)
	seen := map[string]bool{}
	for _, h := range hits {
		if seen[h.Path] {
			t.Errorf("page %s appears twice in %v", h.Path, paths(hits))
		}
		seen[h.Path] = true
	}
	for i := 1; i < len(hits); i++ {
		if hits[i].Score > hits[i-1].Score {
			t.Errorf("hits are not in descending score order: %v", hits)
		}
	}
}

func TestSearchNoHit(t *testing.T) {
	ix := loadFixture(t)
	for _, q := range []string{
		"quantum entanglement",
		"hidden",           // only the page in a hidden directory has it
		"markdown indexed", // only the text file has it
	} {
		if hits := search(t, ix, q, 5); len(hits) != 0 {
			t.Errorf("Search(%q) = %v, want nothing", q, paths(hits))
		}
	}
}

func TestSearchQueryErrors(t *testing.T) {
	ix := loadFixture(t)
	for _, q := range []string{"", "   ", "the of and to", "!!! ???", "a"} {
		if _, err := ix.Search(context.Background(), q, 5); !errors.Is(err, ErrEmptyQuery) {
			t.Errorf("Search(%q) error = %v, want ErrEmptyQuery", q, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ix.Search(ctx, "agent", 5); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: error = %v", err)
	}
}

func TestSearchLimit(t *testing.T) {
	ix := loadFixture(t)
	tests := []struct {
		limit   int
		wantLen int
	}{
		{1, 1}, {2, 2}, {0, DefaultLimit}, {-3, DefaultLimit}, {100, MaxLimit},
	}
	for _, tt := range tests {
		// "agent" and "limit" and "thread" together reach every page of the fixture.
		got := len(search(t, ix, "agent limit thread relay error binding session", tt.limit))
		if want := min(tt.wantLen, 6); got != want {
			t.Errorf("limit %d: %d hits, want %d", tt.limit, got, want)
		}
	}
}

func TestSearchIsDeterministic(t *testing.T) {
	first := search(t, loadFixture(t), "agent binding limit", 10)
	for i := 0; i < 20; i++ {
		again := search(t, loadFixture(t), "agent binding limit", 10)
		if !reflect.DeepEqual(first, again) {
			t.Fatalf("run %d differs:\n%+v\n%+v", i, first, again)
		}
	}
}

// TestSearchTiesBreakByPath pins the order of pages that score the same, whatever order they were read in.
func TestSearchTiesBreakByPath(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"c.md", "a.md", "b.md"} {
		write(t, dir, name, "# Same\n\nidentical words about widgets\n")
	}
	ix, err := Load(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := paths(search(t, ix, "widgets", 5)); !reflect.DeepEqual(got, []string{"a.md", "b.md", "c.md"}) {
		t.Errorf("tie order = %v, want path order", got)
	}
}

func TestSearchExcerpt(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("filler words that say nothing at all. ", 40)
	write(t, dir, "long.md", "# Long page\n\n## Needle section\n\n"+long+"\nThe unique zebra sentence sits here.\n"+long+"\n")
	ix, err := Load(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	hits := search(t, ix, "zebra", 5)
	if len(hits) != 1 {
		t.Fatalf("hits = %+v", hits)
	}
	e := hits[0].Excerpt
	if !strings.Contains(e, "zebra") {
		t.Errorf("excerpt lacks the matched word: %q", e)
	}
	if strings.ContainsAny(e, "\n\r\t") || len([]rune(e)) > 320 {
		t.Errorf("excerpt must be one short line, got %d runes: %q", len([]rune(e)), e)
	}
	if !strings.HasPrefix(e, "…") || !strings.HasSuffix(e, "…") {
		t.Errorf("an excerpt cut from the middle of a section says so: %q", e)
	}
	if hits[0].Heading != "Needle section" {
		t.Errorf("Heading = %q", hits[0].Heading)
	}

	// A match in the heading alone shows the start of the section.
	hits = search(t, ix, "needle", 5)
	if len(hits) != 1 || !strings.HasPrefix(hits[0].Excerpt, "filler words") {
		t.Errorf("heading-only match: %+v", hits)
	}
}

func TestSearchLongQuery(t *testing.T) {
	ix := loadFixture(t)
	q := strings.Repeat("agent ", 5000)
	if hits := search(t, ix, q, 5); len(hits) == 0 {
		t.Error("a very long query is cut, not refused")
	}
}

func TestTokens(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"botMessage trigger", []string{"botmessag", "bot", "messag", "trigger"}},
		{"the A2A agent", []string{"a2a", "agent"}},
		{"max_runs.per-day", []string{"max", "run", "per", "day"}},
		{"HTTPServer", []string{"httpserver", "http", "server"}},
		{"triggers triggered triggering", []string{"trigger", "trigger", "trigger"}},
		{"created creating creates create", []string{"creat", "creat", "creat", "creat"}},
		{"status class access", []string{"status", "class", "access"}},
		{"policies", []string{"policy"}},
		{"it's 429", []string{"429"}},
		// Combining marks stay inside the word, and the word is normalised to NFC.
		{"caf\u00e9", []string{"caf\u00e9"}},
		{"cafe\u0301", []string{"caf\u00e9"}},
		{"Cafe\u0301Bar", []string{"caf\u00e9bar", "caf\u00e9", "bar"}},
		{"na\u00efve nai\u0308ve", []string{"na\u00efv", "na\u00efv"}},
		{"\u0939\u093f\u0928\u094d\u0926\u0940 docs", []string{"\u0939\u093f\u0928\u094d\u0926\u0940", "doc"}}, // Devanagari: letters, a vowel sign (Mc) and a virama (Mn)
		{"\u0301 alone", []string{"alon"}},                                                                     // a mark that follows no letter starts no word
	}
	for _, tt := range tests {
		var got []string
		scan(tt.in, func(tok string, _, _ int) { got = append(got, tok) })
		if !reflect.DeepEqual(got, tt.want) {
			t.Errorf("tokens(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSearchClampsToMaxLimit pins that a caller cannot ask for more than MaxLimit pages, however many match.
func TestSearchClampsToMaxLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < MaxLimit+3; i++ {
		write(t, dir, fmt.Sprintf("page-%02d.md", i), "# Page\n\nwidgets everywhere\n")
	}
	ix, err := Load(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(search(t, ix, "widgets", 1000)); got != MaxLimit {
		t.Errorf("Search with a limit of 1000 returned %d pages, want %d", got, MaxLimit)
	}
	if got := len(search(t, ix, "widgets", 0)); got != DefaultLimit {
		t.Errorf("Search with no limit returned %d pages, want %d", got, DefaultLimit)
	}
}

// TestSearchHeadingBeatsBody pins the heading boost: a word once in a heading outranks the same word twice in a body.
// The page with the body mentions sorts first by path, so only the boost can put the other one on top.
func TestSearchHeadingBeatsBody(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "a-body.md", "# Alpha\n\nThe zebra and the zebra again, plus filler words about gadgets and more.\n")
	write(t, dir, "z-heading.md", "# Omega\n\n## Zebra\n\nSome filler words about gadgets and more, with no animal at all.\n")
	ix, err := Load(context.Background(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := paths(search(t, ix, "zebra", 5))
	if !reflect.DeepEqual(got, []string{"z-heading.md", "a-body.md"}) {
		t.Errorf("order = %v, want the heading match first", got)
	}
}

// TestSearchFindsAWordInEitherNormalForm pins that a page and a query meet whether either is written composed (NFC)
// or decomposed (NFD), and that a word with combining marks is searched whole.
func TestSearchFindsAWordInEitherNormalForm(t *testing.T) {
	dir := t.TempDir()
	pages := map[string]string{
		"composed.md":   "# Composed\n\nThe caf\u00e9 relay.\n",
		"decomposed.md": "# Decomposed\n\nThe cafe\u0301 gateway.\n",
		"hindi.md":      "# Hindi\n\nA \u0939\u093f\u0928\u094d\u0926\u0940 guide.\n",
		"plain.md":      "# Plain\n\nThe cafe closes early.\n",
	}
	for name, body := range pages {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := Load(t.Context(), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{"caf\u00e9", "cafe\u0301"} {
		got := paths(search(t, ix, q, 10))
		slices.Sort(got)
		if want := []string{"composed.md", "decomposed.md"}; !slices.Equal(got, want) {
			t.Errorf("Search(%+q) = %v, want %v: both forms of the word, and not \"cafe\"", q, got, want)
		}
	}
	for _, q := range []string{"\u0939\u093f\u0928\u094d\u0926\u0940", "\u0939\u093f"} {
		got := paths(search(t, ix, q, 10))
		want := []string{"hindi.md"}
		if q == "\u0939\u093f" {
			want = nil // the word is not split at its vowel sign
		}
		if !slices.Equal(got, want) {
			t.Errorf("Search(%+q) = %v, want %v", q, got, want)
		}
	}
}
