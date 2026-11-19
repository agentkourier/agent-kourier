package config

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/goleak"

	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// errTwoConnections is what the Accept of these tests says of a snapshot with other than one ChatConnection.
var errTwoConnections = errors.New("not one ChatConnection")

// oneConnection is an Accept that refuses a snapshot with other than one ChatConnection, and records what it was asked.
type oneConnection struct{ current, next []*Snapshot }

func (o *oneConnection) accept(current, next *Snapshot) error {
	o.current, o.next = append(o.current, current), append(o.next, next)
	if len(next.ChatConnections()) != 1 {
		return errTwoConnections
	}
	return nil
}

// A snapshot that Accept refuses is refused as a bad file is: the last good snapshot stays, the status is stale and counts the failure,
// OnError and the log are told, and an unchanged directory is not read again. Accept is asked with the snapshot it would replace, nil for
// the first, and a directory that it accepts again is published.
func TestReloaderRefusesASnapshotThatAcceptRefuses(t *testing.T) {
	dir := specDir(t)
	var seen []error
	var logs bytes.Buffer
	o := &oneConnection{}
	r := NewReloader(ReloaderConfig{Dir: dir, Secrets: specSecrets(), Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		OnError: func(err error) { seen = append(seen, err) }, Accept: o.accept})
	mustReload(t, r, true)
	first := r.Current()
	if len(o.current) != 1 || o.current[0] != nil || o.next[0] != first {
		t.Fatalf("the first load asked Accept with current %v, next %v; want nil and the snapshot published", o.current, o.next)
	}

	second := filepath.Join(dir, "chatconnection2.yaml")
	writeFile(t, second, strings.Replace(mustRead(t, filepath.Join(dir, "chatconnection.yaml")), "name: slack-main,", "name: slack-other,", 1))
	err := r.reload(context.Background(), false)
	if !errors.Is(err, errTwoConnections) {
		t.Fatalf("a reload Accept refuses = %v; want its error", err)
	}
	if st := r.Status(); r.Current() != first || !st.Stale || st.Errors != 1 {
		t.Errorf("after the refusal: snapshot kept %v, status %+v; want the first, stale, one error", r.Current() == first, st)
	}
	if len(o.current) != 2 || o.current[1] != first || len(o.next[1].ChatConnections()) != 2 {
		t.Errorf("the refused reload asked Accept with current %v, next %v; want the published snapshot and the one with two", o.current, o.next)
	}
	if len(seen) != 1 || !errors.Is(seen[0], errTwoConnections) {
		t.Errorf("OnError saw %v; want the refusal once", seen)
	}
	if !strings.Contains(logs.String(), `msg="config reload failed; keeping the last good snapshot" error="not one ChatConnection"`) {
		t.Errorf("the refusal is not logged as a failed reload:\n%s", logs.String())
	}
	if err := r.reload(context.Background(), false); err != nil || len(o.next) != 2 {
		t.Errorf("an unchanged directory was read again: %v, Accept asked %d times", err, len(o.next))
	}

	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	mustReload(t, r, false)
	if st := r.Status(); r.Current() == first || st.Stale || st.Errors != 1 {
		t.Errorf("after the directory was put back: new snapshot %v, status %+v", r.Current() != first, st)
	}
	if o.current[2] != first {
		t.Errorf("the reload after the refusal asked Accept with %v; want the last good snapshot", o.current[2])
	}
}

// A first load that Accept refuses fails Start, as a bad file does.
func TestReloaderStartFailsWhenAcceptRefusesTheFirstLoad(t *testing.T) {
	r := NewReloader(ReloaderConfig{Dir: specDir(t), Secrets: specSecrets(), Logger: testutil.DiscardLogger(),
		Accept: func(*Snapshot, *Snapshot) error { return errTwoConnections }})
	if err := r.Start(context.Background()); !errors.Is(err, errTwoConnections) || r.Current() != nil {
		t.Errorf("Start = %v, snapshot %v; want the refusal and no snapshot", err, r.Current())
	}
	r.Wait()
	goleak.VerifyNone(t)
}
