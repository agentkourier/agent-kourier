// Package signaler sends SIGSTOP, SIGCONT or SIGKILL to the processes of one container, named by its container ID,
// for the failover scenario of the fast E2E tier (S16). It runs in a pod that shares the node's PID namespace
// (hostPID), as the node's own tooling would: from inside the cluster there is no other way to pause or kill a
// courier process the way a frozen node or a crash does, since courier is PID 1 of its own namespace and the kernel
// drops a SIGSTOP or SIGKILL that an init gets from inside it.
//
//	POST /stop?container=ID: SIGSTOP, so the process is frozen, not ended
//	POST /cont?container=ID: SIGCONT, which resumes it
//	POST /kill?container=ID: SIGKILL, a crash with no shutdown
//
// ID is the container's full ID, 64 hex digits, as a pod's status shows it after "containerd://". The processes are
// found by the container's cgroup, which names the ID. The server has no authentication and can end any process of the
// node, so it is for a test cluster, behind a ClusterIP Service, and the fast tier brings it up only when a scenario
// needs it.
package signaler

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

// containerID is what an ID must be: the whole 64 hex digits, so that a few characters cannot name several containers
// and a path cannot be smuggled into a match.
var containerID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// signals are the verbs the server takes, and the signal each sends.
var signals = map[string]syscall.Signal{"stop": syscall.SIGSTOP, "cont": syscall.SIGCONT, "kill": syscall.SIGKILL}

// Option configures NewHandler.
type Option func(*Server)

// WithProc sets where the processes are listed: /proc by default.
func WithProc(dir string) Option { return func(s *Server) { s.proc = dir } }

// WithKill sets what sends a signal: syscall.Kill by default.
func WithKill(kill func(pid int, sig syscall.Signal) error) Option {
	return func(s *Server) { s.kill = kill }
}

// withSelf sets the pid the server never signals: its own, by default.
func withSelf(pid int) Option { return func(s *Server) { s.self = pid } }

// Server is the signaler. It is an http.Handler and is safe for concurrent use.
type Server struct {
	proc string
	kill func(pid int, sig syscall.Signal) error
	self int
}

// NewHandler returns a signaler that reads /proc and signals with kill(2).
func NewHandler(opts ...Option) *Server {
	s := &Server{proc: "/proc", kill: syscall.Kill, self: os.Getpid()}
	for _, o := range opts {
		o(s)
	}
	return s
}

// ServeHTTP signals the container's processes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sig, ok := signals[strings.TrimPrefix(r.URL.Path, "/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	id := r.URL.Query().Get("container")
	if !containerID.MatchString(id) {
		http.Error(w, "container must be a full container ID, 64 hex digits", http.StatusBadRequest)
		return
	}
	pids, err := s.members(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(pids) == 0 {
		http.Error(w, "no process of that container is running on this node", http.StatusNotFound)
		return
	}
	signalled := 0
	var failed []error
	for _, pid := range pids {
		switch err := s.kill(pid, sig); {
		case err == nil:
			signalled++
		case errors.Is(err, syscall.ESRCH): // it ended between the listing and the signal, which is no failure
		default:
			failed = append(failed, fmt.Errorf("pid %d: %w", pid, err))
		}
	}
	if err := errors.Join(failed...); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"signalled": signalled})
}

// members returns the processes whose cgroup names the container, not counting the server's own.
func (s *Server) members(id string) ([]int, error) {
	entries, err := os.ReadDir(s.proc)
	if err != nil {
		return nil, fmt.Errorf("list %s: %w", s.proc, err)
	}
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == s.self {
			continue // not a process, or the signaler itself
		}
		raw, err := os.ReadFile(s.proc + "/" + e.Name() + "/cgroup")
		if err != nil {
			continue // the process ended since the listing
		}
		if strings.Contains(string(raw), id) {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
