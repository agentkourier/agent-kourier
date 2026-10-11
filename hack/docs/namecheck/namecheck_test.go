package main

import (
	"os"
	"path/filepath"
	"slices"
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

func TestCheckSkipsExcludedDirectories(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, text string) {
		t.Helper()
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/index.md", "Agent Kourier\n")
	write("docs/brand/notes.md", "courier\n")
	write("docs/spikes/old.md", "courier\n")
	write("docs/guide.md", "Kourier alone\n")
	write("docs/assets/film/x.mp4", "courier")
	got, err := check([]string{filepath.Join(dir, "docs")}, []string{"brand", "spikes"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || filepath.Base(got[0].path) != "guide.md" {
		t.Errorf("check = %v, want one finding, in docs/guide.md", got)
	}
}

// TestSiteSpellsTheName runs the check over the real site sources, so `go test ./...` fails on a misspelt name.
func TestSiteSpellsTheName(t *testing.T) {
	got, err := check(sitePaths("../../.."), excluded)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Errorf("%s", f)
	}
}
