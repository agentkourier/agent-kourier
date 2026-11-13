package session

import (
	"cmp"
	"context"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// goneNotice tells the thread the agent no longer has the task's session. Its ID is the task's and the thread's, so
// the runner and a control that both find it gone post one notice.
func (m *Manager) goneNotice(ctx context.Context, key Key, id a2a.TaskID, ts string) {
	m.taskNotice(ctx, key, id, NoticeSessionGone, ts)
}

// taskNotice tells the thread something about the task id. The notice is named after the task and its
// kind, so that every path that finds the same fact, whichever turn row it holds, posts it once.
func (m *Manager) taskNotice(ctx context.Context, key Key, id a2a.TaskID, kind NoticeKind, ts string) {
	m.cfg.Sink.Notice(ctx, key, Notice{ID: taskNoticeID(key, id, kind), Kind: kind, MessageRef: ts})
}

// taskNoticeID names the notice of kind about the task id in the thread key. The thread is part of it
// because the outbox keeps one row per ID, and a backend's task IDs may repeat across threads (the stub
// agent's do): a notice named after the task alone would be dropped in the second thread.
func taskNoticeID(key Key, id a2a.TaskID, kind NoticeKind) string {
	return "task:" + key.Connection + ":" + key.Channel + ":" + key.Thread + ":" + string(id) + "/" + string(kind)
}

// Notice IDs that are not a NoticeKind's name. The outbox keeps one row for an ID and drops a second notice of it, so two notices of one
// task that say different things are named apart; each is the same across restarts, so that a restart that finds the same again raises
// the same post.
const (
	noticeTooLarge     NoticeKind = "too_large"      // what the agent sent was more than Agent Kourier reads or holds (tooLarge)
	noticeCardTooLarge NoticeKind = "card_too_large" // the agent's card was too large to follow the task (lost)
)

// tooLargeNoticeID names the notice of a task whose reply was too large (tooLarge), and, for a reply that names no task, of the turn.
func tooLargeNoticeID(t *tstate) string {
	if t.taskID == "" {
		return cmp.Or(t.nid, t.r.ID) + "/" + string(noticeTooLarge)
	}
	return taskNoticeID(t.key, t.taskID, noticeTooLarge)
}

func (m *Manager) notice(t *tstate, kind NoticeKind, detail string) {
	switch kind {
	case NoticeWedged, NoticeDuplicateRun, NoticeRetrying:
		telemetry.Count(m.ctx, "agentkourier_session_recovery_total", string(kind))
	}
	m.cfg.Sink.Notice(m.ctx, t.key, Notice{ID: cmp.Or(t.nid, t.r.ID) + "/" + string(kind), Kind: kind, MessageRef: t.r.MessageRef, Detail: detail})
}
