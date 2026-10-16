// Command contentcheck fails when the public documentation holds something that must not be there. It exists because
// the validation of 2026-10-11 found internal planning IDs, removed field names and workspace identifiers in pages that
// the site and the repository publish, and nothing would have noticed them coming back.
//
// It reads the public sources: README.md, charts/agent-kourier/README.md, charts/agent-kourier/values.yaml (all of it,
// not only its comments: the comments feed docs/reference/helm-values.md and the values are the defaults the page
// shows), every README.md and every YAML file under examples/recipes (the recipes' pages and their resources and evals,
// at any depth; their code is not scanned yet), every text file under docs/ except the directories that mkdocs.yml's exclude_docs names (the brand kit and the
// spike notes), and the site theme overrides under hack/docs/overrides. Text files are Markdown, HTML, CSS, JavaScript,
// YAML, JSON, SVG, WebVTT, the web manifest, plain text and the Cloudflare _headers file; images and video are skipped.
// Two kinds of check run:
//
//   - Content rules (the table in this file): internal work-package, decision and risk IDs and tasks/ paths, names that
//     were removed from the product or the docs, fields the API types do not have yet, and identifiers of the owner's
//     workspace. They apply to every line, code fences and inline code included: a leak in an example is still a leak.
//   - Link hygiene, for the two READMEs and the recipes' READMEs only (mkdocs --strict already checks docs/): a relative link must name a file
//     or directory that exists in the repository, and an anchor into a Markdown page must name one of its headings as
//     GitHub slugs it, because GitHub renders the READMEs.
//
// A deliberate exception goes in hack/docs/contentcheck/allowlist.txt, one line each, with a reason. An entry that no
// longer matches anything is itself a finding, so the list cannot rot.
//
// Run it from the repository root, through `make docs-check`. The exit status is 0 on a clean tree, 1 with findings
// (all listed, as path:line: rule: message) and 2 when the sources cannot be read.
package main

import (
	"cmp"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// rule is one content check: a pattern that must not appear in a public page, and why.
type rule struct {
	id  string // names the rule in an error and in an allowlist entry
	re  *regexp.Regexp
	why string // what to do about it, in a sentence that follows the quoted match
	// fields, when set, are the field names re is built from. A name that is a json tag in api/v1alpha1 is a field the
	// API has, and activeRules drops it from the pattern, so the rule retires itself as the fields are built.
	fields []string
}

// r builds a table row.
func r(id, pattern, why string) rule {
	return rule{id: id, re: regexp.MustCompile(pattern), why: why}
}

// fieldRule builds a table row for names of API fields that are not in the types yet.
func fieldRule(id, why string, names ...string) rule {
	return rule{id: id, re: namesPattern(names), why: why, fields: names}
}

// namesPattern matches any of names as a whole word.
func namesPattern(names []string) *regexp.Regexp {
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = regexp.QuoteMeta(n)
	}
	return regexp.MustCompile(`\b(?:` + strings.Join(quoted, "|") + `)\b`)
}

// rules is the content table. Adding a rule is one line. Patterns are Go RE2, matched against one line at a time.
//
// Known false positives, which the allowlist excuses with a reason: a cloud product named R2 ("Cloudflare (R2)"), a
// Markdown reference label such as "[R2]:", a colour name such as D65, and a path like /api/Users/ (home-path is
// case-sensitive on /Users/).
var rules = []rule{
	// An ID is two capital letters, a hyphen and a number or F (WP-27g, TA-12, TA-F), or a gate (H-01, H2-1). Longer
	// prefixes are not IDs: DNS-1123, UTF-8, UTF-16 and SHA-256 do not match.
	r("planning-id", `\b[A-Z]{2}-(?:\d\w*|F)\b|\bH\d?-\d\w*`,
		"is an internal work-package, plan or gate ID; say what the code does instead"),
	r("decision-id", `\bD\d{2}\b`,
		"is a decision ID from the internal spec; say what the code does and drop the ID"),
	r("risk-id", `\bR\d{1,2}\b`,
		"is a risk ID from the internal spec; say what the code does and drop the ID"),
	r("tasks-path", "\\btasks/(?:plans\\b|[\\w.-]+\\.md\\b)|`tasks/`",
		"is a path into the internal tasks/ tree, which the public docs do not carry"),

	r("webhook-path", `\bwebhookPath\b|hooks/alertmanager`,
		"names the removed Alertmanager webhook; alerts enter only through chat.triggers"),
	r("spec-alerts", `\bspec\.alerts\b`,
		"names the removed Binding alerts block, which the strict decoder rejects; alerts enter only through chat.triggers"),
	r("removed-preset", "(?i)\\b[\"'`]?preset[\"'`]?:\\s*[\"'`]?alertmanager\\b|\\balertmanager\\b[\"'`]?\\s+[\"'`]?preset\\b|\\bpreset\\b[\"'`]?\\s+[\"'`]?alertmanager\\b",
		"names the removed trigger preset; a botMessage trigger is written in full, as the Alertmanager how-to shows"),
	fieldRule("unbuilt-field",
		"names a field that api/v1alpha1 does not have yet; it stops being flagged when a type carries it as a json tag",
		"approverGroups", "toolApprovals", "directMessages"),
	r("removed-page", `(?i)\b(?:roadmap|comparison)\.md\b`,
		"names a page that was removed from the docs (ROADMAP.md, roadmap.md, comparison.md)"),
	r("wrong-claim", `(?i)wakes it if needed`,
		"is a phrase the 2026-10-11 validation found wrong; say what the code does"),

	r("workspace-id", `C0EXAMPLE01|B0EXAMPLE01`,
		"is a Slack ID from the owner's workspace; use an obviously fake ID such as C0123456789"),
	r("workspace-name", `(?i)example|cluster-gitops|alerts|cluster-apps`,
		"names the owner's organisation, cluster or alert setup, which a public page must not"),
	r("home-path", `/Users/`,
		"is a path on a developer's machine"),
}

