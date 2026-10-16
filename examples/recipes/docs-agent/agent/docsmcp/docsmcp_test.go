package docsmcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsmcp"
)

const fixture = "../corpus/testdata/docs"

func loadIndex(t *testing.T) *corpus.Index {
	t.Helper()
	ix, err := corpus.Load(context.Background(), fixture, corpus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return ix
}

// connect starts a server over an in-memory transport and returns a client session on it.
func connect(t *testing.T, cfg docsmcp.Config) *mcp.ClientSession {
	t.Helper()
	srv, err := docsmcp.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ct, st := mcp.NewInMemoryTransports()
	ctx := t.Context()
	ss, err := srv.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// result is a tool call's outcome: the structured output, or the error text.
type result struct {
	isError bool
	text    string
	out     map[string]any
}

func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) result {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool(%s): protocol error: %v", name, err)
	}
	r := result{isError: res.IsError}
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			r.text += tc.Text
		}
	}
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		if err := json.Unmarshal(b, &r.out); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestToolsAreListedReadOnly(t *testing.T) {
	cs := connect(t, docsmcp.Config{Searcher: loadIndex(t), Reader: loadIndex(t)})
	list, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]*mcp.Tool{}
	for _, tool := range list.Tools {
		got[tool.Name] = tool
	}
	if len(got) != 2 || got[docsmcp.ToolSearch] == nil || got[docsmcp.ToolRead] == nil {
		t.Fatalf("tools = %v, want exactly %s and %s", got, docsmcp.ToolSearch, docsmcp.ToolRead)
	}
	for name, tool := range got {
		if tool.Description == "" {
			t.Errorf("%s has no description", name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("%s is not marked read-only", name)
		}
		if tool.Annotations != nil && tool.Annotations.OpenWorldHint != nil && *tool.Annotations.OpenWorldHint {
			t.Errorf("%s is marked open-world; it reads a fixed corpus", name)
		}
		if tool.InputSchema == nil || tool.OutputSchema == nil {
			t.Errorf("%s lacks an input or an output schema", name)
		}
	}
}

func TestSearchDocs(t *testing.T) {
	ix := loadIndex(t)
	cs := connect(t, docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: "https://docs.example.com/"})
	r := call(t, cs, docsmcp.ToolSearch, map[string]any{"query": "credentials rejected", "limit": 2})
	if r.isError {
		t.Fatalf("error: %s", r.text)
	}
	results, _ := r.out["results"].([]any)
	if len(results) == 0 || len(results) > 2 {
		t.Fatalf("results = %v, want 1 or 2", r.out)
	}
	first := results[0].(map[string]any)
	want := map[string]string{
		"path":    "reference/errors.md",
		"title":   "Error codes",
		"heading": "Credentials rejected",
		"anchor":  "credentials-rejected",
		"url":     "https://docs.example.com/reference/errors/#credentials-rejected",
	}
	for k, v := range want {
		if first[k] != v {
			t.Errorf("results[0].%s = %v, want %q", k, first[k], v)
		}
	}
	if e, _ := first["excerpt"].(string); !strings.Contains(e, "401") {
		t.Errorf("excerpt = %q, want a passage with the match", e)
	}
	if _, ok := first["score"]; ok {
		t.Error("the score is not part of the tool's output; the order is the ranking")
	}
}

