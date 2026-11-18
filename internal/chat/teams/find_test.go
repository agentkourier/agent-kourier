package teams

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// carrierShape is the shape of every carrier: Agent Kourier's prefix and 32 lowercase hex digits, which need no escape in an HTML
// attribute or a card's JSON.
var carrierShape = regexp.MustCompile(`^kourier[0-9a-f]{32}$`)

// findTag and otherTag are two outbox IDs, as the outbox makes them.
const (
	findTag  = "session-1/turn-2/reply"
	otherTag = "session-1/turn-3/reply"
)

// posted is the one activity the fake Bot Connector took since the last take.
func (ct *clientTest) posted(t *testing.T) outActivity {
	t.Helper()
	reqs := ct.f.take()
	if len(reqs) != 1 {
		t.Fatalf("%d calls, want 1: %+v", len(reqs), reqs)
	}
	return reqs[0].activity
}

// echo keeps a, which the bot's app appID posted as the message id (a reply to root, or a root if root is ""), in Graph as Teams
// keeps it, by the shape measured on a test tenant in November 2026: each paragraph of the markdown is a <p> that keeps the
// paragraph's raw HTML (a <span title> among it) and its character references as they are, and each card is its JSON as a string.
// What Teams' markdown makes of emphasis and lists is not reproduced: Find reads none of it.
func (ct *clientTest) echo(t *testing.T, id, root, appID string, a outActivity) {
	t.Helper()
	var html strings.Builder
	if a.Text != "" {
		for _, p := range strings.Split(a.Text, "\n\n") {
			html.WriteString("<p>" + p + "</p>")
		}
	}
	var cards []string
	for _, att := range a.Attachments {
		raw, err := json.Marshal(att.Content)
		if err != nil {
			t.Fatal(err)
		}
		cards = append(cards, string(raw))
		html.WriteString(`<attachment id="` + strconv.Itoa(len(cards)) + `"></attachment>`)
	}
	ct.f.putMessage(testChannel, app(id, root, appID, html.String(), cards...))
}

// postAndEcho posts o in target with tag, and keeps what was posted in Graph as the bot's app's message.
func (ct *clientTest) postAndEcho(t *testing.T, target chat.Target, o chat.Outgoing, tag string) chat.Ref {
	t.Helper()
	ref, err := ct.c.Post(t.Context(), target, o, tag)
	if err != nil {
		t.Fatal(err)
	}
	ct.echo(t, ref.ID, target.Thread, testAppID, ct.posted(t))
	return ref
}

// richReply is a reply shaped like the agent's: a paragraph, a list and a fenced block last, after which the carrier goes.
func richReply() chat.Outgoing {
	return chat.Say(chat.Message{Fallback: "the answer", Parts: []chat.Part{chat.Body{Markdown: "It is the disk.\n\n- one\n- two\n\n```\nrate(x[5m])\n```"}}})
}

// A text post carries the tag at its end, as a paragraph of its own that is an empty span titled with the tag's carrier, after
// everything that render writes, the fence that closes a code block included. The text before it is render's, unchanged.
func TestATextPostCarriesItsTagAtItsEnd(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	o := richReply()
	if _, err := ct.c.Post(t.Context(), target, o, findTag); err != nil {
		t.Fatal(err)
	}
	a := ct.posted(t)
	plain, err := render(o)
	if err != nil {
		t.Fatal(err)
	}
	carrier := carrierOf(findTag)
	if !carrierShape.MatchString(carrier) {
		t.Fatalf("carrier %q is not of the shape %v", carrier, carrierShape)
	}
	if want := plain.Text + "\n\n" + `<span title="` + carrier + `"></span>`; a.Text != want {
		t.Errorf("text = %q,\nwant %q", a.Text, want)
	}
	if !strings.HasSuffix(plain.Text, "```") {
		t.Fatalf("the reply does not end with a fence: %q", plain.Text)
	}
	if carrierOf(otherTag) == carrier || carrierOf(findTag) != carrier {
		t.Error("the carrier is not a function of the tag alone")
	}
}

