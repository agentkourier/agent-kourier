//go:build testhooks

package outbox_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// loudPoster posts without a network and says so on stdout, which is how a parent test learns that
// a child process got as far as the post.
type loudPoster struct{}

func (loudPoster) Post(_ context.Context, _, _ string, _ slack.Content, id string) (string, error) {
	fmt.Println("posted", id)
	return "100.001", nil
}

func (loudPoster) Find(context.Context, string, string, string, time.Time) (string, bool, error) {
	return "", false, nil
}

// TestFaultHookChild is the child process of TestFaultHookKillsBetweenPostAndRecord, and does nothing when run on its own.
func TestFaultHookChild(t *testing.T) {
	path := os.Getenv("OUTBOX_CRASH_DB")
	if path == "" {
		t.Skip("only runs as the child of TestFaultHookKillsBetweenPostAndRecord")
	}
	ts, err := newOutbox(t, openStore(t, path), loudPoster{}).Send(t.Context(), reply("ev1"))
	fmt.Println("recorded", ts, err)
}

// child runs TestFaultHookChild in a new process on the database at path, with COURIER_FAULT set to
// fault, and returns its output and how it ended.
func child(t *testing.T, path, fault string) (string, error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestFaultHookChild$")
	cmd.Env = append(os.Environ(), "OUTBOX_CRASH_DB="+path, "COURIER_FAULT="+fault)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestFaultHookKillsBetweenPostAndRecord is S5's server half: armed, the process exits with 137 between the post and the record,
// so the message is in Slack and the row has no ref; unarmed, the same code records it.
func TestFaultHookKillsBetweenPostAndRecord(t *testing.T) {
	dir := t.TempDir()
	crashed := filepath.Join(dir, "crash.db")
	out, err := child(t, crashed, "crash-after-post")
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 137 {
		t.Fatalf("armed child: %v, output %q; want exit code 137", err, out)
	}
	if !strings.Contains(out, "posted ev1/reply") || strings.Contains(out, "recorded") {
		t.Fatalf("armed child output %q; want it to have posted and never recorded", out)
	}
	row, err := openStore(t, crashed).GetOutbox(t.Context(), "ev1/reply")
	if err != nil || row.MessageRef != "" || row.Payload == "" {
		t.Fatalf("row after the crash = %+v, %v; want it unrecorded with its payload, for Reconcile", row, err)
	}

	for _, fault := range []string{"", "no-such-fault"} {
		out, err := child(t, filepath.Join(dir, "calm"+fault+".db"), fault)
		if err != nil || !strings.Contains(out, "recorded 100.001 <nil>") {
			t.Errorf("child with COURIER_FAULT=%q: %v, output %q; want it to record", fault, err, out)
		}
	}
}
