package store_test

import (
	"context"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
)

// fakeAdapter is a store.Adapter that stores nothing: every method records that it was called, remembers the row it was
// given where the guard may have changed the row, and returns zero values and err. A call that never reaches it is a call
// the guard refused.
type fakeAdapter struct {
	calls []string
	row   any
	err   error
}

var _ store.Adapter = (*fakeAdapter)(nil)

func (f *fakeAdapter) StaleTaskTurn(_ context.Context, _ store.ConversationKey, _ string) (store.TaskTurn, error) {
	f.record("StaleTaskTurn")
	return zero[store.TaskTurn](), f.err
}

func (f *fakeAdapter) record(name string) { f.calls = append(f.calls, name) }

func zero[T any]() (z T) { return z }

func (f *fakeAdapter) Close() error {
	f.record("Close")
	return f.err
}

func (f *fakeAdapter) ClaimReplyInteraction(_ context.Context, _, _ string) error {
	f.record("ClaimReplyInteraction")
	return f.err
}

func (f *fakeAdapter) AbandonOutbox(_ context.Context, _ string, _ time.Time) (bool, error) {
	f.record("AbandonOutbox")
	return zero[bool](), f.err
}

func (f *fakeAdapter) AbandonOutboxBefore(_ context.Context, _ time.Time, _ time.Time) ([]string, error) {
	f.record("AbandonOutboxBefore")
	return zero[[]string](), f.err
}

func (f *fakeAdapter) AbandonStrandedReplies(_ context.Context, _ time.Time, _ time.Duration) (int, error) {
	f.record("AbandonStrandedReplies")
	return zero[int](), f.err
}

func (f *fakeAdapter) AdmitTriggerRun(_ context.Context, _ store.TriggerRun, _ store.TriggerRunLimits) (store.TriggerAdmission, error) {
	f.record("AdmitTriggerRun")
	return zero[store.TriggerAdmission](), f.err
}

func (f *fakeAdapter) AppendAudit(_ context.Context, a1 store.AuditEntry) error {
	f.record("AppendAudit")
	f.row = a1
	return f.err
}

func (f *fakeAdapter) AppendEvent(_ context.Context, a1 store.Event) (bool, error) {
	f.record("AppendEvent")
	f.row = a1
	return zero[bool](), f.err
}

func (f *fakeAdapter) ClaimEvent(_ context.Context, _ time.Time, _ time.Duration) (store.Event, error) {
	f.record("ClaimEvent")
	return zero[store.Event](), f.err
}

func (f *fakeAdapter) ClearSettledInteractions(_ context.Context, _ time.Time, _ time.Duration, _ time.Duration) (int, error) {
	f.record("ClearSettledInteractions")
	return zero[int](), f.err
}

func (f *fakeAdapter) CloseTriggerThread(_ context.Context, _ string, _ string, _ time.Time) (bool, error) {
	f.record("CloseTriggerThread")
	return zero[bool](), f.err
}

func (f *fakeAdapter) CompleteEvent(_ context.Context, _ string, _ int) error {
	f.record("CompleteEvent")
	return f.err
}

func (f *fakeAdapter) CreateInteraction(_ context.Context, a1 store.Interaction) (bool, error) {
	f.record("CreateInteraction")
	f.row = a1
	return zero[bool](), f.err
}

func (f *fakeAdapter) CreateOutbox(_ context.Context, a1 store.OutboxEntry) (bool, error) {
	f.record("CreateOutbox")
	f.row = a1
	return zero[bool](), f.err
}

func (f *fakeAdapter) DeletePress(_ context.Context, _ int64) error {
	f.record("DeletePress")
	return f.err
}

func (f *fakeAdapter) EndStaleTask(_ context.Context, _ store.SessionHorizons, _ store.StaleTask, _ time.Time) (store.EndedStaleTask, error) {
	f.record("EndStaleTask")
	return zero[store.EndedStaleTask](), f.err
}

func (f *fakeAdapter) EnqueueReply(_ context.Context, a1 store.Reply) (bool, error) {
	f.record("EnqueueReply")
	f.row = a1
	return zero[bool](), f.err
}

func (f *fakeAdapter) FailEvent(_ context.Context, _ string, _ int, _ time.Time, _ int, _ string) error {
	f.record("FailEvent")
	return f.err
}

func (f *fakeAdapter) GetEvent(_ context.Context, _ string) (store.Event, error) {
	f.record("GetEvent")
	return zero[store.Event](), f.err
}

func (f *fakeAdapter) GetInteraction(_ context.Context, _ string) (store.Interaction, error) {
	f.record("GetInteraction")
	return zero[store.Interaction](), f.err
}

