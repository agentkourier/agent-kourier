package broker

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
)

// writeCounter is a Slack client that counts the calls that write to Slack, and answers each as a Slack that has nothing
// against it would.
type writeCounter struct {
	idleSlack
	posts, other atomic.Int32
}

func (w *writeCounter) Post(context.Context, string, string, slack.Content, string) (string, error) {
	w.posts.Add(1)
	return "1.1", nil
}

func (w *writeCounter) React(context.Context, string, string, string) error {
	w.other.Add(1)
	return nil
}

func (w *writeCounter) writes() int32 { return w.posts.Load() + w.other.Load() }

// stepping is a clock that a minute passes between every two readings of: a leader whose lease clock reads it has a lease
// that is always a minute old, however often it renews, which is a leader that was paused, and came back, for all the
// chat fence can tell.
func stepping() func() time.Time { return steppingBy(time.Minute) }

// steppingBy is a clock that d passes between every two readings of.
func steppingBy(d time.Duration) func() time.Time {
	base, n := time.Now(), new(atomic.Int64)
	return func() time.Time { return base.Add(time.Duration(n.Add(1)) * d) }
}

// A leader whose lease is past its renew deadline writes nothing to the chat. Here the outbox has a post and a reaction
// waiting that the leader half delivers as it starts, and the clock makes the lease a minute old at every reading: no write
// reaches Slack, the process is stopped once, with exit 5, for the reason that a chat write was refused, and not for the
// loss of the lease, which refuses nothing.
func TestRunALeaderPastItsRenewDeadlineWritesNothingToTheChat(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	c, rep := electionConfig(t, client, fence, "pod-a_1")
	api := &writeCounter{}
	c.newSlack = func(slack.Config) slackAPI { rep.built.Add(1); return api }
	c.leaseNow = stepping()
	seedPost(t, c)
	seedReaction(t, c)
	rep.running = start(t, c)

	select {
	case code := <-rep.exits:
		if code != ExitLeadershipLost {
			t.Errorf("exit code = %d; want %d", code, ExitLeadershipLost)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("the process did not stop: a leader past its renew deadline made its chat writes\n%s", rep.logs.String())
	}
	time.Sleep(testElection.RetryPeriod * 3) // the outbox's other attempts, if it were to make them, would be refused too
	if n := api.writes(); n != 0 {
		t.Errorf("%d writes reached Slack; want none: a refused write never leaves the process", n)
	}
	logs := rep.logs.String()
	if n := strings.Count(logs, "leadership lost"); n != 1 {
		t.Errorf("%d leadership-lost lines; want exactly one:\n%s", n, logs)
	}
	if !strings.Contains(logs, "a chat write (") || strings.Contains(logs, leaseLostReason) {
		t.Errorf("the log does not say that a chat write was refused, or says the lease was lost:\n%s", logs)
	}
}

// The leader's lease is refused at the renew deadline and not only at the lease duration: here every reading of the clock is
// halfway between the two (the lease is at least that old at a write, however a renewal fell), and a write is refused.
func TestRunTheChatFenceRefusesAtTheRenewDeadlineBeforeTheLeaseDurationHasPassed(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	c, rep := electionConfig(t, client, fence, "pod-a_1")
	api := &writeCounter{}
	c.newSlack = func(slack.Config) slackAPI { rep.built.Add(1); return api }
	c.leaseNow = steppingBy((testElection.RenewDeadline + testElection.LeaseDuration) / 2)
	seedPost(t, c)
	rep.running = start(t, c)

	select {
	case <-rep.exits:
	case <-time.After(10 * time.Second):
		t.Fatalf("the process did not stop: a write was made with a lease older than the renew deadline\n%s", rep.logs.String())
	}
	if n := api.writes(); n != 0 {
		t.Errorf("%d writes reached Slack; want none", n)
	}
}

// The control of the one above: with a lease that is as young as renewals keep it, the same writes reach Slack and nothing
// stops the process.
func TestRunALeaderWhoseLeaseIsFreshWritesToTheChat(t *testing.T) {
	client, fence := cluster(), &fakeFence{}
	c, rep := electionConfig(t, client, fence, "pod-a_1")
	api := &writeCounter{}
	c.newSlack = func(slack.Config) slackAPI { rep.built.Add(1); return api }
	now := time.Now()
	c.leaseNow = func() time.Time { return now } // time stands still, so the lease is never older than its last renewal
	seedPost(t, c)
	seedReaction(t, c)
	rep.running = start(t, c)

	eventually(t, 10*time.Second, "the post and the reaction did not reach Slack", func() bool { return api.posts.Load() == 1 && api.other.Load() == 1 })
	select {
	case code := <-rep.exits:
		t.Errorf("the process stopped with exit %d; want it to lead\n%s", code, rep.logs.String())
	default:
	}
}

// With SQLite there is no election, so no lease and no fence: the clock is not read, and a write goes through whatever it says.
func TestRunWithSQLiteDoesNotFenceTheChat(t *testing.T) {
	c := startConfig(t)
	api := &writeCounter{}
	c.newSlack = func(slack.Config) slackAPI { return api }
	c.leaseNow = stepping()
	exits := make(chan int, 1)
	c.exit = func(code int) { exits <- code }
	c.Logger = slog.New(slog.DiscardHandler)
	seedPost(t, c)
	start(t, c)

	eventually(t, 10*time.Second, "the post did not reach Slack", func() bool { return api.posts.Load() == 1 })
	select {
	case code := <-exits:
		t.Errorf("a SQLite replica stopped with exit %d", code)
	default:
	}
}

// Nothing builds the chat platform but leaderHalf.openPlatform, which fences it, so that no write to the chat that the leader half
// makes goes round the fence: the outbox, the renderer, the interactions and the intake all take the platform that it returns.
// A second builder, or a use of the Slack adapter's constructor outside platform.go,
// would be an unfenced platform; this fails until the new one is decided on.
func TestOnlyOpenPlatformBuildsTheChatPlatform(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		checked++
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				where := name + ":" + fn.Name.Name
				switch sel.Sel.Name {
				case "newPlatform":
					if where != "leader.go:openPlatform" {
						t.Errorf("%s: calls newPlatform; only leaderHalf.openPlatform may, because it fences the platform it builds", fset.Position(call.Pos()))
					}
				case "NewPlatform":
					if x, ok := sel.X.(*ast.Ident); ok && x.Name == "slack" && name != "platform.go" {
						t.Errorf("%s: calls slack.NewPlatform; the platform is built in platform.go and fenced in leaderHalf.openPlatform", fset.Position(call.Pos()))
					}
				}
				return true
			})
		}
	}
	if checked < 10 {
		t.Fatalf("checked %d source files; the scan is not looking at the package", checked)
	}
}
