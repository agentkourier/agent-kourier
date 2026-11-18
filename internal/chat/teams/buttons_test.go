package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

// feedbackButtons are the useful and not-useful buttons, as feedback.Parts makes them.
func feedbackButtons() chat.Buttons {
	return chat.Buttons{Buttons: []chat.Button{
		{ID: "feedback-useful", Label: "Useful", Value: "fb1|investigation|useful|100.1|turn-1"},
		{ID: "feedback-not-useful", Label: "Not useful", Value: "fb1|investigation|not_useful|100.1|turn-1"},
	}}
}

// rated is a final answer with its feedback footer: text, then the buttons.
func rated(body string) chat.Outgoing {
	return chat.Say(chat.Message{Fallback: "the answer", Parts: []chat.Part{
		chat.Body{Markdown: body}, chat.Context{Text: "Was this useful?"}, feedbackButtons(),
	}})
}

// activityCalls are the calls of reqs that post or edit an activity, in order.
func activityCalls(reqs []connRequest) []connRequest {
	var out []connRequest
	for _, r := range reqs {
		if r.method == http.MethodPost || r.method == http.MethodPut {
			out = append(out, r)
		}
	}
	return out
}

// threadPath is the path of the activities of testChannel's thread root, or of its activity id when id is set.
func threadPath(root, id string) string {
	return "/amer/" + testTenant + "/" + activityPath(conversation(testChannel, root), id)
}

// actionData is Agent Kourier's own data of each action of the one card of a, in order.
func actionData(t *testing.T, a outActivity) []map[string]any {
	t.Helper()
	actions, _ := cardOf(t, a)["actions"].([]any)
	var out []map[string]any
	for _, act := range actions {
		data, _ := act.(map[string]any)["data"].(map[string]any)
		own, _ := data[dataKey].(map[string]any)
		out = append(out, own)
	}
	return out
}