// A post that is a card alone (a prompt, say) carries the tag as the id of the first element of its body that has none, and every
// other element is as render made it: an input keeps its own id, which a press reads back. A post with no tag carries nothing.
func TestACardPostCarriesItsTagAsAnElementID(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	prompt := chat.Ask(chat.Prompt{ID: "p1", Questions: []chat.Question{{Text: "Which?", Choices: []string{"a", "b"}}, {Text: "Why?"}}})
	if _, err := ct.c.Post(t.Context(), target, prompt, findTag); err != nil {
		t.Fatal(err)
	}
	a := ct.posted(t)
	plain, err := render(prompt)
	if err != nil {
		t.Fatal(err)
	}
	if a.Text != "" || len(a.Attachments) != 1 {
		t.Fatalf("a prompt is %+v, want a card alone", a)
	}
	body := a.Attachments[0].Content["body"].([]any)
	plainBody := plain.Attachments[0].Content["body"].([]any)
	ids := map[string]int{}
	for i, el := range body {
		got, want := el.(map[string]any), plainBody[i].(map[string]any)
		id, _ := got["id"].(string)
		ids[id]++
		if i == 0 {
			if id != carrierOf(findTag) {
				t.Errorf("the first element's id = %q, want the carrier", id)
			}
			delete(got, "id")
		}
		if g, w := mustJSON(t, got), mustJSON(t, want); g != w {
			t.Errorf("element %d = %s, want %s", i, g, w)
		}
	}
	for id, n := range ids {
		if id != "" && n > 1 {
			t.Errorf("the id %q is on %d elements", id, n)
		}
	}
	if ids["r0"] != 1 || ids["t1"] != 1 {
		t.Errorf("the inputs' ids are %v, want r0 and t1 kept", ids)
	}

	if _, err := ct.c.Post(t.Context(), target, prompt, ""); err != nil {
		t.Fatal(err)
	}
	if g, w := mustJSON(t, ct.posted(t)), mustJSON(t, plain); g != w {
		t.Errorf("a post with no tag = %s, want render's %s", g, w)
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Find finds the bot's own post by its tag in the thread's replies, through Graph, as the Ref that Post returned; and the root that
// a post with no thread made, in the channel's messages. A post of another tag is not it.
func TestFindFindsTheBotsOwnPost(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.replies(3)
	reply := ct.postAndEcho(t, target, richReply(), findTag)
	prompt := ct.postAndEcho(t, target, chat.Ask(chat.Prompt{ID: "p1", Questions: []chat.Question{{Text: "Why?"}}}), otherTag)
	for tag, want := range map[string]chat.Ref{findTag: reply, otherTag: prompt} {
		got, found, err := ct.c.Find(t.Context(), target, tag, time.Time{})
		if err != nil || !found || got != want {
			t.Errorf("Find(%q) = %+v, %t, %v; want %+v", tag, got, found, err, want)
		}
	}
	if reqs := ct.f.graphTaken(); len(reqs) != 2 || reqs[0].path != messagePath(testGroup, testChannel, testRoot)+"/replies" ||
		reqs[0].query != "$top=50" {
		t.Errorf("Graph calls = %+v, want one read of the replies each", reqs)
	}

	channel := chat.Target{Conn: testConn, Channel: testChannel}
	root := ct.postAndEcho(t, channel, chat.Say(chat.Message{Fallback: "3 messages not investigated in the last 10m (rate limit)."}), "digest/1")
	got, found, err := ct.c.Find(t.Context(), channel, "digest/1", time.Time{})
	if err != nil || !found || got != root {
		t.Errorf("Find of a root = %+v, %t, %v; want %+v", got, found, err, root)
	}
	if reqs := ct.f.graphTaken(); len(reqs) != 1 || reqs[0].path != "/teams/"+testGroup+"/channels/"+testChannel+"/messages" ||
		reqs[0].query != "$top=50" {
		t.Errorf("Graph calls = %+v, want one read of the channel's messages", reqs)
	}
	if _, found, err := ct.c.Find(t.Context(), channel, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find of a reply among the roots = %t, %v; want not found", found, err)
	}
}

// Only a message of the bot's own app is the post: a person or another app that posts the same span, or a card with the same id,
// does not keep the outbox from posting. Nor does a message with no sender.
func TestFindTakesOnlyTheBotsOwnMessage(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	carrier := carrierOf(findTag)
	span := `<p>hi</p><p><span title="` + carrier + `"></span></p>`
	card := `{"type":"AdaptiveCard","body":[{"type":"TextBlock","id":"` + carrier + `","text":"x"}]}`
	ct.f.putMessage(testChannel, person(replyID(1), testRoot, testUser, span))
	ct.f.putMessage(testChannel, app(replyID(2), testRoot, testWorkflows, span, card))
	ct.f.putMessage(testChannel, app(replyID(3), testRoot, testWorkflows, "<attachment></attachment>", card))
	noSender := app(replyID(4), testRoot, testAppID, span)
	noSender["from"] = nil
	ct.f.putMessage(testChannel, noSender)
	if _, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); found || err != nil {
		t.Fatalf("Find = %t, %v; want not found: no message is the bot's", found, err)
	}
	ct.f.putMessage(testChannel, app(replyID(6), testRoot, testAppID, span))
	if got, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); !found || err != nil || got.ID != replyID(6) {
		t.Errorf("Find = %+v, %t, %v; want the bot's message %s", got, found, err, replyID(6))
	}
}

