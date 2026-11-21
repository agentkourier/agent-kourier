package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/packages/ssestream"
)

// SDK is the Backend over the Anthropic Go SDK. Sessions run AgentID (its latest version) in EnvironmentID.
type SDK struct {
	Client        anthropic.Client
	AgentID       string
	EnvironmentID string
}

var _ Backend = (*SDK)(nil)

// CreateSession implements Backend.
func (s *SDK) CreateSession(ctx context.Context, title string, metadata map[string]string) (string, error) {
	session, err := s.Client.Beta.Sessions.New(ctx, anthropic.BetaSessionNewParams{
		Agent:         anthropic.BetaSessionNewParamsAgentUnion{OfString: anthropic.String(s.AgentID)},
		EnvironmentID: s.EnvironmentID,
		Title:         anthropic.String(title),
		Metadata:      metadata,
	})
	if err != nil {
		return "", err
	}
	return session.ID, nil
}

// Sessions implements Backend. It pages through every unarchived session of the agent and keeps the environment's
// sessions that carry an A2A context ID; a terminated session cannot take another turn, so it is left out.
func (s *SDK) Sessions(ctx context.Context) ([]SessionInfo, error) {
	pager := s.Client.Beta.Sessions.ListAutoPaging(ctx, anthropic.BetaSessionListParams{AgentID: anthropic.String(s.AgentID)})
	var out []SessionInfo
	for pager.Next() {
		sess := pager.Current()
		contextID := sess.Metadata[contextMetadataKey]
		if contextID == "" || sess.EnvironmentID != s.EnvironmentID || !sess.ArchivedAt.IsZero() ||
			sess.Status == anthropic.BetaManagedAgentsSessionStatusTerminated {
			continue
		}
		out = append(out, SessionInfo{ID: sess.ID, ContextID: contextID, LastActive: sess.UpdatedAt})
	}
	if err := pager.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Archive implements Backend. A session that no longer exists counts as archived.
func (s *SDK) Archive(ctx context.Context, sessionID string) error {
	_, err := s.Client.Beta.Sessions.Archive(ctx, sessionID, anthropic.BetaSessionArchiveParams{})
	if apiErr := (*anthropic.Error)(nil); errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

// Stream implements Backend.
func (s *SDK) Stream(ctx context.Context, sessionID string) (EventStream, error) {
	stream := s.Client.Beta.Sessions.Events.StreamEvents(ctx, sessionID, anthropic.BetaSessionEventStreamParams{})
	if err := stream.Err(); err != nil {
		_ = stream.Close()
		return nil, err
	}
	return &sdkStream{s: stream}, nil
}

// Send implements Backend.
func (s *SDK) Send(ctx context.Context, sessionID string, events ...Outbound) error {
	params := make([]anthropic.BetaManagedAgentsEventParamsUnion, 0, len(events))
	for _, ev := range events {
		switch {
		case ev.Message != nil:
			params = append(params, anthropic.BetaManagedAgentsEventParamsUnion{OfUserMessage: &anthropic.BetaManagedAgentsUserMessageEventParams{
				Content: []anthropic.BetaManagedAgentsUserMessageEventParamsContentUnion{{
					OfText: &anthropic.BetaManagedAgentsTextBlockParam{Text: *ev.Message},
				}},
			}})
		case ev.Confirmation != nil:
			c := &anthropic.BetaManagedAgentsUserToolConfirmationEventParams{
				ToolUseID: ev.Confirmation.ToolUseID,
				Result:    anthropic.BetaManagedAgentsUserToolConfirmationEventParamsResultDeny,
			}
			if ev.Confirmation.Allow {
				c.Result = anthropic.BetaManagedAgentsUserToolConfirmationEventParamsResultAllow
			} else if ev.Confirmation.DenyMessage != "" {
				c.DenyMessage = anthropic.String(ev.Confirmation.DenyMessage)
			}
			params = append(params, anthropic.BetaManagedAgentsEventParamsUnion{OfUserToolConfirmation: c})
		case ev.Interrupt:
			params = append(params, anthropic.BetaManagedAgentsEventParamsUnion{OfUserInterrupt: &anthropic.BetaManagedAgentsUserInterruptEventParams{}})
		}
	}
	_, err := s.Client.Beta.Sessions.Events.Send(ctx, sessionID, anthropic.BetaSessionEventSendParams{Events: params})
	return err
}

type sdkStream struct {
	s   *ssestream.Stream[anthropic.BetaManagedAgentsStreamSessionEventsUnion]
	cur Event
}

func (s *sdkStream) Next() bool {
	if !s.s.Next() {
		return false
	}
	raw := s.s.Current().RawJSON()
	var ev Event
	if err := json.Unmarshal([]byte(raw), &ev); err != nil {
		// Keep what identifies the event; the adapter ignores types it does not act on.
		var head struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(raw), &head)
		ev = Event{ID: head.ID, Type: head.Type}
	}
	s.cur = ev
	return true
}

func (s *sdkStream) Current() Event { return s.cur }
func (s *sdkStream) Err() error     { return s.s.Err() }
func (s *sdkStream) Close() error   { return s.s.Close() }