// buttonsCalls is the count of PostButtons in agentkourier_chat_api_calls_total with the outcome, on the test connection.
func buttonsCalls(t *testing.T, o observed, outcome string) float64 {
	t.Helper()
	mfs, err := o.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() != "agentkourier_chat_api_calls_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["operation"] == "PostButtons" && labels["outcome"] == outcome && labels[telemetry.ConnectionLabel] == testConn {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// A message with text and buttons is two posts to the thread, in order: the text, with no attachment and with the tag's carrier, whose
// ID is the post's Ref; then the buttons, as a card alone with no tag, each action naming the text by its ID. Teams answers one
// activity of text and a card 202 with no ID and shows it as two messages (measured, round 3 of the carrier probe): each of the two
// posts is one of those messages, with its own ID.
func TestATextWithButtonsIsTwoPosts(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ref, err := ct.c.Post(t.Context(), target, rated("It is the disk."), findTag)
	if err != nil {
		t.Fatal(err)
	}
	calls := activityCalls(ct.f.take())
	if len(calls) != 2 {
		t.Fatalf("%d calls, want the text and then the buttons: %+v", len(calls), calls)
	}
	text, card := calls[0], calls[1]
	if text.method != http.MethodPost || text.path != threadPath(testRoot, "") || card.method != http.MethodPost || card.path != threadPath(testRoot, "") {
		t.Errorf("calls %s %s and %s %s, want two posts to %s", text.method, text.path, card.method, card.path, threadPath(testRoot, ""))
	}
	if len(text.activity.Attachments) != 0 || !strings.HasPrefix(text.activity.Text, "It is the disk.\n\nWas this useful?") ||
		!strings.HasSuffix(text.activity.Text, `<span title="`+carrierOf(findTag)+`"></span>`) || text.activity.TextFormat != "markdown" {
		t.Errorf("the text's post = %+v, want the text with its carrier and no attachment", text.activity)
	}
	if card.activity.Text != "" || card.activity.TextFormat != "" || len(card.activity.Entities) != 0 {
		t.Errorf("the buttons' post = %+v, want a card alone", card.activity)
	}
	if raw := mustJSON(t, card.activity.Attachments); strings.Contains(raw, `"id"`) || strings.Contains(raw, carrierOf(findTag)) {
		t.Errorf("the buttons' card = %s, want no id and no carrier", raw)
	}
	if ref != ct.c.RefAt(testChannel, testRoot, "1700000000001") {
		t.Errorf("Ref = %+v, want the text's, the first post's ID", ref)
	}
	data := actionData(t, card.activity)
	if len(data) != 2 {
		t.Fatalf("the card has %d actions, want 2", len(data))
	}
	for i, d := range data {
		want := map[string]any{"button": feedbackButtons().Buttons[i].ID, "value": feedbackButtons().Buttons[i].Value, "of": ref.ID}
		if mustJSON(t, d) != mustJSON(t, want) {
			t.Errorf("action %d's data = %v, want %v", i, d, want)
		}
	}
}

// The buttons of a post that opens a thread go into that thread, under the text, as every post of buttons is a reply in the text's
// thread.
func TestTheButtonsOfARootGoIntoItsThread(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	target.Thread = ""
	ref, err := ct.c.Post(t.Context(), target, rated("It is the disk."), findTag)
	if err != nil {
		t.Fatal(err)
	}
	calls := activityCalls(ct.f.take())
	if len(calls) != 2 || calls[0].path != threadPath("", "") || calls[1].path != threadPath(ref.ID, "") {
		t.Fatalf("calls %+v, want the text to the channel and the buttons to the text's thread %s", calls, threadPath(ref.ID, ""))
	}
	if ref.Thread != "" {
		t.Errorf("Ref = %+v, want the root's", ref)
	}
}

// A message of buttons and no text is one post, a card alone, as a question's card is: Teams answered such posts 201 with an ID (a
// prompt's and a tool approval's card, measured). Its ID is the Ref, and a press names that card itself, so its actions name no text.
// A message of buttons and a fallback is the fallback's text, then the buttons, as any text with buttons. A text alone is one post.
func TestButtonsAloneAreOneCardAndATextAloneOnePost(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ref, err := ct.c.Post(t.Context(), target, chat.Say(chat.Message{Parts: []chat.Part{feedbackButtons()}}), findTag)
	if err != nil {
		t.Fatal(err)
	}
	a := ct.posted(t)
	if a.Text != "" || ref != ct.c.RefAt(testChannel, testRoot, "1700000000001") {
		t.Errorf("buttons alone: %+v as %+v, want a card alone", a, ref)
	}
	for i, d := range actionData(t, a) {
		if _, named := d["of"]; named {
			t.Errorf("action %d of a card alone names a text: %v", i, d)
		}
	}

	withFallback := chat.Say(chat.Message{Fallback: "Rate it", Parts: []chat.Part{feedbackButtons()}})
	if _, err := ct.c.Post(t.Context(), target, withFallback, findTag); err != nil {
		t.Fatal(err)
	}
	calls := activityCalls(ct.f.take())
	if len(calls) != 2 || !strings.HasPrefix(calls[0].activity.Text, "Rate it") || len(calls[0].activity.Attachments) != 0 ||
		calls[1].activity.Text != "" || len(calls[1].activity.Attachments) != 1 {
		t.Errorf("buttons with a fallback: %+v, want the fallback's text, then the card", calls)
	}

	if _, err := ct.c.Post(t.Context(), target, richReply(), findTag); err != nil {
		t.Fatal(err)
	}
	if a := ct.posted(t); len(a.Attachments) != 0 || a.Text == "" {
		t.Errorf("a text alone: %+v", a)
	}
}

// A post of the buttons that fails leaves the text posted and its Ref returned, with no error: an error would have the outbox look the
// post up, adopt the text, and post nothing more, and the text is never posted twice. The failure is counted as PostButtons. The next
// update of the message that has the buttons posts them, once.
func TestAFailedButtonsPostLeavesTheText(t *testing.T) {
	t.Parallel()
	o := newObserved(t)
	ct, target := threadTest(t)
	var posts atomic.Int32
	ct.f.setFail(func(r *http.Request) (int, string, bool) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/activities") && posts.Add(1) == 2 {
			return http.StatusTooManyRequests, `{"error":{"code":"Throttled"}}`, true
		}
		return 0, "", false
	})
	ref, err := ct.c.Post(o.ctx, target, rated("It is the disk."), findTag)
	if err != nil || ref != ct.c.RefAt(testChannel, testRoot, "1700000000001") {
		t.Fatalf("Post = %+v, %v; want the text's Ref and no error", ref, err)
	}
	if calls := activityCalls(ct.f.take()); len(calls) != 2 || len(calls[0].activity.Attachments) != 0 || len(calls[1].activity.Attachments) != 1 {
		t.Fatalf("calls %+v, want the text once and the buttons tried once", calls)
	}
	if got := buttonsCalls(t, o, "rate_limited"); got != 1 {
		t.Errorf("PostButtons rate_limited = %v, want 1", got)
	}

	if err := ct.c.Update(o.ctx, ref, rated("It is the disk.")); err != nil {
		t.Fatal(err)
	}
	calls := activityCalls(ct.f.take())
	if len(calls) != 2 || calls[0].method != http.MethodPut || calls[0].path != threadPath(testRoot, ref.ID) ||
		calls[1].method != http.MethodPost || calls[1].path != threadPath(testRoot, "") {
		t.Fatalf("the update: %+v, want the text put and the buttons posted", calls)
	}
	if err := ct.c.Update(o.ctx, ref, rated("It is the disk.")); err != nil {
		t.Fatal(err)
	}
	if calls := activityCalls(ct.f.take()); len(calls) != 1 || calls[0].method != http.MethodPut || calls[0].path != threadPath(testRoot, ref.ID) {
		t.Errorf("the update after: %+v, want the text put alone", calls)
	}
	if got := buttonsCalls(t, o, "success"); got != 1 {
		t.Errorf("PostButtons success = %v, want 1", got)
	}
}

