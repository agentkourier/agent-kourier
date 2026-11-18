package broker

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/teams"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// teamsConversations is the teams.ConversationStore of the Microsoft Teams receivers over the store, so that what Teams tells the
// receiver of a channel outlives a restart and moves with the leader: an alert read from Graph may come before any activity. Only the
// leader receives activities, and the store refuses a write of a replica that is not the leader (store.ErrDeposed), which Remember
// returns. MemoryConversations refuses only an empty channel, and only in Remember; this store also refuses an empty connection, and
// refuses both in Lookup.
type teamsConversations struct {
	s store.TeamsConversations
	// now is the time a record is remembered at.
	now func() time.Time
}

var _ teams.ConversationStore = (*teamsConversations)(nil)

// newTeamsConversations returns the teams.ConversationStore over s, the broker's store.
func newTeamsConversations(s store.TeamsConversations) *teamsConversations {
	return &teamsConversations{s: s, now: time.Now}
}

// Remember implements teams.ConversationStore.
func (t *teamsConversations) Remember(ctx context.Context, conn string, c teams.Conversation) error {
	err := t.s.RememberTeamsConversation(ctx, store.TeamsConversation{
		Connection: conn, Channel: c.Channel,
		ServiceURL: c.ServiceURL, TeamID: c.TeamID, AADGroupID: c.AADGroupID, TenantID: c.TenantID,
		UpdatedAt: t.now(),
	})
	if err != nil {
		return fmt.Errorf("teams conversation store: %w", err)
	}
	return nil
}

// Lookup implements teams.ConversationStore.
func (t *teamsConversations) Lookup(ctx context.Context, conn, channel string) (teams.Conversation, bool, error) {
	r, err := t.s.GetTeamsConversation(ctx, conn, channel)
	if errors.Is(err, store.ErrNotFound) {
		return teams.Conversation{}, false, nil
	}
	if err != nil {
		return teams.Conversation{}, false, fmt.Errorf("teams conversation store: %w", err)
	}
	return teams.Conversation{Channel: r.Channel, ServiceURL: r.ServiceURL, TeamID: r.TeamID, AADGroupID: r.AADGroupID, TenantID: r.TenantID}, true, nil
}
