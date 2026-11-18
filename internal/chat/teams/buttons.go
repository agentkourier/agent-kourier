package teams

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// This file posts a message's buttons under its text, as a card of their own. Teams answers one activity of text and a card 202 with
// no ID, and shows it as two messages, the text and a card of the buttons alone (measured on a test tenant, web client, 2026-11-18):
// so the text is posted alone, and is the message that Post returns and the outbox records, and the buttons follow it as a card
// alone, which Teams answered 201 with an ID for every card alone that was posted (a question's and a tool approval's). The card names
// the text (buttonsCard's "of"), so that a press of it is a press of the message that Post returned (press).
//
// The live reply of a turn gets its buttons on its last update, and may be updated again with them (the steps its post did not show,
// or a second write of a post that may be an earlier attempt's): an update shows the buttons once, by the card this Client remembers
// posting under the text (buttonCards). Nothing takes a message's buttons off by an update: an update with none leaves the card.

// maxButtonCards is how many cards of buttons a Client remembers, the oldest forgotten first. A card is needed from its post until the
// last update of its text, which is the end of the turn's live reply, so this is many more than the replies being written at once.
const maxButtonCards = 4096

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

// buttonsUnder shows buttons under the text message text, through the Bot Connector at serviceURL, as a card of their own in the text's
// thread (buttonsActivity): posted when the Client remembers no card of them under the text, edited when it remembers one with other
// buttons, and left as it is when it remembers one of the same. A card that Teams took and named no ID is remembered, so that the
// same buttons are not posted again (other buttons are posted anew: it cannot be edited), and the error is errNoID. The call is counted as PostButtons; one that the card needs no call for is not counted.
func (c *Client) buttonsUnder(ctx context.Context, serviceURL string, text chat.Ref, buttons []chat.Button) (err error) {
	a := buttonsActivity(buttons, text.ID)
	sum, err := activitySum(a)
	if err != nil {
		return err
	}
	key := text.Channel + idSeparator + text.ID
	had, ok := c.cards.get(key)
	if ok && had.sum == sum {
		return nil
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.PostButtons")
	defer func() { c.finish(ctx, op, "PostButtons", err) }()
	conv := conversation(text.Channel, cmp.Or(text.Thread, text.ID))
	if ok && had.id != "" {
		if err := c.api.update(ctx, serviceURL, conv, had.id, a); err != nil {
			return err
		}
		c.cards.put(key, buttonCard{id: had.id, sum: sum})
		return nil
	}
	id, err := c.api.send(ctx, serviceURL, conv, a)
	if err == nil || errors.Is(err, errNoID) {
		c.cards.put(key, buttonCard{id: id, sum: sum})
	}
	return err
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