// An update of a message with buttons edits the text, with no attachment, and shows the buttons under it once: the first update that
// has them posts them to the text's thread (a live reply gets its buttons on its last edit), an update with the same buttons makes no
// call for them, and one with other buttons edits the card it posted. An update after a post that posted the buttons posts none.
func TestAnUpdateShowsTheButtonsOnce(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	text := ct.c.RefAt(testChannel, testRoot, "1794965001000")
	if err := ct.c.Update(t.Context(), text, rated("It is the disk.")); err != nil {
		t.Fatal(err)
	}
	calls := activityCalls(ct.f.take())
	if len(calls) != 2 || calls[0].method != http.MethodPut || calls[0].path != threadPath(testRoot, text.ID) || len(calls[0].activity.Attachments) != 0 ||
		calls[1].method != http.MethodPost || calls[1].path != threadPath(testRoot, "") || calls[1].activity.Text != "" {
		t.Fatalf("the first update: %+v, want the text put and the buttons posted", calls)
	}
	if of := actionData(t, calls[1].activity)[0]["of"]; of != text.ID {
		t.Errorf("the card names %v, want the text %s", of, text.ID)
	}
	card := "1700000000002" // the fake's ID of the second call
	if err := ct.c.Update(t.Context(), text, rated("It is the disk, and the logs.")); err != nil {
		t.Fatal(err)
	}
	if calls := activityCalls(ct.f.take()); len(calls) != 1 || calls[0].method != http.MethodPut || calls[0].path != threadPath(testRoot, text.ID) {
		t.Errorf("an update with the same buttons: %+v, want the text put alone", calls)
	}
	other := chat.Say(chat.Message{Parts: []chat.Part{chat.Body{Markdown: "x"}, chat.Buttons{Buttons: []chat.Button{{ID: "b", Label: "B", Value: "v"}}}}})
	if err := ct.c.Update(t.Context(), text, other); err != nil {
		t.Fatal(err)
	}
	calls = activityCalls(ct.f.take())
	if len(calls) != 2 || calls[1].method != http.MethodPut || calls[1].path != threadPath(testRoot, card) {
		t.Errorf("an update with other buttons: %+v, want the text put and the card %s put", calls, card)
	}

	ref, err := ct.c.Post(t.Context(), target, rated("It is the disk."), findTag)
	if err != nil {
		t.Fatal(err)
	}
	ct.f.take()
	if err := ct.c.Update(t.Context(), ref, rated("It is the disk.")); err != nil {
		t.Fatal(err)
	}
	if calls := activityCalls(ct.f.take()); len(calls) != 1 || calls[0].method != http.MethodPut || calls[0].path != threadPath(testRoot, ref.ID) {
		t.Errorf("an update after the post: %+v, want the text put alone", calls)
	}
}

