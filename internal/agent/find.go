package agent

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// maxListPages bounds FindTask's walk, so a server that never ends its pages cannot hold it.
const maxListPages = 100

// FindTask returns the task in contextID whose history holds messageID, or nil if there is
// none, and the IDs of every task that holds it, the returned one first. More than one ID
// means the turn already ran twice: the caller should tell the thread. It is how the caller
// finds a turn whose task it never recorded, for example after a
// crash: kagent does not deduplicate a resend that carries a contextId (WP-03, spec 8.2), so
// resending blindly would run the turn twice. Found: reconcile with GetTask, then
// SubscribeToTask if the task has not ended. Absent: send. A -32004 on that send means some
// task is active, possibly an input-required pause, so queue the turn and repeat the lookup
// when the active task ends.
//
// It follows nextPageToken to the end and does not rely on page order. Of several holders it
// returns one that has not ended, else the earliest, and lists the rest in that order. A
// task that shows on two pages, as the list shifts, counts once, as its newest copy. Each
// page is a unary call under UnaryTimeout. A page token seen before fails the lookup at
// once. UNCONFIRMED: WP-03 measured the lookup on a context of three tasks; paging past
// kagent's page size of 50 is not measured.
func (a *Agent) FindTask(ctx context.Context, contextID, messageID string) (*a2a.Task, []a2a.TaskID, error) {
	if contextID == "" || messageID == "" {
		return nil, nil, errors.New("find task: a contextId and a messageId are both required")
	}
	sctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	conn, err := a.source(sctx)
	cancel()
	if err != nil {
		return nil, nil, fmt.Errorf("connect to agent: %w", typed(err, false))
	}
	var holders []*a2a.Task
	byID := map[a2a.TaskID]int{} // index in holders
	seen := map[string]bool{}
	req := &a2a.ListTasksRequest{ContextID: contextID}
	for range maxListPages {
		page, err := a.listTasks(ctx, conn, req)
		if err != nil {
			return nil, nil, err
		}
		for _, task := range page.Tasks {
			if task == nil || !holds(task, messageID) {
				continue
			}
			if i, seen := byID[task.ID]; !seen { // a task can show on two pages while the list shifts
				byID[task.ID] = len(holders)
				holders = append(holders, task)
			} else if old := holders[i].Status.Timestamp; old == nil || (task.Status.Timestamp != nil && !task.Status.Timestamp.Before(*old)) {
				holders[i] = task // keep the newest copy
			}
		}
		switch next := page.NextPageToken; {
		case next == "":
			return preferred(holders)
		case seen[next]:
			return nil, nil, fmt.Errorf("find task: the task list of context %s repeats page token %q", contextID, next)
		default:
			seen[next], req.PageToken = true, next
		}
	}
	return nil, nil, fmt.Errorf("find task: the task list of context %s ran past %d pages", contextID, maxListPages)
}

// preferred orders the holders by earlier, and returns the first with every ID.
func preferred(holders []*a2a.Task) (*a2a.Task, []a2a.TaskID, error) {
	if len(holders) == 0 {
		return nil, nil, nil
	}
	slices.SortStableFunc(holders, func(a, b *a2a.Task) int {
		switch {
		case earlier(a, b):
			return -1
		case earlier(b, a):
			return 1
		}
		return 0
	})
	ids := make([]a2a.TaskID, len(holders))
	for i, h := range holders {
		ids[i] = h.ID
	}
	return holders[0], ids, nil
}

func (a *Agent) listTasks(ctx context.Context, conn Conn, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	ctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	defer cancel()
	page, err := conn.ListTasks(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("list tasks of context %s: %w", req.ContextID, typed(err, false))
	}
	return page, nil
}

func holds(task *a2a.Task, messageID string) bool {
	return slices.ContainsFunc(task.History, func(m *a2a.Message) bool { return m != nil && m.ID == messageID })
}

// earlier reports whether task ranks before best in FindTask's answer: a task that has not
// ended beats one that has, then the earlier status timestamp wins, then the lower ID.
func earlier(task, best *a2a.Task) bool {
	if live, bestLive := !task.Status.State.Terminal(), !best.Status.State.Terminal(); live != bestLive {
		return live
	}
	at, bt := task.Status.Timestamp, best.Status.Timestamp
	switch {
	case at != nil && bt != nil && !at.Equal(*bt):
		return at.Before(*bt)
	case (at == nil) != (bt == nil):
		return at != nil
	}
	return strings.Compare(string(task.ID), string(best.ID)) < 0
}
