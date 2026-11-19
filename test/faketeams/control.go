package faketeams

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"time"
)

// The control API. It has no authentication: the fake runs only behind a ClusterIP Service, as the fake Slack does.
//
//	POST /_control/install    {app, channel}: the bot's app is added to the team, which Teams tells it with a conversationUpdate on the team
//	POST /_control/mention    {app, channel, user, name, text, thread, plain}: a person's post, with a mention of the bot unless plain
//	POST /_control/press      {app, message, user, name, action, inputs}: a press of an Action.Execute of a bot's card
//	POST /_control/workflows  {app, channel, card, text}: another app's post, such as Workflows' alert card; no bot is sent it
//	GET  /_control/messages?channel=C[&root=R]: the channel's top-level posts, or a thread, as MessageViews
//	GET  /_control/subscriptions: the live subscriptions
//	GET  /_control/stats: what the fake counted
//	GET  /_control/healthz

// routeControl adds the control API to mux.
func (s *Service) routeControl(mux *http.ServeMux) {
	mux.HandleFunc("GET /_control/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("POST /_control/install", s.controlInstall)
	mux.HandleFunc("POST /_control/mention", s.controlMention)
	mux.HandleFunc("POST /_control/press", s.controlPress)
	mux.HandleFunc("POST /_control/workflows", s.controlWorkflows)
	mux.HandleFunc("GET /_control/messages", s.controlMessages)
	mux.HandleFunc("GET /_control/subscriptions", s.controlSubscriptions)
	mux.HandleFunc("GET /_control/stats", s.controlStats)
}

// Install is the body of /_control/install.
type Install struct {
	App     string `json:"app"`
	Channel string `json:"channel"`
}

// Mention is the body of /_control/mention: a person's post in a channel, or in the thread of Thread, which mentions the bot of App
// unless Plain.
type Mention struct {
	App     string `json:"app"`
	Channel string `json:"channel"`
	Thread  string `json:"thread,omitempty"`
	User    string `json:"user"`
	Name    string `json:"name,omitempty"`
	Text    string `json:"text"`
	Plain   bool   `json:"plain,omitempty"`
}

// Press is the body of /_control/press: a person's press of the action titled Action of a card of the message Message, with what
// they put in the card's inputs, by input ID.
type Press struct {
	App     string            `json:"app"`
	Message string            `json:"message"`
	User    string            `json:"user"`
	Name    string            `json:"name,omitempty"`
	Action  string            `json:"action"`
	Inputs  map[string]string `json:"inputs,omitempty"`
}

// Workflows is the body of /_control/workflows: a post of the app App, with an Adaptive Card and, if set, an HTML text before it.
type Workflows struct {
	App     string          `json:"app"`
	Channel string          `json:"channel"`
	Card    json.RawMessage `json:"card"`
	Text    string          `json:"text,omitempty"`
}

// Delivered is the answer of a control call that sent a bot an activity: the ID of the message it made, if any, and the bot's answer.
type Delivered struct {
	ID     string `json:"id,omitempty"`
	Status int    `json:"status"`
	Body   string `json:"body,omitempty"`
}

// readJSON reads a control call's body into v, or answers 400.
func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v); err != nil {
		http.Error(w, "the body is not the call's JSON", http.StatusBadRequest)
		return false
	}
	return true
}

