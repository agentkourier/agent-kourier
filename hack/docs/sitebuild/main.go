// Command sitebuild builds the versioned documentation site that agentkourier.dev serves. It builds every release
// line that hack/docs/versions.yaml names from that line's newest final tag (vX.Y.Z, no prerelease suffix), each in
// its own git worktree with that tag's pinned MkDocs requirements, and the working tree as dev. Each version lands in
// its own directory (0.1/, dev/) of the output, built with its own site_url and Material's version selector through a
// wrapper config that inherits the version's mkdocs.yml, so an old tag builds unchanged. The first release line with a
// tag is latest.
//
// At the root of the output it writes what Material and Cloudflare Pages read there: versions.json (the format mike
// writes, which Material's selector reads), _redirects (the root and /latest/ to the latest line, and the paths of
// the unversioned site to /latest/, so old links keep working) and _headers (each version's own rules, under its
// directory). It copies the latest line's 404.html and favicon.ico to the root.
//
// Run it from the repository root, through `make docs-site`. It needs the release tags (a full clone), Python 3 and
// the docs virtualenv that `make docs-build` installs.
package main

import (
	"bufio"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"sigs.k8s.io/yaml"
)

const (
	// devVersion is the directory and versions.json entry of the working tree's build.
	devVersion = "dev"
	// latestAlias is the alias of the newest release line, and the version Material's banner treats as current.
	latestAlias = "latest"
	// requirementsFile pins a tree's MkDocs and Material versions.
	requirementsFile = "hack/docs/requirements.txt"
	// wrapperFile is the per-version config sitebuild writes next to a tree's mkdocs.yml and removes after the build.
	// MkDocs resolves INHERIT and docs_dir against the file's own directory, so it cannot live elsewhere.
	wrapperFile = "mkdocs.sitebuild.yml"
	// Cloudflare Pages' limits on the two root files.
	maxHeaderRules      = 100
	maxStaticRedirects  = 2000
	maxDynamicRedirects = 100
)

// versionsFile is hack/docs/versions.yaml.
type versionsFile struct {
	Releases []string `json:"releases"`
}

// release is a release line and the tag it is built from.
type release struct {
	Line string // 0.1
	Tag  string // v0.1.2
}

// versionEntry is one entry of versions.json, in the format mike writes.
type versionEntry struct {
	Version string   `json:"version"`
	Title   string   `json:"title"`
	Aliases []string `json:"aliases"`
}

