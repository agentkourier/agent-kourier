package faketeams

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
)

// routeMicrosoft adds the fake's Microsoft side to mux: Entra's token endpoint, the Bot Framework's metadata and keys, the Bot
// Connector under serviceRoot, and Graph under /v1.0.
func (s *Service) routeMicrosoft(mux *http.ServeMux) {
	mux.HandleFunc("POST /token", s.token)
	mux.HandleFunc("GET /openid", s.openID)
	mux.HandleFunc("GET /keys", s.keys)

	mux.HandleFunc("POST /teams/v3/conversations/{conv}/activities", s.bot(s.send))
	mux.HandleFunc("PUT /teams/v3/conversations/{conv}/activities/{id}", s.bot(s.update))
	mux.HandleFunc("GET /teams/v3/conversations/{conv}/pagedmembers", s.bot(s.members))
	mux.HandleFunc("GET /teams/v3/teams/{team}/conversations", s.bot(s.teamChannels))

	mux.HandleFunc("GET /v1.0/teams/{group}/channels/{channel}/messages", s.graph(s.listMessages))
	mux.HandleFunc("GET /v1.0/teams/{group}/channels/{channel}/messages/delta", s.graph(s.delta))
	mux.HandleFunc("GET /v1.0/teams/{group}/channels/{channel}/messages/{id}", s.graph(s.getMessage))
	mux.HandleFunc("GET /v1.0/teams/{group}/channels/{channel}/messages/{id}/replies", s.graph(s.listReplies))
	mux.HandleFunc("GET /v1.0/teams/{group}/channels/{channel}/messages/{root}/replies/{id}", s.graph(s.getMessage))
	mux.HandleFunc("GET /v1.0/subscriptions", s.graph(s.listSubscriptions))
	mux.HandleFunc("POST /v1.0/subscriptions", s.graph(s.createSubscription))
	mux.HandleFunc("PATCH /v1.0/subscriptions/{id}", s.graph(s.renewSubscription))
	mux.HandleFunc("DELETE /v1.0/subscriptions/{id}", s.graph(s.deleteSubscription))
}

// apiError answers status with the error body of Microsoft's APIs, {"error":{"code":…}}.
func apiError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": code}})
}

// token is Entra's client credentials grant: an app the fake hosts, with the client secret, gets a bot token or a Graph token.
func (s *Service) token(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBody)
	if err := r.ParseForm(); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	app, scope := r.PostForm.Get("client_id"), r.PostForm.Get("scope")
	if _, ok := s.cfg.Bots[app]; !ok || r.PostForm.Get("client_secret") != s.cfg.ClientSecret || r.PostForm.Get("grant_type") != "client_credentials" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":"invalid_client","error_codes":[7000215]}`)
		return
	}
	if scope != botScope && scope != graphScope {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_scope"}`)
		return
	}
	kind := "bot"
	if scope == graphScope {
		kind = "graph"
	}
	tok := "fake-teams-" + kind + "-token-" + app + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	s.mu.Lock()
	s.tokens[tok] = grant{app: app, graph: scope == graphScope}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"token_type": "Bearer", "expires_in": 3599, "access_token": tok})
}

// openID is the Bot Framework's OpenID metadata, which names the fake's key set.
func (s *Service) openID(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{"issuer": tokenIssuer, "jwks_uri": s.base + "/keys", "id_token_signing_alg_values_supported": []string{"RS256"}})
}

