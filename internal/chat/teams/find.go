package teams

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// This file tags each post with the outbox's ID for it, in a part of the post that people do not see and that Microsoft Graph gives
// back, and finds the post by that tag (Find), so that a post that was sent and not recorded before a crash is not sent again. What
// was measured on a test tenant, web client, channel thread, in November 2026 (a bot's posts read back through the replies list and a
// GET of each message, before and after a PUT that sent the same tag again; and a bot's post that opened a thread, read back through
// the channel's messages):
//   - an empty <span title="…"> at the end of a markdown text comes back as <p><span title="…"></span></p> in the body's HTML, in a
//     reply and in a post that opened a thread;
//   - the id of a card's TextBlock and of its RichTextBlock (the first element of a prompt's card) comes back in the card's JSON; an
//     id on an element of another type was not measured;
//   - neither shows in the client, and the span's paragraph adds a little space under the text;
//   - Teams drops, and so cannot carry a tag: the activity's channelData and entities, its summary, an HTML comment, a span's data-
//     attributes, an attachment's name and a card's unknown keys;
//   - a text with a card of buttons in one activity is answered 202 with no ID, and shows as two messages: the text, with the span,
//     and the card alone, with no tag. So Post sends a message's text and its buttons as two posts (buttons.go): the text, which
//     carries the tag, and the card, which carries none;
//   - a reply was in Graph's replies at the first read, 1.4 s after it was posted.
//
// A zero-width run at the start of a text changed how Teams parsed the first line; the span was put only at the end, and not tried at
// the start. Not measured: Teams' mobile and desktop clients, screen readers, Teams' other surfaces (search, notifications), and the
// order of the channel's messages.

// findRootPages is how many pages of a channel's messages Find reads for a post that opened a thread (a trigger's digest line): at
// graphPageTop a page, the 200 threads that Graph gives first. Microsoft documents the list as sorted by the last change of each
// thread, and not in which direction (not measured): a post that Graph does not give in these pages is not found, and is posted again.
const findRootPages = 4

// carrierPrefix begins every carrier, so that a carrier reads as Agent Kourier's in Graph's HTML and a card's JSON.
const carrierPrefix = "kourier"

// carrierOf is the carrier of the outbox's tag: carrierPrefix and the first 16 bytes of the tag's SHA-256 in lowercase hex, which
// need no escape in an HTML attribute or a card's id and are the same length for every tag, so that a post's size does not depend on
// its tag. No tag has none.
func carrierOf(tag string) string {
	if tag == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(tag))
	return carrierPrefix + hex.EncodeToString(sum[:16])
}

// withCarrier is a carrying the carrier, if it is not "": a text ends with a paragraph of an empty span titled with it (after
// everything render wrote, a block's closing fence included; a zero-width tag at the start changed how Teams parsed the first line,
// and the span was measured only at the end); a card alone has it as the id of the first element of its body with no id, which no
// input of a card has (inputID): a prompt's and an ending's first element is a RichTextBlock or a TextBlock, whose id was measured.
// A message's buttons under its text are a post of their own with no carrier (buttons.go): the text is the post that Find finds.
// The span is the adapter's alone: no "<" of the agent's, a person's or an alert's text reaches Teams raw (markdown.go, escapeLine).
// render's budget leaves room for the carrier, whose length is fixed.
func withCarrier(a outActivity, carrier string) outActivity {
	switch {
	case carrier == "":
	case a.Text != "":
		a.Text += "\n\n" + `<span title="` + carrier + `"></span>`
	case len(a.Attachments) > 0:
		body, _ := a.Attachments[0].Content["body"].([]any)
		for _, el := range body {
			if el, ok := el.(map[string]any); ok && el["id"] == nil {
				el["id"] = carrier
				break
			}
		}
	}
	return a
}