// An update whose buttons cannot be posted fails, as Classify says of the post's error, after the text was edited: the caller tries the
// update again, which edits the text to the same content and posts the buttons. One whose buttons' post Teams took with no ID is done:
// the buttons are shown, and a later update with them posts no more.
func TestAnUpdateWhoseButtonsFailFails(t *testing.T) {
	t.Parallel()
	ct, _ := threadTest(t)
	text := ct.c.RefAt(testChannel, testRoot, "1794965001000")
	var answer atomic.Int32 // 0 the fake's own, 1 a 503, 2 a 202 with no body
	ct.f.setFail(func(r *http.Request) (int, string, bool) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/activities") {
			return 0, "", false
		}
		switch answer.Load() {
		case 1:
			return http.StatusServiceUnavailable, "", true
		case 2:
			return http.StatusAccepted, "", true
		}
		return 0, "", false
	})
	answer.Store(1)
	err := ct.c.Update(t.Context(), text, rated("It is the disk."))
	if err == nil || ct.c.Classify(chat.OpPost, err).Class != chat.Transient {
		t.Fatalf("Update = %v, want a Transient error", err)
	}
	if calls := activityCalls(ct.f.take()); len(calls) != 2 || calls[0].method != http.MethodPut {
		t.Fatalf("calls %+v, want the text put and the buttons tried", calls)
	}
	answer.Store(2)
	if err := ct.c.Update(t.Context(), text, rated("It is the disk.")); err != nil {
		t.Fatalf("an update whose buttons Teams took with no ID: %v", err)
	}
	ct.f.take()
	answer.Store(0)
	if err := ct.c.Update(t.Context(), text, rated("It is the disk.")); err != nil {
		t.Fatal(err)
	}
	if calls := activityCalls(ct.f.take()); len(calls) != 1 || calls[0].method != http.MethodPut {
		t.Errorf("an update after the buttons were taken with no ID: %+v, want the text put alone", calls)
	}
}