// keys is the fake's key set: its one signing key, endorsed for msteams.
func (s *Service) keys(w http.ResponseWriter, _ *http.Request) {
	pub := s.key.PublicKey
	writeJSON(w, map[string]any{"keys": []any{map[string]any{
		"kty": "RSA", "use": "sig", "kid": keyID, "endorsements": []string{"msteams"},
		"n": base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		"e": base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}})
}

// bearerOf is the grant of the request's bearer token, and false for none the fake issued.
func (s *Service) bearerOf(r *http.Request) (grant, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return grant{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.tokens[tok]
	return g, ok
}

// bot is h behind a bot token: the Bot Connector's calls.
func (s *Service) bot(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g, ok := s.bearerOf(r)
		if !ok || g.graph {
			apiError(w, http.StatusUnauthorized, "Unauthorized")
			return
		}
		h(w, r, g.app)
	}
}

// graph is h behind a Graph token.
func (s *Service) graph(h func(http.ResponseWriter, *http.Request, string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		g, ok := s.bearerOf(r)
		if !ok || !g.graph {
			apiError(w, http.StatusUnauthorized, "InvalidAuthenticationToken")
			return
		}
		h(w, r, g.app)
	}
}

// outActivity is what the Bot Connector reads of a bot's post or edit.
type outActivity struct {
	Type        string       `json:"type"`
	Text        *string      `json:"text"`
	Attachments []attachment `json:"attachments"`
}

// splitConv is the channel and the thread's root of a conversation ID, "<channel>;messageid=<root>".
func splitConv(conv string) (channel, root string) {
	channel, root, _ = strings.Cut(conv, ";messageid=")
	return channel, root
}

// readActivity reads the body of a bot's post or edit.
func readActivity(w http.ResponseWriter, r *http.Request) (outActivity, bool) {
	var a outActivity
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(&a); err != nil || a.Type != "message" {
		apiError(w, http.StatusBadRequest, "BadArgument")
		return outActivity{}, false
	}
	return a, true
}

// send is a bot's post: to a channel, which starts a thread, or to a thread of it (conversation "<channel>;messageid=<root>"). An
// activity of both text and attachments is answered 202 with no body and no ID, and kept as two messages, the text and then a card
// of its own, as Teams was measured to answer and show one (on a test tenant, web client, 2026-11-18; internal/chat/teams/buttons.go).
// Where Teams puts the card when the text starts a thread was not measured: the fake puts it beside the text.
func (s *Service) send(w http.ResponseWriter, r *http.Request, app string) {
	channel, root := splitConv(r.PathValue("conv"))
	a, ok := readActivity(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	if !slices.Contains(s.channels, channel) || (root != "" && s.messages[root] == nil) {
		s.mu.Unlock()
		apiError(w, http.StatusNotFound, "ConversationNotFound")
		return
	}
	if text := deref(a.Text); text != "" && len(a.Attachments) > 0 {
		s.notifyLocked(s.postLocked(&message{channel: channel, root: root, kind: SenderBot, from: app, name: botName, text: text}), "created")
		s.notifyLocked(s.postLocked(&message{channel: channel, root: root, kind: SenderBot, from: app, name: botName, attachments: a.Attachments}), "created")
		s.mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		return
	}
	m := s.postLocked(&message{channel: channel, root: root, kind: SenderBot, from: app, name: botName, text: deref(a.Text), attachments: a.Attachments})
	s.notifyLocked(m, "created")
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": m.id})
}

// update is a bot's edit of its own post. A body with no text keeps the text it had, as the real service was seen to; its attachments
// are the body's.
func (s *Service) update(w http.ResponseWriter, r *http.Request, app string) {
	channel, _ := splitConv(r.PathValue("conv"))
	a, ok := readActivity(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	m := s.messages[r.PathValue("id")]
	if m == nil || m.channel != channel {
		s.mu.Unlock()
		apiError(w, http.StatusNotFound, "ActivityNotFound")
		return
	}
	if m.kind != SenderBot || m.from != app {
		s.mu.Unlock()
		apiError(w, http.StatusForbidden, "Forbidden")
		return
	}
	if a.Text != nil {
		m.text = *a.Text
	}
	m.attachments = a.Attachments
	m.versions = append(m.versions, m.text)
	s.seq++
	m.seq = s.seq
	s.notifyLocked(m, "updated")
	s.mu.Unlock()
	writeJSON(w, map[string]string{"id": m.id})
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// members is the roster of a channel or of the team: the team's members, in one page. A person's Teams ID is 29: and their object ID.
func (s *Service) members(w http.ResponseWriter, r *http.Request, _ string) {
	channel, _ := splitConv(r.PathValue("conv"))
	s.mu.Lock()
	known := channel == s.cfg.TeamID || slices.Contains(s.channels, channel)
	people := make([]map[string]string, 0, len(s.people))
	for _, p := range s.people {
		people = append(people, map[string]string{"id": "29:" + p.ID, "name": p.Name, "aadObjectId": p.ID})
	}
	s.mu.Unlock()
	if !known {
		apiError(w, http.StatusNotFound, "ConversationNotFound")
		return
	}
	slices.SortFunc(people, func(a, b map[string]string) int { return strings.Compare(a["id"], b["id"]) })
	writeJSON(w, map[string]any{"members": people})
}

// teamChannels is the team's channels.
func (s *Service) teamChannels(w http.ResponseWriter, r *http.Request, _ string) {
	if r.PathValue("team") != s.cfg.TeamID {
		apiError(w, http.StatusNotFound, "NotFound")
		return
	}
	s.mu.Lock()
	list := make([]map[string]string, 0, len(s.channels))
	for _, c := range s.channels {
		list = append(list, map[string]string{"id": c, "name": c})
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"conversations": list})
}

// channelOf is the channel of a Graph request, if the request names the team's group and one of its channels; it answers 404 if not.
// It counts the read under count.
func (s *Service) channelOf(w http.ResponseWriter, r *http.Request, count func(*Reads)) (string, bool) {
	channel := r.PathValue("channel")
	s.mu.Lock()
	ok := r.PathValue("group") == s.cfg.GroupID && slices.Contains(s.channels, channel)
	if ok {
		reads := s.stats.Reads[channel]
		count(&reads)
		s.stats.Reads[channel] = reads
	}
	s.mu.Unlock()
	if !ok {
		apiError(w, http.StatusNotFound, "NotFound")
	}
	return channel, ok
}

// top is the request's $top, or def.
func top(r *http.Request, def int) int {
	if n, err := strconv.Atoi(r.URL.Query().Get("$top")); err == nil && n > 0 {
		return n
	}
	return def
}

// newestFirst returns ms, newest first, cut to n.
func newestFirst(ms []*message, n int) []*message {
	ms = slices.Clone(ms)
	slices.Reverse(ms)
	return ms[:min(n, len(ms))]
}

// listMessages is a channel's top-level posts, newest first, each with its replies when $expand=replies.
func (s *Service) listMessages(w http.ResponseWriter, r *http.Request, _ string) {
	channel, ok := s.channelOf(w, r, func(c *Reads) { c.Messages++ })
	if !ok {
		return
	}
	expand := r.URL.Query().Get("$expand") == "replies"
	s.mu.Lock()
	var value []map[string]any
	for _, m := range newestFirst(s.threadLocked(channel, ""), top(r, 20)) {
		g := s.chatMessageLocked(m)
		if expand {
			replies := []map[string]any{}
			for _, reply := range s.threadLocked(channel, m.id)[1:] {
				replies = append(replies, s.chatMessageLocked(reply))
			}
			g["replies"] = replies
		}
		value = append(value, g)
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"value": orEmpty(value)})
}

// listReplies is a thread's replies, newest first.
func (s *Service) listReplies(w http.ResponseWriter, r *http.Request, _ string) {
	channel, ok := s.channelOf(w, r, func(c *Reads) { c.Replies++ })
	if !ok {
		return
	}
	root := r.PathValue("id")
	s.mu.Lock()
	m := s.messages[root]
	if m == nil || m.channel != channel || m.root != "" {
		s.mu.Unlock()
		apiError(w, http.StatusNotFound, "NotFound")
		return
	}
	var value []map[string]any
	for _, reply := range newestFirst(s.threadLocked(channel, root)[1:], top(r, 50)) {
		value = append(value, s.chatMessageLocked(reply))
	}
	s.mu.Unlock()
	writeJSON(w, map[string]any{"value": orEmpty(value)})
}

// getMessage is one message: a top-level post (messages/{id}) or a reply (messages/{root}/replies/{id}).
func (s *Service) getMessage(w http.ResponseWriter, r *http.Request, _ string) {
	channel, ok := s.channelOf(w, r, func(c *Reads) { c.Message++ })
	if !ok {
		return
	}
	s.mu.Lock()
	m := s.messages[r.PathValue("id")]
	if m == nil || m.channel != channel || m.root != r.PathValue("root") {
		s.mu.Unlock()
		apiError(w, http.StatusNotFound, "NotFound")
		return
	}
	g := s.chatMessageLocked(m)
	s.mu.Unlock()
	writeJSON(w, g)
}

// delta is a channel's messages/delta, as measured: top-level posts only, no reply. With no $deltatoken it is the first sync, which
// gives the posts there are and a delta link; with one, the posts made or changed since the link was given, and the next link.
func (s *Service) delta(w http.ResponseWriter, r *http.Request, _ string) {
	token := r.URL.Query().Get("$deltatoken")
	channel, ok := s.channelOf(w, r, func(c *Reads) {
		c.Delta++
		if token != "" {
			c.Synced++
		}
	})
	if !ok {
		return
	}
	since := int64(-1)
	if token != "" {
		n, err := strconv.ParseInt(token, 10, 64)
		if err != nil || n < 0 {
			apiError(w, http.StatusBadRequest, "BadRequest")
			return
		}
		since = n
	}
	s.mu.Lock()
	var value []map[string]any
	for _, m := range s.threadLocked(channel, "") {
		if m.seq > since {
			value = append(value, s.chatMessageLocked(m))
		}
	}
	link := s.base + "/v1.0/teams/" + url.PathEscape(s.cfg.GroupID) + "/channels/" + url.PathEscape(channel) +
		"/messages/delta?$deltatoken=" + strconv.FormatInt(s.seq, 10)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"value": orEmpty(value), "@odata.deltaLink": link})
}

