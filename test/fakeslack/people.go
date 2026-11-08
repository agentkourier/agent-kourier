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

// SetArchived marks channel archived, or not. It does not register the channel: conversations.info reports
// channel_not_found until SetMembers has. Only conversations.info reads the mark: the fake's chat.postMessage and its other
// methods still accept a post to an archived channel.
func (s *Server) SetArchived(channel string, archived bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if archived {
		s.archived[channel] = true
	} else {
		delete(s.archived, channel)
	}
}

// SetMissingScope makes the Web API method answer missing_scope, as Slack does for a token installed without the scope
// the method needs, or lets it work again. It models no scope of its own: a test says which method the app lacks one for.
func (s *Server) SetMissingScope(method string, missing bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if missing {
		s.noScope[method] = true
	} else {
		delete(s.noScope, method)
	}
}

func (s *Server) scopeMissing(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noScope[method]
}

// conversationsInfo implements conversations.info
// (https://docs.slack.dev/reference/methods/conversations.info/), read for the bot token's own view of a channel:
// is_member is whether the calling app's bot user is among the members, so two apps see one channel differently.
// Error code from the docs: channel_not_found, for a channel value that was invalid.
//
// UNCONFIRMED(BV-3): what Slack answers for a private channel the bot is not in (channel_not_found, or the channel with
// is_member false): the page does not say. This fake knows every registered channel, so it answers the second. On the
// page (read 2026-11-01), neither field is described in prose; its public-channel example response shows
// is_archived (false) and no is_member, and it has no private-channel example. Both are in slack-go v0.33.0's Channel type
// (channels.go, conversation.go).
// That Slack sends is_member to a bot token at all is unconfirmed too: the page's example has none.
// invalid_arguments for a call with no channel is also a guess: the page lists the code among its general ones.
func (s *Server) conversationsInfo(app App, form url.Values) (response, string) {
	channel := form.Get("channel")
	if channel == "" {
		return nil, "invalid_arguments"
	}
	s.mu.Lock()
	users, ok := s.members[channel]
	archived := s.archived[channel]
	s.mu.Unlock()
	if !ok {
		return nil, "channel_not_found"
	}
	return response{"channel": map[string]any{
		"id": channel, "is_channel": true, "is_member": slices.Contains(users, app.BotUser), "is_archived": archived,
	}}, ""
}

// usersInfo implements users.info
// (https://docs.slack.dev/reference/methods/users.info/).
//
// Error code from the docs: user_not_found.
//
// UNCONFIRMED(WP-04): invalid_arguments for a missing user. The email is
// always returned; real Slack returns it only to apps with the
// users:read.email scope, so confirm the scope Agent Kourier needs.
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

// SetUserGroup registers the user group id and replaces its membership. Duplicate IDs are
// dropped and the first occurrences keep their order; the slice is copied. An empty
// membership is a registered group with no members. usergroups.users.list reports
// no_such_subteam for any group that was never registered. The fake has no disabled groups.
func (s *Server) SetUserGroup(id string, users ...string) {
	members := []string{}
	for _, u := range users {
		if !slices.Contains(members, u) {
			members = append(members, u)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.usergroups[id] = members
}

// usergroupsUsersList implements usergroups.users.list
// (https://docs.slack.dev/reference/methods/usergroups.users.list/), which takes a bot token
// with the usergroups:read scope and has no paging. Error code from the docs: no_such_subteam
// for a group ID Slack does not know.
//
// UNCONFIRMED(WP-04): invalid_arguments for a missing usergroup (the page lists the code among
// its general ones), what Slack says of a group that is disabled when include_disabled is not
// set (this fake has no disabled groups and ignores include_disabled and team_id), and
// plan_upgrade_required, which a workspace without user groups gets and this fake never says.
func (s *Server) usergroupsUsersList(form url.Values) (response, string) {
	id := form.Get("usergroup")
	if id == "" {
		return nil, "invalid_arguments"
	}
	s.mu.Lock()
	users, ok := s.usergroups[id]
	s.mu.Unlock()
	if !ok {
		return nil, "no_such_subteam"
	}
	return response{"users": users}, ""
}
