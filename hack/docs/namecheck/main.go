// Command namecheck fails when the documentation site misspells the product's name. The name is always "Agent
// Kourier": never "Kourier" alone (Knative's net-kourier) and never "courier" (courier.com, and the project's old
// name). The identifiers agent-kourier and agentkourier are fine. The exceptions: the SQLite file courier.db, which an
// install from before the rename keeps, the Postgres function courier_audit_purge, which migration 0024 dropped, a
// lowercase "kourier/" that starts a path segment, as in a recipe's kourier/binding.yaml, which is a directory name,
// and lines between <!-- name-check: off --> and <!-- name-check: on -->, for a historical note such as an upgrade
// from the old chart name.
//
// It reads the site's sources: docs/ (without the brand kit and the spike notes, which the site does not publish),
// mkdocs.yml and the theme overrides. It also reads every file that a Markdown page of docs/ includes with the
// snippets extension (--8<-- "path"), found under the extension's base_path in mkdocs.yml and read whole even when
// the page includes one section of it, and it reports an included file it cannot find. Run it from the repository
// root, through `make docs-check`.
package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"sigs.k8s.io/yaml"
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
	// repos are repository paths whose name has kourier without "agent" before it; a link to one is not prose.
	repos = []string{"agentkourier/kourier-incident-demo"}
)

type finding struct {
	path string
	line int
	text string
	msg  string // what is wrong, when it is not a misspelt name
}

func (f finding) String() string {
	if f.msg != "" {
		return fmt.Sprintf("%s:%d: %s: %s", f.path, f.line, strings.TrimSpace(f.text), f.msg)
	}
	return fmt.Sprintf("%s:%d: %s: write \"Agent Kourier\", never \"Kourier\" alone or \"courier\"", f.path, f.line, strings.TrimSpace(f.text))
}

// pathSegment reports whether the match of kourier at line[start:end] is the directory name "kourier/" that starts a
// path segment: lowercase, followed by a slash, and at the start of the line or after a space, quote, backtick,
// bracket or slash. "net-kourier/" and "Kourier/" are not.
func pathSegment(line string, start, end int) bool {
	if line[start:end] != "kourier" || !strings.HasPrefix(line[end:], "/") {
		return false
	}
	return start == 0 || strings.ContainsRune(" \t\"'`(</", rune(line[start-1]))
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
		unrepo := line // the line with each repository path blanked, so its kourier is not a match
		for _, r := range repos {
			unrepo = strings.ReplaceAll(unrepo, r, strings.Repeat(" ", len(r)))
		}
		for _, m := range kourier.FindAllStringIndex(unrepo, -1) {
			if start := offsets[i] + m[0]; !fullName.MatchString(text[max(0, start-6):start]) && !pathSegment(line, m[0], m[1]) {
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

// check reads every text file under paths until ctx ends, skipping the directories named in skip, and returns the findings. It
// reads too the files that the Markdown files among them include with the snippets extension, resolved against the
// extension's base_path in root/mkdocs.yml, and reports each include it cannot resolve.
func check(ctx context.Context, root string, paths []string, skip []string) ([]finding, error) {
	bases, err := snippetBases(ctx, root)
	if err != nil {
		return nil, err
	}
	var out []finding
	included := map[string]bool{} // files read as includes, each once
	scan := func(path string, b []byte, markdown bool) error {
		for _, f := range findings(string(b)) {
			f.path = path
			out = append(out, f)
		}
		if !markdown || bases == nil { // without the extension a marker is only text
			return nil
		}
		for _, inc := range includes(string(b)) {
			file, ok, err := resolveInclude(ctx, bases, inc.path)
			if err != nil {
				return err
			}
			if !ok {
				out = append(out, finding{path: path, line: inc.line, text: inc.text,
					msg: fmt.Sprintf("included file %q not found under the snippets base_path %q", inc.path, bases)})
				continue
			}
			if included[file] {
				continue
			}
			included[file] = true
			data, err := os.ReadFile(file) //nolint:gosec // a file the site includes
			if err != nil {
				out = append(out, finding{path: path, line: inc.line, text: inc.text, msg: fmt.Sprintf("read included file: %v", err)})
				continue
			}
			for _, f := range findings(string(data)) {
				f.path = file
				out = append(out, f)
			}
		}
		return nil
	}
	for _, p := range paths {
		if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
			continue // a source the site does not have yet, such as the overrides
		}
		err := filepath.WalkDir(p, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
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
			return scan(path, b, filepath.Ext(path) == ".md")
		})
		if err != nil {
			return nil, fmt.Errorf("walk %s: %w", p, err)
		}
	}
	return out, nil
}

// snippetBases returns the directories the snippets extension looks in, from root/mkdocs.yml: its base_path
// (a string or a list, relative to root), or the root itself when the extension is on without one. With no
// mkdocs.yml, or the extension off, the result is nil.
func snippetBases(ctx context.Context, root string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(root, "mkdocs.yml")) //nolint:gosec // the site's own config
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("read mkdocs.yml: %w", err)
	}
	var cfg struct {
		Extensions []any `json:"markdown_extensions"`
	}
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse mkdocs.yml: %w", err)
	}
	const name = "pymdownx.snippets"
	for _, ext := range cfg.Extensions {
		var basePath any = "." // the extension's default
		switch v := ext.(type) {
		case string:
			if v != name {
				continue
			}
		case map[string]any:
			conf, ok := v[name]
			if !ok {
				continue
			}
			if m, _ := conf.(map[string]any); m["base_path"] != nil {
				basePath = m["base_path"]
			}
		default:
			continue
		}
		bases := []string{} // not nil: the extension is on
		switch bp := basePath.(type) {
		case string:
			bases = []string{bp}
		case []any:
			for _, e := range bp {
				s, ok := e.(string)
				if !ok {
					return nil, fmt.Errorf("mkdocs.yml: %s base_path holds %v, want a path", name, e)
				}
				bases = append(bases, s)
			}
		default:
			return nil, fmt.Errorf("mkdocs.yml: %s base_path is %v, want a path or a list of paths", name, bp)
		}
		for i, b := range bases {
			bases[i] = filepath.Join(root, b)
		}
		return bases, nil
	}
	return nil, nil
}

