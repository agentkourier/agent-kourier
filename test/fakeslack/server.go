// Package fakeslack is an in-memory fake of the Slack Web API and Socket Mode,
// for courier's unit and end-to-end tests (spec 12.1). Courier reaches it
// through slack-go's slack.OptionAPIURL(fake.URL() + "/"), or, when the fake
// runs behind another listener, by serving the Server as an http.Handler.
// Socket Mode clients connect with an xapp- token; apps.connections.open
// answers with a websocket URL on the same listener. The same handler serves a
// JSON control surface under /_control/ (controlhttp.go), which needs no Slack
// token and is only for tests inside a cluster.
//
// It is built from Slack's API documentation and slack-go's request and
// response types, not from captured traffic. Comments name the docs page for
// each method. Behavior that is guessed, remembered, or inferred from slack-go
// rather than read from the docs carries the tag UNCONFIRMED(WP-04):, and
// WP-04 replays real captures against the fake to settle each one. Courier
// should attach message metadata with slack.MsgOptionMetadata, not through
// PostMessageParameters. The tagged items are:
//
//   - Auth: accepting the token from the header or the token form field, any
//     token with the right prefix being valid (xoxb- for the Web API, xapp-
//     for apps.connections.open), the not_authed and invalid_auth codes, and
//     invalid_auth for the wrong token type, where Slack may say
//     not_allowed_token_type.
//   - Socket Mode (socketmode.go): the websocket path and its lack of a
//     ticket; the content of hello; envelopes going to the newest connection
//     only, where Slack spreads them over up to 10; and unacked envelopes
//     never being redelivered, where Slack retries.
//   - User actions (control.go): the order of the app_mention and message
//     events for a mention, in a thread too, and both being sent; the fields
//     of each event, which are the ones courier needs; a mention in a direct
//     message getting no app_mention; the channel_type of a channel being
//     "channel" unless SetChannelType says otherwise ("group" for private
//     ones); and no message events for the bot's own posts, which Slack sends.
//   - Events for the bot's own posts (ownevents.go), on by default: a message
//     event with no subtype, and with bot_id and app_id, for each bot post;
//     blocks being included and metadata not; a hidden message_changed event
//     with its own ts for each chat.update; for streaming, a message event at
//     startStream, none at appendStream and a message_changed at stopStream;
//     and no message_changed for a thread parent that gains a reply, where
//     Slack sends one.
//   - Interactions (interaction.go): Submit answering an inline ask-user with a
//     block_actions press that carries state.values, not a view_submission
//     (the fake has no views); the payload's fields, which are the ones courier
//     needs; the message in it omitting metadata; the envelope's
//     accepts_response_payload being false; the name of each element type's
//     value field in state.values, and unanswered inputs being sent as null;
//     rejecting select options that the element does not offer; and Click and
//     Submit refusing a message whose interactive block has no block_id, where
//     Slack makes one up.
//   - auth.test: the fields returned, and its placeholder url and user name.
//   - Bot identity: a bot post carrying user, bot_id and app_id together, with
//     no subtype.
//   - Content limits (validate.go): 50 blocks; 40000 characters of message
//     text (msg_too_long versus truncation); 3000 of section text and 2000 per
//     section field, each at least 1, and 10 fields; 150 of header text; 12000
//     across a message's markdown blocks; 25 actions elements, 10 context
//     elements, 75 characters of button text, 2000 of button value, 255 of any
//     block_id or action_id; the invalid_blocks code; rejecting the agent-UI
//     blocks (alert, card, carousel) outside streaming; and the counting unit
//     (this fake counts code points, Slack may count UTF-16 units).
//   - Block rules (validate.go): a block_id appearing once in a message and an
//     action_id once in a block; a section needing text or fields; a section
//     accessory button held to the limits of an actions button; and button and
//     header text each being at least 1 character.
//   - chat.postEphemeral: user_not_in_channel for a recipient outside a channel
//     with registered members; invalid_arguments for a missing user; a thread_ts
//     being taken as it is; the message_ts in the response; and ephemeral
//     messages getting no event and being impossible to update.
//   - chat.postMessage: invalid_metadata_format; replies must target a
//     top-level message; the response message omits metadata; blocks=[] with
//     text stores no blocks.
//   - chat.update: omitted fields are kept, blocks=[] clears blocks, and
//     invalid_arguments for a missing ts; a message still streaming is
//     rejected with cant_update_message.
//   - Ephemerals (ephemeral.go) being readable through GET /_control/ephemerals alone, for e2e
//     tests, and through no other read path, where Slack shows one to its recipient only.
//   - reactions.add (reactions.go): already_reacted, message_not_found (and the code for a
//     deleted message), the characters a name may have, and no event being sent.
//   - conversations.history and conversations.replies: invalid_limit; metadata
//     only with include_all_metadata=1; with only oldest set, results stay
//     newest first and cursors walk back toward oldest; replies return the
//     parent on the first page only, reuse history's limit default (100) and
//     cap (999), and use invalid_ts_oldest and invalid_ts_latest; the oldest
//     and latest window applies to replies but not the parent.
//   - conversations.members reuses history's invalid_limit, default and cap.
//   - users.info: invalid_arguments for a missing user; the email is always
//     returned, whatever the app's scopes.
//   - Streaming: recipient_team_id being required in channels alongside
//     recipient_user_id; the error codes invalid_arguments (no thread_ts, or an
//     append with nothing to append), missing_recipient_user_id,
//     missing_recipient_team_id, message_not_in_streaming_state and
//     invalid_chunks (also for any chunk type but markdown_text, task_update,
//     plan_update and blocks, and for a blocks chunk with no blocks);
//     appendStream returning only ok; a running
//     stream being visible to reads; stopStream accepting blocks and metadata;
//     a metadata parameter being ignored by startStream and discarded by
//     appendStream; task_update, plan_update and blocks chunks being accepted
//     and kept without rendering; channel IDs starting with "D" needing no
//     recipient; 12000 characters of markdown_text per call (msg_too_long);
//     and blocks chunks going through the block rules. Q2 (rate limits) is
//     out of a fake's reach.
package fakeslack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Server is a fake Slack Web API server. It is an http.Handler and is safe for
// concurrent use.
type Server struct {
	srv *httptest.Server // nil when used only as a handler
	now func() time.Time

	pingEvery   time.Duration
	bot         botIdentity // set by options, read-only after construction
	noOwnEvents bool        // set by WithoutOwnPostEvents, read-only after construction
	team        teamIdentity
	sock        sockets
	control     http.Handler // the /_control/ routes

	mu       sync.Mutex
	last     int64                 // microseconds of the last ts issued, guarded by mu
	events   int                   // Events API events issued, guarded by mu
	channels map[string][]*Message // messages per channel in ts order, guarded by mu
	users    map[string]User       // guarded by mu
	members  map[string][]string   // guarded by mu

	channelTypes map[string]string      // set by SetChannelType, guarded by mu
	ephemerals   map[string][]Ephemeral // per channel, in order, guarded by mu
	ownPending   []ownEvent             // events queued by the request holding ownMu, for flushOwn; guarded by mu
	ownSent      map[ownKey][]string    // envelope IDs of events sent for each bot message, guarded by mu

	// ownMu is held from dispatch to flushOwn by the methods that queue events, so one request's queue
	// is its own. That includes flushOwn's writes to the socket, which have a 5 second deadline: a
	// client that connects and stops reading stalls every own-post call, one after another, for up to
	// 5 seconds each, and the event whose write times out is dropped, with no envelope recorded, as
	// Slack drops events for an app that is not keeping up. The call itself still succeeds. A test
	// that wants that case must read the socket, or expect the stall.
	ownMu sync.Mutex

	afterDispatch func() // a test hook, called between queueing a request's own events and sending them
}

