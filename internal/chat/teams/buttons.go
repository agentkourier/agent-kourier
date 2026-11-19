package teams

import (
	"cmp"
	"context"
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// This file posts a message's buttons under its text, as a card of their own. Teams answered one activity of text and a card 202 with
// no ID, and showed it as two messages, the text and a card of the buttons alone (measured on a test tenant, web client, 2026-11-18):
// so the text is posted alone, and is the message that Post returns and the outbox records, and the buttons follow it as a card
// alone, which Teams answered with an ID for every card alone that was posted: a question's, a tool approval's, and the card of
// buttons under a text that opened a thread, put in as the text's reply (measured on the same tenant and client, November 2026; a
// post into a thread already open, an edit of the card and a 429 on it were not).
//
// The card names the text (buttonsCard's "of"), so that a press of it is a press of the message that Post returned (press). A press's
// data comes from the presser's client, so the text is bound to the card: each action carries an HMAC-SHA256 of the connection, the
// channel, the card's thread, the text and the button's value, under a key that HKDF derives from the connection's client secret
// (buttonKey). Every replica, and the broker after a restart, has the same key; a rotation of the secret makes the presses of the
// cards posted before it refused. The key is never logged or stored.
//
// The live reply of a turn gets its buttons on its last update, and may be updated again with them (the steps its post did not show,
// or a second write of a post that may be an earlier attempt's): an update shows the buttons once, by the card this Client remembers
// posting under the text (buttonCards). Nothing takes a message's buttons off by an update: an update with none leaves the card. A
// message has one writer at a time, the turn's live run, so two calls for one text do not race; were they to, the buttons could show
// twice.

// maxButtonCards is how many cards of buttons a Client remembers, the oldest forgotten first. A card is needed from its post until the
// last update of its text, which is the end of the turn's live reply, so this is many more than the replies being written at once.
const maxButtonCards = 4096

// buttonsWaitMax is the longest wait that a refusal of a post of buttons may ask for and still be waited out, once, within Post;
// buttonsRetryWait is the wait before that second try when the refusal asked for none (a 5xx, a timeout).
const (
	buttonsWaitMax   = 5 * time.Second
	buttonsRetryWait = time.Second
)

// buttonKeyInfo is HKDF's info of the key of the buttons' MACs: what the key is for, so that no other use of the secret derives it.
const buttonKeyInfo = "agent-kourier/teams/button-mac/v1"

// buttonKey is the key of the MACs of a connection's buttons, which HKDF-SHA256 derives from its client secret.
func buttonKey(clientSecret string) ([]byte, error) {
	key, err := hkdf.Key(sha256.New, []byte(clientSecret), nil, buttonKeyInfo, sha256.Size)
	if err != nil {
		return nil, fmt.Errorf("teams: deriving the key of the buttons: %w", err)
	}
	return key, nil
}

// buttonMAC is the MAC that binds a button of value, on a card in the thread root of channel, to the text of: HMAC-SHA256 under the
// Client's key of the connection and each of them, each prefixed by its length, in lowercase hex.
func (c *Client) buttonMAC(channel, root, of, value string) string {
	m := hmac.New(sha256.New, c.buttonKey)
	for _, part := range []string{c.conn, channel, root, of, value} {
		_, _ = fmt.Fprintf(m, "%d:%s", len(part), part)
	}
	return hex.EncodeToString(m.Sum(nil))
}

// nameableText says whether a card's data can name the text id, which a press then names (press): not empty, no ':', which
// chat.MessageID joins on, and at most maxActivityID bytes.
func nameableText(id string) bool {
	return id != "" && !strings.Contains(id, idSeparator) && len(id) <= maxActivityID
}

// buttonCard is a card of buttons that the Client posted under a text: its ID, "" when Teams took the post and named none, and the
// SHA-256 of the activity it sent, in hex.
type buttonCard struct{ id, sum string }

// buttonCards is the cards of buttons a Client posted, by the channel and the ID of the text they are under, at most maxButtonCards.
// It is kept in memory alone: after a restart, or on another replica, an update of a text with its buttons posts them again.
type buttonCards struct {
	mu    sync.Mutex
	cards map[string]buttonCard // guarded by mu
	order []string              // guarded by mu: the keys of cards, oldest first
}

func newButtonCards() *buttonCards { return &buttonCards{cards: map[string]buttonCard{}} }

func (b *buttonCards) get(key string) (buttonCard, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, ok := b.cards[key]
	return c, ok
}

// put remembers c as the card of key, in key's place if it has one, and forgets the oldest past maxButtonCards.
func (b *buttonCards) put(key string, c buttonCard) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cards[key]; !ok {
		b.order = append(b.order, key)
	}
	b.cards[key] = c
	if len(b.order) > maxButtonCards {
		delete(b.cards, b.order[0])
		b.order = b.order[1:]
	}
}

// forget forgets the card of key.
func (b *buttonCards) forget(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.cards[key]; !ok {
		return
	}
	delete(b.cards, key)
	for i, k := range b.order {
		if k == key {
			b.order = append(b.order[:i], b.order[i+1:]...)
			break
		}
	}
}

