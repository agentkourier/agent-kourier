package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/agentkourier/agent-kourier/internal/telemetry"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

// maxListPages bounds FindTask's walk, so a server that never ends its pages cannot hold it. A page that had to be asked for
// smaller than the server's own is shorter, so the walk may need more of them; the bound is 200 of the smallest (minListPageSize).
const maxListPages = 200

// A page of ListTasks that is over dialect.MaxResponseBytes is asked for again at half the size, down to minListPageSize. The
// protocol's default page is 50 tasks (UNCONFIRMED for every backend: kagent's is named in the comment of FindTask), and the
// server's own is not known, so the first retry is half of it. Each retry reads up to the bound again, so there are few.
const (
	defaultListPageSize = 50
	minListPageSize     = 3
)

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
// once.
//
// A page past dialect.MaxResponseBytes (each task of it carries its history) is asked for again at half the size, down to
// minListPageSize, and the pages after it at the size that fit. If a page does not fit even then, and an earlier page held
// the turn, the holders found so far are the answer: the agent has the turn. If none did, the lookup fails with an error that
// wraps dialect.ErrResponseTooLarge, and the caller does not know whether the agent has the turn: the agent sends the same page
// again, so the lookup can never answer, and the caller must not send the turn again to find out. UNCONFIRMED: WP-03 measured
// the lookup on a context of three tasks; paging past kagent's page size of 50 is not measured, nor is a page token
// continued with another page size.
func (a *Agent) FindTask(ctx context.Context, contextID, messageID string) (telemetryResult0 *a2a.Task, telemetryResult1 []a2a.TaskID, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.agent.find")
	defer func() { finishAgent(op, telemetryErr) }()
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
		page, err := a.listPage(ctx, conn, req)
		if tooLarge(err) && len(holders) > 0 {
			return preferred(holders)
		}
		if err != nil {
			return nil, nil, err
		}
		for _, task := range page.Tasks {
			if task == nil || !holds(task, messageID) {
				continue
			}
			if i, known := byID[task.ID]; !known { // a task can show on two pages while the list shifts
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

// listPage reads the page req asks for, and asks for it again at half the size while it is over the bound, down to
// minListPageSize. The size that worked stays in req, for the pages after it.
func (a *Agent) listPage(ctx context.Context, conn Conn, req *a2a.ListTasksRequest) (*a2a.ListTasksResponse, error) {
	for {
		page, err := a.listTasks(ctx, conn, req)
		if !tooLarge(err) {
			return page, err
		}
		half := cmp.Or(req.PageSize, defaultListPageSize) / 2
		if half < minListPageSize {
			return nil, err
		}
		req.PageSize = half
	}
}

func (a *Agent) listTasks(ctx context.Context, conn Conn, req *a2a.ListTasksRequest) (telemetryResult0 *a2a.ListTasksResponse, telemetryErr error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.agent.list")
	defer func() { finishAgent(op, telemetryErr) }()
	ctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	defer cancel()
	page, err := conn.ListTasks(ctx, req)
	if refusedLocally(err) {
		// On a send this means busy (typed), but a list is never busy: the agent has no list (A2A 0.3).
		return nil, fmt.Errorf("list tasks of context %s: %w: %w", req.ContextID, ErrLookupUnsupported, err)
	}
	if err != nil {
		return nil, fmt.Errorf("list tasks of context %s: %w", req.ContextID, typed(classOnly(err), false))
	}
	return page, nil
}

// refusedLocally reports whether err is the client's own "not supported": the SDK's 0.3 transport has no
// tasks/list and answers ErrUnsupportedOperation without a request. A -32004 that an agent answered with
// arrives as an *a2a.Error that wraps the same sentinel, and means what it always has (busy, or in a lifecycle
// operation): it must stay ErrBusy, so that a 1.0 agent's lookup is retried and never taken for an absent turn.
func refusedLocally(err error) bool {
	var wire *a2a.Error
	return errors.Is(err, a2a.ErrUnsupportedOperation) && !errors.As(err, &wire)
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
