package faketeams

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// What Service fakes, beside the socket service of SocketMode, is the rest of what a Microsoft Teams connection talks to, as the
// adapter of internal/chat/teams speaks to it and as the live run of November 2026 measured it: Microsoft Entra's token endpoint, the
// Bot Framework's OpenID metadata and its key set, the Bot Connector that a channel's activities name as their serviceUrl, and the
// part of Microsoft Graph that reads a channel's messages and keeps subscriptions to them. It is one tenant and one team, with one
// Microsoft Entra group, whose channels come into being when the control API first names them; its apps are the bots it hosts, each
// with the messaging endpoint it sends the bot's activities to. Its control API (/_control/) is what a test drives: an install, a
// person's mention, a press of a card's button, a Workflows post of an alert's card, and the reads of what the bots posted.
//
// What it does not fake: the real service's error bodies beyond their code, throttling, paging past one page, a person's edits and
// deletions, conversations outside a team's channel, lifecycle notifications, and the timing of anything.

// The fake's names, which the real service would choose.
const (
	// botName is every bot's display name, as a mention shows it.
	botName = "Agent Kourier"
	// keyID is the kid of the fake's one signing key.
	keyID = "faketeams-1"
	// serviceRoot is the path of the fake's Bot Connector under its base: the serviceUrl of its activities, as /teams/ is the
	// global route of smba.trafficmanager.net.
	serviceRoot = "/teams/"
	// tokenIssuer is the iss of the tokens that the Bot Framework signs.
	tokenIssuer = "https://api.botframework.com" //nolint:gosec // G101: an issuer URL, no credential
	// botScope and graphScope are the two scopes that the token endpoint issues a token for.
	botScope   = "https://api.botframework.com/.default"
	graphScope = "https://graph.microsoft.com/.default"
	// cardType is the content type of an Adaptive Card attachment.
	cardType = "application/vnd.microsoft.card.adaptive"
	// deliveryTimeout bounds one request that the fake sends a broker: an activity, a validation handshake or a notification.
	deliveryTimeout = 30 * time.Second
	// maxBody caps a body that the fake reads.
	maxBody = 1 << 20
)

// Config is the fake's world.
type Config struct {
	// PublicURL is the fake's https base as the broker reaches it, such as https://faketeams.e2e.svc.cluster.local:8443: the
	// serviceUrl of its activities, the jwks_uri of its metadata and the base of its Graph links are under it. Required.
	PublicURL string
	// TenantID is the team's tenant, which every activity and notification names. Required.
	TenantID string
	// TeamID is the team's Teams ID (the ID of its General channel) and GroupID its Microsoft Entra group, by which Graph names it.
	// Required.
	TeamID, GroupID string
	// Channels are the team's channels at the start. A control call that names another adds it.
	Channels []string
	// ClientSecret is the client secret of every app: the token endpoint refuses any other. Required.
	ClientSecret string
	// Bots are the apps that the fake hosts: an app ID and the messaging endpoint that its activities are sent to. At least one.
	Bots map[string]string
	// DeliverHTTP sends a notification or a validation handshake for an https notificationUrl over plain HTTP, as a TLS-terminating
	// ingress in front of a broker's chat listener would. The bots' endpoints are used as they are.
	DeliverHTTP bool
	// Client sends what the fake sends a broker; a client with a timeout of 30 seconds if nil.
	Client *http.Client
	// Retry is how often an undelivered notification is sent again, and RetryFor for how long: 1 second and 3 minutes if zero.
	Retry, RetryFor time.Duration
	// Logger is slog.Default() if nil.
	Logger *slog.Logger
}

// Service is the fake. Its zero value is not usable: NewService makes one.
type Service struct {
	cfg    Config
	base   string // PublicURL without a trailing slash
	key    *rsa.PrivateKey
	client *http.Client
	log    *slog.Logger
	ctx    context.Context //nolint:containedctx // the life of the background deliveries, which Close ends
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	lastID   int64               // the last message ID issued: milliseconds since the epoch, made to increase
	seq      int64               // the last change of a message (a post or an edit), which a delta token names
	channels []string            // the team's channels, in the order they were first named
	messages map[string]*message // by ID
	people   map[string]Person   // the team's members, by Microsoft Entra object ID
	tokens   map[string]grant    // what each access token was issued for
	subs     map[string]*subscription
	subSeq   int
	stats    Stats
}