// Option configures a Server.
type Option func(*Server)

// WithClock sets the clock that message timestamps come from. The default is
// time.Now. A ts is the clock's time in microseconds, or one microsecond after
// the previous ts if the clock has not moved on, so ts values are unique and
// ordered.
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithBot sets the identity that bot-token posts carry: the bot's user ID, its
// bot_id, and the app_id. The defaults are UFAKEBOT, BFAKEBOT and AFAKEAPP.
// Courier learns its own user and bot IDs from auth.test; slack-go does not
// decode app_id on messages. Set at construction, read-only after.
func WithBot(user, botID, appID string) Option {
	return func(s *Server) { s.bot = botIdentity{user: user, id: botID, app: appID} }
}

// WithTeam sets the workspace's team ID and name, which auth.test returns and
// events and interactions carry. The defaults are TFAKE and fakeworkspace.
// Set at construction, read-only after.
func WithTeam(id, name string) Option {
	return func(s *Server) { s.team = teamIdentity{id: id, name: name} }
}

type botIdentity struct{ user, id, app string }

type teamIdentity struct{ id, name string }

// NewHandler returns a Server for use as an http.Handler, with no listener of
// its own. Mount it at / and reach it directly: the Socket Mode URL it hands
// out is built from the request's Host, as ws:// (wss:// if the request came
// over TLS), so a path prefix or a TLS-terminating proxy in front of it would
// break Socket Mode.
func NewHandler(opts ...Option) *Server {
	s := &Server{
		now:       time.Now,
		pingEvery: defaultPingInterval,
		bot:       botIdentity{user: "UFAKEBOT", id: "BFAKEBOT", app: "AFAKEAPP"},
		team:      teamIdentity{id: "TFAKE", name: "fakeworkspace"},
		sock:      sockets{pending: map[string]*envelope{}, closing: make(chan struct{})},
		channels:  map[string][]*Message{},
		users:     map[string]User{},
		members:   map[string][]string{},

		channelTypes: map[string]string{},
		ephemerals:   map[string][]Ephemeral{},
		ownSent:      map[ownKey][]string{},
	}
	for _, o := range opts {
		o(s)
	}
	s.control = s.newControlRoutes()
	return s
}

