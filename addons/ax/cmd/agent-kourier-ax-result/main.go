// agent-kourier-ax-result claims one AX command invocation and reports its result.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
)

const (
	requestTimeout = 10 * time.Second
	childWaitDelay = 2 * time.Second
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	err := run(ctx, os.Args[1:], os.Getenv)
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string) error {
	workspace, err := os.Getwd()
	if err != nil {
		return errors.New("cannot find AX working directory")
	}
	return runWith(ctx, args, getenv, httpClient(), workspace, time.Now(), os.Stderr)
}

func runWith(ctx context.Context, args []string, getenv func(string) string, client *http.Client, workspace string, now time.Time, diagnostics io.Writer) error {
	if len(args) < 2 || args[0] != "--" || args[1] == "" {
		return errors.New("usage: agent-kourier-ax-result -- command [arguments...]")
	}
	endpoint, err := result.URL(getenv(result.URLEnv))
	if err != nil {
		return err
	}
	deadline, err := time.Parse(time.RFC3339, getenv(result.DeadlineEnv))
	if err != nil || !deadline.After(now) {
		return errors.New("missing or expired AX result deadline")
	}
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	claim := result.Claim{ResultID: getenv(result.IDEnv), InvocationID: result.NewID()}
	claim.TaskUID, err = taskUID(ctx, client, getenv("AX_METADATA_URL"))
	if err != nil || !claim.Valid() {
		return errors.New("invalid AX startup metadata")
	}

	marker := ".agent-kourier-ax-result-" + claim.ResultID
	body := savedResult(workspace, marker, claim)
	if body == nil {
		if err := post(ctx, client, endpoint.String()+"/start", claim); err != nil {
			return fmt.Errorf("AX start claim: %w", err)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		report := execute(ctx, args[1:], workspace, getenv("AX_OUTPUT_FILE"))
		report.Claim = claim
		body, err = json.Marshal(report)
		if err != nil {
			return errors.New("could not encode result")
		}
		if err := saveResult(workspace, marker, body); err != nil {
			_, _ = fmt.Fprintf(diagnostics, "cannot save AX result for replay: %v\n", fileError(err))
		}
	}
	if err := postBody(ctx, client, endpoint.String(), body); err != nil {
		return fmt.Errorf("AX result report: %w", err)
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return fmt.Errorf("open workspace to remove delivered AX result: %w", fileError(err))
	}
	defer func() { _ = root.Close() }()
	if err := root.Remove(marker); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove delivered AX result: %w", fileError(err))
	}
	return nil
}

