package teams

import (
	"context"
	"errors"
	"net/http"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// goneCodes are the Bot Connector's codes of a 403 that refuses one conversation, not the bot: the bot was removed from the team
// (BotNotInConversationRoster), or the person blocked it (ConversationBlockedByUser). They are Permanent, as a conversation that is
// gone is, and not FatalAuth, which would stop the connection for every other channel. disabledCode is the one 403 that refuses the
// bot itself, in its tenant: an admin disabled the app (BotDisabledByAdmin), which is FatalAuth. Every other 403
// (ForbiddenOperationException, InvalidBotApiHost, NotEnoughPermissions, or a code not listed) refuses one call, and is Permanent
// with no more said. The codes are Microsoft's documented ones ("Status codes from agent conversational APIs" in Teams' platform
// documentation, read 2026-11-18), not measured.
var goneCodes = map[string]bool{"BotNotInConversationRoster": true, "ConversationBlockedByUser": true}

const disabledCode = "BotDisabledByAdmin"

// gone403 says whether ae is a 403 of goneCodes.
func gone403(ae *APIError) bool { return ae.Status == http.StatusForbidden && goneCodes[ae.Code] }

// Classify implements chat.Platform. What no retry mends:
//   - Entra's refusal of the bot's token (400, 401) and the Bot Connector's of the bot (401, and a 403 BotDisabledByAdmin) are
//     FatalAuth: the app, its secret or its tenant is wrong, the bot is not the Bot Framework's, or an admin disabled it;
//   - a conversation or a message that is not there (404, ActivityNotFoundInConversation and the like) and a 403 of goneCodes are
//     Permanent, ReasonChannelGone; a message too large (413, MessageSizeTooBig) is ReasonTooLong; any other 400 is
//     ReasonInvalidContent; any other 403 is ReasonOther;
//   - the errors of the Client itself: chat.ErrInvalid, chat.ErrTooLarge, chat.ErrWouldDropText, chat.ErrNoEphemeral,
//     chat.ErrNoReactions, and for ChannelAccess chat.ErrChannelNotFound.
//
// Anything else may pass on another try and is Transient: a 429, with its Retry-After (at least a second) as RetryAfter; a 412, a
// 5xx, a timeout, a network failure, a cancelled context. The caller's own backoff paces the retry (the outbox's doubles, with
// jitter where the caller adds it); the Client does not retry inside a call.
func (c *Client) Classify(_ chat.Op, err error) chat.Verdict {
	var (
		te *TokenRequestError
		ae *APIError
	)
	switch {
	case errors.As(err, &te):
		if te.rejected() {
			return chat.Verdict{Class: chat.FatalAuth, Code: te.Code}
		}
		return transient(te.Status, te.RetryAfter)
	case errors.Is(err, chat.ErrChannelNotFound):
		v := chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone}
		if errors.As(err, &ae) {
			v.Code = ae.Code
		}
		return v
	case errors.As(err, &ae):
		switch {
		case gone403(ae), ae.Status == http.StatusNotFound:
			return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: ae.Code}
		case ae.Status == http.StatusUnauthorized, ae.Status == http.StatusForbidden && ae.Code == disabledCode:
			return chat.Verdict{Class: chat.FatalAuth, Code: ae.Code}
		case ae.Status == http.StatusForbidden:
			return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther, Code: ae.Code}
		case ae.Status == http.StatusRequestEntityTooLarge:
			return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonTooLong, Code: ae.Code}
		case ae.Status == http.StatusBadRequest:
			return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent, Code: ae.Code}
		}
		return transient(ae.Status, ae.RetryAfter)
	case errors.Is(err, chat.ErrTooLarge):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonTooLong}
	case errors.Is(err, chat.ErrInvalid), errors.Is(err, chat.ErrWouldDropText):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonInvalidContent}
	case errors.Is(err, chat.ErrNoEphemeral), errors.Is(err, chat.ErrNoReactions):
		return chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonOther}
	}
	return chat.Verdict{Class: chat.Transient}
}

// transient is the verdict of an answer that may pass on another try: a rate limit (429) carries its wait, at least a second.
func transient(status int, wait time.Duration) chat.Verdict {
	if status != http.StatusTooManyRequests {
		return chat.Verdict{Class: chat.Transient}
	}
	return chat.Verdict{Class: chat.Transient, RetryAfter: max(wait, time.Second)}
}

// outcome is the label of a call's result in agentkourier_chat_api_calls_total: success, rate_limited, auth_rejected, channel_gone,
// too_long, api_error (another answer of Entra or the Bot Connector), invalid (content the Client refused before any call), or
// the class of a transport error (timeout, cancelled, error).
func (c *Client) outcome(err error) string {
	if err == nil {
		return "success"
	}
	var (
		te *TokenRequestError
		ae *APIError
	)
	v := c.Classify(chat.OpPost, err)
	switch {
	case v.Class == chat.FatalAuth:
		return "auth_rejected"
	case v.RetryAfter > 0:
		return "rate_limited"
	case v.Reason == chat.ReasonChannelGone:
		return "channel_gone"
	case v.Reason == chat.ReasonTooLong:
		return "too_long"
	case errors.As(err, &te), errors.As(err, &ae):
		return "api_error"
	case errors.Is(err, chat.ErrInvalid), errors.Is(err, chat.ErrWouldDropText):
		return "invalid"
	}
	return telemetry.ErrorClass(err)
}

// finish ends op and counts the call, by the connection.
func (c *Client) finish(ctx context.Context, op *telemetry.Operation, operation string, err error) {
	outcome := c.outcome(err)
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("agentkourier.platform", PlatformName),
		attribute.String("agentkourier.chat.operation", operation))
	telemetry.Count(ctx, "agentkourier_chat_api_calls_total", operation, outcome, c.conn)
	op.FinishOutcome(err, outcome)
}
