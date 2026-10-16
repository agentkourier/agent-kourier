// Package corpus loads a directory of Markdown files into memory and answers two questions about it: which pages
// match some keywords, and what does this page, or this section of it, say.
//
// The directory is read once, by Load, through an os.Root, and never written. After Load no request touches the
// file system: a path from a caller is checked for shape and looked up in the index, so no path, however it is
// spelled, reaches a file that Load did not read. Load refuses a symlink anywhere inside the directory. A change
// to the files needs a new Load.
//
// Index implements the two small interfaces that the MCP tools in package docsmcp are written against: a Searcher
// (keywords to ranked pages) and a Reader (a page, or a section of it). Keyword ranking is one Searcher. A
// Searcher backed by embeddings and a vector store can replace it without changing the Reader or the tools.
package corpus

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits on what is loaded and returned.
const (
	// DefaultMaxFileBytes is the largest Markdown file Load accepts.
	DefaultMaxFileBytes = 1 << 20
	// DefaultMaxReadBytes is the most text one Read returns.
	DefaultMaxReadBytes = 24 << 10
	// maxPathBytes is the longest page path Read looks up.
	maxPathBytes = 256
)

// Errors returned by Read and Search. They carry no file system path.
var (
	// ErrInvalidPath is a page path that is not the plain, relative, clean form of a Markdown file's path in the
	// corpus: it is absolute, climbs with "..", holds a backslash, a control character or a segment that starts
	// with a dot, is not clean, is too long, or does not end in ".md".
	ErrInvalidPath = errors.New("invalid page path")
	// ErrNotFound is a well-formed page path that names no page in the corpus.
	ErrNotFound = errors.New("page not found")
	// ErrSectionNotFound is a section that the page does not have.
	ErrSectionNotFound = errors.New("section not found")
	// ErrEmptyQuery is a query with no searchable word in it.
	ErrEmptyQuery = errors.New("query has no searchable word")
)

// Options tunes Load. The zero value is usable.
type Options struct {
	// MaxFileBytes is the largest file Load accepts; a larger one is an error. Zero means DefaultMaxFileBytes.
	MaxFileBytes int64
	// MaxReadBytes is the most text Read returns for one page or section; a longer text is cut at a line end and
	// marked truncated. Zero means DefaultMaxReadBytes.
	MaxReadBytes int
	// Exclude lists paths, relative to the corpus directory and slash-separated, that Load skips: a directory
	// with everything under it, or one file. A path that is empty, absolute, not clean or climbing out is an error.
	Exclude []string
}

// Heading is an entry of a page's outline.
type Heading struct {
	// Text is the heading as written, without its Markdown marks.
	Text string
	// Anchor is the heading's id on the published page, and the value Read takes as a section.
	Anchor string
	// Level is 1 for "#", 2 for "##", and so on.
	Level int
}

// Page is the text of a page or of one section of it.
type Page struct {
	// Path is the page's path in the corpus, with forward slashes.
	Path string
	// Title is the front matter title, or the first level-1 heading, or the file name.
	Title string
	// Section and Anchor name the section that was read; both are empty for the whole page.
	Section string
	Anchor  string
	// Text is the Markdown source of the page or section, cut at a line end to Options.MaxReadBytes.
	Text string
	// Truncated says Text was cut.
	Truncated bool
	// Outline lists the headings of the whole page, so a caller can ask for one of its sections.
	Outline []Heading
}

// Index is the loaded corpus. It is read-only after Load and safe for concurrent use.
type Index struct {
	opts    Options
	pages   []*page // sorted by path
	byPath  map[string]*page
	ranking ranking
}

// page is one loaded file.
type page struct {
	path string
	parsed
	secs        []indexedSection
	outline     []Heading
	titleTokens map[string]bool
}