func (s *Service) controlInstall(w http.ResponseWriter, r *http.Request) {
	var in Install
	if !readJSON(w, r, &in) {
		return
	}
	if _, ok := s.cfg.Bots[in.App]; !ok || in.Channel == "" {
		http.Error(w, "an install names a bot the fake hosts and a channel", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.channelLocked(in.Channel)
	id := s.nextIDLocked(time.Now())
	s.mu.Unlock()
	// As Microsoft's documentation of the bot's install describes it, and not measured here: a conversationUpdate of eventType
	// teamMemberAdded, the bot in membersAdded, on the team's conversation (its General channel's ID). The channel the test names is
	// in channelData alone. So the install tells the bot the team's record, not the channel's: a channel's comes with the first
	// activity in it, a person's mention of the bot, which is the setup a team does.
	a := s.activity("conversationUpdate", id, s.cfg.TeamID)
	a["membersAdded"] = []any{map[string]string{"id": "28:" + in.App, "name": botName}}
	data := a["channelData"].(map[string]any)
	data["eventType"], data["teamsChannelId"], data["channel"] = "teamMemberAdded", in.Channel, map[string]string{"id": in.Channel}
	writeJSON(w, s.sendActivity(r.Context(), in.App, a))
}

func (s *Service) controlMention(w http.ResponseWriter, r *http.Request) {
	var in Mention
	if !readJSON(w, r, &in) {
		return
	}
	if _, ok := s.cfg.Bots[in.App]; !ok || in.Channel == "" || in.User == "" || in.Text == "" {
		http.Error(w, "a mention names a bot the fake hosts, a channel, a user and a text", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	if in.Thread != "" {
		if root := s.messages[in.Thread]; root == nil || root.channel != in.Channel || root.root != "" {
			s.mu.Unlock()
			http.Error(w, "the thread is no top-level post of the channel", http.StatusBadRequest)
			return
		}
	}
	s.channelLocked(in.Channel)
	s.joinLocked(in.User, in.Name)
	m := &message{channel: in.Channel, root: in.Thread, kind: SenderPerson, from: in.User, name: s.people[in.User].Name, text: in.Text}
	if !in.Plain {
		m.mentioned = in.App
	}
	m = s.postLocked(m)
	s.mu.Unlock()

	a := s.activity("message", m.id, conversationOf(in.Channel, cmpOr(in.Thread, m.id)))
	a["from"] = s.personAccount(in.User)
	a["text"], a["textFormat"] = in.Text, "plain"
	if !in.Plain {
		at := "<at>" + botName + "</at>"
		a["text"] = at + " " + in.Text
		a["entities"] = []any{map[string]any{"type": "mention", "text": at, "mentioned": map[string]string{"id": "28:" + in.App, "name": botName}}}
	}
	out := s.sendActivity(r.Context(), in.App, a)
	out.ID = m.id
	// Graph tells the subscriptions of the post too, after the activity, as the live run saw it come.
	s.mu.Lock()
	s.notifyLocked(m, "created") //nolint:contextcheck // a notification outlives the request: the fake's life bounds it (Close)
	s.mu.Unlock()
	writeJSON(w, out)
}

func (s *Service) controlPress(w http.ResponseWriter, r *http.Request) {
	var in Press
	if !readJSON(w, r, &in) {
		return
	}
	if _, ok := s.cfg.Bots[in.App]; !ok || in.User == "" || in.Action == "" {
		http.Error(w, "a press names a bot the fake hosts, a user and an action", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	m := s.messages[in.Message]
	var data map[string]any
	var verb string
	found := false
	if m != nil && m.kind == SenderBot && m.from == in.App {
		data, verb, found = actionOf(m, in.Action)
	}
	if !found {
		s.mu.Unlock()
		http.Error(w, "no card of the bot's message has that action", http.StatusNotFound)
		return
	}
	s.joinLocked(in.User, in.Name)
	channel, root := m.channel, cmpOr(m.root, m.id)
	id := s.nextIDLocked(time.Now())
	s.mu.Unlock()

	for k, v := range in.Inputs {
		data[k] = v
	}
	a := s.activity("invoke", "f:"+id, conversationOf(channel, root))
	a["name"], a["replyToId"], a["from"] = "adaptiveCard/action", m.id, s.personAccount(in.User)
	a["value"] = map[string]any{"action": map[string]any{"type": "Action.Execute", "verb": verb, "data": data}, "trigger": "manual"}
	writeJSON(w, s.sendActivity(r.Context(), in.App, a))
}

// actionOf finds, in the cards of m, the Action.Execute titled title, and returns a copy of its data and its verb.
func actionOf(m *message, title string) (map[string]any, string, bool) {
	for _, a := range m.attachments {
		if a.ContentType != cardType {
			continue
		}
		var card struct {
			Actions []struct {
				Type  string         `json:"type"`
				Title string         `json:"title"`
				Verb  string         `json:"verb"`
				Data  map[string]any `json:"data"`
			} `json:"actions"`
		}
		if json.Unmarshal(a.Content, &card) != nil {
			continue
		}
		for _, act := range card.Actions {
			if act.Type == "Action.Execute" && act.Title == title {
				return maps.Clone(act.Data), act.Verb, true
			}
		}
	}
	return nil, "", false
}

func (s *Service) controlWorkflows(w http.ResponseWriter, r *http.Request) {
	var in Workflows
	if !readJSON(w, r, &in) {
		return
	}
	if in.App == "" || in.Channel == "" || !json.Valid(in.Card) {
		http.Error(w, "a post of another app names the app, a channel and a card", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	s.channelLocked(in.Channel)
	m := s.postLocked(&message{channel: in.Channel, kind: SenderApp, from: in.App, name: "Workflows", text: in.Text,
		attachments: []attachment{{ContentType: cardType, Content: in.Card}}})
	s.notifyLocked(m, "created") //nolint:contextcheck // a notification outlives the request: the fake's life bounds it (Close)
	s.mu.Unlock()
	writeJSON(w, Delivered{ID: m.id})
}

func (s *Service) controlMessages(w http.ResponseWriter, r *http.Request) {
	channel, root := r.URL.Query().Get("channel"), r.URL.Query().Get("root")
	s.mu.Lock()
	views := []MessageView{}
	for _, m := range s.threadLocked(channel, root) {
		views = append(views, m.view())
	}
	s.mu.Unlock()
	writeJSON(w, views)
}

func (s *Service) controlSubscriptions(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	views := []SubscriptionView{}
	for _, app := range slices.Sorted(maps.Keys(s.cfg.Bots)) {
		for _, sub := range s.liveLocked(app) {
			views = append(views, SubscriptionView{ID: sub.id, App: sub.app, Channel: sub.channel, NotificationURL: sub.notificationURL, Expires: sub.expires})
		}
	}
	s.mu.Unlock()
	writeJSON(w, views)
}

func (s *Service) controlStats(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	st := Stats{Reads: maps.Clone(s.stats.Reads), SubscriptionCreates: maps.Clone(s.stats.SubscriptionCreates),
		SubscriptionDeletes: maps.Clone(s.stats.SubscriptionDeletes), SubscriptionRenewals: maps.Clone(s.stats.SubscriptionRenewals),
		Notifications: slices.Clone(s.stats.Notifications),
		Activities:    slices.Clone(s.stats.Activities)}
	s.mu.Unlock()
	writeJSON(w, st)
}

// joinLocked makes the person user a member of the team, named name (or a name made of the ID).
func (s *Service) joinLocked(user, name string) {
	if p, ok := s.people[user]; ok && (name == "" || p.Name == name) {
		return
	}
	s.people[user] = Person{ID: user, Name: cmpOr(name, "Person "+user[:min(8, len(user))])}
}

// personAccount is the from of a person's activity.
func (s *Service) personAccount(user string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]string{"id": "29:" + user, "name": s.people[user].Name, "aadObjectId": user}
}

// conversationOf is the conversation ID of a thread: Teams names a top-level post as the root of its own thread.
func conversationOf(channel, root string) string { return channel + ";messageid=" + root }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// activity is the frame of an activity of the type typ in the conversation conv of the team's channel.
func (s *Service) activity(typ, id, conv string) map[string]any {
	channel, _ := splitConv(conv)
	return map[string]any{
		"type": typ, "id": id, "channelId": "msteams", "serviceUrl": s.ServiceURL(), "timestamp": time.Now().UTC().Format(time.RFC3339Nano),
		"conversation": map[string]any{"isGroup": true, "conversationType": "channel", "id": conv, "tenantId": s.cfg.TenantID},
		"channelData": map[string]any{"teamsChannelId": channel, "teamsTeamId": s.cfg.TeamID, "channel": map[string]string{"id": channel},
			"team": map[string]string{"id": s.cfg.TeamID, "aadGroupId": s.cfg.GroupID}, "tenant": map[string]string{"id": s.cfg.TenantID}},
	}
}

// sendActivity sends the bot of app the activity a, signed as the Bot Framework signs it, and returns its answer. A bot that cannot be
// reached is status 0, with the error as the body.
func (s *Service) sendActivity(ctx context.Context, app string, a map[string]any) Delivered {
	a["recipient"] = map[string]string{"id": "28:" + app, "name": botName}
	out := Delivered{}
	defer func() {
		s.mu.Lock()
		s.stats.Activities = append(s.stats.Activities, Sent{App: app, Type: fmt.Sprint(a["type"]), ID: fmt.Sprint(a["id"]), Status: out.Status})
		s.mu.Unlock()
	}()
	body, err := json.Marshal(a)
	if err != nil {
		out.Body = err.Error()
		return out
	}
	tok, err := s.sign(app)
	if err != nil {
		out.Body = err.Error()
		return out
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), deliveryTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.Bots[app], bytes.NewReader(body))
	if err != nil {
		out.Body = err.Error()
		return out
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := s.client.Do(req)
	if err != nil {
		out.Body = err.Error()
		return out
	}
	defer func() { _ = resp.Body.Close() }()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	out.Status, out.Body = resp.StatusCode, string(got)
	return out
}

// sign makes the token of an activity for app: RS256 with the fake's key, issued by the Bot Framework, for the bot, with the
// serviceUrl of the fake's activities, valid for an hour.
func (s *Service) sign(app string) (string, error) {
	now := time.Now()
	seg := func(v any) (string, error) {
		b, err := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b), err
	}
	h, err := seg(map[string]string{"alg": "RS256", "typ": "JWT", "kid": keyID})
	if err != nil {
		return "", err
	}
	c, err := seg(map[string]any{"iss": tokenIssuer, "aud": app, "serviceurl": s.ServiceURL(),
		"nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(), "iat": now.Unix()})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(h + "." + c))
	sig, err := rsa.SignPKCS1v15(nil, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", errors.New("faketeams: signing an activity's token")
	}
	return h + "." + c + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}
