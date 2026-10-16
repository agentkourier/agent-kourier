package smoke_test

import (
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/scriptedmodel"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/smoke"
)

const (
	fixture  = "../corpus/testdata/docs"
	repoDocs = "../../../../../docs" // the repository's own documentation, from this package
)

func TestRunPassesOnTheFixture(t *testing.T) {
	cases := []smoke.Case{
		{Question: "How do I connect an agent?", WantAll: []string{"how-to/connect-agent.md", "https://docs.example.com/how-to/connect-agent/"}},
		{Question: "quantum entanglement", WantAll: []string{"does not cover"}, WantNone: []string{".md"}},
	}
	got, err := smoke.Run(t.Context(), smoke.Options{Docs: fixture, SiteURL: "https://docs.example.com", Model: &scriptedmodel.Model{}, Cases: cases})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) != len(cases) || !strings.Contains(got[0].Answer, "how-to/connect-agent.md") {
		t.Errorf("exchanges = %+v", got)
	}
}

func TestRunNamesTheFailedExpectation(t *testing.T) {
	tests := []struct {
		name string
		c    smoke.Case
		want string
	}{
		{"a citation that is missing", smoke.Case{Question: "How do I connect an agent?", WantAll: []string{"reference/errors.md"}}, `"reference/errors.md"`},
		{"text that must not appear", smoke.Case{Question: "How do I connect an agent?", WantNone: []string{"connect-agent.md"}}, `"connect-agent.md"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := smoke.Run(t.Context(), smoke.Options{Docs: fixture, Model: &scriptedmodel.Model{}, Cases: []smoke.Case{tt.c}})
			if err == nil || !strings.Contains(err.Error(), tt.want) || !strings.Contains(err.Error(), "How do I connect an agent?") {
				t.Fatalf("error = %v, want one naming the question and %s", err, tt.want)
			}
		})
	}
}

func TestRunRefusesBadOptions(t *testing.T) {
	tests := []struct {
		name string
		opts smoke.Options
	}{
		{"no model", smoke.Options{Docs: fixture, Cases: []smoke.Case{{Question: "q"}}}},
		{"no cases", smoke.Options{Docs: fixture, Model: &scriptedmodel.Model{}}},
		{"no corpus", smoke.Options{Docs: "testdata/missing", Model: &scriptedmodel.Model{}, Cases: []smoke.Case{{Question: "q"}}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := smoke.Run(t.Context(), tt.opts); err == nil {
				t.Fatal("Run succeeded")
			}
		})
	}
}

// TestDefaultCasesOnTheRepositoryDocs is the loopback smoke: the agent, the MCP server and the repository's own
// documentation, with the scripted model, answering the fixed questions and citing the right page.
func TestDefaultCasesOnTheRepositoryDocs(t *testing.T) {
	got, err := smoke.Run(t.Context(), smoke.Options{
		Docs: repoDocs, Exclude: smoke.DocsExclude, SiteURL: smoke.DocsSiteURL,
		Model: &scriptedmodel.Model{}, Cases: smoke.DefaultCases(),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(got) < 3 {
		t.Fatalf("exchanges = %d, want the default cases", len(got))
	}
	if !strings.Contains(got[0].Answer, "https://agentkourier.dev/how-to/connect-a2a-agent/") {
		t.Errorf("the first answer does not cite the connect page's URL: %q", got[0].Answer)
	}
}
