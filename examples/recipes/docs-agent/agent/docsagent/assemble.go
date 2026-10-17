package docsagent

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/corpus"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsmcp"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/httpserve"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/urlcheck"
)

// AssembleOptions say where the agent's pages come from and which model answers.
type AssembleOptions struct {
	// Corpus is the directory of Markdown files, with Exclude and SiteURL as for corpus.Options and docsmcp.Config.
	// It is required unless MCPURL is set.
	Corpus  string
	Exclude []string
	SiteURL string
	// MCPURL, if set, is the streamable HTTP endpoint of another MCP server that offers search_docs and read_page.
	// The agent then serves no corpus of its own, and uses only the tools named search_docs and read_page, whatever
	// else the server offers. That is how a different retrieval, such as a vector search, is
	// attached without changing the agent.
	MCPURL string
	// Model, PublicURL and Subject are as for Config.
	Model     model.LLM
	PublicURL string
	Subject   string
}

// Assemble builds the agent's handler as the docs-agent command runs it. Unless MCPURL is set it loads the corpus,
// serves it as an MCP server on a loopback port that only this process reaches, and points the agent's MCP toolset
// at that. The returned channel receives once, when that embedded server stops, and is nil if there is none; ctx
// ends it.
func Assemble(ctx context.Context, o AssembleOptions) (http.Handler, <-chan error, error) {
	endpoint := o.MCPURL
	var done <-chan error
	var transport mcp.Transport // nil for an external server: the toolset builds its own from the endpoint
	switch {
	case endpoint != "":
		if _, err := urlcheck.HTTP(endpoint); err != nil {
			return nil, nil, fmt.Errorf("docsagent: MCPURL %w", err)
		}
	case o.Corpus == "":
		return nil, nil, errors.New("docsagent: Corpus is required when MCPURL is not set")
	default:
		ix, err := corpus.Load(ctx, o.Corpus, corpus.Options{Exclude: o.Exclude})
		if err != nil {
			return nil, nil, err
		}
		srv, err := docsmcp.New(docsmcp.Config{Searcher: ix, Reader: ix, SiteURL: o.SiteURL})
		if err != nil {
			return nil, nil, err
		}
		endpoint, done, err = httpserve.Loopback(ctx, docsmcp.Handler(srv), 5*time.Second)
		if err != nil {
			return nil, nil, err
		}
		transport = &mcp.StreamableClientTransport{Endpoint: endpoint, HTTPClient: loopbackClient(ctx)}
	}
	set, err := mcptoolset.New(mcptoolset.Config{Endpoint: endpoint, Transport: transport})
	if err != nil {
		return nil, nil, fmt.Errorf("docsagent: build the MCP toolset: %w", err)
	}
	// Only the two tools the prompt names reach the model, whatever else an external server offers.
	tools := tool.FilterToolset(set, tool.AllowedToolsPredicate([]string{docsmcp.ToolSearch, docsmcp.ToolRead}))
	h, err := New(Config{Model: o.Model, Tools: tools, PublicURL: o.PublicURL, Subject: o.Subject})
	if err != nil {
		return nil, nil, err
	}
	return h, done, nil
}

// loopbackClient returns the HTTP client that the agent's MCP toolset uses to reach the embedded server, with a
// transport of its own whose idle connections it closes when ctx ends, as the server shuts down. The transport can
// hold a connection that has carried no request (one dialed for a request that another connection served first),
// and http.Server.Shutdown waits up to about five seconds before it counts such a connection as idle: longer than
// the embedded server's grace, so the stop would fail. Nothing calls the tools by then, because the A2A server has
// drained first.
func loopbackClient(ctx context.Context) *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	context.AfterFunc(ctx, t.CloseIdleConnections)
	return &http.Client{Transport: t}
}
