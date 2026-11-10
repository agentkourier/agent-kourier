package broker

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
)

// attributer is a store.Attribution that answers with rows and err, and records the connection it was asked for. The count
// UnattributedRows reads is unattributed, or unattributedErr.
type attributer struct {
	rows            store.AttributedRows
	err             error
	got             string
	unattributed    store.AttributedRows
	unattributedErr error
}

func (a *attributer) AttributeConnection(_ context.Context, connection string) (store.AttributedRows, error) {
	a.got = connection
	return a.rows, a.err
}

func (a *attributer) UnattributedRows(context.Context) (store.AttributedRows, error) {
	return a.unattributed, a.unattributedErr
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

func TestRefuseUnattributedRowsPassesWhenThereAreNone(t *testing.T) {
	if err := refuseUnattributedRows(t.Context(), &attributer{}); err != nil {
		t.Errorf("refuseUnattributedRows = %v; want nil: no row lacks a connection", err)
	}
}

// With no own connection to give them to, the rows that have no connection stop the start, and the operator is told how
// to give them an owner. The error holds the counts and names, never a row.
func TestRefuseUnattributedRowsWrapsErrWiringAndSaysWhatToDo(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows store.AttributedRows
		want []string
	}{
		{"outbox rows and presses", store.AttributedRows{Outbox: 7, Presses: 3}, []string{"7 outbox rows", "3 button presses"}},
		{"outbox rows only", store.AttributedRows{Outbox: 2}, []string{"2 outbox rows", "0 button presses"}},
		{"presses only", store.AttributedRows{Presses: 1}, []string{"0 outbox rows", "1 button presses"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &attributer{unattributed: tc.rows}
			err := refuseUnattributedRows(t.Context(), a)
			if !errors.Is(err, ErrWiring) {
				t.Fatalf("refuseUnattributedRows = %v; want an error wrapping ErrWiring (exit 2)", err)
			}
			for _, want := range append(tc.want, "config.chatConnection", "AGENTKOURIER_CHAT_CONNECTION", "unset it") {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q lacks %q", err, want)
				}
			}
			if a.got != "" {
				t.Errorf("it attributed the rows to %q; it must only count them", a.got)
			}
		})
	}
}

// Against a real store: the count is what the rows are, and the message holds nothing a row held.
func TestRefuseUnattributedRowsHoldsNoRowContent(t *testing.T) {
	st, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "agent-kourier.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	const secretText = "xoxb-not-a-real-token deploy password hunter2"
	if _, err := st.CreateOutbox(t.Context(), store.OutboxEntry{ID: "old/reply-id", EventID: "evt-secret-id", Role: store.OutboxReply, Channel: "C0SECRETCH", Thread: "1700000000.000100", CreatedAt: time.Now(), Payload: `{"text":"` + secretText + `"}`}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RecordPress(t.Context(), store.Press{InteractionID: "ix-secret-id", User: "USECRETUSR", Channel: "C0SECRETCH", MessageRef: "1700000000.000200", Inputs: []byte(`{"note":"` + secretText + `"}`), PressedAt: time.Now()}, store.PressLimits{Total: 10, PerUser: 10}); err != nil {
		t.Fatal(err)
	}
	err = refuseUnattributedRows(t.Context(), st)
	if !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "1 outbox rows and 1 button presses") {
		t.Fatalf("refuseUnattributedRows = %v; want ErrWiring naming one outbox row and one press", err)
	}
	for _, leak := range []string{secretText, "xoxb", "hunter2", "old/reply-id", "evt-secret-id", "C0SECRETCH", "1700000000", "ix-secret-id", "USECRETUSR"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("error %q holds row content %q", err, leak)
		}
	}
	// Once the rows have an owner the start goes on, which is what the message tells the operator to do.
	if _, err := st.AttributeConnection(t.Context(), "ns/own"); err != nil {
		t.Fatal(err)
	}
	if err := refuseUnattributedRows(t.Context(), st); err != nil {
		t.Errorf("refuseUnattributedRows after the rows have an owner = %v; want nil", err)
	}
}

func TestRefuseUnattributedRowsReturnsTheStoresErrorAsNotAWiringFault(t *testing.T) {
	boom := errors.New("the store is deposed")
	err := refuseUnattributedRows(t.Context(), &attributer{unattributedErr: boom})
	if !errors.Is(err, boom) {
		t.Errorf("refuseUnattributedRows = %v; want the store's error", err)
	}
	if errors.Is(err, ErrWiring) {
		t.Errorf("refuseUnattributedRows = %v; a store that cannot be read is not the operator's wiring: a restart may mend it", err)
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

	r := start(t, c)
	r.until("/readyz", http.StatusOK) // ready does not mean leading: the attribution runs when the leader starts

	st, err := sqlite.Open(t.Context(), c.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	var row store.OutboxEntry
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if row, err = st.GetOutbox(t.Context(), "old/reply"); err == nil && row.Connection == own {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the outbox row = %+v, %v; want connection %q", row, err, own)
		}
	}
	r.cancel()
	if err := r.wait(); err != nil {
		t.Fatal(err)
	}
}
