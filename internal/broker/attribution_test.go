package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// attributer is a store.Attribution that answers with rows and err, and records the connection it was asked for.
type attributer struct {
	rows store.AttributedRows
	err  error
	got  string
}

func (a *attributer) AttributeConnection(_ context.Context, connection string) (store.AttributedRows, error) {
	a.got = connection
	return a.rows, a.err
}

func TestAttributeStoredRowsNamesTheOwnConnectionAndLogsWhatItChanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		rows    store.AttributedRows
		wantLog []string
	}{
		{"it changed rows", store.AttributedRows{Outbox: 3, Presses: 2}, []string{"level=INFO", "outbox_rows=3", "presses=2", "connection=ns/own"}},
		{"it changed nothing", store.AttributedRows{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			a := &attributer{rows: tc.rows}
			if err := attributeStoredRows(t.Context(), a, slog.New(slog.NewTextHandler(&buf, nil)), "ns/own"); err != nil {
				t.Fatal(err)
			}
			if a.got != "ns/own" {
				t.Errorf("asked for connection %q; want ns/own", a.got)
			}
			if tc.wantLog == nil && buf.Len() != 0 {
				t.Errorf("logged %q; want nothing when nothing changed", buf.String())
			}
			for _, want := range tc.wantLog {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("log %q lacks %q", buf.String(), want)
				}
			}
		})
	}
}

func TestAttributeStoredRowsReturnsTheStoresError(t *testing.T) {
	boom := errors.New("the store is deposed")
	err := attributeStoredRows(t.Context(), &attributer{err: boom}, slog.New(slog.DiscardHandler), "ns/own")
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "ns/own") {
		t.Errorf("attributeStoredRows = %v; want the store's error, with the connection named", err)
	}
}

// Run gives the rows that an earlier broker wrote before it knew connections to its own connection when it starts leading.
func TestRunAttributesTheRowsWrittenBeforeConnectionsToTheOwnConnection(t *testing.T) {
	const own = "agent-kourier-system/slack-main" // the ChatConnection of the example config
	c := startConfig(t)
	seed, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := seed.CreateOutbox(t.Context(), store.OutboxEntry{ID: "old/reply", EventID: "old", Role: store.OutboxReply, Channel: "C0123456789", Thread: "1.1", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := seed.RecordMessageRef(t.Context(), "old/reply", "1.2"); err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}

	// /readyz answers 200 before the leader half starts, so the test waits for the sessions' recovery instead: the rows are
	// attributed before it, and it is the first step of the leader's start that the trace is told of.
	recovered := make(chan struct{})
	var once sync.Once
	c.startTrace = func(step string) {
		if step == "recover-sessions" {
			once.Do(func() { close(recovered) })
		}
	}
	r := start(t, c)
	select {
	case <-recovered:
	case <-time.After(10 * time.Second):
		t.Fatal("the leader half never recovered its sessions")
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}

	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if row, err := st.GetOutbox(t.Context(), "old/reply"); err != nil || row.Connection != own {
		t.Errorf("the outbox row = %+v, %v; want connection %q", row, err, own)
	}
}