// grant is what an access token was issued for.
type grant struct {
	app   string
	graph bool // a Graph token; false is a bot token
}

// Person is a member of the team.
type Person struct {
	// ID is the person's Microsoft Entra object ID.
	ID string `json:"id"`
	// Name is their display name.
	Name string `json:"name"`
}

// The kinds of a message's sender.
const (
	// SenderPerson is a person's post.
	SenderPerson = "person"
	// SenderBot is a post of a bot that the fake hosts, through the Bot Connector.
	SenderBot = "bot"
	// SenderApp is a post of another app, as Workflows posts an alert's card: no bot is sent it, and only Graph shows it.
	SenderApp = "app"
)

// message is a channel's message.
type message struct {
	id, channel, root string // root is "" for a top-level post
	kind, from, name  string // kind is a sender kind; from is the person's object ID or the app's ID
	text              string // a person's plain text, a bot's markdown, or another app's HTML
	mentioned         string // the app that a person's post mentions, or ""
	attachments       []attachment
	created           time.Time
	seq               int64
	versions          []string // a bot's message: the text of each version, the first as it was posted
}

// attachment is one attachment of a message, its content as JSON.
type attachment struct {
	ContentType string          `json:"contentType"`
	Content     json.RawMessage `json:"content,omitempty"`
}

// NewService returns the fake of cfg, with a signing key of its own. Close ends what it sends in the background.
func NewService(cfg Config) (*Service, error) {
	switch {
	case cfg.PublicURL == "" || cfg.TenantID == "" || cfg.TeamID == "" || cfg.GroupID == "" || cfg.ClientSecret == "":
		return nil, errors.New("faketeams: the public URL, tenant, team, group and client secret are required")
	case len(cfg.Bots) == 0:
		return nil, errors.New("faketeams: no bot")
	}
	u, err := url.Parse(cfg.PublicURL)
	if err != nil || u.Scheme != "https" || u.Host == "" {
		return nil, errors.New("faketeams: the public URL must be an https URL with a host")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, fmt.Errorf("faketeams: a signing key: %w", err)
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: deliveryTimeout}
	}
	cfg.Retry, cfg.RetryFor = cmp.Or(cfg.Retry, time.Second), cmp.Or(cfg.RetryFor, 3*time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	return &Service{cfg: cfg, base: strings.TrimSuffix(cfg.PublicURL, "/"), key: key, client: client, log: cmp.Or(cfg.Logger, slog.Default()),
		ctx: ctx, cancel: cancel, channels: slices.Compact(slices.Clone(cfg.Channels)), messages: map[string]*message{}, people: map[string]Person{}, tokens: map[string]grant{},
		subs: map[string]*subscription{}, stats: Stats{Reads: map[string]Reads{}}}, nil
}

// Close stops the deliveries in flight and waits for them.
func (s *Service) Close() {
	s.cancel()
	s.wg.Wait()
}

// ServiceURL is the serviceUrl of the fake's activities: its Bot Connector.
func (s *Service) ServiceURL() string { return s.base + serviceRoot }

// Handler serves the fake's Microsoft side and its control API.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	s.routeMicrosoft(mux)
	s.routeControl(mux)
	return mux
}

// nextIDLocked issues a message ID: the milliseconds of now, or one more than the last, as Teams' 13-digit IDs are.
func (s *Service) nextIDLocked(now time.Time) string {
	s.lastID = max(s.lastID+1, now.UnixMilli())
	return strconv.FormatInt(s.lastID, 10)
}

// channelLocked makes channel one of the team's, if it is not yet.
func (s *Service) channelLocked(channel string) {
	if !slices.Contains(s.channels, channel) {
		s.channels = append(s.channels, channel)
	}
}

