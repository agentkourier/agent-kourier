package broker

import (
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/session"
)

// agentWrites are the methods of session.AgentAPI that change what the agent holds, and agentReads the ones that cannot. A
// method that is in neither fails the test below: a new call to the agent is decided on, fenced or not, when it is added.
var (
	agentWrites = map[string]bool{"Send": true, "Answer": true, "CancelTask": true}
	agentReads  = map[string]bool{"Follow": true, "GetTask": true, "FindTask": true}
)

// recordingAgent is a session.AgentAPI that notes the calls it gets.
type recordingAgent struct {
	mu   sync.Mutex
	seen []string
}

func (a *recordingAgent) note(name string) { a.mu.Lock(); a.seen = append(a.seen, name); a.mu.Unlock() }
func (a *recordingAgent) calls() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.seen...)
}

func (a *recordingAgent) Send(context.Context, *a2a.Message, agent.Handler) error {
	a.note("Send")
	return nil
}

func (a *recordingAgent) Answer(context.Context, *a2a.Message, agent.Handler) error {
	a.note("Answer")
	return nil
}

func (a *recordingAgent) Follow(context.Context, a2a.TaskID, agent.Handler) error {
	a.note("Follow")
	return nil
}

func (a *recordingAgent) GetTask(context.Context, a2a.TaskID) (*a2a.Task, error) {
	a.note("GetTask")
	return nil, nil
}

func (a *recordingAgent) CancelTask(context.Context, a2a.TaskID) (*a2a.Task, error) {
	a.note("CancelTask")
	return nil, nil
}

func (a *recordingAgent) FindTask(context.Context, string, string) (*a2a.Task, []a2a.TaskID, error) {
	a.note("FindTask")
	return nil, nil, nil
}

// A call that changes the agent passes while the lease was renewed within the renew deadline, the deadline itself included, and
// from the first instant after it is refused: the call is never made, the error says why, and the process is stopped. A call
// that only reads passes always. Every method of the agent's API is tried, found by reflection, so that a method that is
// added is tried too.
func TestAgentFenceRefusesEveryCallThatChangesTheAgentOncePastTheRenewDeadlineAndNoRead(t *testing.T) {
	const limit = 10 * time.Second
	typ := reflect.TypeFor[session.AgentAPI]()
	for i := range typ.NumMethod() {
		name := typ.Method(i).Name
		write, read := agentWrites[name], agentReads[name]
		if write == read {
			t.Fatalf("%s is in agentWrites %t and agentReads %t; a method of the agent's API is exactly one of the two", name, write, read)
		}
		t.Run(name+map[bool]string{true: " (changes the agent)", false: " (a read)"}[write], func(t *testing.T) {
			clock, inner := newStepClock(), &recordingAgent{}
			lease := newLeaseClock(clock.now)
			lease.renewed(clock.now())
			var mu sync.Mutex
			var stops []string
			fenced := fenceAgent(inner, &leaseFence{clock: lease, limit: limit, stop: func(reason string) {
				mu.Lock()
				stops = append(stops, reason)
				mu.Unlock()
			}})

			clock.advance(limit) // exactly at the deadline: the lease is still the leader's
			if err := call(t, fenced, name); err != nil {
				t.Fatalf("%s at the renew deadline = %v; want it to pass", name, err)
			}
			if got := inner.calls(); len(got) != 1 {
				t.Fatalf("%s at the deadline made %d calls of the agent; want 1", name, len(got))
			}
			if len(stops) != 0 {
				t.Fatalf("stopped at the deadline: %v", stops)
			}

			clock.advance(time.Nanosecond)
			err := call(t, fenced, name)
			calls := len(inner.calls()) - 1
			if !write {
				if err != nil || calls != 1 || len(stops) != 0 {
					t.Errorf("a read past the deadline: err %v, %d calls of the agent, stopped %v; want it to pass, once, and stop nothing", err, calls, stops)
				}
				return
			}
			if !errors.Is(err, ErrAgentFenced) {
				t.Errorf("%s past the deadline = %v; want ErrAgentFenced", name, err)
			}
			if !errors.Is(err, agent.ErrNotSent) {
				t.Errorf("%s past the deadline = %v; want it to match agent.ErrNotSent, which is what it is: the message never left", name, err)
			}
			if calls != 0 {
				t.Errorf("%s past the deadline reached the agent %d times; a refused call never does", name, calls)
			}
			if len(stops) != 1 || !strings.Contains(stops[0], "an agent call ("+name+")") || strings.Contains(stops[0], leaseLostReason) {
				t.Errorf("fail-stops = %q; want one that names the refused call (%s) and is not the loss of the lease", stops, name)
			}
		})
	}
}