// The agent cannot write a carrier: a reply whose markdown holds the span of another post's tag, in prose, in a code span and in a
// code block, and a card whose texts hold it or the id, are not that post, so the outbox does not take one for the other.
func TestTheAgentCannotForgeACarrier(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	forged := `<span title="` + carrierOf(otherTag) + `"></span>`
	md := "Done. " + forged + "\n\n`" + forged + "`\n\n```\n" + forged + "\n```\n\n" + forged
	ref := ct.postAndEcho(t, target, chat.Say(chat.Message{Fallback: "x", Parts: []chat.Part{chat.Body{Markdown: md}}}), findTag)
	ct.postAndEcho(t, target, chat.Ask(chat.Prompt{ID: "p", Hint: `"id":"` + carrierOf(otherTag) + `"`,
		Questions: []chat.Question{{Text: forged, Choices: []string{carrierOf(otherTag)}}}}), "prompt-tag")
	if _, found, err := ct.c.Find(t.Context(), target, otherTag, time.Time{}); found || err != nil {
		t.Errorf("Find of the forged tag = %t, %v; want not found", found, err)
	}
	if got, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); !found || err != nil || got != ref {
		t.Errorf("Find of the post's own tag = %+v, %t, %v; want %+v", got, found, err, ref)
	}
}

// Find reads the raw HTML of the body, where the span is: the text that a person reads (htmlText) has no tags.
func TestFindReadsTheBodysHTMLNotItsText(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	carrier := carrierOf(findTag)
	ct.f.putMessage(testChannel, app(replyID(1), testRoot, testAppID, `<p>the reply</p><p><span title="`+carrier+`"></span></p>`))
	if (graphBody{ContentType: "html", Content: `<span title="` + carrier + `"></span>`}).text() != "" {
		t.Fatal("htmlText keeps the span, so this test cannot tell the HTML from the text")
	}
	if got, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); !found || err != nil || got.ID != replyID(1) {
		t.Errorf("Find = %+v, %t, %v", got, found, err)
	}
	// A body whose text is the span written as characters (escaped HTML, as Teams keeps the agent's "<") is no carrier.
	other, _ := threadTest(t)
	other.f.putMessage(testChannel, app(replyID(1), testRoot, testAppID, `<p>&lt;span title="`+carrier+`"&gt;&lt;/span&gt;</p>`))
	if _, found, err := other.c.Find(t.Context(), target, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find in escaped text = %t, %v; want not found", found, err)
	}
	// A body of the type text is not HTML, and holds no tag.
	plain, _ := threadTest(t)
	m := app(replyID(1), testRoot, testAppID, `<span title="`+carrier+`"></span>`)
	m["body"].(map[string]any)["contentType"] = "text"
	plain.f.putMessage(testChannel, m)
	if _, found, err := plain.c.Find(t.Context(), target, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find in a text body = %t, %v; want not found", found, err)
	}
}

