package fakeslack

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// App is the identity of one Slack app installed in a workspace: who it posts as, which workspace it is in, and
// the tokens it authenticates with. A Server hosts several, so one URL can stand for the Slack API of a broker
// that serves several ChatConnections: each call is answered as the app whose bot token it carries, and each
// Socket Mode client is the app whose app token opened it.
//
// The default app is the one WithBot and WithTeam configure. It has no tokens of its own: a token with the
// right prefix that no registered App owns belongs to it, so a test that registers no App behaves as it always
// did. An App registered with WithApp is a different app, with its own bot user, bot and app IDs.
type App struct {
	// TeamID and TeamName are the app's workspace. An empty TeamID takes the default app's, so apps in one
	// workspace need not repeat it.
	TeamID   string `json:"team_id"`
	TeamName string `json:"team_name"`
	// BotUser is the app's bot user ID (what auth.test returns as user_id, and what members lists and
	// mentions name), BotID its bot_id, and AppID its app_id (api_app_id on its envelopes). All three are
	// required, and AppID names the app everywhere else in this package.
	BotUser string `json:"bot_user"`
	BotID   string `json:"bot_id"`
	AppID   string `json:"app_id"`
	// BotToken (xoxb-) authenticates the app's Web API calls and AppToken (xapp-) opens its Socket Mode
	// connections. Either may be empty, and neither is ever returned by the control surface.
	BotToken string `json:"-"`
	AppToken string `json:"-"`
}

// Validate reports why a is not an App WithApp can register: a missing AppID, BotUser or BotID, or a token with
// the wrong prefix, which the Web API and apps.connections.open would refuse before they looked it up.
func (a App) Validate() error {
	var errs []error
	for _, f := range []struct{ name, v string }{{"AppID", a.AppID}, {"BotUser", a.BotUser}, {"BotID", a.BotID}} {
		if f.v == "" {
			errs = append(errs, fmt.Errorf("fakeslack: an App needs a %s", f.name))
		}
	}
	if a.BotToken != "" && !strings.HasPrefix(a.BotToken, "xoxb-") {
		errs = append(errs, errors.New("fakeslack: an App's BotToken must start with xoxb-"))
	}
	if a.AppToken != "" && !strings.HasPrefix(a.AppToken, "xapp-") {
		errs = append(errs, errors.New("fakeslack: an App's AppToken must start with xapp-"))
	}
	return errors.Join(errs...)
}

// WithApp registers another Slack app. A registered app's bot token reaches the Web API as that app, its app
// token opens Socket Mode connections for that app alone, and it receives the events of the channels whose
// members (SetMembers, Join) include its bot, and of no other. An App with the AppID of one already registered,
// the default app included, replaces it. If two apps share a token, the first registered owns it.
//
// An App that fails Validate makes WithApp panic when the Server is built, so that a mistake in a test's setup
// is not hidden; the message names the fields, never a token. Check Validate first to avoid that. Set at
// construction, read-only after.
func WithApp(a App) Option {
	return func(s *Server) {
		if err := a.Validate(); err != nil {
			panic(err)
		}
		if i := slices.IndexFunc(s.apps, func(x App) bool { return x.AppID == a.AppID }); i >= 0 {
			s.apps[i] = a
			return
		}
		s.apps = append(s.apps, a)
	}
}

// WithStrictTokens makes the fake refuse a well-formed token that no registered app owns with invalid_auth, where
// it otherwise belongs to the default app. It applies once any app has a token (WithApp), so a Server with none
// behaves as it always did. The default app owns no token unless WithApp gave it some (an App with the default
// app's ID), so with this option it is reachable by none otherwise.
//
// UNCONFIRMED(MC-10): the code, which is invalid_auth as for any other token the fake does not accept.
func WithStrictTokens() Option { return func(s *Server) { s.strictTokens = true } }

// SetRevoked revokes the tokens of the app appID, or restores them. While an app is revoked its bot token's Web
// API calls answer token_revoked, apps.connections.open with its app token answers the same, and a websocket for
// it is refused. Revoking it also ends its open Socket Mode connections, each sent a disconnect envelope and then
// closed. Clearing the flag lets the app connect again; nothing reopens a connection for it. The default app's
// ID revokes every token the default app owns, which are all the tokens no registered app owns. It fails with
// ErrNotFound for an app the Server does not host.
//
// UNCONFIRMED(MC-10): the code token_revoked, where Slack may say invalid_auth or account_inactive for a token
// that was revoked or whose app was uninstalled, and the disconnect envelope's reason (link_disabled).
func (s *Server) SetRevoked(appID string, revoked bool) error {
	if _, ok := s.appByID(appID); !ok {
		return fmt.Errorf("fakeslack: no app %q: %w", appID, ErrNotFound)
	}
	s.mu.Lock()
	if revoked {
		s.revoked[appID] = true
	} else {
		delete(s.revoked, appID)
	}
	s.mu.Unlock()
	if revoked {
		s.sock.disconnect(appID)
	}
	return nil
}

// isRevoked says whether SetRevoked revoked the app's tokens.
func (s *Server) isRevoked(appID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.revoked[appID]
}

