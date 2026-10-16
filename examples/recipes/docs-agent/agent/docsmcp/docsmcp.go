// Package docsmcp is an MCP server with two read-only tools over a documentation corpus: search_docs finds pages
// by keywords, and read_page returns a page or one section of it.
//
// The tools are written against two small interfaces, Searcher and Reader, so that how pages are found is a
// choice. package corpus implements both with keyword ranking over Markdown files. To search by meaning instead,
// implement Searcher over an embedding model and a vector store, return the same corpus.Hit values, and pass it
// to New with the same Reader. The tools, their schemas and the agent that calls them do not change.
package docsmcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/urlcheck"
)

// The tool names.
const (
	// ToolSearch is the tool that finds pages by keywords.
	ToolSearch = "search_docs"
	// ToolRead is the tool that returns a page or a section of one.
	ToolRead = "read_page"
)

// Searcher finds the pages that match a query, best first. limit is already within 1 to corpus.MaxLimit.
// An implementation returns each page at most once, with the section that matches best, and an empty list when
// nothing matches. It returns corpus.ErrEmptyQuery for a query it cannot search; any error text it returns is
// shown to the model, so it must not carry secrets or file system paths.
type Searcher interface {
	Search(ctx context.Context, query string, limit int) ([]corpus.Hit, error)
}

// Reader returns a page, or one section of it, by the path that a Searcher gave. It returns the errors that
// corpus.Index.Read does, for the same cases.
type Reader interface {
	Read(ctx context.Context, path, section string) (corpus.Page, error)
}

// Config configures the server.
type Config struct {
	// Searcher and Reader are required.
	Searcher Searcher
	Reader   Reader
	// SiteURL, if set, is the address of the published documentation, such as "https://agentkourier.dev/". The
	// tools then give each page's and section's URL next to its path. It must be an absolute http or https URL
	// with no credentials, query or fragment.
	SiteURL string
}

// New returns an MCP server with the two tools. Serve it over any transport, for example with Handler for
// streamable HTTP, or mcp.StdioTransport.
func New(cfg Config) (*mcp.Server, error) {
	if cfg.Searcher == nil || cfg.Reader == nil {
		return nil, errors.New("docsmcp: a Searcher and a Reader are required")
	}
	site, err := cleanSiteURL(cfg.SiteURL)
	if err != nil {
		return nil, err
	}
	t := &tools{searcher: cfg.Searcher, reader: cfg.Reader, site: site}
	srv := mcp.NewServer(&mcp.Implementation{Name: "docs-mcp", Version: "0.1.0"}, nil)
	closed := false
	readOnly := &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &closed}
	mcp.AddTool(srv, &mcp.Tool{
		Name: ToolSearch,
		Description: "Search the documentation by keywords. Returns up to `limit` pages, best match first, each with its path, " +
			"title, the heading of the best-matching section, a short excerpt and, when known, its URL. Use plain keywords " +
			"(names, field names, error text), not a full question. An empty result means no page matched.",
		Annotations: readOnly,
	}, t.search)
	mcp.AddTool(srv, &mcp.Tool{
		Name: ToolRead,
		Description: "Read a documentation page by the `path` that search_docs returned, or one section of it by its " +
			"`section` (the anchor or the heading). Returns the Markdown text and the page's outline. A long page is " +
			"cut: read it by section.",
		Annotations: readOnly,
	}, t.read)
	return srv, nil
}

// Handler serves srv over streamable HTTP, statelessly: the tools keep nothing between calls, so a client needs
// no session to survive a restart.
func Handler(srv *mcp.Server) http.Handler {
	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
}

type tools struct {
	searcher Searcher
	reader   Reader
	site     string
}

// SearchInput is the input of search_docs.
type SearchInput struct {
	Query string `json:"query" jsonschema:"keywords to look for, for example a field name or an error message"`
	Limit int    `json:"limit,omitempty" jsonschema:"how many pages to return, 1 to 10; 5 if left out"`
}

// SearchResult is one page of the output of search_docs.
type SearchResult struct {
	Path    string `json:"path" jsonschema:"the page's path, to give to read_page and to cite"`
	Title   string `json:"title"`
	Heading string `json:"heading,omitempty" jsonschema:"the heading of the best-matching section"`
	Anchor  string `json:"anchor,omitempty" jsonschema:"the section's anchor, to give to read_page as section"`
	URL     string `json:"url,omitempty" jsonschema:"where the section is published, to cite"`
	Excerpt string `json:"excerpt" jsonschema:"a short passage of the section around the match"`
}

// SearchOutput is the output of search_docs.
type SearchOutput struct {
	Results []SearchResult `json:"results"`
	Note    string         `json:"note,omitempty"`
}