// include is one file a page includes with the snippets extension.
type include struct {
	path string // as written, without the ":section" or ":lines" suffix
	line int
	text string
}

var (
	// inlineInclude is --8<-- "path": the marker, then the path in quotes. A leading ";" disables it.
	inlineInclude = regexp.MustCompile(`^\s*-+8<-+\s+(?:"([^"]+)"|'([^']+)')\s*$`)
	// blockMarker opens and closes a block of paths, one to a line.
	blockMarker = regexp.MustCompile(`^\s*-+8<-+\s*$`)
)

// includes returns the files the Markdown text includes, in order: the inline form and the lines of a block.
// Addresses of other hosts are left out; the extension fetches them itself.
func includes(text string) []include {
	var out []include
	inBlock := false
	add := func(i int, line, p string) {
		p = strings.TrimSpace(p)
		if strings.HasPrefix(p, "http:") || strings.HasPrefix(p, "https:") {
			return
		}
		if before, _, found := strings.Cut(p, ":"); found {
			p = before // path:section or path:first:last
		}
		if p != "" {
			out = append(out, include{path: p, line: i + 1, text: line})
		}
	}
	for i, line := range strings.Split(text, "\n") {
		switch m := inlineInclude.FindStringSubmatch(line); {
		case blockMarker.MatchString(line):
			inBlock = !inBlock
		case m != nil:
			add(i, line, m[1]+m[2])
		case inBlock && !strings.HasPrefix(strings.TrimSpace(line), ";"):
			add(i, line, strings.Trim(strings.TrimSpace(line), `"'`))
		}
	}
	return out
}

// resolveInclude finds the file that an include names: the first base directory that holds it. A path that leaves
// its base directory does not count, as the extension's restrict_base_path has it. The error is the context's, if it
// ended before the file was found.
func resolveInclude(ctx context.Context, bases []string, name string) (string, bool, error) {
	for _, base := range bases {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		file := filepath.Join(base, filepath.FromSlash(name))
		if rel, err := filepath.Rel(base, file); err != nil || !filepath.IsLocal(rel) {
			continue
		}
		if fi, err := os.Stat(file); err == nil && fi.Mode().IsRegular() {
			return file, true, nil
		}
	}
	return "", false, ctx.Err()
}

func main() { os.Exit(run()) }

// run is main, with an exit status, so that its deferred call runs: 0 for a clean site, 1 for findings, 2 for a
// failure to read the sources or an interrupt.
func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	got, err := check(ctx, ".", sitePaths("."), excluded)
	if err != nil {
		fmt.Fprintln(os.Stderr, "namecheck:", err)
		return 2
	}
	for _, f := range got {
		fmt.Fprintln(os.Stderr, f)
	}
	if len(got) > 0 {
		return 1
	}
	return 0
}