// spanTitled reads tags, not text: a span is a "<span" that white space, a "/" or a ">" ends, whose title, quoted either way or bare
// and with its character references decoded, is the carrier, among other attributes, in any case.
func TestSpanTitledReadsTheTagsAttributes(t *testing.T) {
	t.Parallel()
	const known = "kourier0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		html string
		want bool
	}{
		{`<p><span title="` + known + `"></span></p>`, true},
		{`<span title='` + known + `'>`, true},
		{`<span title=` + known + `>`, true},
		{`<span title=` + known + `/>`, false}, // a bare value runs to the ">"
		{`<SPAN TITLE="` + known + `">`, true},
		{`<span style="display:none" title="` + known + `"></span>`, true},
		{`<span title="` + known + `" style="display:none">`, true},
		{"<span\ttitle=\"" + known + "\"/>", true},
		{`<span title="kourier&#48;123456789abcdef0123456789abcdef">`, true},
		{`<span title = "` + known + `">`, true},
		{`<span hidden title="` + known + `">`, true},
		{`<span data-title="` + known + `">`, false},
		{`<span xtitle="` + known + `">`, false},
		{`<span title="` + known + `x">`, false},
		{`<spanx title="` + known + `">`, false},
		{`<p title="` + known + `">`, false},
		{`<span title="` + known + `"`, false},
		{`<span title="` + known, false},
		{`&lt;span title="` + known + `"&gt;`, false},
		{`title="` + known + `"`, false},
		{`<span title>` + known, false},
		{`<span title="other"></span><span title="` + known + `">`, true},
		{`<span title="other"><b>` + known + `</b>`, false},
		{`<span`, false},
		{`<`, false},
		{``, false},
	} {
		if got := spanTitled(tc.html, known); got != tc.want {
			t.Errorf("spanTitled(%q) = %t, want %t", tc.html, got, tc.want)
		}
	}
	if spanTitled(`<span title="">`, "") {
		t.Error("an empty carrier matched")
	}
}