// postLocked adds m to its channel with an ID and a change of its own, and returns it.
func (s *Service) postLocked(m *message) *message {
	now := time.Now().UTC()
	m.id, m.created = s.nextIDLocked(now), now
	s.seq++
	m.seq = s.seq
	if m.kind == SenderBot {
		m.versions = []string{m.text}
	}
	s.messages[m.id] = m
	return m
}

// threadLocked returns the messages of channel in the thread of root, the root first, or the channel's top-level posts when root is
// "", in the order of their IDs.
func (s *Service) threadLocked(channel, root string) []*message {
	var out []*message
	for _, m := range s.messages {
		switch {
		case m.channel != channel:
		case root == "" && m.root == "":
			out = append(out, m)
		case root != "" && (m.id == root || m.root == root):
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b *message) int { return compareIDs(a.id, b.id) })
	return out
}

// compareIDs orders two message IDs, decimal numbers, by their value.
func compareIDs(a, b string) int {
	return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b))
}

// MessageView is a channel's message as the control API shows it.
type MessageView struct {
	ID      string `json:"id"`
	Channel string `json:"channel"`
	// Root is the ID of the thread's root for a reply, and "" for a top-level post.
	Root string `json:"root,omitempty"`
	// Kind is SenderPerson, SenderBot or SenderApp, and From is the person's object ID or the app's ID.
	Kind string `json:"kind"`
	From string `json:"from"`
	// Text is the message's text as it is now: a bot's markdown, a person's plain text, another app's HTML.
	Text string `json:"text,omitempty"`
	// Cards are the content of its Adaptive Cards as they are now.
	Cards []json.RawMessage `json:"cards,omitempty"`
	// Versions is, for a bot's message, the text of each version: the first as it was posted, then one for each edit.
	Versions []string `json:"versions,omitempty"`
	// Edits counts the edits of a bot's message.
	Edits   int       `json:"edits"`
	Created time.Time `json:"created"`
}

func (m *message) view() MessageView {
	v := MessageView{ID: m.id, Channel: m.channel, Root: m.root, Kind: m.kind, From: m.from, Text: m.text,
		Versions: slices.Clone(m.versions), Created: m.created}
	if len(m.versions) > 0 {
		v.Edits = len(m.versions) - 1
	}
	for _, a := range m.attachments {
		if a.ContentType == cardType {
			v.Cards = append(v.Cards, a.Content)
		}
	}
	return v
}

// Stats are what the fake counts of what the brokers asked of it.
type Stats struct {
	// Reads are Graph's reads of each channel's messages.
	Reads map[string]Reads `json:"reads"`
	// SubscriptionCreates counts the subscriptions made, by channel, SubscriptionDeletes those deleted, and SubscriptionRenewals the
	// renewals that passed their validation handshake.
	SubscriptionCreates  map[string]int `json:"subscriptionCreates"`
	SubscriptionDeletes  map[string]int `json:"subscriptionDeletes"`
	SubscriptionRenewals map[string]int `json:"subscriptionRenewals"`
	// Notifications are the change notifications sent, each with how it went.
	Notifications []Notification `json:"notifications"`
	// Activities are the activities sent to the bots, each with the answer.
	Activities []Sent `json:"activities"`
}

// Reads counts Graph's reads of one channel.
type Reads struct {
	// Delta counts the reads of messages/delta, and Synced those that carried a delta token: a poll after the first sync.
	Delta, Synced int
	// Messages counts the reads of the channel's posts, Replies of a thread's replies, and Message of one message.
	Messages, Replies, Message int
}

// Notification is a change notification that the fake sent.
type Notification struct {
	Subscription string `json:"subscription"`
	ChangeType   string `json:"changeType"`
	Message      string `json:"message"`
	// Attempts counts the sends, and Status is the last answer's status, 0 for none.
	Attempts  int  `json:"attempts"`
	Status    int  `json:"status"`
	Delivered bool `json:"delivered"`
}

// Sent is an activity that the fake sent a bot.
type Sent struct {
	App    string `json:"app"`
	Type   string `json:"type"`
	ID     string `json:"id"`
	Status int    `json:"status"`
}