// Find implements chat.Platform: the bot's own message in t that carries tag's carrier (withCarrier), read through Microsoft Graph
// with the app's Graph token: the replies of the thread t.Thread, at most threadMaxPages pages, or for a target with no thread the
// channel's messages that open a thread, at most findRootPages pages, all within the thread read's bound (threadTimeout). The read
// stops at the page that holds it. since is not used: Graph takes no bound of the replies, and the tag is the post's alone.
//
// Only a message whose sender is the bot's own app counts, and only its body's HTML (an empty span, read as a tag: spanTitled) or the
// id of an element of its cards' bodies (cardIDed): a person or another app can post either, and the text a person reads holds no
// tag. Not found is false and a nil error, and so is what no retry would mend, so that the outbox posts, and the post may show twice,
// instead of holding it back until it is abandoned: a target or tag that cannot be read for (with no call), and, each logged at WARN,
// a channel whose team is not known yet (with no call), a thread with more pages than are read, a read that ran out of its time after
// Graph gave a page (a thread too long to read in time, every try: were it an error, the outbox would abandon the post once its
// lookups had timed out often enough), a next page that is not Graph's or that the read already took, an answer past graphMaxBody,
// and a refusal that Classify says is Permanent (Graph's 400, 401, 403 and 404, and Entra's refusal of the Graph token). Any other
// error is returned, which Classify says is Transient (a 429, with its wait; a 5xx; an answer that is not JSON; a read that ran out
// of time before any page came back), and the outbox looks again later. None is FatalAuth. Every read is counted as Find by what it
// met, what is answered as not found too.
func (c *Client) Find(ctx context.Context, t chat.Target, tag string, _ time.Time) (chat.Ref, bool, error) {
	carrier := carrierOf(tag)
	if carrier == "" || t.Channel == "" || (t.Thread != "" && !isMessageID(t.Thread)) {
		return chat.Ref{}, false, nil
	}
	ctx, op := telemetry.Start(ctx, "agentkourier.chat.Find")
	readCtx, cancel := context.WithTimeout(ctx, c.threadTimeout)
	defer cancel()
	ref, found, pages, err := c.find(readCtx, t, carrier)
	if pages > 0 && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
		err = fmt.Errorf("%w: after %d pages: %w", errReadCut, pages, err)
	}
	c.finish(ctx, op, "Find", err) // counted as what the read met, an error answered as not found too
	if err != nil && c.cannotLook(err) {
		c.log.WarnContext(ctx, "teams: the outbox could not look for a post it may have made before a restart; it posts it, and the "+
			"thread may show it twice", "channel", t.Channel, "outbox_id", tag, "error", err.Error())
		return chat.Ref{}, false, nil
	}
	return ref, found, err
}

// errReadCut is wrapped by the error of a Find whose read ran out of its time after Graph gave a page: a thread too long to read in time,
// which it is again on the next try.
var errReadCut = errors.New("teams: the read of the thread ran out of time after Graph gave a page")

// find reads t's replies, or its channel's roots, for the bot's message that carries carrier. pages is how many pages Graph gave.
func (c *Client) find(ctx context.Context, t chat.Target, carrier string) (ref chat.Ref, found bool, pages int, err error) {
	group, err := c.teamGroup(ctx, t.Channel)
	if err != nil {
		return chat.Ref{}, false, 0, err
	}
	link, maxPages := c.graph.resource("teams", group, "channels", t.Channel, "messages"), findRootPages
	if t.Thread != "" {
		link, maxPages = c.graph.resource("teams", group, "channels", t.Channel, "messages", t.Thread, "replies"), threadMaxPages
	}
	var at string
	more, err := c.graph.pages(ctx, link+fmt.Sprintf("?$top=%d", graphPageTop), maxPages, func(page []graphMessage) bool {
		pages++
		for _, m := range page {
			if isMessageID(m.ID) && m.ID != t.Thread && c.carries(m, carrier) {
				at = m.ID
				return true
			}
		}
		return false
	})
	switch {
	case err != nil:
		return chat.Ref{}, false, pages, err
	case at != "":
		return c.ref(t.Channel, t.Thread, at), true, pages, nil
	case more:
		return chat.Ref{}, false, pages, fmt.Errorf("teams find: %w: more than %d pages", chat.ErrThreadTooLong, maxPages)
	}
	return chat.Ref{}, false, pages, nil
}

