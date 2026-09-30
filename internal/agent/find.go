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
// none. It is how the caller finds a turn whose task it never recorded, for example after a
// crash: kagent does not deduplicate a resend that carries a contextId (WP-03, spec 8.2), so
// resending blindly would run the turn twice. Found: reconcile with GetTask, then
// SubscribeToTask if the task has not ended. Absent: send. A -32004 on that send means some
// task is active, possibly an input-required pause, so queue the turn and repeat the lookup
// when the active task ends.
//
// It follows nextPageToken to the end and does not rely on page order. If several tasks
// hold the messageId (the turn already ran twice), it returns one that has not ended, else
// the earliest. Each page is a unary call under UnaryTimeout. UNCONFIRMED: WP-03 measured
// the lookup on a context of three tasks; paging past kagent's page size of 50 is not measured.
func (a *Agent) FindTask(ctx context.Context, contextID, messageID string) (*a2a.Task, error) {
	if contextID == "" || messageID == "" {
		return nil, errors.New("find task: a contextId and a messageId are both required")
	}
	sctx, cancel := context.WithTimeout(ctx, a.cfg.UnaryTimeout)
	conn, err := a.source(sctx)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("connect to agent: %w", err)
	}
	var found *a2a.Task
	req := &a2a.ListTasksRequest{ContextID: contextID}
	for range maxListPages {
		page, err := a.listTasks(ctx, conn, req)
		if err != nil {
			return nil, err
		}
		for _, task := range page.Tasks {
			if task != nil && holds(task, messageID) && earlier(task, found) {
				found = task
			}
		}
		if page.NextPageToken == "" {
			return found, nil
		}
		req.PageToken = page.NextPageToken
	}
	return nil, fmt.Errorf("find task: the task list of context %s ran past %d pages", contextID, maxListPages)
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

// earlier reports whether task should replace best as FindTask's answer: a task that has
// not ended beats one that has, then the earlier status timestamp wins, then the lower ID.
func earlier(task, best *a2a.Task) bool {
	if best == nil {
		return true
	}
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