// activeRules returns the table with the fields the API has taken out: a field rule keeps the names that are not in
// apiFields and is dropped when none are left.
func activeRules(apiFields map[string]bool) []rule {
	var out []rule
	for _, ru := range rules {
		if len(ru.fields) == 0 {
			out = append(out, ru)
			continue
		}
		var left []string
		for _, n := range ru.fields {
			if !apiFields[n] {
				left = append(left, n)
			}
		}
		if len(left) == 0 {
			continue
		}
		ru.re = namesPattern(left)
		ru.fields = left
		out = append(out, ru)
	}
	return out
}

// apiFieldNames returns the json names of the struct fields of the Go files (not the tests) of api/v1alpha1 under
// root. It parses the files, so a tag-looking string in a comment is not a field.
func apiFieldNames(root string) (map[string]bool, error) {
	dir := filepath.Join(root, "api", "v1alpha1")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the API types: %w", err)
	}
	names := map[string]bool{}
	fset := token.NewFileSet()
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".go") || strings.HasSuffix(n, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, n), nil, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", n, err)
		}
		ast.Inspect(file, func(node ast.Node) bool {
			f, ok := node.(*ast.Field)
			if !ok || f.Tag == nil {
				return true
			}
			tag, err := strconv.Unquote(f.Tag.Value)
			if err != nil {
				return true
			}
			if name, _, _ := strings.Cut(reflect.StructTag(tag).Get("json"), ","); name != "" && name != "-" {
				names[name] = true
			}
			return true
		})
	}
	return names, nil
}

// linkRules are the IDs of the link checks, for the allowlist.
var linkRules = []string{"dead-link", "dead-anchor"}

// allowlistPath is the allowlist, relative to the repository root.
const allowlistPath = "hack/docs/contentcheck/allowlist.txt"

// linkChecked are the files whose links are checked here; mkdocs --strict checks the pages under docs/.
var linkChecked = []string{"README.md", "charts/agent-kourier/README.md"}

// recipesDir holds the recipes. Its README.md files are public pages, and GitHub renders them; its YAML files (the
// resources of a recipe, and its evals, which an author fills with messages copied from a real channel) are public too.
const recipesDir = "examples/recipes"

// isLinkChecked reports whether the links of the public source p (a slash-separated path from the root) are checked.
func isLinkChecked(p string) bool {
	return slices.Contains(linkChecked, p) || isRecipeREADME(p)
}

// isRecipeREADME reports whether p is a README.md at any depth under recipesDir.
func isRecipeREADME(p string) bool {
	return strings.HasPrefix(p, recipesDir+"/") && path.Base(p) == "README.md"
}

// isRecipeYAML reports whether p is a YAML file at any depth under recipesDir.
func isRecipeYAML(p string) bool {
	return strings.HasPrefix(p, recipesDir+"/") && (path.Ext(p) == ".yaml" || path.Ext(p) == ".yml")
}

// recipeFiles returns the public files under recipesDir in root, the READMEs and the YAML files, as slash-separated
// paths relative to it. The directory may be absent.
func recipeFiles(root string) ([]string, error) {
	all, err := textFiles(root, recipesDir, nil)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(all, func(p string) bool { return !isRecipeREADME(p) && !isRecipeYAML(p) }), nil
}