// TestSearchDocsClampsTheLimit pins that the tool returns at most corpus.MaxLimit pages whatever the caller asks.
func TestSearchDocsClampsTheLimit(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < corpus.MaxLimit+3; i++ {
		name := filepath.Join(dir, fmt.Sprintf("page-%02d.md", i))
		if err := os.WriteFile(name, []byte("# Page\n\nwidgets everywhere\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ix, err := corpus.Load(t.Context(), dir, corpus.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cs := connect(t, docsmcp.Config{Searcher: ix, Reader: ix})
	r := call(t, cs, docsmcp.ToolSearch, map[string]any{"query": "widgets", "limit": 1000})
	if r.isError {
		t.Fatalf("error: %s", r.text)
	}
	if got := len(r.out["results"].([]any)); got != corpus.MaxLimit {
		t.Errorf("results = %d, want %d", got, corpus.MaxLimit)
	}
}

func TestSearchDocsWithoutASiteURLOmitsURL(t *testing.T) {
	ix := loadIndex(t)
	cs := connect(t, docsmcp.Config{Searcher: ix, Reader: ix})
	r := call(t, cs, docsmcp.ToolSearch, map[string]any{"query": "storm"})
	first := r.out["results"].([]any)[0].(map[string]any)
	if _, ok := first["url"]; ok {
		t.Errorf("url present without a site URL: %v", first)
	}
}

func TestSearchDocsNoMatch(t *testing.T) {
	ix := loadIndex(t)
	cs := connect(t, docsmcp.Config{Searcher: ix, Reader: ix})
	r := call(t, cs, docsmcp.ToolSearch, map[string]any{"query": "quantum entanglement"})
	if r.isError {
		t.Fatalf("no match is an answer, not an error: %s", r.text)
	}
	if results, _ := r.out["results"].([]any); len(results) != 0 {
		t.Errorf("results = %v", results)
	}
	if note, _ := r.out["note"].(string); !strings.Contains(note, "No page") {
		t.Errorf("note = %q, want a statement that nothing matched", note)
	}
}

func TestSearchDocsErrors(t *testing.T) {
	ix := loadIndex(t)
	cs := connect(t, docsmcp.Config{Searcher: ix, Reader: ix})
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"a query of stopwords", map[string]any{"query": "the of and"}, "searchable"},
		{"a missing query", map[string]any{}, "query"},
		{"a limit of the wrong type", map[string]any{"query": "agent", "limit": "many"}, "limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := call(t, cs, docsmcp.ToolSearch, tt.args)
			if !r.isError || !strings.Contains(r.text, tt.want) {
				t.Errorf("result = %+v, want a tool error mentioning %q", r, tt.want)
			}
		})
	}
}

func TestReadPage(t *testing.T) {
	ix := loadIndex(t)
	cs := connect(t, docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: "https://docs.example.com"})

	whole := call(t, cs, docsmcp.ToolRead, map[string]any{"path": "how-to/connect-agent.md"})
	if whole.isError {
		t.Fatalf("error: %s", whole.text)
	}
	if whole.out["title"] != "Connect a generic agent" || whole.out["url"] != "https://docs.example.com/how-to/connect-agent/" {
		t.Errorf("whole page = %v", whole.out)
	}
	if text, _ := whole.out["text"].(string); !strings.Contains(text, "dialect: a2a") {
		t.Errorf("text = %q", text)
	}
	if outline, _ := whole.out["outline"].([]any); len(outline) != 5 {
		t.Errorf("outline = %v, want the page's five headings", whole.out["outline"])
	}

	sec := call(t, cs, docsmcp.ToolRead, map[string]any{"path": "how-to/connect-agent.md", "section": "static-headers"})
	if sec.isError {
		t.Fatalf("error: %s", sec.text)
	}
	if sec.out["anchor"] != "static-headers" || sec.out["url"] != "https://docs.example.com/how-to/connect-agent/#static-headers" {
		t.Errorf("section = %v", sec.out)
	}
	if text, _ := sec.out["text"].(string); strings.Contains(text, "dialect: a2a") || !strings.Contains(text, "identity are refused") {
		t.Errorf("section text = %q", text)
	}
}