// cannotLook says whether err, of a Find's read, is one that no retry mends: what Classify says is Permanent, and what Graph's answers
// or the thread's length make so again on every try.
func (c *Client) cannotLook(err error) bool {
	for _, never := range []error{errTeamNotKnown, chat.ErrThreadTooLong, errReadCut, errNotGraph, errPageCycle, errAnswerTooLong} {
		if errors.Is(err, never) {
			return true
		}
	}
	return c.Classify(chat.OpPost, err).Class == chat.Permanent
}

// carries says whether m is a message of the bot's own app that carries carrier, in its body's HTML or its cards.
func (c *Client) carries(m graphMessage, carrier string) bool {
	if m.From == nil || m.From.Application == nil || m.From.Application.ID != c.app {
		return false
	}
	return (strings.EqualFold(cmp.Or(m.Body.ContentType, "html"), "html") && spanTitled(m.Body.Content, carrier)) || m.cardIDed(carrier)
}

// spanTitled says whether the HTML h holds a span tag whose title attribute, its character references decoded, is carrier (not ""):
// a "<span" that white space, "/" or ">" ends, read to the next ">". It reads tags and not text, so that a span written as text
// ("&lt;span …", which is how the agent's "<" reaches Graph) is none. It is linear in h: each "<" is looked at once, and a tag is read
// once to its ">".
func spanTitled(h, carrier string) bool {
	if carrier == "" {
		return false
	}
	for i := 0; ; {
		j := strings.IndexByte(h[i:], '<')
		if j < 0 {
			return false
		}
		i += j + 1
		if len(h)-i < len("span") || !strings.EqualFold(h[i:i+len("span")], "span") {
			continue
		}
		i += len("span")
		if i < len(h) && !strings.ContainsRune(" \t\n\r\f/>", rune(h[i])) {
			continue // another tag, such as <spanx>
		}
		end := strings.IndexByte(h[i:], '>')
		if end < 0 {
			return false
		}
		if title, ok := attrValue(h[i:i+end], "title"); ok && html.UnescapeString(title) == carrier {
			return true
		}
		i += end
	}
}

// attrValue is the value of the attribute name (in any case) among the attributes attrs of an HTML tag: double-quoted, single-quoted
// or bare. A bare value runs to the next white space. An attribute with no value is not one with the value "".
func attrValue(attrs, name string) (string, bool) {
	space := func(b byte) bool { return strings.IndexByte(" \t\n\r\f", b) >= 0 }
	for i := 0; i < len(attrs); {
		for i < len(attrs) && (space(attrs[i]) || attrs[i] == '/') {
			i++
		}
		start := i
		for i < len(attrs) && !space(attrs[i]) && attrs[i] != '=' && attrs[i] != '/' {
			i++
		}
		key := attrs[start:i]
		for i < len(attrs) && space(attrs[i]) {
			i++
		}
		if i >= len(attrs) || attrs[i] != '=' {
			continue
		}
		i++
		for i < len(attrs) && space(attrs[i]) {
			i++
		}
		var value string
		switch {
		case i < len(attrs) && (attrs[i] == '"' || attrs[i] == '\''):
			q := attrs[i]
			end := strings.IndexByte(attrs[i+1:], q)
			if end < 0 {
				return "", false // an unclosed quote ends the tag's attributes
			}
			value, i = attrs[i+1:i+1+end], i+2+end
		default:
			start := i
			for i < len(attrs) && !space(attrs[i]) {
				i++
			}
			value = attrs[start:i]
		}
		if strings.EqualFold(key, name) {
			return value, true
		}
	}
	return "", false
}

// cardIDed says whether one of m's Adaptive Cards has an element of its body, at the top, whose id is carrier (not ""), by the card's
// JSON, which Graph gives as a string: a text that holds the id, or an element deeper in the card, is not it.
func (m graphMessage) cardIDed(carrier string) bool {
	if carrier == "" {
		return false
	}
	for _, a := range m.Attachments {
		var content string
		if a.ContentType != cardContentTypeIn || json.Unmarshal(a.Content, &content) != nil {
			continue
		}
		var card struct {
			Body []json.RawMessage `json:"body"`
		}
		if json.Unmarshal([]byte(content), &card) != nil {
			continue
		}
		for _, raw := range card.Body {
			var el struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &el) == nil && el.ID == carrier {
				return true
			}
		}
	}
	return false
}
