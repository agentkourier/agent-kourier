// Command namecheck fails when the documentation site misspells the product's name. The name is always "Agent
// Kourier": never "Kourier" alone (Knative's net-kourier) and never "courier" (courier.com, and the project's old
// name). The identifiers agent-kourier and agentkourier are fine. The exceptions: the SQLite file courier.db, which kept
// its name across the rename, the Postgres function courier_audit_purge, which stays for the release before it, and
// lines between <!-- name-check: off --> and <!-- name-check: on -->, for a
// historical note such as an upgrade from the old chart name.
//
// It reads the site's sources: docs/ (without the brand kit and the spike notes, which the site does not publish),
// mkdocs.yml and the theme overrides. Run it from the repository root, through `make docs-check`.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// excluded are the directories under docs/ that the site does not publish (mkdocs.yml, exclude_docs).
var excluded = []string{"brand", "spikes"}

// sitePaths are the site's sources under the repository root.
func sitePaths(root string) []string {
	return []string{filepath.Join(root, "docs"), filepath.Join(root, "mkdocs.yml"), filepath.Join(root, "hack/docs/overrides")}
}

// textExt are the files read; the rest (images, video) are skipped.
var textExt = []string{".md", ".html", ".yml", ".yaml", ".css", ".js", ".vtt", ".webmanifest", ".txt"}

var (
	// kourier and courier match in any case, as a word part.
	kourier = regexp.MustCompile(`(?i)kourier`)
	courier = regexp.MustCompile(`(?i)courier`)
	// fullName is what must come before kourier: "Agent " or "Agent" and a line break in prose, agent-, agentkourier or AGENTKOURIER_ in identifiers.
	fullName = regexp.MustCompile(`(?i)agent[ \n_-]?$`)
	allowed  = []string{"courier.db", "courier_audit_purge"}
)

type finding struct {
	path string
	line int
	text string
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s: write \"Agent Kourier\", never \"Kourier\" alone or \"courier\"", f.path, f.line, strings.TrimSpace(f.text))
}

// findings returns the misspellings in text, by line.
func findings(text string) []finding {
	var out []finding
	lines := strings.Split(text, "\n")
	off := false
	offsets := make([]int, len(lines)) // where each line starts in text
	for i := 1; i < len(lines); i++ {
		offsets[i] = offsets[i-1] + len(lines[i-1]) + 1
	}
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case "<!-- name-check: off -->":
			off = true
			continue
		case "<!-- name-check: on -->":
			off = false
			continue
		}
		if off {
			continue
		}
		bad := false
		for _, m := range kourier.FindAllStringIndex(line, -1) {
			if start := offsets[i] + m[0]; !fullName.MatchString(text[max(0, start-6):start]) {
				bad = true
			}
		}
		masked := line
		for _, a := range allowed {
			masked = strings.ReplaceAll(masked, a, "")
		}
		if courier.MatchString(masked) {
			bad = true
		}
		if bad {
			out = append(out, finding{line: i + 1, text: line})
		}
	}
	return out
}

// check reads every text file under paths, skipping the directories named in skip, and returns the findings.
func check(paths []string, skip []string) ([]finding, error) {
	var out []finding
	for _, p := range paths {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			continue // a source the site does not have yet, such as the overrides
		}
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if path != p && slices.Contains(skip, d.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			if !slices.Contains(textExt, filepath.Ext(path)) {
				return nil
			}
			b, err := os.ReadFile(path) //nolint:gosec // the site's own sources
			if err != nil {
				return fmt.Errorf("read %s: %w", path, err)
			}
			for _, f := range findings(string(b)) {
				f.path = path
				out = append(out, f)
			}
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", p, err)
		}
	}
	return out, nil
}

func main() {
	got, err := check(sitePaths("."), excluded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "namecheck:", err)
		os.Exit(2)
	}
	for _, f := range got {
		fmt.Fprintln(os.Stderr, f)
	}
	if len(got) > 0 {
		os.Exit(1)
	}
}
