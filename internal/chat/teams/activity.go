package teams

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/chat"
)

// PlatformName is Microsoft Teams' name in a chat.Ref, the ChatConnection's spec.platform.
const PlatformName = v1alpha1.PlatformTeams

// What an activity is, is Microsoft's (the Bot Framework's Activity schema), and what Teams puts in one was read from the live run of
// 2026-11-18 on the test tenant: a channel message's conversation.id is "<channel>;messageid=<root>", for a new top-level post too, with
// replyToId empty; channelData carries the channel, the team with its Microsoft Entra group ID, and the tenant; from carries the
// person's Teams ID (29:…) and their Entra object ID; a bot is 28:<app ID>; a mention is an entity of type mention.
const (
	// teamsChannel is the channelId of every activity Teams sends.
	teamsChannel = "msteams"
	// botPrefix begins the Teams ID of a bot: 28:<app ID>.
	botPrefix = "28:"
	// threadMarker joins a channel and the root of a thread in a conversation ID.
	threadMarker = ";messageid="
	// channelConversation is the conversationType of a team's channel; personal and groupChat are the others.
	channelConversation = "channel"
	// maxActivityID caps an activity's ID. Real ones are a message's 13 digits or an invoke's "f:" and a GUID (38 bytes); a longer one
	// is refused, so that the dedup and the message IDs built from it stay small.
	maxActivityID = 64
	// idSeparator is what chat.MessageID joins a message's platform, connection, channel and ID on, so no message ID may hold it.
	idSeparator = ":"
)

// account is a sender or a mentioned party.
type account struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	AADObjectID string `json:"aadObjectId"`
}

// activity is the part of a Bot Framework activity the adapter reads. Every field is untrusted: Microsoft signed the request, but the
// text is what a person wrote.
type activity struct {
	Type         string  `json:"type"`
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	ChannelID    string  `json:"channelId"`
	ServiceURL   string  `json:"serviceUrl"`
	Text         string  `json:"text"`
	ReplyToID    string  `json:"replyToId"`
	From         account `json:"from"`
	Conversation struct {
		ID               string `json:"id"`
		ConversationType string `json:"conversationType"`
	} `json:"conversation"`
	Entities []struct {
		Type      string   `json:"type"`
		Mentioned *account `json:"mentioned"`
	} `json:"entities"`
	ChannelData struct {
		Team *struct {
			ID         string `json:"id"`
			AADGroupID string `json:"aadGroupId"`
		} `json:"team"`
		Tenant *struct {
			ID string `json:"id"`
		} `json:"tenant"`
	} `json:"channelData"`
	Value json.RawMessage `json:"value"`
}

// decodeActivity reads a body as an activity of Teams, and refuses one that is not a JSON object or not of the msteams channel. It
// quotes nothing of the body.
func decodeActivity(body []byte) (activity, error) {
	var a activity
	if err := json.Unmarshal(body, &a); err != nil {
		return activity{}, errors.New("the body is not an activity in JSON")
	}
	if a.ChannelID != teamsChannel {
		return activity{}, fmt.Errorf("the activity's channelId is not %s", teamsChannel)
	}
	if len(a.ID) > maxActivityID {
		return activity{}, fmt.Errorf("the activity's ID is longer than %d bytes", maxActivityID)
	}
	return a, nil
}

// tenant is the tenant the activity says it comes from, channelData.tenant.id, or "".
func (a activity) tenant() string {
	if a.ChannelData.Tenant == nil {
		return ""
	}
	return a.ChannelData.Tenant.ID
}

// activityKind is what the adapter does with an activity.
type activityKind int

const (
	// kindIgnored is acknowledged and becomes no message: an edit or deletion (the Slack adapter ignores those too), an install
	// event (conversationUpdate, installationUpdate), which only tells the conversation store, typing, a reaction, and any type the
	// adapter does not know.
	kindIgnored activityKind = iota
	// kindMessage is a message, which may be a person's post for the Sink.
	kindMessage
	// kindInvoke is an invoke, answered in the response to its request (InvokeHandler).
	kindInvoke
)

// kind says what the adapter does with a.
func (a activity) kind() activityKind {
	switch a.Type {
	case "message":
		return kindMessage
	case "invoke":
		return kindInvoke
	}
	return kindIgnored
}

// verdict is what message makes of a message activity.
type verdict int

const (
	// dropped is acknowledged, and nothing more: no person's post.
	dropped verdict = iota
	// forward is a person's post for the Sink.
	forward
	// ownPost is a post of Agent Kourier itself, which Teams does not send a bot, and which is never forwarded.
	ownPost
	// invalid is an activity that cannot name its message: no ID, an ID with ':', or no channel.
	invalid
)