// A card carries the tag as the id of an element of its body, in the card's JSON, which Graph gives as a string: an id deeper in the
// card, a text that holds it, or a card that is not one, is not.
func TestCardIDedReadsTheCardsBody(t *testing.T) {
	t.Parallel()
	const known = "kourier0123456789abcdef0123456789abcdef"
	att := func(contentType, content string) graphAttached {
		raw, _ := json.Marshal(content)
		return graphAttached{ContentType: contentType, Content: raw}
	}
	for name, tc := range map[string]struct {
		atts []graphAttached
		want bool
	}{
		"the first element": {[]graphAttached{att(cardContentTypeIn, `{"body":[{"type":"TextBlock","id":"`+known+`"}]}`)}, true},
		"a later element":   {[]graphAttached{att(cardContentTypeIn, `{"body":[{"type":"TextBlock"},{"id":"`+known+`"}]}`)}, true},
		"a later card": {[]graphAttached{att(cardContentTypeIn, `{"body":[]}`),
			att(cardContentTypeIn, `{"body":[{"id":"`+known+`"}]}`)}, true},
		"in a text":        {[]graphAttached{att(cardContentTypeIn, `{"body":[{"type":"TextBlock","text":"\"id\":\"`+known+`\""}]}`)}, false},
		"nested":           {[]graphAttached{att(cardContentTypeIn, `{"body":[{"type":"Container","items":[{"id":"`+known+`"}]}]}`)}, false},
		"an action's id":   {[]graphAttached{att(cardContentTypeIn, `{"actions":[{"id":"`+known+`"}]}`)}, false},
		"another type":     {[]graphAttached{att("application/vnd.microsoft.card.hero", `{"body":[{"id":"`+known+`"}]}`)}, false},
		"not JSON":         {[]graphAttached{att(cardContentTypeIn, `{"body":[{"id":"`+known+`"`)}, false},
		"content not text": {[]graphAttached{{ContentType: cardContentTypeIn, Content: json.RawMessage(`{"body":[{"id":"` + known + `"}]}`)}}, false},
		"an odd element":   {[]graphAttached{att(cardContentTypeIn, `{"body":[1,"x",{"id":"`+known+`"}]}`)}, true},
		"a number id":      {[]graphAttached{att(cardContentTypeIn, `{"body":[{"id":7}]}`)}, false},
		"none":             {nil, false},
	} {
		if got := (graphMessage{Attachments: tc.atts}).cardIDed(known); got != tc.want {
			t.Errorf("%s: %t, want %t", name, got, tc.want)
		}
	}
	if (graphMessage{Attachments: []graphAttached{att(cardContentTypeIn, `{"body":[{"id":""}]}`)}}).cardIDed("") {
		t.Error("an empty carrier matched")
	}
}

// Find reads at most threadMaxPages pages of a thread's replies and findRootPages of a channel's messages, and stops at the page that
// holds the post. A post past the pages is not found, and the outbox posts it again: an error would hold the post back for good. A
// read past the reader's time is its deadline.
func TestFindReadsWithinTheReadersBounds(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.replies(2*threadMaxPages + 2)
	ct.f.setGraph(func(g *fakeGraph) { g.pageAt = 2 })
	if _, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find past the pages = %t, %v; want not found", found, err)
	}
	if n := len(ct.f.graphTaken()); n != threadMaxPages {
		t.Errorf("%d Graph calls, want %d", n, threadMaxPages)
	}
	// The fake gives the newest first: the bot's post, the newest, is on the first page.
	ct.f.putMessage(testChannel, app(replyID(1000), testRoot, testAppID, `<p><span title="`+carrierOf(findTag)+`"></span></p>`))
	if _, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); !found || err != nil {
		t.Errorf("Find on the first page = %t, %v", found, err)
	}
	if n := len(ct.f.graphTaken()); n != 1 {
		t.Errorf("%d Graph calls, want 1: the read stops at the page that holds the post", n)
	}

	roots, _ := threadTest(t)
	for k := 1; k <= 2*findRootPages+1; k++ {
		roots.f.putMessage(testChannel, person(replyID(k), "", testUser, "<p>a thread</p>"))
	}
	roots.f.setGraph(func(g *fakeGraph) { g.pageAt = 2 })
	channel := chat.Target{Conn: testConn, Channel: testChannel}
	if _, found, err := roots.c.Find(t.Context(), channel, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find past the roots' pages = %t, %v; want not found", found, err)
	}
	if n := len(roots.f.graphTaken()); n != findRootPages {
		t.Errorf("%d Graph calls for the roots, want %d", n, findRootPages)
	}

	held, heldTarget := threadTest(t)
	held.c.threadTimeout = 50 * time.Millisecond
	hold := make(chan struct{})
	t.Cleanup(func() { close(hold) })
	held.f.setGraph(func(g *fakeGraph) { g.hold = hold })
	heldCtx, cancelHeld := context.WithTimeout(t.Context(), 5*time.Second) // a Find that did not bound its read fails, and does not hang
	defer cancelHeld()
	start := time.Now()
	if _, found, err := held.c.Find(heldCtx, heldTarget, findTag, time.Time{}); found || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > 2*time.Second {
		t.Errorf("a held Find = %t, %v; want its deadline, which the outbox counts as a lookup that timed out", found, err)
	}
}