// Load reads every Markdown file (".md") under dir, at any depth, into an Index. Names that start with a dot are
// skipped, with everything under them. Load is an error if dir is not a directory, if it holds no Markdown, if a
// file is over Options.MaxFileBytes, or if anything inside dir is a symlink: a link is never followed, so a
// corpus cannot point outside itself. dir itself may be a symlink, as a mounted volume often is.
func Load(ctx context.Context, dir string, opts Options) (*Index, error) {
	if opts.MaxFileBytes <= 0 {
		opts.MaxFileBytes = DefaultMaxFileBytes
	}
	if opts.MaxReadBytes <= 0 {
		opts.MaxReadBytes = DefaultMaxReadBytes
	}
	skip := make(map[string]bool, len(opts.Exclude))
	for _, e := range opts.Exclude {
		if !fs.ValidPath(e) || e == "." || e == "" {
			return nil, fmt.Errorf("exclude %q: want a clean relative path inside the corpus", e)
		}
		skip[e] = true
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("open corpus directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	fsys := root.FS()

	ix := &Index{opts: opts, byPath: map[string]*page{}}
	err = fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if p == "." {
			return nil
		}
		if skip[p] {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(d.Name(), ".") {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink; symlinks in the corpus are refused", p)
		}
		if d.IsDir() || !d.Type().IsRegular() || path.Ext(p) != ".md" {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return fmt.Errorf("stat %s: %w", p, err)
		}
		if info.Size() > opts.MaxFileBytes {
			return fmt.Errorf("%s is %d bytes, over the %d-byte limit", p, info.Size(), opts.MaxFileBytes)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		if int64(len(data)) > opts.MaxFileBytes {
			return fmt.Errorf("%s is over the %d-byte limit", p, opts.MaxFileBytes)
		}
		ix.pages = append(ix.pages, newPage(p, strings.ToValidUTF8(string(data), "�")))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load corpus: %w", err)
	}
	if len(ix.pages) == 0 {
		return nil, errors.New("load corpus: no Markdown files found")
	}
	sort.Slice(ix.pages, func(i, j int) bool { return ix.pages[i].path < ix.pages[j].path })
	for _, p := range ix.pages {
		ix.byPath[p.path] = p
	}
	ix.ranking = newRanking(ix.pages)
	return ix, nil
}

func newPage(p, src string) *page {
	pg := &page{path: p, parsed: parse(src, p), titleTokens: map[string]bool{}}
	scan(pg.title, func(tok string, _, _ int) { pg.titleTokens[tok] = true })
	for _, s := range pg.sections {
		pg.secs = append(pg.secs, indexSection(s))
		if s.level > 0 {
			pg.outline = append(pg.outline, Heading{Text: s.heading, Anchor: s.anchor, Level: s.level})
		}
	}
	return pg
}

// Paths returns the path of every page, sorted.
func (ix *Index) Paths() []string {
	out := make([]string, len(ix.pages))
	for i, p := range ix.pages {
		out[i] = p.path
	}
	return out
}

// Read returns the page at pagePath, or, if section is not empty, the section of it that section names: its
// anchor or its heading, ignoring case and surrounding space. A section includes its subsections. When two
// sections have the same heading, the heading names the first and the anchor ("example_1") names the others.
// It returns ErrInvalidPath for a path that is not a clean relative path to a Markdown file, ErrNotFound for one
// that names no page, and ErrSectionNotFound, listing the page's anchors, for a section it does not have.
func (ix *Index) Read(ctx context.Context, pagePath, section string) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if !validPath(pagePath) {
		return Page{}, fmt.Errorf("%w: %q", ErrInvalidPath, pagePath)
	}
	pg, ok := ix.byPath[pagePath]
	if !ok {
		return Page{}, fmt.Errorf("%w: %q", ErrNotFound, pagePath)
	}
	out := Page{Path: pg.path, Title: pg.title, Outline: pg.outline, Text: strings.TrimSpace(pg.body)}
	if section = normalize(section); section != "" {
		i := pg.sectionIndex(section)
		if i < 0 {
			anchors := make([]string, len(pg.outline))
			for j, h := range pg.outline {
				anchors[j] = h.Anchor
			}
			return Page{}, fmt.Errorf("%w: %q; the page's sections are %s", ErrSectionNotFound, section, strings.Join(anchors, ", "))
		}
		out.Section, out.Anchor, out.Text = pg.sections[i].heading, pg.sections[i].anchor, pg.span(i)
	}
	out.Text, out.Truncated = cut(out.Text, ix.opts.MaxReadBytes)
	return out, nil
}

// sectionIndex finds the section named by a normalized anchor or heading: an anchor first, then a heading.
func (p *page) sectionIndex(name string) int {
	for i, s := range p.sections {
		if s.level > 0 && s.anchor == name {
			return i
		}
	}
	for i, s := range p.sections {
		if s.level > 0 && strings.ToLower(s.heading) == name {
			return i
		}
	}
	return -1
}

// normalize lower-cases s and collapses its white space.
func normalize(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// cut shortens s to at most limit bytes, at the end of a line, and says whether it did.
func cut(s string, limit int) (string, bool) {
	if len(s) <= limit {
		return s, false
	}
	s = s[:limit]
	if i := strings.LastIndexByte(s, '\n'); i > 0 {
		return strings.TrimRight(s[:i], " \t"), true
	}
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s, true
}

// validPath reports whether p is the clean, relative, slash-separated path of a Markdown file, with no segment
// that starts with a dot (Load skips those) and no character a file name should not hold.
func validPath(p string) bool {
	if p == "" || len(p) > maxPathBytes || !utf8.ValidString(p) || path.Ext(p) != ".md" || path.Clean(p) != p || strings.HasPrefix(p, "/") {
		return false
	}
	for _, r := range p {
		if r == '\\' || unicode.IsControl(r) {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if strings.HasPrefix(seg, ".") {
			return false
		}
	}
	return true
}

// SplitList splits a comma-separated list, as an environment variable or a flag holds it, dropping blanks and
// surrounding space. It is how Options.Exclude is usually written.
func SplitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}
