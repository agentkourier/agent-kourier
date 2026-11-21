// Command livecheck drives a running CMA A2A adapter with Agent Kourier's own A2A client and a2a dialect, the code a
// Binding uses, and checks a text turn, a tool approval allowed and denied, context continuity, and a cancel.
//
//	ADAPTER_URL=http://127.0.0.1:8090/a2a go run .
//
// The agent behind the adapter must have bash set to always_ask. Each scenario spends model tokens.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

func main() {
	url := os.Getenv("ADAPTER_URL")
	if url == "" {
		url = "http://127.0.0.1:8090/a2a"
	}
	only := os.Getenv("ONLY")
	c := newChecker(url)
	failed := 0
	for _, s := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"text", c.text},
		{"allowed", c.allowed},
		{"approve", c.approve},
		{"continuity", c.continuity},
		{"deny", c.deny},
		{"cancel", c.cancel},
	} {
		if only != "" && !strings.Contains(","+only+",", ","+s.name+",") {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
		start := time.Now()
		err := s.run(ctx)
		cancel()
		if err != nil {
			failed++
			fmt.Printf("FAIL %-10s %6.1fs  %v\n", s.name, time.Since(start).Seconds(), err)
		} else {
			fmt.Printf("ok   %-10s %6.1fs\n", s.name, time.Since(start).Seconds())
		}
	}
	if failed > 0 {
		os.Exit(1)
	}
}

type checker struct {
	d     *generic.Dialect
	agent *agent.Agent
	// contextID carries the approve scenario's context into continuity.
	contextID string
	hostname  string
}

func newChecker(url string) *checker {
	d := generic.New(30 * time.Second)
	backend := &v1alpha1.AgentBackend{Spec: v1alpha1.AgentBackendSpec{Dialect: v1alpha1.DialectA2A, URL: url}}
	ref := v1alpha1.AgentRef{Name: "cma"}
	source := func(ctx context.Context) (agent.Conn, error) {
		return d.Connect(ctx, backend, ref, dialect.Credentials{UserID: "livecheck"})
	}
	return &checker{d: d, agent: agent.New(source, agent.Config{ParseInteraction: d.ParseInteraction})}
}

// result is what a send delivered to the handler.
type result struct {
	text    map[a2a.ArtifactID]string
	order   []a2a.ArtifactID
	calls   []agent.ToolCallEvent
	results []agent.ToolResultEvent
	final   *agent.FinalEvent
}

func (r *result) allText() string {
	var b strings.Builder
	for _, id := range r.order {
		b.WriteString(r.text[id])
		b.WriteString("\n")
	}
	return b.String()
}

func (c *checker) send(ctx context.Context, msg *a2a.Message, answer bool) (*result, error) {
	r := &result{text: map[a2a.ArtifactID]string{}}
	h := func(ev agent.Event) {
		switch e := ev.(type) {
		case agent.TextEvent:
			if _, ok := r.text[e.ArtifactID]; !ok {
				r.order = append(r.order, e.ArtifactID)
			}
			r.text[e.ArtifactID] = e.Text
		case agent.ToolCallEvent:
			r.calls = append(r.calls, e)
		case agent.ToolResultEvent:
			r.results = append(r.results, e)
		case agent.FinalEvent:
			r.final = &e
		}
	}
	var err error
	if answer {
		err = c.agent.Answer(ctx, msg, h)
	} else {
		err = c.agent.Send(ctx, msg, h)
	}
	if err != nil {
		return r, err
	}
	if r.final == nil || r.final.Task == nil {
		return r, errors.New("no final event")
	}
	return r, nil
}

func userMessage(contextID, text string) *a2a.Message {
	m := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart(text))
	m.ContextID = contextID
	return m
}

func want(state a2a.TaskState, r *result) error {
	if got := r.final.Task.Status.State; got != state {
		return fmt.Errorf("task state %s, want %s; text: %q", got, state, r.allText())
	}
	return nil
}

func (c *checker) text(ctx context.Context) error {
	r, err := c.send(ctx, userMessage("", "Reply with exactly the word pong and nothing else. Do not use any tools."), false)
	if err != nil {
		return err
	}
	if err := want(a2a.TaskStateCompleted, r); err != nil {
		return err
	}
	if !strings.Contains(strings.ToLower(r.allText()), "pong") {
		return fmt.Errorf("text %q has no pong", r.allText())
	}
	return nil
}

