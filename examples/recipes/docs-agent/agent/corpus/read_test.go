package corpus

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestReadWholePage(t *testing.T) {
	ix := loadFixture(t)
	p, err := ix.Read(context.Background(), "how-to/connect-agent.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Path != "how-to/connect-agent.md" || p.Title != "Connect a generic agent" || p.Section != "" {
		t.Errorf("Page = %+v", p)
	}
	for _, want := range []string{"# Connect a generic agent", "## Declare the backend", "dialect: a2a", "## Check it"} {
		if !strings.Contains(p.Text, want) {
			t.Errorf("Text lacks %q", want)
		}
	}
	wantOutline := []Heading{
		{"Connect a generic agent", "connect-a-generic-agent", 1},
		{"Before you start", "before-you-start", 2},
		{"Declare the backend", "declare-the-backend", 2},
		{"Static headers", "static-headers", 3},
		{"Check it", "check-it", 2},
	}
	if !reflect.DeepEqual(p.Outline, wantOutline) {
		t.Errorf("Outline = %+v, want %+v", p.Outline, wantOutline)
	}
}

func TestReadFrontMatterIsNotText(t *testing.T) {
	ix := loadFixture(t)
	p, err := ix.Read(context.Background(), "explanation/threads.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if p.Title != "Threads and sessions" {
		t.Errorf("Title = %q, want the front matter title", p.Title)
	}
	if strings.Contains(p.Text, "tags:") || strings.Contains(p.Text, "---") {
		t.Errorf("Text holds the front matter: %q", p.Text)
	}
}

func TestReadSection(t *testing.T) {
	ix := loadFixture(t)
	tests := []struct {
		name, section string
		wantAnchor    string
		has, lacks    []string
	}{
		{"by heading", "Declare the backend", "declare-the-backend",
			[]string{"## Declare the backend", "dialect: a2a", "### Static headers", "identity are refused"},
			[]string{"## Check it", "Before you start"}},
		{"by anchor", "static-headers", "static-headers",
			[]string{"### Static headers", "identity are refused"},
			[]string{"dialect: a2a", "## Check it"}},
		{"case and spacing are ignored", "  STATIC HEADERS ", "static-headers",
			[]string{"### Static headers"}, []string{"## Check it"}},
		{"the last section runs to the end", "Check it", "check-it",
			[]string{"## Check it", "turn started"}, []string{"Declare"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, err := ix.Read(context.Background(), "how-to/connect-agent.md", tt.section)
			if err != nil {
				t.Fatal(err)
			}
			if p.Anchor != tt.wantAnchor {
				t.Errorf("Anchor = %q, want %q", p.Anchor, tt.wantAnchor)
			}
			for _, s := range tt.has {
				if !strings.Contains(p.Text, s) {
					t.Errorf("Text lacks %q:\n%s", s, p.Text)
				}
			}
			for _, s := range tt.lacks {
				if strings.Contains(p.Text, s) {
					t.Errorf("Text holds %q:\n%s", s, p.Text)
				}
			}
			if len(p.Outline) != 5 {
				t.Errorf("a section read still lists the page's outline, got %d headings", len(p.Outline))
			}
		})
	}
}

func TestReadDuplicateHeadings(t *testing.T) {
	ix := loadFixture(t)
	first, err := ix.Read(context.Background(), "reference/limits.md", "Example")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first.Text, "first example") || first.Anchor != "example" {
		t.Errorf("by heading, the first one: anchor %q, text %q", first.Anchor, first.Text)
	}
	second, err := ix.Read(context.Background(), "reference/limits.md", "example_1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.Text, "second example") || strings.Contains(second.Text, "first example") {
		t.Errorf("by anchor, the second one: %q", second.Text)
	}
}

func TestReadErrors(t *testing.T) {
	ix := loadFixture(t)
	_, err := ix.Read(context.Background(), "how-to/connect-agent.md", "no such section")
	if !errors.Is(err, ErrSectionNotFound) {
		t.Errorf("unknown section: error = %v, want ErrSectionNotFound", err)
	}
	if err != nil && !strings.Contains(err.Error(), "declare-the-backend") {
		t.Errorf("the error should list the page's anchors so the caller can recover: %v", err)
	}
	if _, err := ix.Read(context.Background(), "how-to/missing.md", ""); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing page: error = %v, want ErrNotFound", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ix.Read(ctx, "index.md", ""); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled context: error = %v", err)
	}
}