// New starts a fake Slack on a local port. Call Close when done.
func New(opts ...Option) *Server {
	s := NewHandler(opts...)
	s.srv = httptest.NewServer(s)
	return s
}

// URL returns the base URL of a Server started with New, without a trailing
// slash. slack-go needs one appended: slack.OptionAPIURL(fake.URL() + "/").
// It is empty for a Server from NewHandler.
func (s *Server) URL() string {
	if s.srv == nil {
		return ""
	}
	return s.srv.URL
}

// Close drops every Socket Mode client and waits for their goroutines, then
// shuts down a Server started with New. Call it on a Server from NewHandler
// too.
func (s *Server) Close() {
	s.sock.stop()
	if s.srv != nil {
		s.srv.Close()
	}
}

// response is the JSON body of a successful call, before "ok" is added.
type response map[string]any

// ServeHTTP authenticates a request, dispatches it to its method, and writes
// Slack's envelope: HTTP 200 with ok=true, or ok=false and an error code.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == socketPath:
		s.serveSocket(w, r)
		return
	case isControlPath(r.URL.Path):
		s.control.ServeHTTP(w, r)
		return
	}
	// Requests that queue events for the bot's own posts take turns, from
	// queueing to sending, so that each sends exactly its own events before it
	// answers, and a post's event goes out before its update's.
	own := ownEventMethods[strings.TrimPrefix(r.URL.Path, "/")]
	if own {
		s.ownMu.Lock()
		defer s.ownMu.Unlock()
	}
	out, code := s.dispatch(r)
	if own {
		if s.afterDispatch != nil {
			s.afterDispatch()
		}
		s.flushOwn()
	}
	if code != "" {
		out = response{"ok": false, "error": code}
	} else {
		out["ok"] = true
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(out)
}

func (s *Server) dispatch(r *http.Request) (response, string) {
	// Form-encoded requests only: slack-go sends these methods as forms.
	// apps.connections.open takes no parameters, so its JSON body is ignored.
	if err := r.ParseForm(); err != nil {
		return nil, "invalid_arguments"
	}
	method := strings.TrimPrefix(r.URL.Path, "/")
	if code := authenticate(r, tokenPrefix(method)); code != "" {
		return nil, code
	}
	switch method {
	case "apps.connections.open":
		return s.connectionsOpen(r)
	case "auth.test":
		return s.authTest()
	case "chat.postEphemeral":
		return s.postEphemeral(r.Form)
	case "chat.postMessage":
		return s.postMessage(r.Form)
	case "chat.update":
		return s.updateMessage(r.Form)
	case "reactions.add":
		return s.addReaction(r.Form)
	case "conversations.history":
		return s.history(r.Form)
	case "conversations.replies":
		return s.replies(r.Form)
	case "chat.startStream":
		return s.startStream(r.Form)
	case "chat.appendStream":
		return s.streamChunk(r.Form, false)
	case "chat.stopStream":
		return s.streamChunk(r.Form, true)
	case "users.info":
		return s.usersInfo(r.Form)
	case "conversations.members":
		return s.conversationsMembers(r.Form)
	default:
		return nil, "unknown_method"
	}
}

// authTest implements auth.test
// (https://docs.slack.dev/reference/methods/auth.test/), which tells courier
// who it is: the bot's user and bot IDs and the team's.
//
// UNCONFIRMED(WP-04): the fields returned, and the placeholder url and user name.
func (s *Server) authTest() (response, string) {
	return response{
		"url": "https://" + s.team.name + ".slack.com/", "team": s.team.name, "user": "fakebot",
		"team_id": s.team.id, "user_id": s.bot.user, "bot_id": s.bot.id,
	}, ""
}

// tokenPrefix returns the prefix of the token type a method accepts: an
// app-level token for opening a Socket Mode connection, a bot token for the
// rest.
func tokenPrefix(method string) string {
	if method == "apps.connections.open" {
		return "xapp-"
	}
	return "xoxb-"
}

// authenticate checks for a token with the given prefix. Slack accepts it in
// the Authorization header or in a token parameter, and slack-go uses the
// latter for the Web API methods faked here and the former for
// apps.connections.open (https://docs.slack.dev/authentication/tokens/).
//
// UNCONFIRMED(WP-04): the not_authed and invalid_auth codes, that either
// location works for every faked method, and that any token with the right
// prefix is valid.
func authenticate(r *http.Request, prefix string) string {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		token = r.Form.Get("token")
	}
	switch {
	case token == "":
		return "not_authed"
	case !strings.HasPrefix(token, prefix):
		return "invalid_auth"
	}
	return ""
}