var (
	lineRe    = regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
	finalTag  = regexp.MustCompile(`^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)
	siteURLRe = regexp.MustCompile(`(?m)^site_url:\s*(\S+)\s*$`)
)

// lineOrder orders two release lines (MAJOR.MINOR) by version, as cmp.Compare does.
func lineOrder(a, b string) int {
	am, an, _ := strings.Cut(a, ".")
	bm, bn, _ := strings.Cut(b, ".")
	x, _ := strconv.Atoi(am)
	y, _ := strconv.Atoi(bm)
	if c := cmp.Compare(x, y); c != 0 {
		return c
	}
	x, _ = strconv.Atoi(an)
	y, _ = strconv.Atoi(bn)
	return cmp.Compare(x, y)
}

// parseVersions reads the release lines of hack/docs/versions.yaml: X.Y each, newest first, no repeats, at least
// one. The order decides which line is latest, so a line out of order fails rather than moving latest.
func parseVersions(data []byte) ([]string, error) {
	var f versionsFile
	if err := yaml.UnmarshalStrict(data, &f); err != nil {
		return nil, fmt.Errorf("parse: %w", err)
	}
	if len(f.Releases) == 0 {
		return nil, errors.New("no release lines")
	}
	for i, line := range f.Releases {
		if !lineRe.MatchString(line) {
			return nil, fmt.Errorf("release line %q is not MAJOR.MINOR", line)
		}
		if slices.Contains(f.Releases[:i], line) {
			return nil, fmt.Errorf("release line %q is listed twice", line)
		}
		if i > 0 && lineOrder(line, f.Releases[i-1]) > 0 {
			return nil, fmt.Errorf("release line %q is listed after the older %q; list the lines newest first", line, f.Releases[i-1])
		}
	}
	return f.Releases, nil
}

// newestTag returns the newest final tag of a release line, or "" when the line has none yet.
func newestTag(line string, tags []string) string {
	best, bestPatch := "", -1
	for _, tag := range tags {
		m := finalTag.FindStringSubmatch(tag)
		if m == nil || m[1]+"."+m[2] != line {
			continue
		}
		if patch, _ := strconv.Atoi(m[3]); patch > bestPatch {
			best, bestPatch = tag, patch
		}
	}
	return best
}

// untrackedTag returns a final tag whose release line is newer than every line in the file, or "" when there is none:
// the release pull request of a new minor version did not add its line, and latest would stay on the older line.
func untrackedTag(lines, tags []string) string {
	for _, tag := range tags {
		m := finalTag.FindStringSubmatch(tag)
		if m != nil && lineOrder(m[1]+"."+m[2], lines[0]) > 0 {
			return tag
		}
	}
	return ""
}

// resolve pairs each release line with its newest final tag, in the file's order, and drops a line with no tag yet.
func resolve(lines, tags []string) (got []release, skipped []string) {
	for _, line := range lines {
		if tag := newestTag(line, tags); tag != "" {
			got = append(got, release{Line: line, Tag: tag})
		} else {
			skipped = append(skipped, line)
		}
	}
	return got, skipped
}

// versionsJSON is the site's versions.json: dev first, then the release lines, the first of them latest.
func versionsJSON(releases []release) ([]byte, error) {
	entries := []versionEntry{{Version: devVersion, Title: "main (unreleased)", Aliases: []string{}}}
	for i, r := range releases {
		aliases := []string{}
		if i == 0 {
			aliases = append(aliases, latestAlias)
		}
		entries = append(entries, versionEntry{Version: r.Line, Title: r.Tag, Aliases: aliases})
	}
	b, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("versions.json: %w", err)
	}
	return append(b, '\n'), nil
}

// wrapperConfig is the config a version is built with: its own mkdocs.yml, its directory's site_url, the selector,
// and the path of the site's root (extra.site_root), which the banner links to. A relative link cannot do: MkDocs
// gives 404.html an absolute base_url.
func wrapperConfig(siteURL, version string) (string, error) {
	u, err := url.Parse(siteURL)
	if err != nil {
		return "", fmt.Errorf("site_url: %w", err)
	}
	return fmt.Sprintf(`# Written by hack/docs/sitebuild for one version of the versioned site, and removed after the build.
INHERIT: mkdocs.yml
site_url: %s%s/
extra:
  site_root: %s
  version:
    provider: mike
    default: %s
    alias: true
`, siteURL, version, cmp.Or(u.Path, "/"), latestAlias), nil
}

// redirects is the site's _redirects. The root and /latest/ lead to the latest release line with a temporary
// redirect, since a release moves them. Each top-level entry of the unversioned site leads to the same path under
// /latest/ with a permanent one. Cloudflare Pages applies a redirect
// even where a file exists, so the rules name those entries rather than match everything. Static rules come first, as
// Pages asks.
func redirects(latest string, topLevel []entry) (string, error) {
	var static, dynamic []string
	static = append(static,
		"/ /"+latestAlias+"/ 302",
		"/"+latestAlias+" /"+latest+"/ 302",
	)
	dynamic = append(dynamic, "/"+latestAlias+"/* /"+latest+"/:splat 302")
	for _, e := range topLevel {
		if e.dir {
			dynamic = append(dynamic, fmt.Sprintf("/%s/* /%s/%s/:splat 301", e.name, latestAlias, e.name))
		} else {
			static = append(static, fmt.Sprintf("/%s /%s/%s 301", e.name, latestAlias, e.name))
		}
	}
	if len(static) > maxStaticRedirects || len(dynamic) > maxDynamicRedirects {
		return "", fmt.Errorf("_redirects has %d static and %d dynamic rules; Cloudflare Pages allows %d and %d",
			len(static), len(dynamic), maxStaticRedirects, maxDynamicRedirects)
	}
	var b strings.Builder
	b.WriteString("# Written by hack/docs/sitebuild. The root and /latest/ lead to the latest release line; the paths of the\n")
	b.WriteString("# unversioned site lead to the same page under /latest/.\n")
	for _, r := range append(static, dynamic...) {
		b.WriteString(r + "\n")
	}
	return b.String(), nil
}

// headerBlock is one version's _headers rules, each path moved under the version's directory. A rule's path is a
// line that starts with a slash; its headers are the indented lines after it; comments and blank lines are dropped.
func headerBlock(version string, rules []byte) (block string, n int, err error) {
	var b strings.Builder
	sc := bufio.NewScanner(strings.NewReader(string(rules)))
	for sc.Scan() {
		line := sc.Text()
		trimmed := strings.TrimSpace(line)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
		case strings.HasPrefix(line, "/"):
			b.WriteString("/" + version + line + "\n")
			n++
		case line[0] == ' ' || line[0] == '\t':
			if n == 0 {
				return "", 0, fmt.Errorf("header %q comes before any path", trimmed)
			}
			b.WriteString("  " + trimmed + "\n")
		default:
			return "", 0, fmt.Errorf("line %q is neither a path nor an indented header", line)
		}
	}
	return b.String(), n, sc.Err()
}

// entry is a top-level file or directory of a build.
type entry struct {
	name string
	dir  bool
}

// rootFiles are the latest build's files that are copied to the root rather than redirected: the page Pages shows
// for a path no version has, and the icon browsers ask the root for.
var rootFiles = []string{"404.html", "favicon.ico"}

// legacyPaths are the top-level entries the unversioned site served, before the site had versions, other than
// rootFiles. They are fixed, not read from the latest build: a later release that drops one must keep its old links
// working, and one that adds an entry must not gain a permanent redirect for a path the unversioned site never had.
var legacyPaths = []entry{
	{"assets", true}, {"demo", true}, {"explanation", true}, {"how-to", true}, {"reference", true},
	{"search", true}, {"stylesheets", true}, {"tutorials", true},
	{"index.html", false}, {"site.webmanifest", false}, {"sitemap.xml", false}, {"sitemap.xml.gz", false},
}

// builder runs the builds.
type builder struct {
	root    string // the repository root: the dev tree and the git repository
	out     string // the output directory
	venv    string // the docs virtualenv for the root's requirements
	siteURL string // the unversioned site_url of the root's mkdocs.yml
}

// run runs a command in dir, its output on stderr.
func run(ctx context.Context, dir, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...) //nolint:gosec // git, python3, pip and mkdocs with sitebuild's own arguments
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// tags lists the repository's tags.
func (b *builder) tags(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "tag", "--list", "v*")
	cmd.Dir = b.root
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git tag: %w", err)
	}
	return strings.Fields(string(out)), nil
}

// requirementsHash is the short hash of a tree's pinned requirements.
func requirementsHash(tree string) (string, error) {
	data, err := os.ReadFile(filepath.Join(tree, requirementsFile)) //nolint:gosec // the tree's own pinned requirements
	if err != nil {
		return "", fmt.Errorf("read requirements: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:12], nil
}

// venvFor returns the virtualenv to build a tree with: the root's when the requirements match, else one installed
// from the tree's own requirements, kept under bin/ by hash for the next run.
func (b *builder) venvFor(ctx context.Context, tree string) (string, error) {
	want, err := requirementsHash(tree)
	if err != nil {
		return "", err
	}
	have, err := requirementsHash(b.root)
	if err != nil {
		return "", err
	}
	if want == have {
		return b.venv, nil
	}
	venv := filepath.Join(b.root, "bin", "docs-venv-"+want)
	if _, err := os.Stat(filepath.Join(venv, ".installed")); err == nil {
		return venv, nil
	}
	if err := run(ctx, b.root, "python3", "-m", "venv", "--clear", venv); err != nil {
		return "", err
	}
	if err := run(ctx, b.root, filepath.Join(venv, "bin", "pip"), "install", "--quiet", "--disable-pip-version-check",
		"-r", filepath.Join(tree, requirementsFile)); err != nil {
		return "", err
	}
	return venv, os.WriteFile(filepath.Join(venv, ".installed"), nil, 0o644) //nolint:gosec // an empty marker, as the Makefile's
}

// build builds one tree into the output's version directory, strictly, as `make docs-build` does.
func (b *builder) build(ctx context.Context, tree, version string) (err error) {
	venv, err := b.venvFor(ctx, tree)
	if err != nil {
		return err
	}
	config, err := wrapperConfig(b.siteURL, version)
	if err != nil {
		return err
	}
	wrapper := filepath.Join(tree, wrapperFile)
	if err := os.WriteFile(wrapper, []byte(config), 0o644); err != nil { //nolint:gosec // a config beside mkdocs.yml, as readable
		return fmt.Errorf("write %s: %w", wrapper, err)
	}
	defer func() { err = errors.Join(err, os.Remove(wrapper)) }()
	// The tree is the working directory: the snippets extension resolves its base_path against it.
	return run(ctx, tree, filepath.Join(venv, "bin", "mkdocs"), "build", "--strict", "--config-file", wrapperFile,
		"--site-dir", filepath.Join(b.out, version))
}

// buildRelease builds a release line from its tag, in a worktree that is removed afterwards.
func (b *builder) buildRelease(ctx context.Context, r release) (err error) {
	tmp, err := os.MkdirTemp("", "sitebuild-")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer func() { err = errors.Join(err, os.RemoveAll(tmp)) }()
	tree := filepath.Join(tmp, r.Tag)
	if err := run(ctx, b.root, "git", "worktree", "add", "--quiet", "--detach", tree, r.Tag); err != nil {
		return err
	}
	defer func() {
		// Not ctx: an interrupted build still removes its worktree.
		err = errors.Join(err, run(context.WithoutCancel(ctx), b.root, "git", "worktree", "remove", "--force", tree))
	}()
	return b.build(ctx, tree, r.Line)
}

// assemble writes the root files once every version is built.
func (b *builder) assemble(releases []release) error {
	latest := releases[0].Line
	vj, err := versionsJSON(releases)
	if err != nil {
		return err
	}
	redir, err := redirects(latest, legacyPaths)
	if err != nil {
		return err
	}
	var hdr strings.Builder
	hdr.WriteString("# Written by hack/docs/sitebuild: each version's own _headers, under its directory.\n")
	hdr.WriteString("/versions.json\n  Cache-Control: public, max-age=300, must-revalidate\n")
	rules := 1
	versions := []string{devVersion}
	for _, r := range releases {
		versions = append(versions, r.Line)
	}
	for _, v := range versions {
		path := filepath.Join(b.out, v, "_headers")
		data, err := os.ReadFile(path) //nolint:gosec // a file of the build sitebuild just wrote
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		block, n, err := headerBlock(v, data)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		hdr.WriteString(block)
		rules += n
		// Pages reads only the root's _headers; a version's own would be served as a page.
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove %s: %w", path, err)
		}
	}
	if rules > maxHeaderRules {
		return fmt.Errorf("_headers has %d rules; Cloudflare Pages allows %d", rules, maxHeaderRules)
	}
	for name, data := range map[string]string{"versions.json": string(vj), "_redirects": redir, "_headers": hdr.String()} {
		if err := os.WriteFile(filepath.Join(b.out, name), []byte(data), 0o644); err != nil { //nolint:gosec // the site is world-readable
			return fmt.Errorf("write %s: %w", name, err)
		}
	}
	for _, name := range rootFiles {
		data, err := os.ReadFile(filepath.Join(b.out, latest, name)) //nolint:gosec // a file of the latest build
		if err != nil {
			return fmt.Errorf("copy %s: %w", name, err)
		}
		if err := os.WriteFile(filepath.Join(b.out, name), data, 0o644); err != nil { //nolint:gosec // the site is world-readable
			return fmt.Errorf("copy %s: %w", name, err)
		}
	}
	return nil
}

// siteURL reads the unversioned site_url from the root's mkdocs.yml, which this does not parse as YAML: it holds
// Python tags.
func siteURL(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "mkdocs.yml")) //nolint:gosec // the site's own config
	if err != nil {
		return "", fmt.Errorf("read mkdocs.yml: %w", err)
	}
	m := siteURLRe.FindSubmatch(data)
	if m == nil {
		return "", errors.New("mkdocs.yml has no site_url")
	}
	u := string(m[1])
	if !strings.HasSuffix(u, "/") {
		u += "/"
	}
	return u, nil
}

func main() { os.Exit(runMain()) }

// runMain is main, with an exit status, so that its deferred calls run.
func runMain() int {
	out := flag.String("out", "bin/versioned-site", "the output directory, replaced on each run")
	venv := flag.String("venv", "bin/docs-venv", "the docs virtualenv for this tree's requirements")
	versions := flag.String("versions", "hack/docs/versions.yaml", "the release lines to publish")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := buildSite(ctx, *out, *venv, *versions); err != nil {
		fmt.Fprintln(os.Stderr, "sitebuild:", err)
		return 1
	}
	return 0
}

func buildSite(ctx context.Context, out, venv, versionsPath string) error {
	root, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("working directory: %w", err)
	}
	data, err := os.ReadFile(versionsPath) //nolint:gosec // the versions file the Makefile names
	if err != nil {
		return fmt.Errorf("read versions: %w", err)
	}
	lines, err := parseVersions(data)
	if err != nil {
		return fmt.Errorf("%s: %w", versionsPath, err)
	}
	u, err := siteURL(root)
	if err != nil {
		return err
	}
	b := &builder{root: root, out: absUnder(root, out), venv: absUnder(root, venv), siteURL: u}
	if err := checkOut(b.root, b.out, b.venv); err != nil {
		return err
	}
	tags, err := b.tags(ctx)
	if err != nil {
		return err
	}
	if tag := untrackedTag(lines, tags); tag != "" {
		return fmt.Errorf("%s has no line for the release %s; add its MAJOR.MINOR at the top", versionsPath, tag)
	}
	releases, skipped := resolve(lines, tags)
	for _, line := range skipped {
		fmt.Fprintf(os.Stderr, "sitebuild: release line %s has no final tag yet; not published\n", line)
	}
	if len(releases) == 0 {
		return fmt.Errorf("no release line in %s has a final tag (a shallow clone has no tags)", versionsPath)
	}
	if err := os.RemoveAll(b.out); err != nil {
		return fmt.Errorf("clear %s: %w", b.out, err)
	}
	fmt.Fprintf(os.Stderr, "sitebuild: building %s from the working tree\n", devVersion)
	if err := b.build(ctx, root, devVersion); err != nil {
		return err
	}
	for _, r := range releases {
		fmt.Fprintf(os.Stderr, "sitebuild: building %s from %s\n", r.Line, r.Tag)
		if err := b.buildRelease(ctx, r); err != nil {
			return err
		}
	}
	if err := b.assemble(releases); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sitebuild: wrote %s (latest %s from %s)\n", b.out, releases[0].Line, releases[0].Tag)
	return nil
}

// within reports whether path is dir or inside it.
func within(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// checkOut refuses an output directory that sitebuild's clearing would take sources or the virtualenv with: one that is
// or holds the repository or the virtualenv, one inside the virtualenv, or one inside the repository but outside bin/,
// where the build's own outputs live.
func checkOut(root, out, venv string) error {
	switch {
	case within(out, root) || within(out, venv) || within(venv, out):
		return fmt.Errorf("refusing to replace %s: it is, holds or is inside the repository or the docs virtualenv", out)
	case within(root, out) && !within(filepath.Join(root, "bin"), out):
		return fmt.Errorf("refusing to replace %s: inside the repository, the output goes under bin/", out)
	}
	return nil
}

// absUnder makes a relative path absolute against root.
func absUnder(root, p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(root, p)
}