// What no retry mends is not found, so the outbox posts (the post may show twice) instead of taking the refusal for the post's own,
// which would abandon it: Graph's 400, 401, 403 and 404, Entra's refusal of the Graph token, and a channel whose team is not known
// (with no call). What another try may mend is the error, Transient, so the outbox looks again later: a 429 with its wait, a 5xx.
// No error is FatalAuth: a lookup never stops the connection.
func TestFindsFailuresAreClassified(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		code   string
		token  bool // the refusal is Entra's, of the Graph token
		err    bool
	}{
		{"bad request", http.StatusBadRequest, "BadRequest", false, false},
		{"unauthorized", http.StatusUnauthorized, "InvalidAuthenticationToken", false, false},
		{"forbidden", http.StatusForbidden, "Forbidden", false, false},
		{"not found", http.StatusNotFound, "NotFound", false, false},
		{"Graph token refused", http.StatusBadRequest, "", true, false},
		{"rate limited", http.StatusTooManyRequests, "TooManyRequests", false, true},
		{"server error", http.StatusBadGateway, "BadGateway", false, true},
		{"unavailable", http.StatusServiceUnavailable, "ServiceUnavailable", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct, target := threadTest(t)
			if tc.token {
				ct.f.setFail(func(r *http.Request) (int, string, bool) {
					_ = r.ParseForm()
					return tc.status, `{"error":"invalid_scope"}`, r.URL.Path == "/token" && r.PostForm.Get("scope") == graphScope
				})
			} else {
				ct.f.setGraph(func(g *fakeGraph) {
					g.fail = func(*http.Request) (int, string, bool) {
						return tc.status, `{"error":{"code":"` + tc.code + `"}}`, true
					}
				})
			}
			_, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{})
			if found || (err != nil) != tc.err {
				t.Fatalf("Find = %t, %v; want an error: %t", found, err, tc.err)
			}
			if !tc.err {
				return
			}
			v := ct.c.Classify(chat.OpPost, err)
			if v.Class != chat.Transient {
				t.Errorf("Classify = %+v, want Transient", v)
			}
			if tc.status == http.StatusTooManyRequests && v.RetryAfter != 4*time.Second {
				t.Errorf("RetryAfter = %v, want the 4s that Graph asked for", v.RetryAfter)
			}
		})
	}

	ct, target := threadTest(t)
	unknown := target
	unknown.Channel = "19:no-activity-yet@thread.tacv2"
	if _, found, err := ct.c.Find(t.Context(), unknown, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find in a channel whose team is not known = %t, %v; want not found", found, err)
	}
	for name, tgt := range map[string]chat.Target{"no channel": {Conn: testConn, Thread: testRoot},
		"a thread that is no message ID": {Conn: testConn, Channel: testChannel, Thread: "x/../y"}} {
		if _, found, err := ct.c.Find(t.Context(), tgt, findTag, time.Time{}); found || err != nil {
			t.Errorf("Find with %s = %t, %v; want not found", name, found, err)
		}
	}
	if _, found, err := ct.c.Find(t.Context(), target, "", time.Time{}); found || err != nil {
		t.Errorf("Find of no tag = %t, %v; want not found", found, err)
	}
	if n := len(ct.f.graphTaken()); n != 0 {
		t.Errorf("%d Graph calls, want none", n)
	}
}

