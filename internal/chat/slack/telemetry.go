package slack

import (
	"context"
	"errors"

	slackgo "github.com/slack-go/slack"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func finishChat(ctx context.Context, op *telemetry.Operation, operation string, err error) {
	outcome := telemetry.ErrorClass(err)
	var rate *slackgo.RateLimitedError
	var response slackgo.SlackErrorResponse
	if errors.As(err, &rate) {
		outcome = "rate_limited"
	} else if errors.As(err, &response) {
		switch response.Err {
		case "invalid_auth", "not_authed", "account_inactive", "token_revoked":
			outcome = "auth_rejected"
		case "channel_not_found", "not_in_channel", "is_archived":
			outcome = "channel_gone"
		case "message_not_in_streaming_state":
			outcome = "not_streaming"
		default:
			outcome = "api_error"
		}
	}
	trace.SpanFromContext(ctx).SetAttributes(attribute.String("courier.platform", "slack"), attribute.String("courier.chat.operation", operation))
	telemetry.Count(ctx, "courier_chat_api_calls_total", operation, outcome)
	if outcome == "not_streaming" {
		op.FinishOutcome(nil, outcome)
	} else {
		op.FinishOutcome(err, outcome)
	}
}