func TestReadPageErrors(t *testing.T) {
	ix := loadIndex(t)
	cs := connect(t, docsmcp.Config{Searcher: ix, Reader: ix})
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"a path that climbs", map[string]any{"path": "../secret.md"}, "invalid"},
		{"an absolute path", map[string]any{"path": "/etc/passwd"}, "invalid"},
		{"a page that is not there", map[string]any{"path": "how-to/nope.md"}, "search_docs"},
		{"a section that is not there", map[string]any{"path": "how-to/connect-agent.md", "section": "nope"}, "check-it"},
		{"no path", map[string]any{}, "path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := call(t, cs, docsmcp.ToolRead, tt.args)
			if !r.isError || !strings.Contains(r.text, tt.want) {
				t.Errorf("result = %+v, want a tool error mentioning %q", r, tt.want)
			}
			if strings.Contains(r.text, "testdata") || strings.Contains(r.text, "/Users/") {
				t.Errorf("the error names a directory on disk: %q", r.text)
			}
		})
	}
}

// fakeSearcher stands in for a vector search. It is the point of the Searcher interface: the tools do not care how
// the pages were found.
type fakeSearcher struct{ gotQuery string }

func (f *fakeSearcher) Search(_ context.Context, query string, _ int) ([]corpus.Hit, error) {
	f.gotQuery = query
	return []corpus.Hit{{Path: "index.md", Title: "Welcome to Acme Relay", Heading: "Where to start", Anchor: "where-to-start", Excerpt: "semantic hit", Score: 0.9}}, nil
}

func TestAnotherSearcherReplacesKeywordSearch(t *testing.T) {
	fs := &fakeSearcher{}
	cs := connect(t, docsmcp.Config{Searcher: fs, Reader: loadIndex(t)})
	r := call(t, cs, docsmcp.ToolSearch, map[string]any{"query": "how do I begin"})
	first := r.out["results"].([]any)[0].(map[string]any)
	if fs.gotQuery != "how do I begin" || first["excerpt"] != "semantic hit" {
		t.Errorf("query %q, result %v", fs.gotQuery, r.out)
	}
	if read := call(t, cs, docsmcp.ToolRead, map[string]any{"path": "index.md"}); read.isError {
		t.Errorf("the reader still serves the pages: %s", read.text)
	}
}

func TestNewValidates(t *testing.T) {
	ix := loadIndex(t)
	tests := []struct {
		name string
		cfg  docsmcp.Config
	}{
		{"no searcher", docsmcp.Config{Reader: ix}},
		{"no reader", docsmcp.Config{Searcher: ix}},
		{"a relative site URL", docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: "/docs"}},
		{"a site URL with a query", docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: "https://x.example/?a=1"}},
		{"a site URL with credentials", docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: "https://user:pw@x.example"}},
		{"a site URL that is not http", docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: "ftp://x.example"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := docsmcp.New(tt.cfg); err == nil {
				t.Fatal("New succeeded")
			}
		})
	}
}

func TestPageURL(t *testing.T) {
	tests := []struct {
		site, page, anchor, want string
	}{
		{"https://d.example", "index.md", "", "https://d.example/"},
		{"https://d.example/", "how-to/x.md", "", "https://d.example/how-to/x/"},
		{"https://d.example/docs/", "how-to/x.md", "step-1", "https://d.example/docs/how-to/x/#step-1"},
		{"https://d.example", "how-to/index.md", "", "https://d.example/how-to/"},
		{"", "how-to/x.md", "a", ""},
	}
	for _, tt := range tests {
		if got := docsmcp.PageURL(tt.site, tt.page, tt.anchor); got != tt.want {
			t.Errorf("PageURL(%q, %q, %q) = %q, want %q", tt.site, tt.page, tt.anchor, got, tt.want)
		}
	}
}

// TestServesOverStreamableHTTP drives the handler a deployment mounts, with the client transport an MCP client
// uses to reach it.
func TestServesOverStreamableHTTP(t *testing.T) {
	ix := loadIndex(t)
	srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(docsmcp.Handler(srv))
	defer ts.Close()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: ts.URL}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	r := call(t, cs, docsmcp.ToolSearch, map[string]any{"query": "storm"})
	if r.isError || len(r.out["results"].([]any)) == 0 {
		t.Errorf("result over HTTP = %+v", r)
	}
}