func (t *tools) search(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	limit := in.Limit
	if limit < 1 {
		limit = corpus.DefaultLimit
	}
	limit = min(limit, corpus.MaxLimit)
	hits, err := t.searcher.Search(ctx, in.Query, limit)
	switch {
	case errors.Is(err, corpus.ErrEmptyQuery):
		return nil, SearchOutput{}, errors.New("the query has no searchable word; give keywords such as a field name or an error message")
	case err != nil:
		return nil, SearchOutput{}, fmt.Errorf("search failed: %w", err)
	}
	out := SearchOutput{Results: make([]SearchResult, 0, len(hits))}
	for _, h := range hits {
		out.Results = append(out.Results, SearchResult{
			Path: h.Path, Title: h.Title, Heading: h.Heading, Anchor: h.Anchor,
			URL: PageURL(t.site, h.Path, h.Anchor), Excerpt: h.Excerpt,
		})
	}
	if len(out.Results) == 0 {
		out.Note = "No page matched these words. Try other keywords; if none work, the documentation does not cover the topic."
	}
	return nil, out, nil
}

// ReadInput is the input of read_page.
type ReadInput struct {
	Path    string `json:"path" jsonschema:"the page's path, as search_docs returned it"`
	Section string `json:"section,omitempty" jsonschema:"an anchor or a heading of the page; the whole page if left out"`
}

// OutlineEntry is one heading of a page.
type OutlineEntry struct {
	Heading string `json:"heading"`
	Anchor  string `json:"anchor"`
	Level   int    `json:"level"`
}

// ReadOutput is the output of read_page.
type ReadOutput struct {
	Path      string         `json:"path"`
	Title     string         `json:"title"`
	URL       string         `json:"url,omitempty" jsonschema:"where the page or section is published, to cite"`
	Section   string         `json:"section,omitempty"`
	Anchor    string         `json:"anchor,omitempty"`
	Text      string         `json:"text" jsonschema:"the Markdown source of the page or section"`
	Truncated bool           `json:"truncated,omitempty" jsonschema:"true if the text was cut; read the rest by section"`
	Outline   []OutlineEntry `json:"outline" jsonschema:"the headings of the whole page"`
}

func (t *tools) read(ctx context.Context, _ *mcp.CallToolRequest, in ReadInput) (*mcp.CallToolResult, ReadOutput, error) {
	page, err := t.reader.Read(ctx, in.Path, in.Section)
	switch {
	case errors.Is(err, corpus.ErrInvalidPath):
		return nil, ReadOutput{}, errors.New("invalid page path: give the path exactly as search_docs returned it, for example how-to/example.md")
	case errors.Is(err, corpus.ErrNotFound):
		return nil, ReadOutput{}, errors.New("no such page: use a path that search_docs returned")
	case errors.Is(err, corpus.ErrSectionNotFound):
		return nil, ReadOutput{}, err
	case err != nil:
		return nil, ReadOutput{}, fmt.Errorf("read failed: %w", err)
	}
	out := ReadOutput{
		Path: page.Path, Title: page.Title, URL: PageURL(t.site, page.Path, page.Anchor),
		Section: page.Section, Anchor: page.Anchor, Text: page.Text, Truncated: page.Truncated,
		Outline: make([]OutlineEntry, 0, len(page.Outline)),
	}
	for _, h := range page.Outline {
		out.Outline = append(out.Outline, OutlineEntry{Heading: h.Text, Anchor: h.Anchor, Level: h.Level})
	}
	return nil, out, nil
}

// cleanSiteURL validates a site URL and drops its trailing slash. "" stays "".
func cleanSiteURL(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	if _, err := urlcheck.HTTP(s); err != nil {
		return "", fmt.Errorf("docsmcp: SiteURL %w", err)
	}
	return strings.TrimSuffix(s, "/"), nil
}

// PageURL returns where a corpus page is published, as MkDocs publishes it: "how-to/x.md" is "<site>/how-to/x/",
// "index.md" is "<site>/", and "dir/index.md" is "<site>/dir/". A non-empty anchor is added as the fragment.
// site is the site's address without a trailing slash; with no site it returns "".
func PageURL(site, pagePath, anchor string) string {
	if site == "" {
		return ""
	}
	site = strings.TrimSuffix(site, "/")
	dir := strings.TrimSuffix(pagePath, ".md")
	switch {
	case dir == "index":
		dir = ""
	case strings.HasSuffix(dir, "/index"):
		dir = strings.TrimSuffix(dir, "index")
	default:
		dir += "/"
	}
	u := site + "/" + dir
	if anchor != "" {
		u += "#" + anchor
	}
	return u
}
