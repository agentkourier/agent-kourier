package teams

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

var _ chat.RootReader = (*Client)(nil)

// ThreadRoot reads the message that opened the thread with one GET of it through Graph, and reads no reply, however many the thread
// has: a thread too long for ThreadFromStart still gives its root. It is decoded as ThreadFromStart decodes it.
func TestThreadRootIsOneReadOfTheRoot(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.f.setGraph(func(g *fakeGraph) { g.pageAt = 1 })
	ct.replies(threadMaxPages + 1)
	ct.f.graphTaken()

	m, found, err := ct.c.ThreadRoot(t.Context(), target)

	if err != nil || !found {
		t.Fatalf("ThreadRoot = %+v, %t, %v", m, found, err)
	}
	if reqs := ct.f.graphTaken(); len(reqs) != 1 || reqs[0].path != messagePath(testGroup, testChannel, testRoot) {
		t.Errorf("calls = %+v; want one, of the root", reqs)
	}
	msgs, _, _ := ct.c.ThreadFromStart(t.Context(), target, "", 1)
	if len(msgs) == 0 || m.Message.At != msgs[0].Message.At || m.Message.Text != msgs[0].Message.Text || m.Message.User != msgs[0].Message.User ||
		m.Own != msgs[0].Own || m.Ignored != msgs[0].Ignored {
		t.Errorf("root = %+v; want it as ThreadFromStart decodes it: %+v", m, msgs)
	}
	if m.Message.Text != "the root" || m.Message.User != testUser {
		t.Errorf("root = %+v", m.Message)
	}
}

// A root that Graph says is not there (404) is not found, with no error; Graph answering with another message is an error; any other
// refusal is Graph's error, classified as a read of the thread's is (none is FatalAuth), and the read stops there.
func TestThreadRootThatCannotBeRead(t *testing.T) {
	t.Parallel()
	refuse := func(status int, code string) func(*http.Request) (int, string, bool) {
		return func(*http.Request) (int, string, bool) { return status, `{"error":{"code":"` + code + `"}}`, true }
	}
	t.Run("gone", func(t *testing.T) {
		t.Parallel()
		ct, target := threadTest(t)
		ct.f.setGraph(func(g *fakeGraph) { g.fail = refuse(http.StatusNotFound, "NotFound") })
		if m, found, err := ct.c.ThreadRoot(t.Context(), target); err != nil || found || m.Message.At.ID != "" {
			t.Errorf("ThreadRoot = %+v, %t, %v; want not found and no error", m, found, err)
		}
	})
	t.Run("another message", func(t *testing.T) {
		t.Parallel()
		ct, target := threadTest(t)
		ct.f.setGraph(func(g *fakeGraph) {
			g.msgs[messagePath(testGroup, testChannel, testRoot)] = person(replyID(1), testRoot, testUser, "<p>not the root</p>")
		})
		if _, found, err := ct.c.ThreadRoot(t.Context(), target); err == nil || found {
			t.Errorf("ThreadRoot = %t, %v; want an error", found, err)
		}
	})
	for _, tc := range []struct {
		name   string
		status int
		code   string
		class  chat.Class
		wait   time.Duration
	}{
		{"throttled", http.StatusTooManyRequests, "TooManyRequests", chat.Transient, 4 * time.Second},
		{"rejected token", http.StatusUnauthorized, "InvalidAuthenticationToken", chat.Permanent, 0},
		{"no permission", http.StatusForbidden, "Forbidden", chat.Permanent, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct, target := threadTest(t)
			ct.f.setGraph(func(g *fakeGraph) { g.fail = refuse(tc.status, tc.code) })
			ct.f.graphTaken()
			_, found, err := ct.c.ThreadRoot(t.Context(), target)
			var ge *GraphError
			if found || !errors.As(err, &ge) || ge.Status != tc.status {
				t.Fatalf("ThreadRoot = %t, %v; want Graph's %d", found, err, tc.status)
			}
			if v := ct.c.Classify(chat.OpOther, err); v.Class != tc.class || v.RetryAfter != tc.wait {
				t.Errorf("verdict = %+v; want class %d wait %v", v, tc.class, tc.wait)
			}
			if n := len(ct.f.graphTaken()); n != 1 {
				t.Errorf("%d calls; want 1", n)
			}
		})
	}
}

// ThreadRoot is bounded as a thread's read is: an invalid target makes no call, a channel whose team is not known is an error with no
// call, and a held read ends at threadTimeout. Each read counts as ThreadRoot in agentkourier_chat_api_calls_total.
func TestThreadRootIsBoundedAndCounted(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	for _, bad := range []chat.Target{{Conn: testConn, Thread: testRoot}, {Conn: testConn, Channel: testChannel, Thread: "not-an-id"}} {
		if _, _, err := ct.c.ThreadRoot(t.Context(), bad); !errors.Is(err, chat.ErrInvalid) {
			t.Errorf("ThreadRoot(%+v) = %v; want ErrInvalid", bad, err)
		}
	}
	unknown := target
	unknown.Channel = "19:no-activity-yet@thread.tacv2"
	if _, _, err := ct.c.ThreadRoot(t.Context(), unknown); !errors.Is(err, errTeamNotKnown) || errors.Is(err, chat.ErrNoThreadReads) {
		t.Errorf("a channel with no record: %v; want errTeamNotKnown", err)
	}
	if n := len(ct.f.graphTaken()); n != 0 {
		t.Errorf("%d Graph calls; want none", n)
	}

	o := newObserved(t)
	if _, found, err := ct.c.ThreadRoot(o.ctx, target); err != nil || !found {
		t.Fatalf("ThreadRoot = %t, %v", found, err)
	}
	if got := o.value(t, "agentkourier_chat_api_calls_total", "ThreadRoot"); got != 1 {
		t.Errorf("ThreadRoot calls = %v; want 1", got)
	}

	held, heldTarget := threadTest(t)
	held.c.threadTimeout = 50 * time.Millisecond
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	held.f.setGraph(func(g *fakeGraph) { g.hold = hold })
	done := make(chan error, 1)
	go func() { _, _, err := held.c.ThreadRoot(t.Context(), heldTarget); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("a held read: %v; want its deadline", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("the held read did not end at its deadline")
	}
}