func (f *fakeAdapter) GetOutbox(_ context.Context, _ string) (store.OutboxEntry, error) {
	f.record("GetOutbox")
	return zero[store.OutboxEntry](), f.err
}

func (f *fakeAdapter) GetSession(_ context.Context, _ store.ConversationKey, _ time.Duration, _ time.Time) (store.Session, error) {
	f.record("GetSession")
	return zero[store.Session](), f.err
}

func (f *fakeAdapter) GetTriggerThread(_ context.Context, _ string, _ string) (store.TriggerThread, error) {
	f.record("GetTriggerThread")
	return zero[store.TriggerThread](), f.err
}

func (f *fakeAdapter) HasReply(_ context.Context, _ string) (bool, error) {
	f.record("HasReply")
	return zero[bool](), f.err
}

func (f *fakeAdapter) HasReplies(_ context.Context, _ []string) (map[string]bool, error) {
	f.record("HasReplies")
	return zero[map[string]bool](), f.err
}

func (f *fakeAdapter) ListAcknowledgmentReplies(_ context.Context) ([]store.Reply, error) {
	f.record("ListAcknowledgmentReplies")
	return zero[[]store.Reply](), f.err
}

func (f *fakeAdapter) ListActiveSessions(_ context.Context) ([]store.Session, error) {
	f.record("ListActiveSessions")
	return zero[[]store.Session](), f.err
}

func (f *fakeAdapter) ListAudit(_ context.Context, _ time.Time, _ int64, _ int) ([]store.AuditEntry, error) {
	f.record("ListAudit")
	return zero[[]store.AuditEntry](), f.err
}

func (f *fakeAdapter) ListDecidedInteractions(_ context.Context, _ time.Time) ([]store.Interaction, error) {
	f.record("ListDecidedInteractions")
	return zero[[]store.Interaction](), f.err
}

func (f *fakeAdapter) ListDueInteractions(_ context.Context, _ time.Time) ([]store.Interaction, error) {
	f.record("ListDueInteractions")
	return zero[[]store.Interaction](), f.err
}

func (f *fakeAdapter) ListPendingAcknowledgmentReplies(_ context.Context) ([]store.Reply, error) {
	f.record("ListPendingAcknowledgmentReplies")
	return zero[[]store.Reply](), f.err
}

func (f *fakeAdapter) ListPresses(_ context.Context) ([]store.Press, error) {
	f.record("ListPresses")
	return zero[[]store.Press](), f.err
}

func (f *fakeAdapter) ListQueuedSessions(_ context.Context) ([]store.ConversationKey, error) {
	f.record("ListQueuedSessions")
	return zero[[]store.ConversationKey](), f.err
}

func (f *fakeAdapter) ListStaleTasks(_ context.Context, _ store.SessionHorizons, _ int) ([]store.StaleTask, error) {
	f.record("ListStaleTasks")
	return zero[[]store.StaleTask](), f.err
}

func (f *fakeAdapter) ListTriggerDigests(_ context.Context, _ time.Time) ([]store.TriggerDigest, error) {
	f.record("ListTriggerDigests")
	return zero[[]store.TriggerDigest](), f.err
}

func (f *fakeAdapter) ListUnposted(_ context.Context) ([]store.OutboxEntry, error) {
	f.record("ListUnposted")
	return zero[[]store.OutboxEntry](), f.err
}

func (f *fakeAdapter) ListVotes(_ context.Context, _ store.ConversationKey) ([]store.FeedbackVote, error) {
	f.record("ListVotes")
	return zero[[]store.FeedbackVote](), f.err
}

func (f *fakeAdapter) MarkInteractionDelivered(_ context.Context, _ string, _ time.Time) error {
	f.record("MarkInteractionDelivered")
	return f.err
}

func (f *fakeAdapter) MarkReplyDelivered(_ context.Context, _ string, _ time.Time) error {
	f.record("MarkReplyDelivered")
	return f.err
}

func (f *fakeAdapter) MarkTriggerDigested(_ context.Context, _ string, _ int, _ time.Time) error {
	f.record("MarkTriggerDigested")
	return f.err
}

func (f *fakeAdapter) MarkTriggerStarted(_ context.Context, _ string, _ string, _ string, _ time.Time) error {
	f.record("MarkTriggerStarted")
	return f.err
}

func (f *fakeAdapter) NextReply(_ context.Context, _ store.ConversationKey) (store.Reply, error) {
	f.record("NextReply")
	return zero[store.Reply](), f.err
}

func (f *fakeAdapter) NextReplyAfter(_ context.Context, _ store.ConversationKey, _ int64) (store.Reply, error) {
	f.record("NextReplyAfter")
	return zero[store.Reply](), f.err
}

