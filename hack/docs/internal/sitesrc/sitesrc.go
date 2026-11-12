// Package sitesrc says which files are the documentation site's text sources, so that the docs checks
// (hack/docs/namecheck, hack/docs/contentcheck) read the same files: the directories of docs/ that mkdocs.yml's
// exclude_docs leaves out, and the text files the site publishes. The checks walk their own roots; this package
// answers only what they must agree on.
package sitesrc

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// ExcludedDocs returns the directories of docs/ that the exclude_docs block of mkdocs, the text of mkdocs.yml, names.
// It accepts only the form `/name/`: another pattern would make a check read different files than the site publishes,
// so it is an error rather than a guess.
func ExcludedDocs(mkdocs string) ([]string, error) {
	lines := strings.Split(mkdocs, "\n")
	for i, l := range lines {
		if strings.TrimRight(l, " \t") != "exclude_docs: |" {
			continue
		}
		var out []string
		for _, b := range lines[i+1:] {
			t := strings.TrimSpace(b)
			if t == "" || strings.HasPrefix(t, "#") {
				continue
			}
			if !strings.HasPrefix(b, " ") && !strings.HasPrefix(b, "\t") {
				break // the block ends at the next key
			}
			name := strings.TrimSuffix(strings.TrimPrefix(t, "/"), "/")
			if !strings.HasPrefix(t, "/") || !strings.HasSuffix(t, "/") || name == "" || name == "." || name == ".." ||
				strings.ContainsAny(name, "/*?[") {
				return nil, fmt.Errorf("mkdocs.yml exclude_docs pattern %q: only /directory/ is supported", t)
			}
			out = append(out, name)
		}
		return out, nil
	}
	return nil, fmt.Errorf("mkdocs.yml has no `exclude_docs: |` block")
}

// Excluded reports whether the directory rel, a path relative to docs/, is one that excluded (from ExcludedDocs)
// names. mkdocs reads exclude_docs as gitignore patterns, and a leading slash anchors one at docs/, so `/brand/`
// excludes docs/brand and not docs/assets/brand: only a top-level directory matches. docs/ itself, rel ".", is never
// excluded.
func Excluded(excluded []string, rel string) bool {
	return rel != "." && filepath.IsLocal(rel) && !strings.ContainsRune(filepath.ToSlash(rel), '/') && slices.Contains(excluded, rel)
}

// textExt are the extensions of the text files the site publishes; images and video are skipped.
var textExt = []string{".md", ".html", ".yml", ".yaml", ".css", ".js", ".json", ".svg", ".vtt", ".webmanifest", ".txt"}

// textNames are published text files with no extension.
var textNames = []string{"_headers"}

// IsText reports whether the file at path is a text file the site publishes: Markdown, HTML, YAML, CSS, JavaScript,
// JSON, SVG, WebVTT, the web manifest, plain text or the Cloudflare _headers file.
func IsText(path string) bool {
	return slices.Contains(textExt, filepath.Ext(path)) || slices.Contains(textNames, filepath.Base(path))
}