// WithSocketSpread makes the fake send each event to exactly one of an app's Socket Mode connections, taking
// them in turn, where it otherwise sends every event to the app's newest connection. It is for tests of what a
// client that holds a second connection of the same app misses.
//
// UNCONFIRMED(MC-S): that Slack spreads one app's events over its connections, which is believed and not
// measured, and that it does so in turn. Real Slack picks a connection by its own rule.
func WithSocketSpread() Option { return func(s *Server) { s.spread = true } }

// WithStrictMembership holds the default app to the rule every other app is under: it receives the events of a
// channel only if its bot is among the members SetMembers registered for it. Without this option the default app
// receives the events of every channel, whoever its members are, which is what the fake did before it hosted
// several apps. Set at construction, read-only after.
func WithStrictMembership() Option { return func(s *Server) { s.strict = true } }

// finishApps fills in what the options left empty and indexes the apps' tokens. It runs once, after the options.
func (s *Server) finishApps() {
	s.tokens = map[string]string{}
	seen := map[string]bool{}
	for i := range s.apps {
		a := &s.apps[i]
		if seen[a.AppID] {
			panic(fmt.Sprintf("fakeslack: two apps have the app ID %q (WithBot after WithApp?)", a.AppID))
		}
		seen[a.AppID] = true
		if a.TeamID == "" {
			a.TeamID, a.TeamName = s.apps[0].TeamID, s.apps[0].TeamName
		}
		for _, tok := range []string{a.BotToken, a.AppToken} {
			if _, taken := s.tokens[tok]; tok != "" && !taken {
				s.tokens[tok] = a.AppID
			}
		}
	}
}

// defaultApp is the app that WithBot and WithTeam configure and that unowned tokens belong to.
func (s *Server) defaultApp() App { return s.apps[0] }

// appByID returns the registered app with the given app ID.
func (s *Server) appByID(id string) (App, bool) {
	i := slices.IndexFunc(s.apps, func(a App) bool { return a.AppID == id })
	if i < 0 {
		return App{}, false
	}
	return s.apps[i], true
}

// appOrDefault is appByID that answers the default app for an ID no app has, which is what a message with no
// app_id (a user's, a webhook's) gets when something needs the app that handles it.
func (s *Server) appOrDefault(id string) App {
	if a, ok := s.appByID(id); ok {
		return a
	}
	return s.defaultApp()
}

// appForToken returns the app that owns token, or the default app for a token no app owns. With
// WithStrictTokens, and an app that has a token, ok is false for a token no app owns.
func (s *Server) appForToken(token string) (app App, ok bool) {
	if id, owned := s.tokens[token]; owned {
		return s.appOrDefault(id), true
	}
	return s.defaultApp(), !s.strictTokens || len(s.tokens) == 0
}

// Apps returns every app the Server hosts, the default app first. The tokens are included, so the result is for
// the test's own use.
func (s *Server) Apps() []App { return slices.Clone(s.apps) }

// Join adds the bot of the app appID to the members of channel, which registers the channel if it was not, as
// SetMembers does. The channel's other members stay. It fails with ErrNotFound for an app the Server does not host.
func (s *Server) Join(channel, appID string) error {
	a, ok := s.appByID(appID)
	if !ok {
		return fmt.Errorf("fakeslack: no app %q: %w", appID, ErrNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !slices.Contains(s.members[channel], a.BotUser) {
		s.members[channel] = append(s.members[channel], a.BotUser)
	}
	return nil
}

// recipients returns the apps that receive the events of a message posted in channel, in the order the apps
// were registered. A direct message goes to the app it was opened with (DMApp; the default app's for a channel
// nobody opened). In any other channel an app receives it when its bot is among the members SetMembers
// registered for the channel, so an app is in no channel nobody registered it in. The default app is the
// exception: it receives the events of every channel, as the fake's one app always did, unless
// WithStrictMembership holds it to the same rule.
func (s *Server) recipients(channel string) []App {
	chType := s.channelType(channel)
	s.mu.Lock()
	defer s.mu.Unlock()
	if chType == "im" {
		return []App{s.appOrDefault(s.dmOwner[channel])}
	}
	var out []App
	for i, a := range s.apps {
		if (i == 0 && !s.strict) || slices.Contains(s.members[channel], a.BotUser) {
			out = append(out, a)
		}
	}
	return out
}

// mentionedApps returns the apps whose bot text mentions, in the order the apps were registered.
func (s *Server) mentionedApps(text string) []App {
	var out []App
	for _, a := range s.apps {
		if strings.Contains(text, "<@"+a.BotUser+">") {
			out = append(out, a)
		}
	}
	return out
}

// onlyApps keeps the apps of all that are also in keep, in all's order.
func onlyApps(all, keep []App) []App {
	var out []App
	for _, a := range all {
		if slices.ContainsFunc(keep, func(k App) bool { return k.AppID == a.AppID }) {
			out = append(out, a)
		}
	}
	return out
}

// appID is the app ID a control call names: the default app's for an empty one.
func (s *Server) appID(named string) string {
	if named == "" {
		return s.defaultApp().AppID
	}
	return named
}

// appNamed returns the app a control call names: the default app for an empty ID, and ErrNotFound for an app the
// Server does not host.
func (s *Server) appNamed(appID string) (App, error) {
	if appID == "" {
		return s.defaultApp(), nil
	}
	a, ok := s.appByID(appID)
	if !ok {
		return App{}, fmt.Errorf("fakeslack: no app %q: %w", appID, ErrNotFound)
	}
	return a, nil
}
