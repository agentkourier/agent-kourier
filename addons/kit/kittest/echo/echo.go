// Package echo is the kit's reference add-on: a backend that answers each message with its own text, asks a question
// when a message says "pause", and runs until it is canceled when one says "hold". The kit's own tests run it, through
// package kittest, on Postgres; an add-on's tests run it on SQLite under the driver the add-on links (the kit links
// none). It shows what an add-on writes on the kit: an Addon, a store opened on the kit's, and an a2asrv.AgentExecutor
// with the hooks it needs. Only tests import it.
//
// The sections between the --8<-- comments are the code of docs/how-to/write-an-add-on.md, which includes them (the
// snippets extension of mkdocs.yml): keep each one whole and readable on its own.
package echo

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"

	"github.com/agentkourier/agent-kourier/addons/kit/kittest"
	"github.com/agentkourier/agent-kourier/addons/kit/runtime"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
	"github.com/agentkourier/agent-kourier/addons/kit/store"
	"github.com/agentkourier/agent-kourier/addons/kit/telemetry"
)

// The texts the backend acts on, and the question it asks.
const (
	Pause    = "pause"
	Hold     = "hold"
	Question = "Which one?"
)

// --8<-- [start:setting]

// greeting is the echo's own setting, which the reply starts with: the add-on's own variables are read with the
// shared ones.
var greeting = settings.Func("ECHO_GREETING", "echo", func(v string, _ settings.Values) (string, error) {
	if strings.TrimSpace(v) == "" {
		return "", errors.New("ECHO_GREETING is blank")
	}
	return v, nil
})

// --8<-- [end:setting]

// --8<-- [start:addon]

// Addon is the echo add-on, version version, named echo: metrics agentkourier_echo_*, process "agent".
func Addon(version string) runtime.Addon[*store.Store] {
	return runtime.Addon[*store.Store]{
		Name: "echo", Noun: "agent", Version: version,
		Telemetry: telemetry.Spec{Calls: "backend", TurnsHelp: "Turns the echo ran, by how they ended.",
			OperationHelp: "Duration of the echo's calls.", AuthRefusedHelp: "Requests the caller check refused, by reason."},
		Settings: []settings.Var{greeting},
		Card:     runtime.Card{Name: "echo", Description: "The kit's reference add-on"},
		Open:     Open,
		Build: func(_ context.Context, env runtime.Env[*store.Store]) (runtime.Backend, error) {
			return &Backend{Greeting: greeting.From(env.Settings)}, nil
		},
	}
}

// --8<-- [end:addon]

// --8<-- [start:open]

// Open opens the kit's task state on the SQLite file or the Postgres database cfg names, with each task's create and
// update marking its context active, so that the upkeep's idle sweep deletes a context whose tasks have not changed.
func Open(ctx context.Context, cfg settings.Config, opts runtime.StoreOptions) (*store.Store, error) {
	o := store.Options{Noun: "agent", Log: opts.Log, OnDeposed: opts.OnDeposed, MigrationLockKey: 0x6563686f5f6d6967} // "echo_mig"
	var db *store.DB
	var err error
	if cfg.Store == settings.StorePostgres {
		db, err = store.OpenPostgres(ctx, cfg.PostgresDSN, o)
	} else {
		db, err = store.OpenSQLite(ctx, cfg.SQLitePath, o)
	}
	if err != nil {
		return nil, err
	}
	return store.NewStore(db, store.TaskOptions{ContextActivity: true}), nil
}

// --8<-- [end:open]

// --8<-- [start:backend]

// Backend answers the turns.
type Backend struct {
	// Greeting starts each reply.
	Greeting string
}

var (
	_ runtime.Backend          = (*Backend)(nil)
	_ runtime.MessageValidator = (*Backend)(nil)
)

// --8<-- [end:backend]

// --8<-- [start:execute]

// Execute answers the message: a question for "pause", a wait until the turn ends for "hold", the answer to a paused
// question, or the message's own text.
func (b *Backend) Execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		text, err := text(ec.Message)
		if err != nil {
			yield(nil, err)
			return
		}
		if ec.StoredTask == nil && !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
			return
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		paused := ec.StoredTask != nil && ec.StoredTask.Status.State == a2a.TaskStateInputRequired
		switch {
		case paused:
			text = "answered: " + text
		case text == Pause:
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart(Question))), nil)
			return
		case text == Hold:
			<-ctx.Done()
			yield(nil, ctx.Err())
			return
		}
		if !yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart(b.Greeting+": "+text)), nil) {
			return
		}
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil)
	}
}

// --8<-- [end:execute]

// Cancel ends the task canceled.
func (b *Backend) Cancel(_ context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCanceled, nil), nil)
	}
}

// --8<-- [start:validate]

// ValidateMessage accepts a message, new or an answer, of text alone, not blank: the one Execute answers.
func (b *Backend) ValidateMessage(_ context.Context, msg *a2a.Message, _ *a2a.Task) error {
	_, err := text(msg)
	return err
}

// --8<-- [end:validate]

// text is msg's text, its text parts joined, or why it has none.
func text(msg *a2a.Message) (string, error) {
	if msg == nil {
		return "", a2a.ErrInvalidParams
	}
	var parts []string
	for _, p := range msg.Parts {
		if p == nil {
			return "", a2a.ErrInvalidParams
		}
		t, ok := p.Content.(a2a.Text)
		if !ok {
			return "", fmt.Errorf("%w: only text parts are answered", a2a.ErrInvalidParams)
		}
		parts = append(parts, string(t))
	}
	joined := strings.TrimSpace(strings.Join(parts, "\n"))
	if joined == "" {
		return "", fmt.Errorf("%w: the message has no text", a2a.ErrInvalidParams)
	}
	return joined, nil
}

// --8<-- [start:subject]

// Subject is the echo add-on as kittest drives it, version 1.2.3-test.
func Subject() kittest.Subject[*store.Store] {
	return kittest.Subject[*store.Store]{
		Addon: Addon("1.2.3-test"), Env: map[string]string{"ECHO_GREETING": "hi"},
		Complete: "hello", Pause: Pause, Hold: Hold, Answer: "the first one",
		BadAnswer:  []*a2a.Part{a2a.NewDataPart(map[string]any{"not": "text"})},
		BadMessage: []*a2a.Part{a2a.NewTextPart("  ")},
	}
}

// --8<-- [end:subject]