func TestReadTruncates(t *testing.T) {
	dir := t.TempDir()
	var long strings.Builder
	long.WriteString("# Long\n\n")
	for i := 0; i < 200; i++ {
		long.WriteString("a line of text that is long enough to add up\n")
	}
	write(t, dir, "long.md", long.String())
	ix, err := Load(context.Background(), dir, Options{MaxReadBytes: 500})
	if err != nil {
		t.Fatal(err)
	}
	p, err := ix.Read(context.Background(), "long.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if !p.Truncated || len(p.Text) > 500 || !strings.HasSuffix(p.Text, "of text that is long enough to add up") {
		t.Errorf("Truncated = %v, len = %d, tail = %q: want the text cut to 500 bytes at a line end", p.Truncated, len(p.Text), tail(p.Text))
	}
	short, err := loadFixture(t).Read(context.Background(), "index.md", "")
	if err != nil || short.Truncated {
		t.Errorf("a short page: Truncated = %v, err = %v", short.Truncated, err)
	}
}

func tail(s string) string {
	if len(s) > 40 {
		return s[len(s)-40:]
	}
	return s
}

// TestReadRefusesPathsOutsideTheCorpus is the traversal table. Next to the corpus sits a file that a path must
// never reach; every form of reaching for it, or for any file at all outside the corpus, is refused as an invalid
// path before anything is looked up.
func TestReadRefusesPathsOutsideTheCorpus(t *testing.T) {
	parent := t.TempDir()
	write(t, parent, "secret.md", "# Secret\nthe file next to the corpus\n")
	write(t, parent, "docs/inside.md", "# Inside\ninside text\n")
	ix, err := Load(context.Background(), filepath.Join(parent, "docs"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	invalid := []string{
		"../secret.md",
		"../../etc/passwd",
		"..",
		"../",
		"/etc/passwd",
		"/inside.md",
		"sub/../../secret.md",
		"sub/../inside.md", // not the clean form of a path
		"./inside.md",
		"inside.md/",
		"//inside.md",
		`..\secret.md`,
		`sub\..\secret.md`,
		`sub\inside.md`,
		"inside.md\x00.png",
		"inside.md\n",
		"\x00",
		"",
		".",
		"inside.txt",
		"inside",
		".hidden.md",
		"sub/.hidden.md",
		"file:///etc/passwd",
		"C:/Windows/win.ini",
		strings.Repeat("a/", 300) + "x.md",
	}
	for _, p := range invalid {
		t.Run(printable(p), func(t *testing.T) {
			got, err := ix.Read(context.Background(), p, "")
			if !errors.Is(err, ErrInvalidPath) {
				t.Fatalf("Read(%q) = %+v, %v; want ErrInvalidPath", p, got, err)
			}
			if strings.Contains(got.Text, "next to the corpus") {
				t.Fatalf("Read(%q) returned the outside file", p)
			}
			if strings.Contains(err.Error(), parent) {
				t.Errorf("the error names a directory on disk: %v", err)
			}
		})
	}
	// Paths that are well formed but name nothing are not found, never read from disk.
	for _, p := range []string{"secret.md", "~/inside.md", "%2e%2e/secret.md", "docs/inside.md", "sub/inside.md"} {
		if _, err := ix.Read(context.Background(), p, ""); !errors.Is(err, ErrNotFound) {
			t.Errorf("Read(%q) error = %v, want ErrNotFound", p, err)
		}
	}
	if got, err := ix.Read(context.Background(), "inside.md", ""); err != nil || !strings.Contains(got.Text, "inside text") {
		t.Errorf("a valid path is still served: %+v, %v", got, err)
	}
}

func printable(s string) string {
	if s == "" {
		return "empty"
	}
	return strings.NewReplacer("\x00", "NUL", "\n", "LF", "/", "|", `\`, "BS").Replace(s)
}

// TestReadNeverTouchesTheDiskAfterLoad pins that a request is answered from the loaded index: a file swapped for a
// symlink to an outside file after Load changes nothing a caller can read.
func TestReadNeverTouchesTheDiskAfterLoad(t *testing.T) {
	parent := t.TempDir()
	write(t, parent, "secret.md", "# Secret\nthe file next to the corpus\n")
	write(t, parent, "docs/page.md", "# Page\noriginal text\n")
	ix, err := Load(context.Background(), filepath.Join(parent, "docs"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	page := filepath.Join(parent, "docs", "page.md")
	if err := os.Remove(page); err != nil {
		t.Fatal(err)
	}
	mustSymlink(t, filepath.Join(parent, "secret.md"), page)
	got, err := ix.Read(context.Background(), "page.md", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Text, "original text") || strings.Contains(got.Text, "next to the corpus") {
		t.Errorf("Text = %q, want the text loaded at start", got.Text)
	}
}