// Each Find is counted as Find in agentkourier_chat_api_calls_total, by its outcome: a refusal that Find answers as not found is
// counted as what Graph said, so that a missing permission shows.
func TestAFindIsCounted(t *testing.T) {
	t.Parallel()
	o := newObserved(t)
	ct, target := threadTest(t)
	if _, _, err := ct.c.Find(o.ctx, target, findTag, time.Time{}); err != nil {
		t.Fatal(err)
	}
	ct.f.setGraph(func(g *fakeGraph) {
		g.fail = func(*http.Request) (int, string, bool) {
			return http.StatusForbidden, `{"error":{"code":"Forbidden"}}`, true
		}
	})
	if _, found, err := ct.c.Find(o.ctx, target, findTag, time.Time{}); found || err != nil {
		t.Fatalf("Find = %t, %v", found, err)
	}
	if got := o.value(t, "agentkourier_chat_api_calls_total", "success"); got != 1 {
		t.Errorf("success = %v, want 1", got)
	}
	if got := o.value(t, "agentkourier_chat_api_calls_total", "api_error"); got != 1 {
		t.Errorf("api_error = %v, want 1 (Graph's 403, answered as not found)", got)
	}
}

// withCarrier puts the carrier on the first element of a card's body that has no id, and never in place of an id that an element
// has: an input's id is how its answer comes back. A card whose every element has an id carries none.
func TestWithCarrierKeepsTheIDsAnElementHas(t *testing.T) {
	t.Parallel()
	card := func(body ...any) outActivity {
		return cardActivity(map[string]any{"type": "AdaptiveCard", "body": body})
	}
	input := func() map[string]any { return map[string]any{"type": "Input.Text", "id": "t0"} }
	text := func() map[string]any { return map[string]any{"type": "TextBlock", "text": "x"} }
	carrier := carrierOf(findTag)
	a := withCarrier(card(input(), text(), text()), carrier)
	body := a.Attachments[0].Content["body"].([]any)
	if body[0].(map[string]any)["id"] != "t0" || body[1].(map[string]any)["id"] != carrier || body[2].(map[string]any)["id"] != nil {
		t.Errorf("body = %v, want the input's id kept and the carrier on the first text alone", body)
	}
	a = withCarrier(card(input()), carrier)
	if got := a.Attachments[0].Content["body"].([]any)[0].(map[string]any)["id"]; got != "t0" {
		t.Errorf("a card of inputs alone: the input's id is %v", got)
	}
}

// A message with a Body and buttons, as the useful and not-useful buttons go under a reply, carries the tag in its text alone: the
// buttons are a card that Teams shows as a message of its own (measured, round 3 of the probe), which must not be taken for the
// post. Find finds the text.
func TestATextPostWithButtonsCarriesItsTagInTheText(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	o := chat.Say(chat.Message{Fallback: "the answer", Parts: []chat.Part{
		chat.Body{Markdown: "It is the disk."},
		chat.Context{Text: "Was this useful?"},
		chat.Buttons{Buttons: []chat.Button{
			{ID: "feedback-useful", Label: "Useful", Value: "fb1|investigation|useful|100.1|turn-1"},
			{ID: "feedback-not-useful", Label: "Not useful", Value: "fb1|investigation|not_useful|100.1|turn-1"},
		}},
	}})
	ref, err := ct.c.Post(t.Context(), target, o, findTag)
	if err != nil {
		t.Fatal(err)
	}
	a := ct.posted(t)
	if !strings.HasSuffix(a.Text, "\n\n"+`<span title="`+carrierOf(findTag)+`"></span>`) {
		t.Errorf("text = %q, want the carrier at its end", a.Text)
	}
	if len(a.Attachments) != 1 {
		t.Fatalf("attachments = %+v, want the buttons' card", a.Attachments)
	}
	if raw := mustJSON(t, a.Attachments[0].Content); strings.Contains(raw, `"id"`) || strings.Contains(raw, carrierOf(findTag)) {
		t.Errorf("the buttons' card = %s, want no id and no carrier", raw)
	}
	ct.echo(t, ref.ID, testRoot, testAppID, a)
	if got, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); !found || err != nil || got != ref {
		t.Errorf("Find = %+v, %t, %v; want %+v", got, found, err, ref)
	}
}

