package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"golang.org/x/sys/unix"
)

func TestCommandOutputSources(t *testing.T) {
	workspace := t.TempDir()
	out := filepath.Join(workspace, "output")
	for _, test := range []struct {
		name, script, source, text string
		setup                      func() error
		exit                       int
	}{
		{"missing", "printf stdout; printf stderr >&2", "stdout_stderr", "stdoutstderr", nil, 0},
		{"present", "printf stream", "file", "file", func() error { return os.WriteFile(out, []byte("file"), 0600) }, 0},
		{"empty", "printf stream", "file", "", func() error { return os.WriteFile(out, nil, 0600) }, 0},
		{"symlink", "printf stream", "stdout_stderr", "stream", func() error { return os.Symlink("unread", out) }, 0},
		{"directory", "printf stream", "stdout_stderr", "stream", func() error { return os.Mkdir(out, 0700) }, 0},
		{"fifo", "printf stream", "stdout_stderr", "stream", func() error { return unix.Mkfifo(out, 0600) }, 0},
		{"signal", "kill -TERM $$", "none", "", nil, 143},
		{"failure", "printf 'failed' >&2; exit 7", "stdout_stderr", "failed", nil, 7},
	} {
		t.Run(test.name, func(t *testing.T) {
			_ = os.Remove(out)
			if test.setup != nil {
				if err := test.setup(); err != nil {
					t.Fatal(err)
				}
			}
			got := execute(t.Context(), []string{"sh", "-c", test.script}, workspace, out)
			if got.ExitCode != test.exit || got.OutputSource != test.source || got.Output != test.text || got.Truncated {
				t.Fatalf("exit %d source %q output %q truncated %t", got.ExitCode, got.OutputSource, got.Output, got.Truncated)
			}
		})
	}
}

func TestOutputLimitAndUTF8(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "output")
	if err := os.WriteFile(file, []byte(strings.Repeat("x", result.OutputLimit-1)+"界\xff"), 0600); err != nil {
		t.Fatal(err)
	}
	got := execute(t.Context(), []string{"true"}, root, file)
	if !got.Truncated || len(got.Output) > result.OutputLimit || !utf8.ValidString(got.Output) {
		t.Fatalf("output bytes %d, truncated %t, UTF-8 %t", len(got.Output), got.Truncated, utf8.ValidString(got.Output))
	}
	var tail outputTail
	_, _ = tail.Write(bytes.Repeat([]byte("x"), result.OutputLimit))
	_, _ = tail.Write([]byte("tail"))
	if !tail.truncated || len(tail.data) != result.OutputLimit || !bytes.HasSuffix(tail.data, []byte("tail")) {
		t.Fatal("stream tail lost the latest output")
	}
}

func TestRetryUsesSameBodyAfterLostAcknowledgment(t *testing.T) {
	var mu sync.Mutex
	var seen [][]byte
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ax-placeholder" {
			t.Error("missing AX placeholder")
		}
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, raw)
		if len(seen) == 1 {
			h, ok := w.(http.Hijacker)
			if !ok {
				t.Error("expected HTTP/1.1")
				w.WriteHeader(500)
				return
			}
			conn, _, err := h.Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close() // committed, but the response was lost
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	r := result.Report{Claim: result.Claim{ResultID: result.NewID(), InvocationID: result.NewID(), TaskUID: "uid"}, Output: "result", OutputSource: "file"}
	if err := post(ctx, server.Client(), server.URL+result.Path, r); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || !bytes.Equal(seen[0], seen[1]) {
		t.Fatal("result retry changed the body")
	}
}

func TestMissingMetadataPreventsChild(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	defer server.Close()
	marker := filepath.Join(t.TempDir(), "child-started")
	getenv := func(s string) string {
		return map[string]string{result.URLEnv: "https://fixture.test" + result.Path, result.IDEnv: result.NewID(),
			result.DeadlineEnv: time.Now().Add(time.Minute).UTC().Format(time.RFC3339), "AX_METADATA_URL": server.URL}[s]
	}
	if err := run(t.Context(), []string{"--", "touch", marker}, getenv); err == nil {
		t.Fatal("invalid startup metadata was accepted")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("command ran despite missing metadata: %v", err)
	}
}

func TestCompletedInvocationKeepsFrozenResult(t *testing.T) {
	workspace := t.TempDir()
	claim := result.Claim{ResultID: result.NewID(), InvocationID: result.NewID(), TaskUID: "task"}
	report := result.Report{Claim: claim, Output: "finished", OutputSource: "file"}
	body, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	const marker = ".completed-result"
	saveResult(workspace, marker, body)
	restarted := claim
	restarted.InvocationID = result.NewID()
	if got := savedResult(workspace, marker, restarted); !bytes.Equal(got, body) {
		t.Fatal("restarted wrapper lost the frozen receipt")
	}
	restarted.TaskUID = "different"
	if got := savedResult(workspace, marker, restarted); got != nil {
		t.Fatal("receipt from a different Task was reused")
	}
}
