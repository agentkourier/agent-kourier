package agent

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// WP-03 recorded ListTasks on a context holding three tasks (oidc-replay/07): two completed
// ones that both hold m1's messageId, because m1 was replayed with a contextId and ran twice
// (spec 8.2), and a running one that holds m2's.
const (
	replayContext = "01a0f1d6-f62d-7112-881b-f8f2e19632cd"
	m1            = "spike-1790763791916730000"
	m2            = "spike-1790763808395539000"
	m1Original    = a2a.TaskID("01a0f1d6-f6bc-722e-9f5a-0def1120ba8a")
	m2Running     = a2a.TaskID("01a0f1d7-36bf-771b-acc3-ded7148eac42")
)

func TestFindTaskFindsATurnByMessageID(t *testing.T) {
	fa := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) {
		reply(t, w, "oidc-replay/07-m2-listtasks-while-running.response.json")
	})
	for _, tc := range []struct {
		name, messageID string
		want            a2a.TaskID // empty: absent
		wantState       a2a.TaskState
	}{
		{"a running turn", m2, m2Running, a2a.TaskStateWorking},
		{"a duplicated turn: the original, not the copy", m1, m1Original, a2a.TaskStateCompleted},
		{"a turn the agent never saw", "spike-0", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, err := fa.agentWith(t, quick).FindTask(t.Context(), replayContext, tc.messageID)
			switch {
			case err != nil:
				t.Fatal(err)
			case tc.want == "" && task != nil:
				t.Errorf("found task %s, want none", task.ID)
			case tc.want != "" && (task == nil || task.ID != tc.want || task.Status.State != tc.wantState):
				t.Errorf("task = %+v, want %s in %s", task, tc.want, tc.wantState)
			}
		})
	}
	if got := string(fa.callAt(0).Params); !strings.Contains(got, `"contextId":"`+replayContext+`"`) {
		t.Errorf("ListTasks params = %s, want them to name the context", got)
	}
}

func listOf(next string, tasks ...*a2a.Task) *a2a.ListTasksResponse {
	return &a2a.ListTasksResponse{Tasks: tasks, NextPageToken: next}
}

func holding(id a2a.TaskID, state a2a.TaskState, messageID string) *a2a.Task {
	return &a2a.Task{ID: id, ContextID: "c1", Status: a2a.TaskStatus{State: state}, History: []*a2a.Message{{ID: "other"}, {ID: messageID}}}
}

// UNCONFIRMED: WP-03 did not measure paging past 50 tasks, so these pages are constructed in
// the shape of the recorded response, with a nextPageToken as the SDK's types define it.
func TestFindTaskFollowsPagesWithoutRelyingOnOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pages := map[string]*a2a.ListTasksResponse{
			"":   listOf("p2", holding("t1", a2a.TaskStateCompleted, "x")),
			"p2": listOf("p3", holding("t2", a2a.TaskStateCompleted, "turn"), holding("t3", a2a.TaskStateWorking, "y")),
			"p3": listOf("", holding("t4", a2a.TaskStateWorking, "turn")), // a second holder, still running
		}
		var tokens []string
		m := &memConn{list: func(_ context.Context, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
			tokens = append(tokens, req.PageToken)
			return pages[req.PageToken], nil
		}}
		task, err := memAgent(m, nil).FindTask(t.Context(), "c1", "turn")
		if err != nil || task == nil || task.ID != "t4" {
			t.Errorf("task = %+v, %v; want t4: a running holder wins over a finished one on an earlier page", task, err)
		}
		if !slices.Equal(tokens, []string{"", "p2", "p3"}) {
			t.Errorf("page tokens = %q, want the lookup to follow nextPageToken to the end", tokens)
		}
	})
}

func TestFindTaskGivesUpOnAListThatNeverEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &memConn{list: func(context.Context, *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
			return listOf("again", holding("t1", a2a.TaskStateCompleted, "x")), nil
		}}
		if task, err := memAgent(m, nil).FindTask(t.Context(), "c1", "turn"); err == nil {
			t.Errorf("task = %+v, want an error once the pages run past any sane bound", task)
		}
	})
}

func TestFindTaskErrors(t *testing.T) {
	forbidden := newFakeAgent(t, func(_ rpc, w http.ResponseWriter) {
		reply(t, w, "oidc-identity/09-b-listtasks-a-context.response.json")
	})
	if _, err := forbidden.agentWith(t, quick).FindTask(t.Context(), replayContext, m1); !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
	// Without a contextId ListTasks would list every task the caller owns, and a match would mean nothing.
	for _, ids := range [][2]string{{"", m1}, {replayContext, ""}} {
		if _, err := forbidden.agentWith(t, quick).FindTask(t.Context(), ids[0], ids[1]); err == nil {
			t.Errorf("FindTask(%q, %q) = nil error, want one for a missing id", ids[0], ids[1])
		}
	}
	if n := forbidden.count("ListTasks"); n != 1 {
		t.Errorf("%d ListTasks calls, want only the one with both ids", n)
	}
}

// Each page is a unary call under UnaryTimeout, and so is resolving the connection.
func TestFindTaskHasADeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		hang := &memConn{list: func(ctx context.Context, _ *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		if _, err := memAgent(hang, func(c *Config) { c.UnaryTimeout = time.Second }).FindTask(t.Context(), "c1", "turn"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("hung page: err = %v, want DeadlineExceeded", err)
		}
		stuck := New(func(ctx context.Context) (Conn, error) { <-ctx.Done(); return nil, ctx.Err() }, nil, Config{UnaryTimeout: time.Second})
		if _, err := stuck.FindTask(t.Context(), "c1", "turn"); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("stuck Source: err = %v, want DeadlineExceeded", err)
		}
	})
}