// A lease clock that was never told of a renewal refuses an agent call too: nothing says the leader holds the lease.
func TestAgentFenceRefusesWhenTheLeaseWasNeverRenewed(t *testing.T) {
	inner := &recordingAgent{}
	var stops int
	fenced := fenceAgent(inner, &leaseFence{clock: newLeaseClock(nil), limit: time.Hour, stop: func(string) { stops++ }})
	if err := fenced.Send(t.Context(), &a2a.Message{}, nil); !errors.Is(err, ErrAgentFenced) || len(inner.calls()) != 0 || stops != 1 {
		t.Errorf("Send = %v, %d agent calls, %d stops; want ErrAgentFenced, none, one", err, len(inner.calls()), stops)
	}
}

// The Backend a Binding resolves to carries the fenced agent, so that the session manager, the interactions' answers and the
// retention's cancel of a stale task, which all reach the agent through Bindings, are behind the fence. A deposed leader's
// send, answer and cancel do not reach the backend (here a stub that records every request), a read does, and the refusal of
// each is the one error.
func TestBindingsOfAFencedLeaderRefuseSendAnswerAndCancelPastTheRenewDeadline(t *testing.T) {
	agentStub, url := newSendCounter(t)
	f := newFixture(t)
	f.snaps.p.Store(snapshot(t, url, v1alpha1.DialectKagentV1, time.Hour))
	clock := newStepClock()
	lease := newLeaseClock(clock.now)
	lease.renewed(clock.now())
	var stops []string
	var err error
	f.b, err = NewBindings(f.snaps, f.reader, f.audit, Options{OnRejected: func(string) {}, fence: &leaseFence{clock: lease, limit: time.Second, stop: func(r string) { stops = append(stops, r) }}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.b.Close)
	be, err := f.b.Backend("sre/oncall")
	if err != nil {
		t.Fatal(err)
	}

	_, _ = be.Agent.GetTask(t.Context(), "task-1") // a lease that is good: a read, and it is made
	if agentStub.other.Load() == 0 {
		t.Fatal("a read with a good lease did not reach the agent")
	}
	clock.advance(time.Minute) // the leader was frozen

	msg := a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("hi"))
	msg.TaskID = "task-1"
	before := agentStub.other.Load()
	if err := be.Agent.Send(t.Context(), msg, nil); !errors.Is(err, ErrAgentFenced) {
		t.Errorf("Send = %v; want ErrAgentFenced", err)
	}
	if err := be.Agent.Answer(t.Context(), msg, nil); !errors.Is(err, ErrAgentFenced) {
		t.Errorf("Answer = %v; want ErrAgentFenced", err)
	}
	if _, err := f.b.CancelTask(t.Context(), "sre/oncall", "task-1"); !errors.Is(err, ErrAgentFenced) {
		t.Errorf("CancelTask (the retention's) = %v; want ErrAgentFenced", err)
	}
	if n, m := agentStub.sends.Load(), agentStub.other.Load()-before; n != 0 || m != 0 {
		t.Errorf("%d sends and %d other requests reached the agent after the freeze; want none", n, m)
	}
	_, _ = be.Agent.GetTask(t.Context(), "task-1")
	if agentStub.other.Load() == before {
		t.Error("a read after the freeze did not reach the agent; a read is not fenced")
	}
	if len(stops) == 0 {
		t.Error("the process was not stopped")
	}
}

// With no fence (SQLite: one replica, no lease) the agent of a Backend is the pool's, and nothing wraps it.
func TestBindingsWithoutAFenceHandOutTheAgentItself(t *testing.T) {
	f := newFixture(t)
	be, err := f.b.Backend("sre/oncall")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := be.Agent.(*agent.Agent); !ok {
		t.Errorf("the agent is a %T; want the pool's *agent.Agent with no fence to wrap it", be.Agent)
	}
}

