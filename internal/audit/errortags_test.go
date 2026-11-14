package audit

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/dialect"
)

// Every typed cause has the tag it has always had: a tag is stored in the audit log, so a renamed one would split a
// cause's rows in two. The expectations are written out, not read from the table the tags come from.
func TestErrorTagsPinned(t *testing.T) {
	for _, tc := range []struct {
		err error
		tag string
	}{
		{agent.ErrCredentialsRejected, "credentials_rejected"},
		{agent.ErrNotSent, "not_sent"},
		{agent.ErrOutcomeUnknown, "outcome_unknown"},
		{agent.ErrMessageReply, "message_reply"},
		{agent.ErrStreamLost, "stream_lost"},
		{agent.ErrNotReady, "not_ready"},
		{agent.ErrBusy, "busy"},
		{agent.ErrForbidden, "forbidden"},
		{agent.ErrAgentForbidden, "agent_forbidden"},
		{agent.ErrAnswerRejected, "answer_rejected"},
		{agent.ErrCancelFailed, "cancel_failed"},
		{agent.ErrTokenRequired, "token_required"},
		{agent.ErrTokenUnreadable, "token_unreadable"},
		{agent.ErrHeaderUnreadable, "header_unreadable"},
		{agent.ErrPoolClosed, "pool_closed"},
		{agent.ErrStreamingUnsupported, "streaming_unsupported"},
		{dialect.ErrCardTooLarge, "card_too_large"},
		{agent.ErrLookupIncomplete, "lookup_incomplete"},
		// These two differ from the agent metrics (finishAgent in internal/agent), and the two mappings must not be
		// merged: the metrics call them already_ended and not_found and record them with no error, while this log
		// tags every cause alike. These tags are stored rows and the metric outcomes are label values, so neither set
		// can be renamed to match the other.
		{a2a.ErrTaskNotCancelable, "task_not_cancelable"},
		{a2a.ErrTaskNotFound, "task_not_found"},
		{context.Canceled, "canceled"},
		{context.DeadlineExceeded, "deadline_exceeded"},
	} {
		t.Run(tc.tag, func(t *testing.T) {
			for _, err := range []error{tc.err, fmt.Errorf("binding ns/b: %w", tc.err)} {
				if got := errorTags(err); !slices.Equal(got, []string{tc.tag}) {
					t.Errorf("errorTags(%v) = %q; want [%q]", err, got, tc.tag)
				}
			}
		})
	}
}

// An error that carries several causes is tagged with each, in one fixed order, which is the order of the stored row.
// The agent metrics do not share it: finishAgent in internal/agent names one outcome, the first of its cases to match.
func TestErrorTagsOrderPinned(t *testing.T) {
	every := errors.Join(context.DeadlineExceeded, context.Canceled, a2a.ErrTaskNotFound, a2a.ErrTaskNotCancelable,
		agent.ErrLookupIncomplete, dialect.ErrCardTooLarge, agent.ErrStreamingUnsupported, agent.ErrPoolClosed,
		agent.ErrHeaderUnreadable, agent.ErrTokenUnreadable, agent.ErrTokenRequired, agent.ErrCancelFailed,
		agent.ErrAnswerRejected, agent.ErrAgentForbidden, agent.ErrForbidden, agent.ErrBusy, agent.ErrNotReady,
		agent.ErrStreamLost, agent.ErrMessageReply, agent.ErrOutcomeUnknown, agent.ErrNotSent, agent.ErrCredentialsRejected)
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{"no typed cause", errors.New("boom"), []string{"error"}},
		{"every typed cause, joined in reverse", every, []string{"credentials_rejected", "not_sent", "outcome_unknown",
			"message_reply", "stream_lost", "not_ready", "busy", "forbidden", "agent_forbidden", "answer_rejected",
			"cancel_failed", "token_required", "token_unreadable", "header_unreadable", "pool_closed",
			"streaming_unsupported", "card_too_large", "lookup_incomplete", "task_not_cancelable", "task_not_found",
			"canceled", "deadline_exceeded"}},
		{"card 403 on a reconnect", fmt.Errorf("%w: %w", agent.ErrStreamLost, agent.ErrAgentForbidden),
			[]string{"stream_lost", "agent_forbidden"}},
		{"answer rejected, its cancel not cancelable", errors.Join(agent.ErrAnswerRejected, a2a.ErrTaskNotCancelable),
			[]string{"answer_rejected", "task_not_cancelable"}},
		{"answer rejected, its task gone", errors.Join(agent.ErrAnswerRejected, a2a.ErrTaskNotFound),
			[]string{"answer_rejected", "task_not_found"}},
		{"not ready, so busy", fmt.Errorf("x: %w: %w", agent.ErrNotReady, agent.ErrBusy), []string{"not_ready", "busy"}},
		{"busy and refused", errors.Join(agent.ErrBusy, agent.ErrCredentialsRejected), []string{"credentials_rejected", "busy"}},
		{"lookup incomplete on a lost stream", errors.Join(agent.ErrLookupIncomplete, agent.ErrStreamLost),
			[]string{"stream_lost", "lookup_incomplete"}},
		{"not found and not cancelable", errors.Join(a2a.ErrTaskNotFound, a2a.ErrTaskNotCancelable),
			[]string{"task_not_cancelable", "task_not_found"}},
		{"outcome unknown and forbidden", errors.Join(agent.ErrForbidden, agent.ErrOutcomeUnknown),
			[]string{"outcome_unknown", "forbidden"}},
		{"typed causes, then the response and the status", fmt.Errorf("%w: %w: %w", agent.ErrNotSent, dialect.ErrResponseTooLarge,
			&dialect.HTTPStatusError{Code: 413}), []string{"not_sent", "response_too_large", "http_413"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := errorTags(tc.err); !slices.Equal(got, tc.want) {
				t.Errorf("errorTags = %q; want %q", got, tc.want)
			}
		})
	}
}