func orEmpty(v []map[string]any) []map[string]any {
	if v == nil {
		return []map[string]any{}
	}
	return v
}

// chatMessageLocked is m as Graph's chatMessage. A person's post is HTML with an <at> for its mention; a bot's is its markdown in a
// paragraph, as Teams keeps the tag carrier's span; a card is an attachment whose content is the card's JSON as a string, which the
// body names.
func (s *Service) chatMessageLocked(m *message) map[string]any {
	body := ""
	switch m.kind {
	case SenderPerson:
		text := html.EscapeString(m.text)
		if m.mentioned != "" {
			text = `<at id="0">` + botName + `</at> ` + text
		}
		body = "<p>" + text + "</p>"
	case SenderBot:
		if m.text != "" {
			body = "<p>" + m.text + "</p>"
		}
	default:
		body = m.text
	}
	attachments := []map[string]any{}
	for i, a := range m.attachments {
		id := "a" + strconv.Itoa(i)
		attachments = append(attachments, map[string]any{"id": id, "contentType": a.ContentType, "content": string(a.Content)})
		body += `<attachment id="` + id + `"></attachment>`
	}
	from := map[string]any{"user": nil, "application": nil, "device": nil}
	if m.kind == SenderPerson {
		from["user"] = map[string]any{"id": m.from, "displayName": m.name, "userIdentityType": "aadUser"}
	} else {
		from["application"] = map[string]any{"id": m.from, "displayName": m.name, "applicationIdentityType": "bot"}
	}
	mentions := []map[string]any{}
	if m.mentioned != "" {
		mentions = append(mentions, map[string]any{"id": 0, "mentionText": botName,
			"mentioned": map[string]any{"application": map[string]any{"id": m.mentioned, "displayName": botName, "applicationIdentityType": "bot"}}})
	}
	var replyTo any
	if m.root != "" {
		replyTo = m.root
	}
	stamp := m.created.Format("2006-01-02T15:04:05.000Z")
	return map[string]any{
		"id": m.id, "replyToId": replyTo, "messageType": "message", "createdDateTime": stamp, "lastModifiedDateTime": stamp,
		"deletedDateTime": nil, "from": from, "body": map[string]any{"contentType": "html", "content": body},
		"attachments": attachments, "mentions": mentions,
		"channelIdentity": map[string]any{"teamId": s.cfg.GroupID, "channelId": m.channel},
	}
}