// A press of a card of buttons under a text names the text, the message that Post returned and the outbox recorded (the feedback
// buttons check that a press is on the turn's message), in the thread of the card, and keeps the key the invoke makes. A card that
// names no text is named by the press itself; a text that is no activity ID, and a prompt's button that names a text, make no press.
func TestAPressOfButtonsUnderATextNamesTheText(t *testing.T) {
	t.Parallel()
	c := newClientTest(t).c
	button := func(of any) map[string]any {
		own := map[string]any{"button": "useful", "value": "fb1|x"}
		if of != nil {
			own["of"] = of
		}
		return map[string]any{dataKey: own}
	}
	inv := pressedAt(button("1790000000150"))
	got, kind, ok := c.press(inv)
	if !ok || kind != pressButton || got.At != c.RefAt(testChannel, "1790000000100", "1790000000150") || got.PromptID != "fb1|x" {
		t.Errorf("a press under a reply: %+v, %v, %v; want the text in the card's thread", got, kind, ok)
	}
	if got.Delivery != pressKey(inv) {
		t.Errorf("the key = %q, want the invoke's %q", got.Delivery, pressKey(inv))
	}
	if got, _, ok := c.press(pressedAt(button("1790000000100"))); !ok || got.At != c.RefAt(testChannel, "", "1790000000100") {
		t.Errorf("a press under a root: %+v, %v; want the root", got.At, ok)
	}
	if got, _, ok := c.press(pressedAt(button(nil))); !ok || got.At != c.RefAt(testChannel, "1790000000100", "1790000000200") {
		t.Errorf("a press of a card alone: %+v, %v; want the card", got.At, ok)
	}
	prompt := promptData("submit")
	prompt[dataKey].(map[string]any)["of"] = "1790000000150"
	for name, data := range map[string]map[string]any{
		"an empty text":         button(""),
		"a text with a ':'":     button("1:1790000000150"),
		"a text too long":       button(strings.Repeat("1", maxActivityID+1)),
		"a text not a string":   button(1790000000150),
		"a prompt naming texts": prompt,
	} {
		if got, _, ok := c.press(pressedAt(data)); ok {
			t.Errorf("%s: taken as %+v", name, got)
		}
	}
}

// What Post sends comes back as one press: the card's own action, pressed in the card's message, is a press of the text that Post
// returned. Answering it makes no call: what the presser is told is the invoke's answer, which Teams shows under the card that was
// pressed, and nothing edits the text or the card.
func TestThePressOfAPostedCardIsOfThePostsRef(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ref, err := ct.c.Post(t.Context(), target, rated("It is the disk."), findTag)
	if err != nil {
		t.Fatal(err)
	}
	calls := activityCalls(ct.f.take())
	if len(calls) != 2 {
		t.Fatalf("%d calls, want 2", len(calls))
	}
	actions, _ := cardOf(t, calls[1].activity)["actions"].([]any)
	data := actions[0].(map[string]any)["data"]
	inv := Invoke{Name: actionInvoke, ID: "f:00000000-0000-4000-8000-0000000000c5", User: testUser,
		At: ct.c.RefAt(testChannel, testRoot, "1700000000002")} // the card's message, which the invoke's replyToId names
	inv.Value, _ = json.Marshal(map[string]any{"action": map[string]any{"type": "Action.Execute", "data": data}})
	sink := &pressSink{}
	ans := ct.c.Invoke(t.Context(), sink, inv)
	if presses := sink.taken(); len(presses) != 1 || presses[0].At != ref {
		t.Fatalf("the press is %+v, want one of %+v", presses, ref)
	}
	if !strings.Contains(string(ans.Body), textButtonReceived) {
		t.Errorf("the answer %s, want the presser told", ans.Body)
	}
	if calls := ct.f.take(); len(calls) != 0 {
		t.Errorf("the press made calls: %+v", calls)
	}
}

// The text and the card are two activities, each within the budget of a message: the longest reply is cut to its own budget beside any
// buttons, and not further for them; a card of buttons over the budget, which no cut of the text makes smaller, is refused before
// anything is posted.
func TestTheTextAndTheButtonsEachHaveTheBudget(t *testing.T) {
	t.Parallel()
	body := strings.Repeat("中", maxBodyUnits)
	m, err := render(chat.Say(chat.Message{Parts: []chat.Part{chat.Body{Markdown: body}, feedbackButtons()}}))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(m.activity.Text, body) || len(m.activity.Attachments) != 0 || len(m.buttons) != 2 {
		t.Errorf("the longest reply beside its buttons: %d units, %d attachments, %d buttons; want it whole, apart from them",
			chat.Units(m.activity.Text), len(m.activity.Attachments), len(m.buttons))
	}

	var many []chat.Button
	for i := range 40 {
		many = append(many, chat.Button{ID: fmt.Sprintf("b%d", i), Label: strings.Repeat("中", maxTextUnits), Value: "v"})
	}
	big := chat.Say(chat.Message{Parts: []chat.Part{chat.Body{Markdown: "x"}, chat.Buttons{Buttons: many}}})
	if _, err := render(big); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("a card of buttons over the budget: %v, want ErrTooLarge", err)
	}
	ct, target := threadTest(t)
	if _, err := ct.c.Post(t.Context(), target, big, findTag); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("Post of it: %v, want ErrTooLarge", err)
	}
	if calls := activityCalls(ct.f.take()); len(calls) != 0 {
		t.Errorf("%d calls for a message that is too large", len(calls))
	}
}