// finding is one place where a public page breaks a rule.
type finding struct {
	path string
	line int
	rule string
	msg  string
	text string // the offending line; an allowlist entry's text is looked for in it
	// start and end locate the match in text, as byte offsets. A finding with end <= start has no position and is
	// excused by any entry whose text is on the line.
	start, end int
}

func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s: %s", f.path, f.line, f.rule, f.msg)
}

// svgRefs matches what a drawing tool generates and nobody wrote: an element's id attribute and a url(#id) reference to
// one (mermaid names its nodes flowchart-DB-15, which has the shape of a plan ID).
var svgRefs = regexp.MustCompile(`\bid="[^"]*"|url\(#[^)]*\)`)

// scanText applies the rules rs to text and returns a finding for each match, so two matches of one rule on a line are
// two findings and each is excused on its own. In an SVG file, generated element IDs are not read.
func scanText(rs []rule, path, text string) []finding {
	var out []finding
	svg := strings.HasSuffix(path, ".svg")
	for i, line := range strings.Split(text, "\n") {
		if svg {
			line = svgRefs.ReplaceAllString(line, "")
		}
		for _, ru := range rs {
			for _, m := range ru.re.FindAllStringIndex(line, -1) {
				out = append(out, finding{path: path, line: i + 1, rule: ru.id, text: line, start: m[0], end: m[1],
					msg: fmt.Sprintf("%q %s", line[m[0]:m[1]], ru.why)})
			}
		}
	}
	return out
}

// excludedDocs returns the top-level directories of docs/ that mkdocs.yml's exclude_docs block names. It accepts only
// the form `/name/`: another pattern would make this check read different files than the site publishes, so it is an
// error rather than a guess.
func excludedDocs(mkdocs string) ([]string, error) {
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
			if !strings.HasPrefix(t, "/") || !strings.HasSuffix(t, "/") || name == "" || strings.ContainsAny(name, "/*?[") {
				return nil, fmt.Errorf("mkdocs.yml exclude_docs pattern %q: only /directory/ is supported", t)
			}
			out = append(out, name)
		}
		return out, nil
	}
	return nil, fmt.Errorf("mkdocs.yml has no `exclude_docs: |` block")
}

// textExt are the extensions of the text files the site publishes; images and video are skipped.
var textExt = []string{".md", ".html", ".yml", ".yaml", ".css", ".js", ".json", ".svg", ".vtt", ".webmanifest", ".txt"}

// textNames are published text files with no extension.
var textNames = []string{"_headers"}

// textFiles returns the text files under dir (a slash-separated path relative to root) as paths relative to root,
// skipping the top-level directories of dir that skip names. A dir that does not exist yields none.
func textFiles(root, dir string, skip []string) ([]string, error) {
	base := filepath.Join(root, filepath.FromSlash(dir))
	if _, err := os.Stat(base); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	var out []string
	err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && !strings.Contains(rel, string(filepath.Separator)) && slices.Contains(skip, rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if slices.Contains(textExt, filepath.Ext(p)) || slices.Contains(textNames, d.Name()) {
			out = append(out, dir+"/"+filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", dir, err)
	}
	return out, nil
}

// publicFiles returns the public sources under root, as slash-separated paths relative to it.
func publicFiles(root string) ([]string, error) {
	mk, err := os.ReadFile(filepath.Join(root, "mkdocs.yml")) //nolint:gosec // the site's own configuration
	if err != nil {
		return nil, fmt.Errorf("read mkdocs.yml: %w", err)
	}
	skip, err := excludedDocs(string(mk))
	if err != nil {
		return nil, err
	}
	files := []string{"README.md", "charts/agent-kourier/README.md", "charts/agent-kourier/values.yaml"}
	for _, f := range files {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			return nil, fmt.Errorf("public source: %w", err)
		}
	}
	docs, err := textFiles(root, "docs", skip)
	if err != nil {
		return nil, err
	}
	overrides, err := textFiles(root, "hack/docs/overrides", nil)
	if err != nil {
		return nil, err
	}
	recipes, err := recipeFiles(root)
	if err != nil {
		return nil, err
	}
	return slices.Concat(files, recipes, docs, overrides), nil
}

// allowEntry is one deliberate exception: a line of path that matches rule and holds text.
type allowEntry struct {
	line   int
	path   string
	rule   string
	text   string
	reason string
	used   bool
}

// parseAllowlist reads the allowlist. Each entry is one line, `path | rule | text on the line | reason`; the reason
// may hold `|`. A line that starts with # and a blank line are skipped. A malformed entry is a finding.
func parseAllowlist(path, text string) ([]*allowEntry, []finding) {
	var entries []*allowEntry
	var bad []finding
	known := map[string]bool{}
	for _, ru := range rules {
		known[ru.id] = true
	}
	for _, id := range linkRules {
		known[id] = true
	}
	for i, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fail := func(msg string) {
			bad = append(bad, finding{path: path, line: i + 1, rule: "allowlist", msg: msg, text: line})
		}
		f := strings.SplitN(line, "|", 4)
		if len(f) != 4 {
			fail("an allowlist entry needs four fields: path | rule | text on the line | reason")
			continue
		}
		for j := range f {
			f[j] = strings.TrimSpace(f[j])
		}
		switch {
		case f[0] == "" || f[1] == "" || f[2] == "":
			fail("an allowlist entry needs a path, a rule and the text on the line it excuses")
		case f[3] == "":
			fail("an allowlist entry needs a reason")
		case !known[f[1]]:
			fail(fmt.Sprintf("unknown rule %q", f[1]))
		default:
			entries = append(entries, &allowEntry{line: i + 1, path: f[0], rule: f[1], text: f[2], reason: f[3]})
		}
	}
	return entries, bad
}

