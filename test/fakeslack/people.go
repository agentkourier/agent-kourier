package fakeslack

import (
	"net/url"
	"slices"
)

// User is a workspace member registered with AddUser. DisplayName is often
// empty on real Slack, so it is separate from Name and defaults to empty.
type User struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	RealName    string `json:"real_name"`
	Email       string `json:"email"`
	IsBot       bool   `json:"is_bot"`
}

// AddUser registers (or replaces) a user for users.info.
func (s *Server) AddUser(u User) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.users[u.ID] = u
}

// SetMembers registers channel and replaces its membership. Duplicate IDs are
// dropped and the first occurrences keep their order; the slice is copied. An
// empty membership is a registered channel with no members.
// conversations.members reports channel_not_found for any channel that was
// never registered.
func (s *Server) SetMembers(channel string, users ...string) {
	members := []string{}
	for _, u := range users {
		if !slices.Contains(members, u) {
			members = append(members, u)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.members[channel] = members
}

// usersInfo implements users.info
// (https://docs.slack.dev/reference/methods/users.info/).
//
// Error code from the docs: user_not_found.
//
// UNCONFIRMED(WP-04): invalid_arguments for a missing user. The email is
// always returned; real Slack returns it only to apps with the
// users:read.email scope, so confirm the scope courier needs.
func (s *Server) usersInfo(form url.Values) (response, string) {
	id := form.Get("user")
	if id == "" {
		return nil, "invalid_arguments"
	}
	s.mu.Lock()
	u, ok := s.users[id]
	s.mu.Unlock()
	if !ok {
		return nil, "user_not_found"
	}
	profile := map[string]any{"real_name": u.RealName, "display_name": u.DisplayName}
	if u.Email != "" {
		profile["email"] = u.Email
	}
	return response{"user": map[string]any{
		"id": u.ID, "name": u.Name, "real_name": u.RealName, "is_bot": u.IsBot, "deleted": false, "profile": profile,
	}}, ""
}

// conversationsMembers implements conversations.members
// (https://docs.slack.dev/reference/methods/conversations.members/), paginated
// like the message methods. Error codes from the docs: channel_not_found,
// invalid_cursor.
//
// UNCONFIRMED(WP-04): invalid_limit, the default limit of 100 and the cap of
// 999, which are history's, reused here.
func (s *Server) conversationsMembers(form url.Values) (response, string) {
	s.mu.Lock()
	users, ok := s.members[form.Get("channel")]
	s.mu.Unlock()
	if !ok {
		return nil, "channel_not_found"
	}
	out, next, code := paginate(users, form, func(u string) string { return u })
	if code != "" {
		return nil, code
	}
	if out == nil {
		out = []string{}
	}
	return response{"members": out, "response_metadata": map[string]string{"next_cursor": next}}, ""
}