// A message whose ID is not a Teams message ID is no post, even of the bot's own app with the carrier: no Ref can be made of it.
func TestFindTakesNoMessageWhoseIDIsNoMessageID(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ct.f.putMessage(testChannel, app("abc", testRoot, testAppID, `<p><span title="`+carrierOf(findTag)+`"></span></p>`))
	if got, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find = %+v, %t, %v; want not found", got, found, err)
	}
}

// What Graph gives that no retry reads otherwise is not found, so that the post is sent and not held until it expires: a next page
// that names one already read, a next page that is not Graph's, and an answer past graphMaxBody. An answer that is not JSON may be
// a fault of the moment, and is the error.
func TestFindTakesAGraphAnomalyAsNotFound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		edit func(g *fakeGraph)
		err  bool
	}{
		{"a cycle", func(g *fakeGraph) {
			g.pageAt, g.next = 1, func(self string, _ int) string { return self + "?$top=50&$skiptoken=0" }
		}, false},
		{"a page off Graph", func(g *fakeGraph) {
			g.pageAt, g.next = 1, func(string, int) string { return "https://graph.example.test/v1.0/teams/x/replies?$skiptoken=1" }
		}, false},
		{"an answer too long", func(g *fakeGraph) { g.big = graphMaxBody }, false},
		{"not JSON", func(g *fakeGraph) {
			g.fail = func(*http.Request) (int, string, bool) { return http.StatusOK, "not JSON", true }
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct, target := threadTest(t)
			ct.replies(3)
			ct.f.setGraph(tc.edit)
			_, found, err := ct.c.Find(t.Context(), target, findTag, time.Time{})
			if found || (err != nil) != tc.err {
				t.Fatalf("Find = %t, %v; want an error: %t", found, err, tc.err)
			}
			if tc.err && ct.c.Classify(chat.OpPost, err).Class != chat.Transient {
				t.Errorf("Classify(%v) is not Transient", err)
			}
		})
	}
}

// A read that runs out of its own time after Graph gave a page is a thread too long to read in time, every time, and is not found (the
// post is sent): a lookup that timed out each try would have the outbox abandon a post that was never sent. One that runs out before
// any page came back is the deadline, which the outbox tries again (TestFindReadsWithinTheReadersBounds), and so is one whose caller's
// own time ran out first, after a page: that says nothing of the thread.
func TestFindCutAfterAPageIsNotFound(t *testing.T) {
	t.Parallel()
	stall := func(ct *clientTest) {
		ct.replies(3)
		ct.f.setGraph(func(g *fakeGraph) {
			g.pageAt = 1
			g.fail = func(r *http.Request) (int, string, bool) {
				if r.URL.Query().Get("$skiptoken") != "" {
					<-r.Context().Done() // the second page never comes
				}
				return 0, "", false
			}
		})
	}

	ct, target := threadTest(t)
	stall(ct)
	ct.c.threadTimeout = 200 * time.Millisecond
	// The caller's bound is far past the read's, so that a Find that did not bound its read fails here, and does not hang.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, found, err := ct.c.Find(ctx, target, findTag, time.Time{}); found || err != nil {
		t.Errorf("Find cut by its own time after a page = %t, %v; want not found", found, err)
	}
	if n := len(ct.f.graphTaken()); n != 2 {
		t.Errorf("%d Graph calls, want 2: the first page, and the second that never came", n)
	}

	caller, callerTarget := threadTest(t)
	stall(caller)
	caller.c.threadTimeout = 5 * time.Second
	short, cancelShort := context.WithTimeout(t.Context(), 200*time.Millisecond)
	defer cancelShort()
	if _, found, err := caller.c.Find(short, callerTarget, findTag, time.Time{}); found || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Find cut by the caller's time after a page = %t, %v; want the caller's deadline", found, err)
	}
}
