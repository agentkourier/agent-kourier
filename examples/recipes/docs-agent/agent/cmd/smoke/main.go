// Command smoke runs the docs agent on loopback and asks it questions over A2A.
//
// By default it uses the scripted model, which needs no network and no credentials, and checks fixed questions
// against the repository's documentation. With -live it uses the model named by MODEL_PROVIDER, MODEL_NAME and
// MODEL_API_KEY (and MODEL_BASE_URL), which costs model usage, and checks only that the first answer cites a page of
// the documentation; it prints the answers for a person to read. It prints no credential.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"

	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/docsagent"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/scriptedmodel"
	"github.com/agentkourier/agent-kourier/examples/recipes/docs-agent/agent/smoke"
)

func main() { os.Exit(realMain()) }

func realMain() int {
	docs := flag.String("docs", "../../../../docs", "the documentation directory to answer from")
	live := flag.Bool("live", false, "use the real model named by the MODEL_* variables instead of the scripted one")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	var (
		llm   model.LLM = &scriptedmodel.Model{}
		cases           = smoke.DefaultCases()
	)
	if *live {
		var err error
		if llm, err = docsagent.ModelFromEnv(ctx, os.Getenv); err != nil {
			fmt.Fprintln(os.Stderr, "docs-agent live smoke:", err)
			return 1
		}
		// A real model words its answers its own way: ask the first question and require a citation, and show the
		// other for a person to judge.
		cases = []smoke.Case{
			{Question: cases[0].Question, WantAll: []string{"how-to/connect-a2a-agent"}},
			{Question: "Can Agent Kourier post to Microsoft Teams?"},
		}
	}
	got, err := smoke.Run(ctx, smoke.Options{Docs: *docs, Exclude: smoke.DocsExclude, SiteURL: smoke.DocsSiteURL, Model: llm, Cases: cases})
	for _, e := range got {
		fmt.Printf("Q: %s\nA: %s\n\n", e.Question, strings.TrimSpace(e.Answer))
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAIL:", err)
		return 1
	}
	fmt.Printf("PASS: %d question(s) answered over A2A with the %s model.\n", len(got), llm.Name())
	return 0
}
