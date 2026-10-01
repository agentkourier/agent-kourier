package fakeslack

import (
	"net/url"
	"slices"
	"strings"
)

// Reaction is an emoji on a message and the users who added it, in the order they did.
type Reaction struct {
	Name  string   `json:"name"`
	Users []string `json:"users"`
}

// Reactions returns the reactions on the message ts in channel, in the order they were first
// added. It is for test assertions, and is a copy; an unknown message has none.
func (s *Server) Reactions(channel, ts string) []Reaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.channels[channel] {
		if m.TS == ts {
			return cloneReactions(m.Reactions)
		}
	}
	return nil
}

func cloneReactions(rs []Reaction) []Reaction {
	if rs == nil {
		return nil
	}
	out := make([]Reaction, len(rs))
	for i, r := range rs {
		out[i] = Reaction{r.Name, slices.Clone(r.Users)}
	}
	return out
}

// validReactionName reports whether name has only lower-case letters, digits, and _ - +.
func validReactionName(name string) bool {
	return !strings.ContainsFunc(name, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '+'
	})
}

// addReaction implements reactions.add (https://docs.slack.dev/reference/methods/reactions.add/),
// which courier uses to mark a reply that waits for the active task. The reaction is the bot's.
//
// Error codes from the docs: already_reacted, message_not_found, no_item_specified, invalid_name.
//
// UNCONFIRMED(WP-04): already_reacted for a second add of one name by the same user, which courier
// counts as success; message_not_found for a ts that is in no message of the channel, and which
// code Slack gives for a message that was deleted; that a name with a colon, an upper-case letter
// or a space is invalid_name (courier sends the bare name); no limit on the reactions of a
// message or of a user; and that no event is sent, which courier would not subscribe to.
func (s *Server) addReaction(form url.Values) (response, string) {
	channel, ts, name := form.Get("channel"), form.Get("timestamp"), form.Get("name")
	switch {
	case channel == "" || ts == "":
		return nil, "no_item_specified"
	case name == "" || !validReactionName(name):
		return nil, "invalid_name"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, m := range s.channels[channel] {
		if m.TS != ts {
			continue
		}
		i := slices.IndexFunc(m.Reactions, func(r Reaction) bool { return r.Name == name })
		switch {
		case i < 0:
			m.Reactions = append(m.Reactions, Reaction{Name: name, Users: []string{s.bot.user}})
		case slices.Contains(m.Reactions[i].Users, s.bot.user):
			return nil, "already_reacted"
		default:
			m.Reactions[i].Users = append(m.Reactions[i].Users, s.bot.user)
		}
		return response{}, ""
	}
	return nil, "message_not_found"
}