// splitConversation is the channel and the thread root of a conversation ID, "<channel>;messageid=<root>"; root is "" when the ID
// names no thread.
func splitConversation(id string) (channel, root string) {
	channel, root, _ = strings.Cut(id, threadMarker)
	return channel, root
}

// threadOf is the Thread of a chat.Ref for the message id in a conversation whose thread root is root: "" for a message that is the
// root, or names none, since a Ref's Thread is "" for a message in no thread (chat.Ref) and Teams names a new top-level post as the
// root of its own thread.
func threadOf(root, id string) string {
	if root == id {
		return ""
	}
	return root
}

// message maps a message activity of the connection conn, whose bot has the app ID appID, to the neutral model, with what to do with
// it and, for one that is not forwarded, why (for a log line; it quotes nothing a person wrote).
//   - The channel is conversation.id before ";messageid=", and the thread its root after it. A post that opens a thread, which Teams
//     names as its own root, has no Thread, as a top-level post on Slack has none.
//   - The user is the person's Microsoft Entra object ID (aadObjectId), the shape of a mention's user in a Binding. A post with none
//     is no person's, and is dropped.
//   - A sender whose ID begins 28: is a bot. Agent Kourier's own (28:<appID>) is an ownPost; another bot's is dropped, since Teams
//     delivers no other bot's post to a bot (alerts come in through Graph).
//   - Mentioned is set by a mention entity that names 28:<appID>, never by the <at> text, which a person can type.
//   - The text is the activity's, verbatim, with any <at> tag in it. A post with none (a file alone) is dropped.
//   - A conversation that is not a team's channel (a chat, a personal conversation) is dropped, as the Slack adapter drops direct
//     messages.
//   - The message's ID must hold no ':', because chat.MessageID joins on it: such an ID, a missing one, or no channel is invalid.
func (a activity) message(conn, appID string) (chat.Inbound, verdict, string) {
	channel, root := splitConversation(a.Conversation.ID)
	if a.ID == "" || strings.Contains(a.ID, idSeparator) || channel == "" {
		return chat.Inbound{}, invalid, "the activity names no message ID without ':', or no channel"
	}
	if strings.HasPrefix(a.From.ID, botPrefix) {
		if a.From.ID == botPrefix+appID {
			return chat.Inbound{}, ownPost, "Agent Kourier's own post"
		}
		return chat.Inbound{}, dropped, "a bot's post"
	}
	switch {
	case a.Conversation.ConversationType != channelConversation:
		return chat.Inbound{}, dropped, "not a channel of a team"
	case a.From.AADObjectID == "":
		return chat.Inbound{}, dropped, "the sender has no Microsoft Entra object ID"
	case a.Text == "":
		return chat.Inbound{}, dropped, "no text"
	}
	return chat.Inbound{
		At:        chat.Ref{Platform: PlatformName, Conn: conn, Channel: channel, ID: a.ID, Thread: threadOf(root, a.ID)},
		User:      a.From.AADObjectID,
		Text:      a.Text,
		Mentioned: a.mentions(botPrefix + appID),
	}, forward, ""
}

// mentions says whether a mention entity of a names the Teams ID id.
func (a activity) mentions(id string) bool {
	for _, e := range a.Entities {
		if e.Type == "mention" && e.Mentioned != nil && e.Mentioned.ID == id {
			return true
		}
	}
	return false
}

// conversationRecord is what a tells the conversation store, and false for an activity outside a team's channel.
func (a activity) conversationRecord() (Conversation, bool) {
	channel, _ := splitConversation(a.Conversation.ID)
	if a.Conversation.ConversationType != channelConversation || channel == "" || a.ChannelData.Team == nil {
		return Conversation{}, false
	}
	return Conversation{
		Channel:    channel,
		ServiceURL: a.ServiceURL,
		TeamID:     a.ChannelData.Team.ID,
		AADGroupID: a.ChannelData.Team.AADGroupID,
		TenantID:   a.tenant(),
	}, true
}

// invoke is what the invoke seam is given of an invoke activity of the connection conn, and false when its replyToId, the ID of the
// message it came from, holds ':', which chat.MessageID joins on, as for a message's own ID.
func (a activity) invoke(conn string) (Invoke, bool) {
	if strings.Contains(a.ReplyToID, idSeparator) {
		return Invoke{}, false
	}
	channel, root := splitConversation(a.Conversation.ID)
	return Invoke{
		Name:  a.Name,
		ID:    a.ID,
		At:    chat.Ref{Platform: PlatformName, Conn: conn, Channel: channel, ID: a.ReplyToID, Thread: threadOf(root, a.ReplyToID)},
		User:  a.From.AADObjectID,
		Value: a.Value,
	}, true
}