// The card of buttons under a text is sized as it names a text of the longest ID: buttons that fit with no ID and not with that one
// are refused, though the card that a post sends would name a shorter ID.
func TestTheButtonsAreSizedWithTheLongestTextID(t *testing.T) {
	t.Parallel()
	longest := strings.Repeat("9", maxActivityID)
	var edge []chat.Button
	for value := 1000; value < 1200 && edge == nil; value++ {
		var bs []chat.Button
		for i := 0; ; i++ {
			bs = append(bs, chat.Button{ID: fmt.Sprintf("b%d", i), Label: "Useful", Value: strings.Repeat("v", value)})
			if n, _ := size(buttonsActivity(bs, "")); n > maxMessageBytes {
				bs = bs[:len(bs)-1]
				break
			}
		}
		if n, _ := size(buttonsActivity(bs, longest)); n > maxMessageBytes {
			edge = bs
		}
	}
	if edge == nil {
		t.Fatal("no buttons fit with no ID and not with the longest")
	}
	if _, err := render(chat.Say(chat.Message{Parts: []chat.Part{chat.Body{Markdown: "x"}, chat.Buttons{Buttons: edge}}})); !errors.Is(err, chat.ErrTooLarge) {
		t.Errorf("buttons over the budget once they name the longest ID: %v, want ErrTooLarge", err)
	}
}

// The cards of buttons a Client remembers are bounded: past maxButtonCards the oldest is forgotten, and a card remembered again is
// replaced in its place.
func TestTheButtonCardsAreBounded(t *testing.T) {
	t.Parallel()
	cards := newButtonCards()
	for i := range maxButtonCards + 1 {
		cards.put(fmt.Sprint(i), buttonCard{id: fmt.Sprint(i), sum: "s"})
	}
	if _, ok := cards.get("0"); ok {
		t.Error("the oldest card is still remembered")
	}
	for _, k := range []string{"1", fmt.Sprint(maxButtonCards)} {
		if c, ok := cards.get(k); !ok || c.id != k {
			t.Errorf("card %s = %+v, %v", k, c, ok)
		}
	}
	cards.put("1", buttonCard{id: "x", sum: "t"})
	if c, _ := cards.get("1"); c.id != "x" || len(cards.order) != maxButtonCards || !slices.Contains(cards.order, "1") {
		t.Errorf("card 1 = %+v with %d remembered", c, len(cards.order))
	}
}

// Find still adopts the text of a message with buttons, which carries the tag; the card, which carries none, is not taken for it.
func TestFindAdoptsTheTextOfAMessageWithButtons(t *testing.T) {
	t.Parallel()
	ct, target := threadTest(t)
	ref, err := ct.c.Post(t.Context(), target, rated("It is the disk."), findTag)
	if err != nil {
		t.Fatal(err)
	}
	calls := activityCalls(ct.f.take())
	ct.echo(t, "1700000000002", testRoot, testAppID, calls[1].activity) // Graph listed the card before the text in round 3
	ct.echo(t, ref.ID, testRoot, testAppID, calls[0].activity)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if got, found, err := ct.c.Find(ctx, target, findTag, time.Time{}); !found || err != nil || got != ref {
		t.Errorf("Find = %+v, %t, %v; want %+v", got, found, err, ref)
	}
}
