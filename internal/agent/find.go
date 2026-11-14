package agent

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

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

// DefaultLookupTimeout is the default of Config.LookupTimeout: five minutes for the whole walk of FindTask, which is a call of its own
// for each page and for each size a page was asked for at. Without it, 200 pages and the halvings of each, read one at a time, hold the
// conversation's runner for hours.
const DefaultLookupTimeout = 5 * time.Minute

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
// page is a unary call under UnaryTimeout.
//
// A page past dialect.MaxResponseBytes (each task of it carries its history) is asked for again at half the size, down to
// minListPageSize, and the pages after it at the size that fit. The walk as a whole has Config.LookupTimeout and at most
// maxListPages pages, and a page token seen before ends it. If the walk cannot be completed for any of these reasons, and an
// earlier page held the turn, the holders found so far are the answer: the agent has the turn. If none did, the lookup fails with
// an error that wraps ErrLookupIncomplete (and the cause: dialect.ErrResponseTooLarge, a context deadline, ...): the caller does
// not know whether the agent has the turn, and it must not send the turn again to find out: a page too large or a token that comes again comes again, and a walk that took too long may end sooner another time but nothing says it will. A call
// that fails on the first page asked for at a size the walk shrank to is taken as incomplete too: whether a server continues a
// page token with another page size is not known (UNCONFIRMED), so an error there may be the shrink's own. The caller's own
// cancellation is none of these. UNCONFIRMED: WP-03 measured the lookup on a context of three tasks; paging past kagent's page
// size of 50 is not measured.
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
	walk, stop := context.WithTimeout(ctx, a.cfg.LookupTimeout)
	defer stop()
	var holders []*a2a.Task
	byID := map[a2a.TaskID]int{} // index in holders
	seen := map[string]bool{}
	l := &lister{a: a, conn: conn, req: &a2a.ListTasksRequest{ContextID: contextID}}
	// giveUp ends a walk that cannot be completed: with the holders found, if there are any, and else with the error, which says so.
	giveUp := func(err error) (*a2a.Task, []a2a.TaskID, error) {
		if len(holders) > 0 {
			return preferred(holders)
		}
		return nil, nil, err
	}
	for range maxListPages {
		page, err := l.page(walk)
		if err != nil && ctx.Err() == nil && walk.Err() != nil && !errors.Is(err, ErrLookupIncomplete) { // the walk's own deadline, not the caller's end
			err = fmt.Errorf("%w: the walk of the task list of context %s took more than %s: %w", ErrLookupIncomplete, contextID, a.cfg.LookupTimeout, err)
		}
		if errors.Is(err, ErrLookupIncomplete) {
			return giveUp(err)
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
			return giveUp(fmt.Errorf("%w: the task list of context %s repeats page token %q", ErrLookupIncomplete, contextID, next))
		default:
			seen[next], l.req.PageToken = true, next
		}
	}
	return giveUp(fmt.Errorf("%w: the task list of context %s ran past %d pages", ErrLookupIncomplete, contextID, maxListPages))
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

// lister reads the pages of FindTask's walk, at a size that it lowers when a page is over the bound.
type lister struct {
	a           *Agent
	conn        Conn
	req         *a2a.ListTasksRequest
	afterShrink bool // the size was lowered, and no call has been made at it yet
}

// page reads the page req asks for, and asks for it again at half the size while it is over the bound, down to minListPageSize,
// where it gives up with an error that wraps ErrLookupIncomplete. The size that worked stays in req, for the pages after it. The first
// call at a lowered size that is refused (a 4xx but a 408 or 429, or an error the agent answered with) is incomplete too: a page token may
// not be continued at another size, and the walk has no way to tell that from any other refusal. A 5xx, a timeout or a lost connection is
// no sign of that, and is the error it is, which is retried.
func (l *lister) page(ctx context.Context) (*a2a.ListTasksResponse, error) {
	for {
		page, err := l.a.listTasks(ctx, l.conn, l.req)
		switch {
		case err == nil:
			l.afterShrink = false
			return page, nil
		case tooLarge(err):
			half := cmp.Or(l.req.PageSize, defaultListPageSize) / 2
			if half < minListPageSize {
				return nil, fmt.Errorf("%w: %w", ErrLookupIncomplete, err)
			}
			l.req.PageSize, l.afterShrink = half, true
		case l.afterShrink && ctx.Err() == nil && (permanent(err) || isProtocol(err)):
			return nil, fmt.Errorf("%w: at a page size of %d, which the walk lowered it to: %w", ErrLookupIncomplete, l.req.PageSize, err)
		default:
			return nil, err
		}
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