func httpClient() *http.Client {
	return &http.Client{
		Timeout: requestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func taskUID(ctx context.Context, client *http.Client, raw string) (string, error) {
	invalid := errors.New("invalid AX task metadata")
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.Path != "" ||
		u.RawQuery != "" || u.ForceQuery || strings.Contains(raw, "#") || !net.ParseIP(u.Hostname()).IsLoopback() {
		return "", invalid
	}
	u.Path = "/metadata/v1alpha1/ax/task"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", invalid
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", invalid
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", invalid
	}
	const maxMetadata = 512 << 10
	var task struct {
		Metadata struct {
			UID string `yaml:"uid"`
		} `yaml:"metadata"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxMetadata+1))
	if err != nil || len(body) > maxMetadata || !utf8.Valid(body) || yaml.Unmarshal(body, &task) != nil {
		return "", invalid
	}
	return task.Metadata.UID, nil
}

func post(ctx context.Context, client *http.Client, endpoint string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return errors.New("could not encode callback")
	}
	return postBody(ctx, client, endpoint, body)
}

func postBody(ctx context.Context, client *http.Client, endpoint string, body []byte) error {
	backoff := 200 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return errors.New("could not build callback request")
		}
		req.Header.Set("Authorization", "Bearer ax-placeholder")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		wait := backoff/2 + rand.N(backoff/2) //nolint:gosec // jitter is independent of all credentials and IDs
		if err == nil {
			_ = resp.Body.Close()
			code := resp.StatusCode
			switch {
			case code == http.StatusNoContent:
				return nil
			case code == http.StatusTooManyRequests || code >= 500:
				wait = max(wait, retryAfter(resp.Header.Get("Retry-After")))
			default:
				return fmt.Errorf("callback returned HTTP %d", code)
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, 3*time.Second)
	}
}

func retryAfter(value string) time.Duration {
	if n, err := strconv.ParseInt(value, 10, 32); err == nil && n >= 0 {
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(value); err == nil {
		return time.Until(t)
	}
	return 0
}

func execute(ctx context.Context, args []string, workspace, outputFile string) result.Report {
	var tail outputTail
	child := exec.CommandContext(ctx, args[0], args[1:]...) //nolint:gosec // the operator supplies the argv in the Task template
	child.Dir = workspace
	child.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "AGENT_KOURIER_AX_RESULT_")
	})
	child.Stdout, child.Stderr = &tail, &tail
	child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	child.Cancel = func() error {
		err := unix.Kill(-child.Process.Pid, unix.SIGKILL)
		if errors.Is(err, unix.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	child.WaitDelay = childWaitDelay
	var report result.Report
	if err := child.Run(); err != nil {
		report.ExitCode = 1
		var exited *exec.ExitError
		if errors.As(err, &exited) {
			status := exited.Sys().(syscall.WaitStatus)
			if status.Signaled() {
				report.ExitCode = 128 + int(status.Signal())
			} else {
				report.ExitCode = status.ExitStatus()
			}
		}
	}
	data, ok := readOutput(workspace, outputFile)
	switch {
	case ok:
		report.OutputSource = "file"
		report.Truncated = len(data) > result.OutputLimit
		data = data[:min(len(data), result.OutputLimit)]
	case len(tail.data) > 0:
		report.OutputSource = "stdout_stderr"
		report.Truncated = tail.truncated
		data = tail.data
	default:
		report.OutputSource = "none"
	}
	report.Output = strings.ToValidUTF8(string(data), "\uFFFD")
	if len(report.Output) > result.OutputLimit {
		end := result.OutputLimit
		for !utf8.RuneStart(report.Output[end]) {
			end--
		}
		report.Output = report.Output[:end]
		report.Truncated = true
	}
	return report
}

func readOutput(workspace, path string) ([]byte, bool) {
	if path == "" {
		return nil, false
	}
	if filepath.IsAbs(path) {
		rel, err := filepath.Rel(workspace, path)
		if err != nil {
			return nil, false
		}
		path = rel
	}
	return readRegular(workspace, path, result.OutputLimit+1)
}

func readRegular(workspace, path string, limit int64) ([]byte, bool) {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return nil, false
	}
	defer func() { _ = root.Close() }()
	f, err := root.OpenFile(path, os.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, false
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(f, limit))
	return body, err == nil
}

// A completed invocation can replay its frozen result after the wrapper
// restarts. An unfinished invocation gets no replacement start claim.
func savedResult(workspace, marker string, claim result.Claim) []byte {
	body, ok := readRegular(workspace, marker, result.BodyLimit+1)
	if !ok || len(body) > result.BodyLimit {
		return nil
	}
	report, err := result.Decode(body, false)
	if err != nil || report.ResultID != claim.ResultID || report.TaskUID != claim.TaskUID {
		return nil
	}
	return body
}

func fileError(err error) error {
	var path *os.PathError
	var link *os.LinkError
	switch {
	case errors.As(err, &path):
		return path.Err
	case errors.As(err, &link):
		return link.Err
	default:
		return err
	}
}

func saveResult(workspace, marker string, body []byte) error {
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	tmp := marker + "-" + result.NewID()
	f, err := root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = root.Remove(tmp) }()
	_, writeErr := f.Write(body)
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return root.Rename(tmp, marker)
}

type outputTail struct {
	data      []byte
	truncated bool
}

func (t *outputTail) Write(p []byte) (int, error) {
	n := len(p)
	if n >= result.OutputLimit {
		t.truncated = t.truncated || n > result.OutputLimit || len(t.data) > 0
		t.data = append(t.data[:0], p[n-result.OutputLimit:]...)
		return n, nil
	}
	if overflow := len(t.data) + n - result.OutputLimit; overflow > 0 {
		t.data = t.data[:copy(t.data, t.data[overflow:])]
		t.truncated = true
	}
	t.data = append(t.data, p...)
	return n, nil
}