// allowed runs a tool the agent may use without asking: on a self-hosted environment it runs in the sandbox, the
// session idles on it until the worker posts the result, and the turn must complete without a pause. What the tool
// finds does not matter (the worker may refuse paths outside its allowed ones); its call and result must both arrive.
func (c *checker) allowed(ctx context.Context) error {
	r, err := c.send(ctx, userMessage("", "Use the glob tool (not bash) once to list the files matching *, then say how many there are."), false)
	if err != nil {
		return err
	}
	if err := want(a2a.TaskStateCompleted, r); err != nil {
		return err
	}
	if len(r.calls) == 0 || len(r.results) == 0 {
		return fmt.Errorf("tool calls %d, results %d; want a call and its result", len(r.calls), len(r.results))
	}
	return nil
}

// pause sends text and returns the tool approval the turn paused on.
func (c *checker) pause(ctx context.Context, contextID, text string) (*result, *dialect.Interaction, error) {
	r, err := c.send(ctx, userMessage(contextID, text), false)
	if err != nil {
		return r, nil, err
	}
	if err := want(a2a.TaskStateInputRequired, r); err != nil {
		return r, nil, err
	}
	in, err := c.d.ParseInteraction(*r.final.Task)
	if err != nil {
		return r, nil, fmt.Errorf("Kourier cannot read the pause: %w", err)
	}
	if in == nil || in.Kind != dialect.ToolApproval || len(in.Tools) == 0 {
		return r, nil, fmt.Errorf("pause is not a tool approval: %+v", in)
	}
	if len(r.final.AwaitingApproval) == 0 {
		return r, nil, errors.New("final event names no calls awaiting approval")
	}
	return r, in, nil
}

func (c *checker) decide(ctx context.Context, in *dialect.Interaction, approved bool, reason string) (*result, error) {
	ans := dialect.Answer{}
	for _, t := range in.Tools {
		ans.Decisions = append(ans.Decisions, dialect.Decision{ToolID: t.ID, Approved: approved, Reason: reason})
	}
	msg, err := c.d.BuildResponse(*in, ans)
	if err != nil {
		return nil, fmt.Errorf("Kourier cannot build the answer: %w", err)
	}
	return c.send(ctx, &msg, true)
}

func (c *checker) approve(ctx context.Context) error {
	r, in, err := c.pause(ctx, "", "Run the shell command `hostname` with your bash tool, then tell me its output in one line.")
	if err != nil {
		return err
	}
	if in.Tools[0].Name != "bash" {
		return fmt.Errorf("approval is for %q, want bash", in.Tools[0].Name)
	}
	c.contextID = r.final.Task.ContextID
	done, err := c.decide(ctx, in, true, "")
	if err != nil {
		return err
	}
	if err := want(a2a.TaskStateCompleted, done); err != nil {
		return err
	}
	if len(done.results) == 0 {
		return errors.New("no tool result after approval")
	}
	out := fmt.Sprint(done.results[0].Response)
	fmt.Printf("     tool result: %s\n", strings.TrimSpace(out))
	fmt.Printf("     reply: %s\n", strings.TrimSpace(done.allText()))
	c.hostname = strings.TrimSpace(done.allText())
	return nil
}

func (c *checker) continuity(ctx context.Context) error {
	if c.contextID == "" {
		return errors.New("needs the approve scenario first")
	}
	r, err := c.send(ctx, userMessage(c.contextID, "What was the output of the command you just ran? Answer from memory, without tools."), false)
	if err != nil {
		return err
	}
	if err := want(a2a.TaskStateCompleted, r); err != nil {
		return err
	}
	if len(r.calls) > 0 {
		return errors.New("the agent used a tool instead of remembering")
	}
	fmt.Printf("     reply: %s\n", strings.TrimSpace(r.allText()))
	return nil
}

func (c *checker) deny(ctx context.Context) error {
	_, in, err := c.pause(ctx, "", "Run the shell command `touch /workspace/denied-marker` with your bash tool.")
	if err != nil {
		return err
	}
	done, err := c.decide(ctx, in, false, "not allowed in this test")
	if err != nil {
		return err
	}
	if err := want(a2a.TaskStateCompleted, done); err != nil {
		return err
	}
	fmt.Printf("     reply: %s\n", strings.TrimSpace(done.allText()))
	return nil
}

func (c *checker) cancel(ctx context.Context) error {
	_, in, err := c.pause(ctx, "", "Run the shell command `sleep 240` with your bash tool, then say done.")
	if err != nil {
		return err
	}
	// Approve in the background, then cancel the running turn.
	go func() { _, _ = c.decide(context.WithoutCancel(ctx), in, true, "") }()
	time.Sleep(15 * time.Second)
	task, err := c.agent.CancelTask(ctx, in.TaskID)
	if err != nil {
		return fmt.Errorf("cancel: %w", err)
	}
	if task.Status.State != a2a.TaskStateCanceled {
		return fmt.Errorf("after cancel: state %s", task.Status.State)
	}
	return nil
}