// covers reports whether an entry's text excuses finding f: the text is on the line and, when the finding records where
// its match is, one occurrence of the text includes that match. An entry for "Cloudflare (R2)" excuses the R2 inside it
// and not another risk ID later on the line.
func covers(f finding, entry string) bool {
	if !strings.Contains(f.text, entry) {
		return false
	}
	if f.end <= f.start {
		return true
	}
	for from := 0; from <= len(f.text); {
		i := strings.Index(f.text[from:], entry)
		if i < 0 {
			return false
		}
		i += from
		if i <= f.start && f.end <= i+len(entry) {
			return true
		}
		from = i + 1
	}
	return false
}

// applyAllowlist drops the findings an entry excuses and adds a finding for each entry that excused none.
func applyAllowlist(in []finding, entries []*allowEntry, listPath string) []finding {
	var out []finding
	for _, f := range in {
		excused := false
		for _, e := range entries {
			if e.path == f.path && e.rule == f.rule && covers(f, e.text) {
				e.used = true
				excused = true
			}
		}
		if !excused {
			out = append(out, f)
		}
	}
	for _, e := range entries {
		if !e.used {
			out = append(out, finding{path: listPath, line: e.line, rule: "stale-allowlist",
				msg: fmt.Sprintf("the entry for %s (%s) matches nothing; delete it", e.path, e.rule)})
		}
	}
	return out
}

// check runs every check over the public sources under root and returns the findings that no allowlist entry excuses,
// ordered by file and line.
func check(root string) ([]finding, error) {
	files, err := publicFiles(root)
	if err != nil {
		return nil, err
	}
	api, err := apiFieldNames(root)
	if err != nil {
		return nil, err
	}
	active := activeRules(api)
	fsys := os.DirFS(root)
	links := &linkChecker{fsys: fsys}
	var out []finding
	for _, p := range files {
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", p, err)
		}
		out = append(out, scanText(active, p, string(b))...)
		if isLinkChecked(p) {
			out = append(out, links.check(p, string(b))...)
		}
	}
	var entries []*allowEntry
	if b, err := os.ReadFile(filepath.Join(root, allowlistPath)); err == nil { //nolint:gosec // the repository's own allowlist
		var bad []finding
		entries, bad = parseAllowlist(allowlistPath, string(b))
		out = append(out, bad...)
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("read allowlist: %w", err)
	}
	out = applyAllowlist(out, entries, allowlistPath)
	slices.SortStableFunc(out, func(a, b finding) int {
		return cmp.Or(strings.Compare(a.path, b.path), cmp.Compare(a.line, b.line))
	})
	return out, nil
}

// run checks the tree under root, prints each finding to stderr, and returns the exit status.
func run(root string, stderr io.Writer) int {
	found, err := check(root)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "contentcheck:", err) // a failed write to stderr has nowhere to be reported
		return 2
	}
	for _, f := range found {
		_, _ = fmt.Fprintln(stderr, f)
	}
	if len(found) > 0 {
		_, _ = fmt.Fprintf(stderr, "contentcheck: %d findings; fix the page, or add an allowlist entry with a reason to %s\n", len(found), allowlistPath)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(".", os.Stderr))
}