// With SQLite there is no election, so no lease and no fence: the clock is not read, and the queued turn is sent whatever it says.
func TestRunWithSQLiteDoesNotFenceTheAgent(t *testing.T) {
	agentStub, url := newSendCounter(t)
	c := startConfig(t)
	c.Client = clusterWithJWT()
	c.leaseNow = stepping()
	exits := make(chan int, 1)
	c.exit = func(code int) { exits <- code }
	c.Logger = slog.New(slog.DiscardHandler)
	seedQueuedTurn(t, c, url)
	start(t, c)

	eventually(t, 10*time.Second, "the queued turn did not reach the agent", func() bool { return agentStub.sends.Load() == 1 })
	select {
	case code := <-exits:
		t.Errorf("a SQLite replica stopped with exit %d", code)
	default:
	}
}

// Nothing builds the Bindings that hand out an agent but leaderHalf.start, which fences them with an election, and nothing
// reaches the pool's agents but Bindings.Backend, which wraps them: so that no call to the agent that the leader half makes
// goes round the fence. One scan of the non-test code of internal/ and cmd/, whole files (a package-level func literal is
// scanned too): a call of NewBindings (broker.NewBindings under whatever name the file imports the package by) from anywhere
// but leaderHalf.start; a call of agent.New or agent.NewPool (under whatever name the file imports the agent package by)
// from anywhere but NewBindings; and, in this package, any use of the pool field (a copy, p := k.pool, is a use too) outside
// NewBindings, Close and Backend. A second builder or a second use of the pool would be an unfenced agent; this fails until
// the new one is decided on.
func TestOnlyTheLeaderHalfBuildsTheBindingsAndOnlyBackendHandsOutTheAgent(t *testing.T) {
	const (
		agentPkg  = "github.com/agentkourier/agent-kourier/internal/agent"
		brokerPkg = "github.com/agentkourier/agent-kourier/internal/broker"
	)
	fset := token.NewFileSet()
	total := 0
	for _, root := range []string{"../../internal", "../../cmd"} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			total++
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				return err
			}
			rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
			// The name this file calls a package by: the alias of its import, or the package's own name.
			local := map[string]string{} // import path -> local name
			for _, imp := range file.Imports {
				ip, _ := strconv.Unquote(imp.Path.Value)
				if imp.Name != nil {
					local[ip] = imp.Name.Name
				} else {
					local[ip] = filepath.Base(ip)
				}
			}
			inBroker := strings.HasPrefix(rel, "internal/broker/")
			for _, decl := range file.Decls {
				where := rel + ":"
				if fn, ok := decl.(*ast.FuncDecl); ok {
					where += fn.Name.Name
				}
				ast.Inspect(decl, func(n ast.Node) bool {
					switch n := n.(type) {
					case *ast.SelectorExpr:
						if inBroker && n.Sel.Name == "pool" && where != "internal/broker/bindings.go:NewBindings" && where != "internal/broker/bindings.go:Close" && where != "internal/broker/bindings.go:Backend" {
							t.Errorf("%s: uses a pool; only NewBindings, Close and Bindings.Backend may, because Backend wraps the agent in the fence", fset.Position(n.Pos()))
						}
					case *ast.CallExpr:
						var name, pkg string
						switch f := n.Fun.(type) {
						case *ast.Ident:
							name = f.Name
						case *ast.SelectorExpr:
							name = f.Sel.Name
							if x, ok := f.X.(*ast.Ident); ok {
								pkg = x.Name
							}
						}
						switch {
						case name == "NewBindings" && (pkg == "" && inBroker || pkg != "" && pkg == local[brokerPkg]) && where != "internal/broker/leader.go:start":
							t.Errorf("%s: calls NewBindings; only leaderHalf.start may, because it passes the fence", fset.Position(n.Pos()))
						case (name == "NewPool" || name == "New") && pkg != "" && pkg == local[agentPkg] && where != "internal/broker/bindings.go:NewBindings":
							t.Errorf("%s: calls %s.%s; only NewBindings may build the agents, because Bindings.Backend wraps them in the fence", fset.Position(n.Pos()), pkg, name)
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if total < 100 {
		t.Fatalf("checked %d source files under internal/ and cmd/; the scan is not looking at them", total)
	}
}