// errCardGone is wrapped by the error of buttonsUnder when the card it remembered was gone (404), and Teams refused for good to post
// it anew.
var errCardGone = errors.New("teams: the card of a message's buttons is gone, and could not be posted anew")

// buttonsUnder shows buttons under the text message text, through the Bot Connector at serviceURL, as a card of their own in the text's
// thread, each action bound to the text (buttonMAC): posted when the Client remembers no card of them under the text, edited when it
// remembers one with other buttons, and left as it is when it remembers one of the same. A remembered card that Teams answers 404 for
// is forgotten and posted anew; if Teams refuses that for good (Classify: Permanent), the error wraps errCardGone, and any other
// failure of it is returned as it is, so that the caller's next try posts the card. A card that Teams took and named no ID is remembered, so
// that the same buttons are not posted again (other buttons are posted anew: it cannot be edited), and the error is errNoID. Each call is
// counted as PostButtons; one that the card needs no call for is not counted.
func (c *Client) buttonsUnder(ctx context.Context, serviceURL string, text chat.Ref, buttons []chat.Button) error {
	root := cmp.Or(text.Thread, text.ID)
	a := buttonsActivity(buttons, text.ID, func(value string) string { return c.buttonMAC(text.Channel, root, text.ID, value) })
	sum, err := activitySum(a)
	if err != nil {
		return err
	}
	key := text.Channel + idSeparator + text.ID
	had, ok := c.cards.get(key)
	if ok && had.sum == sum {
		return nil
	}
	conv := conversation(text.Channel, root)
	if ok && had.id != "" {
		_, err := c.buttonsCall(ctx, func(ctx context.Context) (string, error) {
			return had.id, c.api.update(ctx, serviceURL, conv, had.id, a)
		})
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusNotFound {
			if err == nil {
				c.cards.put(key, buttonCard{id: had.id, sum: sum})
			}
			return err
		}
		c.cards.forget(key)
		switch err := c.postButtons(ctx, serviceURL, conv, key, sum, a); {
		case err == nil, errors.Is(err, errNoID):
			return nil
		case c.Classify(chat.OpPost, err).Class == chat.Permanent:
			return fmt.Errorf("%w: %w", errCardGone, err)
		default: // the card is forgotten: the caller's next try posts it
			return err
		}
	}
	return c.postButtons(ctx, serviceURL, conv, key, sum, a)
}

// buttonsAfterPost shows the buttons of the text that Post just made (buttonsUnder), and tries once more, after its wait, a post that
// Classify says may pass on another try and that asks for a wait of at most buttonsWaitMax (buttonsRetryWait when it asks for none).
// What is not shown then is logged at WARN, and the text stands: the next update of the message with the buttons posts them. A text
// that a card cannot name (nameableText) gets no buttons, and that is logged too.
func (c *Client) buttonsAfterPost(ctx context.Context, serviceURL string, text chat.Ref, buttons []chat.Button) {
	if !nameableText(text.ID) {
		c.log.WarnContext(ctx, "teams: a message's buttons were not posted: its text's ID cannot be named on a card", "channel", text.Channel)
		return
	}
	err := c.buttonsUnder(ctx, serviceURL, text, buttons)
	if err != nil && !errors.Is(err, errNoID) {
		v := c.Classify(chat.OpPost, err)
		if wait := cmp.Or(v.RetryAfter, buttonsRetryWait); v.Class == chat.Transient && wait <= buttonsWaitMax && c.buttonsWait(ctx, wait) {
			err = c.buttonsUnder(ctx, serviceURL, text, buttons)
		}
	}
	if err != nil {
		c.log.WarnContext(ctx, "teams: a message's buttons were not posted under its text, or Teams named no ID for them; the text "+
			"stands, and an update of the message with them posts them", "channel", text.Channel, "message", text.ID, "error", err.Error())
	}
}

// postButtons posts the card a of buttons to conv and remembers it as key's, with its ID, or with none when Teams named none.
func (c *Client) postButtons(ctx context.Context, serviceURL, conv, key, sum string, a outActivity) error {
	id, err := c.buttonsCall(ctx, func(ctx context.Context) (string, error) { return c.api.send(ctx, serviceURL, conv, a) })
	if err == nil {
		channel, _ := splitConversation(conv)
		c.sent(channel, id)
	}
	if err == nil || errors.Is(err, errNoID) {
		c.cards.put(key, buttonCard{id: id, sum: sum})
	}
	return err
}

// buttonsCall makes one call of a card of buttons, counted as PostButtons, and returns what it does.
func (c *Client) buttonsCall(ctx context.Context, call func(context.Context) (string, error)) (id string, err error) {
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.PostButtons")
	defer func() { c.finish(ctx, op, "PostButtons", err) }()
	return call(ctx)
}

// activitySum is the SHA-256 of a as the connector sends it, in hex.
func activitySum(a outActivity) (string, error) {
	raw, err := encode(a)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
