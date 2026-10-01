package slack_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

var ids = []string{"a", "b", "c", "d", "e"}

// postAll posts one message per id and returns their ts by id.
func postAll(t *testing.T, c *slack.Client, thread string) map[string]string {
	t.Helper()
	ts := map[string]string{}
	for _, id := range ids {
		var err error
		if ts[id], err = c.Post(t.Context(), "C1", thread, slack.Content{Text: "m " + id}, id); err != nil {
			t.Fatal(err)
		}
	}
	return ts
}

// TestFindReadsEveryPage has pages of two. The fake pages newest first, so the first message
// posted is on the last page and the last on the first: a Find that stops early misses one.
func TestFindReadsEveryPage(t *testing.T) {
	_, c := newPoster(t)
	root, err := c.Post(t.Context(), "C1", "", slack.Content{Text: "root"}, "root")
	if err != nil {
		t.Fatal(err)
	}
	for _, thread := range []string{"", root} { // channel history, then a thread's replies
		ts := postAll(t, c, thread)
		for _, id := range ids {
			if got, found, err := c.Find(t.Context(), "C1", thread, id, time.Now()); err != nil || !found || got != ts[id] {
				t.Errorf("Find(thread %q, %s) = %q, %v, %v; want %s", thread, id, got, found, err, ts[id])
			}
		}
		if got, found, err := c.Find(t.Context(), "C1", thread, "zzz", time.Now()); err != nil || found {
			t.Errorf("Find(thread %q, zzz) = %q, %v, %v; want not found, no error", thread, got, found, err)
		}
	}
}

// TestFindWindow checks that the lookup starts five minutes before since, to allow for clock
// skew, and no earlier.
func TestFindWindow(t *testing.T) {
	posted := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	_, c := newPoster(t, fakeslack.WithClock(func() time.Time { return posted }))
	if _, err := c.Post(t.Context(), "C1", "", slack.Content{Text: "hi"}, "a"); err != nil {
		t.Fatal(err)
	}
	for since, want := range map[time.Duration]bool{4 * time.Minute: true, 6 * time.Minute: false} {
		if _, found, err := c.Find(t.Context(), "C1", "", "a", posted.Add(since)); err != nil || found != want {
			t.Errorf("Find with since %v after the post = %v, %v; want found = %v", since, found, err, want)
		}
	}
}

// pg is one page of a stubbed conversations.history.
type pg struct {
	msgs    []map[string]any
	next    string
	hasMore bool
}

// tagged is a message with courier's metadata naming id, posted by user.
func tagged(user, id, ts string) map[string]any {
	return map[string]any{"type": "message", "user": user, "ts": ts, "text": "x",
		"metadata": map[string]any{"event_type": slack.MetadataEventType, "event_payload": map[string]any{"outbox_id": id}}}
}

// stubHistory serves pages of conversations.history by cursor, and auth.test for a bot UBOT.
// It returns a Client over it and a function giving the cursors asked for, in order.
func stubHistory(t *testing.T, pages map[string]pg) (*slack.Client, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		body := map[string]any{"ok": true, "user_id": "UBOT"}
		if strings.HasSuffix(r.URL.Path, "/conversations.history") {
			if r.Form.Get("include_all_metadata") != "1" {
				t.Errorf("include_all_metadata = %q; metadata is hidden without it", r.Form.Get("include_all_metadata"))
			}
			mu.Lock()
			asked = append(asked, r.Form.Get("cursor"))
			mu.Unlock()
			p := pages[r.Form.Get("cursor")]
			body = map[string]any{"ok": true, "messages": p.msgs, "has_more": p.hasMore, "response_metadata": map[string]any{"next_cursor": p.next}}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	cfg := clientConfig(newFake(t))
	cfg.APIURL = srv.URL + "/"
	return slack.New(cfg), func() []string { mu.Lock(); defer mu.Unlock(); return slices.Clone(asked) }
}

// TestFindWalksToTheEnd serves pages that put the match last, and a forgery first: Find must
// read every page whatever order they come in, and take only courier's own message.
func TestFindWalksToTheEnd(t *testing.T) {
	pages := map[string]pg{
		"":   {msgs: []map[string]any{tagged("UOTHER", "t", "1.1")}, next: "c1", hasMore: true},
		"c1": {msgs: []map[string]any{tagged("UBOT", "x", "1.2")}, next: "c2", hasMore: true},
		"c2": {msgs: []map[string]any{tagged("UBOT", "y", "1.3"), tagged("UBOT", "t", "1.4")}},
	}
	tests := []struct {
		name, id string
		pages    map[string]pg
		ts       string
		found    bool
		wantErr  string
		asked    []string
	}{
		{"match on the last page, forgery first", "t", pages, "1.4", true, "", []string{"", "c1", "c2"}},
		{"no match reads to the end", "q", pages, "", false, "", []string{"", "c1", "c2"}},
		{"more pages promised, no cursor", "t", map[string]pg{"": {hasMore: true}}, "", false, "no next cursor", []string{""}},
		{"a cursor repeated", "t", map[string]pg{"": {next: "c1", hasMore: true}, "c1": {next: "c1", hasMore: true}}, "", false, "repeats", []string{"", "c1"}},
	}
	for _, tt := range tests {
		c, asked := stubHistory(t, tt.pages)
		ts, found, err := c.Find(t.Context(), "C1", "", tt.id, time.Now())
		if ts != tt.ts || found != tt.found || (err == nil) != (tt.wantErr == "") || (err != nil && !strings.Contains(err.Error(), tt.wantErr)) {
			t.Errorf("%s: Find = %q, %v, %v; want %q, %v, error %q", tt.name, ts, found, err, tt.ts, tt.found, tt.wantErr)
		}
		if got := asked(); !slices.Equal(got, tt.asked) {
			t.Errorf("%s: asked for cursors %q; want %q", tt.name, got, tt.asked)
		}
	}
}
