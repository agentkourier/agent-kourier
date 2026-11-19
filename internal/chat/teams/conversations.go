package teams

import (
	"cmp"
	"context"
	"errors"
	"sync"
)

// Conversation is what Teams tells a bot about a channel only in what it sends the bot: where to post to it, and which team it is in.
// Posting needs the service URL, and Graph (thread history, alert intake) the team's Microsoft Entra group ID, so the
// receiver records them from every verified activity of the connection's tenant, the bot's install events included.
type Conversation struct {
	// Channel is the channel's ID, 19:…@thread.tacv2: the key. A team's ID is its General channel's.
	Channel string
	// ServiceURL is the base URL of the Bot Connector for the conversation, the activity's serviceUrl, which the token's claim was
	// checked against.
	ServiceURL string
	// TeamID is the team's Teams ID, and AADGroupID the team's Microsoft Entra group ID, which Graph names it by.
	TeamID, AADGroupID string
	// TenantID is the tenant's ID, the connection's own.
	TenantID string
}

// merge is c with the fields that n sets taken from n: an activity that leaves a field out says nothing of it.
func (c Conversation) merge(n Conversation) Conversation {
	return Conversation{
		Channel:    cmp.Or(n.Channel, c.Channel),
		ServiceURL: cmp.Or(n.ServiceURL, c.ServiceURL),
		TeamID:     cmp.Or(n.TeamID, c.TeamID),
		AADGroupID: cmp.Or(n.AADGroupID, c.AADGroupID),
		TenantID:   cmp.Or(n.TenantID, c.TenantID),
	}
}

// ConversationStore keeps the Conversation of each channel of a connection. Remember merges: a field the record leaves empty keeps
// the value it had. Both are safe for concurrent use.
type ConversationStore interface {
	// Remember merges c into what is kept for the channel c.Channel of the connection conn ("namespace/name").
	Remember(ctx context.Context, conn string, c Conversation) error
	// Lookup returns what is kept for channel of conn, and false when nothing is.
	Lookup(ctx context.Context, conn, channel string) (Conversation, bool, error)
}

// MemoryConversations is a ConversationStore in memory: it is lost when the broker stops, and a replica that takes over learns the
// channels again from the activities that come to it. It holds one record for each channel of the connection's tenant that the bot
// has heard from: only activities that passed the token and tenant checks are recorded.
type MemoryConversations struct {
	mu   sync.Mutex
	recs map[[2]string]Conversation
}

// NewMemoryConversations returns an empty MemoryConversations.
func NewMemoryConversations() *MemoryConversations {
	return &MemoryConversations{recs: map[[2]string]Conversation{}}
}

// Remember implements ConversationStore.
func (m *MemoryConversations) Remember(_ context.Context, conn string, c Conversation) error {
	if c.Channel == "" {
		return errors.New("teams conversation store: a record with no channel")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	k := [2]string{conn, c.Channel}
	m.recs[k] = m.recs[k].merge(c)
	return nil
}

// Lookup implements ConversationStore.
func (m *MemoryConversations) Lookup(_ context.Context, conn, channel string) (Conversation, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.recs[[2]string{conn, channel}]
	return c, ok, nil
}

var _ ConversationStore = (*MemoryConversations)(nil)
