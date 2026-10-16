package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestFindings(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []int // the lines of the findings
	}{
		{"the full name", "Agent Kourier routes alerts.\n", nil},
		{"the name across a line break", "On this dialect Agent\nKourier does not see a tool call.\n", nil},
		{"identifiers", "Install agent-kourier, the chart agent-kourier, the group agentkourier.dev.\n", nil},
		{"the second word alone", "Then Kourier posts.\n", []int{1}},
		{"the second word at a line start", "The broker.\nKourier posts.\n", []int{2}},
		{"the knative project in passing", "net-kourier is Knative's.\n", []int{1}},
		{"courier for the product", "fine\nthe courier bot\n", []int{2}},
		{"Courier capitalised", "Courier answers.\n", []int{1}},
		{"the database file keeps its old name", "persistence.dbFile defaults to `courier.db`.\n", nil},
		{"the purge function of the release before the rename", "`courier_audit_purge` of 0006 stays.\n", nil},
		{"a recipe's directory in a title", "```yaml title=\"kourier/binding.yaml\"\n", nil},
		{"a recipe's directory after another", "--8<-- \"sre-triage/kourier/binding.yaml\"\n", nil},
		{"the directory in prose", "Put the files in `kourier/` and the agent in agent/.\nThe kourier/ directory holds them.\n(kourier/binding.yaml)\nkourier/binding.yaml\n", nil},
		{"a path segment must be lowercase", "See Kourier/binding.yaml.\n", []int{1}},
		{"net-kourier is not a path segment", "net-kourier/envoy runs.\n", []int{1}},
		{"a longer word is not a path segment", "the xkourier/ directory\n", []int{1}},
		{"the word without a slash", "the kourier directory\n", []int{1}},
		{"a path segment does not excuse a misspelling on the line", "kourier/binding.yaml and then Kourier alone\n", []int{1}},
		{"a path segment does not excuse courier", "kourier/courier.yaml\n", []int{1}},
		{"a historical block", "a\n<!-- name-check: off -->\nhelm install courier ./chart\n<!-- name-check: on -->\ncourier\n", []int{5}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []int
			for _, f := range findings(tt.text) {
				got = append(got, f.line)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("findings(%q) at lines %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

// writeTree returns a function that writes text to the file rel under dir, making the directories.
func writeTree(t *testing.T, dir string) func(rel, text string) {
	return func(rel, text string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCheckSkipsExcludedDirectories(t *testing.T) {
	dir := t.TempDir()
	write := writeTree(t, dir)
	write("docs/index.md", "Agent Kourier\n")
	write("docs/brand/notes.md", "courier\n")
	write("docs/spikes/old.md", "courier\n")
	write("docs/guide.md", "Kourier alone\n")
	write("docs/assets/film/x.mp4", "courier")
	got, err := check(t.Context(), dir, []string{filepath.Join(dir, "docs")}, []string{"brand", "spikes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0].path) != "guide.md" {
		t.Errorf("check = %v, want one finding, in docs/guide.md", got)
	}
}

// snippetsSite writes a site whose mkdocs.yml turns the snippets extension on with the given base_path lines, and
// returns its root and the function that writes files under it.
func snippetsSite(t *testing.T, basePath string) (string, func(rel, text string)) {
	t.Helper()
	root := t.TempDir()
	write := writeTree(t, root)
	write("mkdocs.yml", "markdown_extensions:\n  - abbr\n  - pymdownx.snippets:\n"+basePath+"      check_paths: true\n  - pymdownx.emoji:\n      emoji_index: !!python/name:material.extensions.emoji.twemoji\n")
	return root, write
}

func checkSite(t *testing.T, root string) []finding {
	t.Helper()
	got, err := check(t.Context(), root, []string{filepath.Join(root, "docs")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// wantFindings requires exactly the findings given, each as "path:line" (relative to root) and a substring of its text.
func wantFindings(t *testing.T, root string, got []finding, want ...string) {
	t.Helper()
	var have []string
	for _, f := range got {
		rel, err := filepath.Rel(root, f.path)
		if err != nil {
			t.Fatal(err)
		}
		have = append(have, filepath.ToSlash(rel)+":"+strconv.Itoa(f.line)+" "+f.String())
	}
	if len(have) != len(want) {
		t.Fatalf("findings:\n%s\nwant %d: %q", strings.Join(have, "\n"), len(want), want)
	}
	for i, w := range want {
		if !strings.Contains(have[i], w) {
			t.Errorf("finding %d is %q, want it to contain %q", i, have[i], w)
		}
	}
}

// A page that includes a recipe's file is checked with the file: the directory name kourier/ passes, with no
// name-check escape on the page, and a real misspelling in the file is found, at the file's own line.
func TestCheckReadsIncludedFiles(t *testing.T) {
	root, write := snippetsSite(t, "      base_path: [examples/recipes]\n")
	write("docs/how-to/recipes.md", "# Recipes\n\n```yaml title=\"kourier/binding.yaml\"\n--8<-- \"ok/kourier/binding.yaml\"\n```\n\n```yaml\n--8<-- \"bad/kourier/binding.yaml:section\"\n```\n")
	write("examples/recipes/ok/kourier/binding.yaml", "# the Agent Kourier Binding\nkind: Binding\n")
	write("examples/recipes/bad/kourier/binding.yaml", "kind: Binding\n# Kourier posts it\n")
	write("examples/recipes/unincluded/kourier/binding.yaml", "# courier is not on any page\n")
	got := checkSite(t, root)
	wantFindings(t, root, got, "examples/recipes/bad/kourier/binding.yaml:2 ")
	if !strings.Contains(got[0].String(), "Kourier posts it") {
		t.Errorf("finding %q does not show the line", got[0])
	}
}

func TestCheckReadsAFileOnceAndHonoursItsEscapes(t *testing.T) {
	root, write := snippetsSite(t, "      base_path: examples/recipes\n")
	write("docs/a.md", "--8<-- \"r/f.yaml\"\n")
	write("docs/b.md", "--8<--\nr/f.yaml\n\"r/g.yaml\"\n;r/h.yaml\n--8<--\n")
	write("examples/recipes/r/f.yaml", "courier\n")
	write("examples/recipes/r/g.yaml", "<!-- name-check: off -->\ncourier\n<!-- name-check: on -->\n")
	write("examples/recipes/r/h.yaml", "courier\n") // ";" disables the line
	wantFindings(t, root, checkSite(t, root), "examples/recipes/r/f.yaml:1 ")
}

func TestCheckReportsAMissingIncludedFile(t *testing.T) {
	root, write := snippetsSite(t, "      base_path: [examples/recipes]\n")
	write("docs/page.md", "text\n--8<-- \"r/gone.yaml\"\n--8<-- \"../../outside.yaml\"\n--8<-- \"https://example.com/x.yaml\"\n;--8<-- \"r/disabled.yaml\"\n")
	write("outside.yaml", "fine\n") // exists, but not under a base_path
	got := checkSite(t, root)
	wantFindings(t, root, got, "docs/page.md:2 ", "docs/page.md:3 ")
	for _, f := range got {
		if !strings.Contains(f.String(), "not found under the snippets base_path") || strings.Contains(f.String(), "Agent Kourier") {
			t.Errorf("finding %q should say the file is missing and not blame the name", f)
		}
	}
}

// A path that merely starts with "http" is a file, and its ":section" is cut off like any other.
func TestCheckIncludesAPathStartingWithHTTP(t *testing.T) {
	root, write := snippetsSite(t, "      base_path: [examples/recipes]\n")
	write("docs/page.md", "--8<-- \"httpd/x.yaml:section\"\n")
	write("examples/recipes/httpd/x.yaml", "courier\n")
	wantFindings(t, root, checkSite(t, root), "examples/recipes/httpd/x.yaml:1 ")
}

func TestCheckLooksInEveryBasePathInOrder(t *testing.T) {
	root, write := snippetsSite(t, "      base_path: [first, second]\n")
	write("docs/page.md", "--8<-- \"both.yaml\"\n--8<-- \"only-second.yaml\"\n")
	write("first/both.yaml", "fine\n")
	write("second/both.yaml", "courier\n") // shadowed by the first
	write("second/only-second.yaml", "Kourier\n")
	wantFindings(t, root, checkSite(t, root), "second/only-second.yaml:1 ")
}

// Includes are the extension's business: without it in mkdocs.yml, or in a file that is not Markdown, the marker is text.
func TestCheckIgnoresIncludesWithoutTheExtension(t *testing.T) {
	root := t.TempDir()
	write := writeTree(t, root)
	write("mkdocs.yml", "markdown_extensions:\n  - abbr\n")
	write("docs/page.md", "--8<-- \"r/gone.yaml\"\n")
	wantFindings(t, root, checkSite(t, root))

	root, write = snippetsSite(t, "      base_path: [examples/recipes]\n")
	write("docs/notes.txt", "--8<-- \"r/gone.yaml\"\n")
	wantFindings(t, root, checkSite(t, root))
}

func TestSnippetBases(t *testing.T) {
	tests := []struct {
		name, mkdocs string
		want         []string // relative to the root
		wantErr      bool
	}{
		{"none", "site_name: x\n", nil, false},
		{"a list", "markdown_extensions:\n  - pymdownx.snippets:\n      base_path: [a, b/c]\n", []string{"a", "b/c"}, false},
		{"a string", "markdown_extensions:\n  - pymdownx.snippets:\n      base_path: a\n", []string{"a"}, false},
		{"no base_path", "markdown_extensions:\n  - pymdownx.snippets:\n      check_paths: true\n", []string{"."}, false},
		{"the extension bare", "markdown_extensions:\n  - pymdownx.snippets\n", []string{"."}, false},
		{"a base_path of the wrong type", "markdown_extensions:\n  - pymdownx.snippets:\n      base_path: 3\n", nil, true},
		{"not YAML", "markdown_extensions: [\n", nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			writeTree(t, root)("mkdocs.yml", tt.mkdocs)
			got, err := snippetBases(t.Context(), root)
			if (err != nil) != tt.wantErr {
				t.Fatalf("snippetBases error = %v, want error: %v", err, tt.wantErr)
			}
			var want []string
			for _, w := range tt.want {
				want = append(want, filepath.Join(root, w))
			}
			if !slices.Equal(got, want) {
				t.Errorf("snippetBases = %v, want %v", got, want)
			}
		})
	}
}

// TestSiteSpellsTheName runs the check over the real site sources, so `go test ./...` fails on a misspelt name.
func TestSiteSpellsTheName(t *testing.T) {
	got, err := check(t.Context(), "../../..", sitePaths("../../.."), excluded)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Errorf("%s", f)
	}
}

// TestCheckStopsWhenTheContextEnds pins that the walk honours its context, so an interrupted run does not read on.
func TestCheckStopsWhenTheContextEnds(t *testing.T) {
	dir := t.TempDir()
	writeTree(t, dir)("docs/index.md", "Agent Kourier\n")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := check(ctx, dir, []string{filepath.Join(dir, "docs")}, nil); !errors.Is(err, context.Canceled) {
		t.Errorf("check with a cancelled context = %v, want context.Canceled", err)
	}
	if _, err := snippetBases(ctx, dir); !errors.Is(err, context.Canceled) {
		t.Errorf("snippetBases with a cancelled context = %v, want context.Canceled", err)
	}
	if _, ok, err := resolveInclude(ctx, []string{dir}, "docs/index.md"); ok || !errors.Is(err, context.Canceled) {
		t.Errorf("resolveInclude with a cancelled context = %v, %v; want not found and context.Canceled", ok, err)
	}
}
