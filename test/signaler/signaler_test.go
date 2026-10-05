package signaler

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
)

// Two container IDs, as containerd names them: 64 hex digits.
var (
	idA = strings.Repeat("a1", 32)
	idB = strings.Repeat("b2", 32)
)

type sent struct {
	pid int
	sig syscall.Signal
}

// recorder is a kill func that records what it is asked, and fails the pids it is told to.
type recorder struct {
	mu   sync.Mutex
	got  []sent
	fail map[int]error
}

func (r *recorder) kill(pid int, sig syscall.Signal) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, sent{pid, sig})
	return r.fail[pid]
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = nil
}

func (r *recorder) sent() []sent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.got)
}

// proc makes a /proc with one directory per process, each holding the cgroup file that the kernel shows for it.
func proc(t *testing.T, cgroups map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for pid, cgroup := range cgroups {
		dir := filepath.Join(root, pid)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "cgroup"), []byte(cgroup), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// What a /proc holds besides processes: files and directories that are not a pid.
	if err := os.WriteFile(filepath.Join(root, "version"), []byte("Linux"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}
	return root
}

// cgroupOf is the cgroup file of a process in the container id: cgroup v2 as the systemd driver names it, relative to
// a reader whose own cgroup is elsewhere.
func cgroupOf(id string) string {
	return "0::/../../kubelet-kubepods-burstable-pod0a1b.slice/cri-containerd-" + id + ".scope\n"
}

func post(t *testing.T, srv *httptest.Server, path string) (int, string) {
	t.Helper()
	return send(t, srv, http.MethodPost, path)
}

func send(t *testing.T, srv *httptest.Server, method, path string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.WithoutCancel(t.Context()), method, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(b)
}

func TestSignalReachesEveryProcessOfTheContainerAndNoOther(t *testing.T) {
	rec := &recorder{}
	root := proc(t, map[string]string{
		"10": cgroupOf(idA),
		"11": cgroupOf(idA), // a second process of the container
		"20": cgroupOf(idB),
		"30": "0::/system.slice/containerd.service\n",
	})
	srv := httptest.NewServer(NewHandler(WithProc(root), WithKill(rec.kill)))
	defer srv.Close()

	for _, tc := range []struct {
		verb string
		sig  syscall.Signal
	}{{"stop", syscall.SIGSTOP}, {"cont", syscall.SIGCONT}, {"kill", syscall.SIGKILL}} {
		t.Run(tc.verb, func(t *testing.T) {
			rec.reset()
			code, body := post(t, srv, "/"+tc.verb+"?container="+idA)
			if code != http.StatusOK {
				t.Fatalf("POST /%s = %d %s; want 200", tc.verb, code, body)
			}
			got := rec.sent()
			slices.SortFunc(got, func(a, b sent) int { return a.pid - b.pid })
			if want := []sent{{10, tc.sig}, {11, tc.sig}}; !slices.Equal(got, want) {
				t.Errorf("signals sent %v; want %v, to the container's processes only", got, want)
			}
			if !strings.Contains(body, `"signalled":2`) {
				t.Errorf("answer %s; want it to say two processes were signalled", body)
			}
		})
	}
}

func TestSignalRefusals(t *testing.T) {
	rec := &recorder{}
	root := proc(t, map[string]string{"10": cgroupOf(idA)})
	srv := httptest.NewServer(NewHandler(WithProc(root), WithKill(rec.kill)))
	defer srv.Close()

	for _, tc := range []struct {
		name, method, path string
		want               int
	}{
		{"no container", http.MethodPost, "/stop", http.StatusBadRequest},
		{"a short ID, which would match more than one container", http.MethodPost, "/stop?container=a1a1", http.StatusBadRequest},
		{"an ID that is not hex", http.MethodPost, "/stop?container=" + strings.Repeat("z", 64), http.StatusBadRequest},
		{"an ID with a path in it", http.MethodPost, "/stop?container=../" + idA, http.StatusBadRequest},
		{"a container with no process", http.MethodPost, "/stop?container=" + idB, http.StatusNotFound},
		{"a verb that is not one", http.MethodPost, "/term?container=" + idA, http.StatusNotFound},
		{"a GET, which changes nothing", http.MethodGet, "/stop?container=" + idA, http.StatusMethodNotAllowed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if code, body := send(t, srv, tc.method, tc.path); code != tc.want {
				t.Errorf("%s %s = %d %s; want %d", tc.method, tc.path, code, body, tc.want)
			}
		})
	}
	if got := rec.sent(); len(got) != 0 {
		t.Errorf("signals sent %v for refused requests; want none", got)
	}
}

func TestSignalSkipsItselfAndAProcessThatHasGone(t *testing.T) {
	rec := &recorder{fail: map[int]error{11: syscall.ESRCH}}
	root := proc(t, map[string]string{"10": cgroupOf(idA), "11": cgroupOf(idA), "12": cgroupOf(idA)})
	srv := httptest.NewServer(NewHandler(WithProc(root), WithKill(rec.kill), withSelf(12)))
	defer srv.Close()

	code, body := post(t, srv, "/stop?container="+idA)
	if code != http.StatusOK || !strings.Contains(body, `"signalled":1`) {
		t.Fatalf("POST /stop = %d %s; want 200 with one process signalled: 11 had gone, 12 is the signaler", code, body)
	}
	for _, s := range rec.sent() {
		if s.pid == 12 {
			t.Errorf("the signaler signalled itself: %v", rec.sent())
		}
	}
}

func TestSignalReportsAProcessItCannotSignal(t *testing.T) {
	rec := &recorder{fail: map[int]error{10: syscall.EPERM}}
	root := proc(t, map[string]string{"10": cgroupOf(idA)})
	srv := httptest.NewServer(NewHandler(WithProc(root), WithKill(rec.kill)))
	defer srv.Close()

	if code, body := post(t, srv, "/stop?container="+idA); code != http.StatusInternalServerError {
		t.Errorf("POST /stop = %d %s; want 500: a container that was not stopped must not look stopped", code, body)
	}
}