func (f *fakeAdapter) OpenTriggerThread(_ context.Context, _ store.TriggerThread, _ store.TriggerStale) (store.TriggerThread, bool, error) {
	f.record("OpenTriggerThread")
	return zero[store.TriggerThread](), zero[bool](), f.err
}

func (f *fakeAdapter) PendingPresses(_ context.Context, _ string, _ time.Time) (bool, error) {
	f.record("PendingPresses")
	return zero[bool](), f.err
}

func (f *fakeAdapter) PendingTextQuestion(_ context.Context, _ store.ConversationKey) (store.Interaction, error) {
	f.record("PendingTextQuestion")
	return zero[store.Interaction](), f.err
}

func (f *fakeAdapter) PreviousReply(_ context.Context, _ store.ConversationKey, _ int64) (store.Reply, error) {
	f.record("PreviousReply")
	return zero[store.Reply](), f.err
}

func (f *fakeAdapter) PurgeAudit(_ context.Context, _ time.Time, _ int) (int, error) {
	f.record("PurgeAudit")
	return zero[int](), f.err
}

func (f *fakeAdapter) PurgeExpiredSessions(_ context.Context, _ store.SessionHorizons, _ int) (store.PurgedSessions, error) {
	f.record("PurgeExpiredSessions")
	return zero[store.PurgedSessions](), f.err
}

func (f *fakeAdapter) PurgePresses(_ context.Context, _ time.Time, _ time.Duration) (int, error) {
	f.record("PurgePresses")
	return zero[int](), f.err
}

func (f *fakeAdapter) PurgeTriggerRuns(_ context.Context, _ time.Time) (int, error) {
	f.record("PurgeTriggerRuns")
	return zero[int](), f.err
}

func (f *fakeAdapter) PurgeTriggerThreads(_ context.Context, _ time.Time, _ time.Time) (int, error) {
	f.record("PurgeTriggerThreads")
	return zero[int](), f.err
}

func (f *fakeAdapter) RecordMessageRef(_ context.Context, _ string, _ string) error {
	f.record("RecordMessageRef")
	return f.err
}

func (f *fakeAdapter) RecordPress(_ context.Context, a1 store.Press, _ store.PressLimits) (int64, error) {
	f.record("RecordPress")
	f.row = a1
	return zero[int64](), f.err
}

func (f *fakeAdapter) RecordVote(_ context.Context, _ store.FeedbackVote) (store.Verdict, error) {
	f.record("RecordVote")
	return zero[store.Verdict](), f.err
}

func (f *fakeAdapter) ReleaseTriggerThread(_ context.Context, _ string, _ string, _ string) (bool, error) {
	f.record("ReleaseTriggerThread")
	return zero[bool](), f.err
}

func (f *fakeAdapter) ReopenTriggerThread(_ context.Context, _ string, _ string, _ time.Time) error {
	f.record("ReopenTriggerThread")
	return f.err
}

func (f *fakeAdapter) ResetSession(_ context.Context, _ store.ConversationKey) error {
	f.record("ResetSession")
	return f.err
}

func (f *fakeAdapter) ResolveInteraction(_ context.Context, _ string, _ store.InteractionState, _ time.Time) error {
	f.record("ResolveInteraction")
	return f.err
}

func (f *fakeAdapter) SetEventStatus(_ context.Context, _ string, _ store.EventStatus, _ string) error {
	f.record("SetEventStatus")
	return f.err
}

func (f *fakeAdapter) SetInteractionMessageRef(_ context.Context, _ string, _ string) error {
	f.record("SetInteractionMessageRef")
	return f.err
}

func (f *fakeAdapter) SetSessionTask(_ context.Context, _ store.ConversationKey, _ string, _ string, _ store.TaskTurn) error {
	f.record("SetSessionTask")
	return f.err
}

func (f *fakeAdapter) ClearSessionAcknowledgment(_ context.Context, _ store.ConversationKey, _, _ string) error {
	f.record("ClearSessionAcknowledgment")
	return f.err
}

func (f *fakeAdapter) SettleAcknowledgmentReplies(_ context.Context, _ []string) error {
	f.record("SettleAcknowledgmentReplies")
	return f.err
}

func (f *fakeAdapter) SubmitInteraction(_ context.Context, _ string, _ []byte, _ string, _ time.Time) error {
	f.record("SubmitInteraction")
	return f.err
}

func (f *fakeAdapter) TouchSession(_ context.Context, _ store.ConversationKey, _ time.Time) error {
	f.record("TouchSession")
	return f.err
}

func (f *fakeAdapter) UpsertSession(_ context.Context, a1 store.Session) error {
	f.record("UpsertSession")
	f.row = a1
	return f.err
}
