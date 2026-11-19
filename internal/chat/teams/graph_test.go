package teams

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

// graphPrefix is the path of the fake's Microsoft Graph, as Config.GraphURL names it.
const graphPrefix = "/v1.0/"

// testWorkflows is a stand-in for the application ID of the app that posts an alert's card (Microsoft's Workflows on a real tenant):
// another app's, not the bot's.
const testWorkflows = "44444444-5555-4666-8777-888888888888"

// graphReq is one call that the fake Graph took.
type graphReq struct {
	path  string // decoded
	query string // as it came on the wire
	auth  string
}

// fakeGraph is the part of Microsoft Graph that the Client reads: a channel message (GET …/messages/{id}), its replies (GET
// …/messages/{id}/replies) and the channel's messages that open a thread (GET …/messages), in pages of $top, newest first, as
// Microsoft's example of the replies shows them, each page naming the next by an @odata.nextLink with a $skiptoken. The fields are the fakeConnector's, guarded by its mu.
type fakeGraph struct {
	msgs   map[string]map[string]any // a message, by its resource path (/teams/{group}/channels/{channel}/messages/{id})
	pageAt int                       // the most a page holds, 0 for $top
	// next, if set, is the @odata.nextLink of the page that skipped skip messages, in place of the fake's own; "" is none.
	next  func(self string, skip int) string
	fail  func(r *http.Request) (status int, body string, ok bool)
	hold  chan struct{}    // if set, a call waits until it is closed or the client gives up
	big   int              // if set, the bytes of padding that each page of replies carries
	extra []map[string]any // put on every page of replies, besides the replies
	// redirect, if set, is where every call is sent with a 307 instead of an answer.
	redirect string
	reqs     []graphReq
	scopes   []string // the scope of each token grant that the fake's Entra took
	subs     fakeSubs // the app's subscriptions (push_test.go)
	// The delta of a channel's messages (poll_test.go): seq counts the puts, and changed is the seq of each message's last put. A read
	// with no $deltatoken gives every root on a first page with a nextLink, then an empty page with the deltaLink, as measured; a read of a
	// deltaLink gives the roots put since, as measured (no reply). deltaGone answers a read of a deltaLink 410.
	seq       int
	changed   map[string]int
	deltaGone bool
}

func newFakeGraph() fakeGraph {
	return fakeGraph{msgs: map[string]map[string]any{}, subs: fakeSubs{byID: map[string]map[string]any{}}, changed: map[string]int{}}
}

// graphURL is the fake's Microsoft Graph, as Config.GraphURL takes it.
func (f *fakeConnector) graphURL() string { return f.srv.URL + strings.TrimSuffix(graphPrefix, "/") }

// messagePath is the resource path of the message id of channel in the team of group.
func messagePath(group, channel, id string) string {
	return "/teams/" + group + "/channels/" + channel + "/messages/" + id
}

// putMessage keeps m (its "id" set, and its "replyToId" for a reply) in channel of the test team.
func (f *fakeConnector) putMessage(channel string, m map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := messagePath(testGroup, channel, m["id"].(string))
	f.graph.msgs[p] = m
	f.graph.seq++
	f.graph.changed[p] = f.graph.seq
}

// graphTaken returns, and forgets, the Graph calls taken since the last.
func (f *fakeConnector) graphTaken() []graphReq {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.graph.reqs
	f.graph.reqs = nil
	return out
}

func (f *fakeConnector) setGraph(edit func(g *fakeGraph)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	edit(&f.graph)
}

func (f *fakeConnector) serveGraph(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, strings.TrimSuffix(graphPrefix, "/"))
	f.mu.Lock()
	f.graph.reqs = append(f.graph.reqs, graphReq{path: path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")})
	g := f.graph
	f.mu.Unlock()
	if g.hold != nil {
		select {
		case <-g.hold:
		case <-r.Context().Done():
			return
		}
	}
	if g.redirect != "" {
		http.Redirect(w, r, g.redirect, http.StatusTemporaryRedirect)
		return
	}
	if g.fail != nil {
		if status, body, ok := g.fail(r); ok {
			w.Header().Set("Retry-After", "4")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
	}
	notFound := func() {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"NotFound","message":"`+path+`"}}`)
	}
	if path == "/subscriptions" || strings.HasPrefix(path, "/subscriptions/") {
		f.serveSubscriptions(w, r, path)
		return
	}
	// A reply, GET …/messages/{root}/replies/{id}: the message id, if it is a reply to root.
	if thread, id, ok := strings.Cut(path, "/replies/"); ok {
		root := thread[strings.LastIndex(thread, "/")+1:]
		f.mu.Lock()
		m, ok := g.msgs[thread[:strings.LastIndex(thread, "/")+1]+id]
		f.mu.Unlock()
		if !ok || m["replyToId"] != root {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(m)
		return
	}
	if channelPath, ok := strings.CutSuffix(path, "/delta"); ok {
		f.serveDelta(w, r, channelPath)
		return
	}
	root, isReplies := strings.CutSuffix(path, "/replies")
	isRoots := strings.HasSuffix(path, "/messages")
	if !isReplies && !isRoots {
		f.mu.Lock()
		m, ok := g.msgs[path]
		f.mu.Unlock()
		if !ok {
			notFound()
			return
		}
		_ = json.NewEncoder(w).Encode(m)
		return
	}
	// The list is a root's replies, or for …/messages the channel's roots, which have no replyToId.
	f.mu.Lock()
	known, prefix, parent := true, path+"/", any(nil)
	if isReplies {
		_, known = g.msgs[root]
		prefix, parent = root[:strings.LastIndex(root, "/")+1], root[strings.LastIndex(root, "/")+1:]
	}
	var replies []map[string]any
	for p, m := range g.msgs {
		if strings.HasPrefix(p, prefix) && m["replyToId"] == parent {
			replies = append(replies, m)
		}
	}
	f.mu.Unlock()
	if !known {
		notFound()
		return
	}
	slices.SortFunc(replies, func(a, b map[string]any) int { return compareIDs(b["id"].(string), a["id"].(string)) })
	top, err := strconv.Atoi(r.URL.Query().Get("$top"))
	if err != nil || top < 1 || top > 50 {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"BadRequest"}}`)
		return
	}
	size := cmp.Or(g.pageAt, top)
	skip, _ := strconv.Atoi(r.URL.Query().Get("$skiptoken"))
	skip = min(skip, len(replies))
	value := slices.Clone(replies[skip:min(skip+size, len(replies))])
	if isRoots && r.URL.Query().Get("$expand") == "replies" {
		for i, m := range value {
			m = maps.Clone(m)
			m["replies"] = f.repliesOf(path + "/" + m["id"].(string))
			value[i] = m
		}
	}
	page := map[string]any{"value": append(value, g.extra...)}
	self := "https://" + r.Host + r.URL.Path
	next := ""
	if skip+size < len(replies) {
		next = fmt.Sprintf("%s?$top=%d&$skiptoken=%d", self, top, skip+size)
	}
	if g.next != nil {
		next = g.next(self, skip)
	}
	if next != "" {
		page["@odata.nextLink"] = next
	}
	if g.big > 0 {
		page["padding"] = strings.Repeat("x", g.big)
	}
	_ = json.NewEncoder(w).Encode(page)
}

// repliesOf is the replies of the root at the resource path rootPath, newest first.
func (f *fakeConnector) repliesOf(rootPath string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix, parent := rootPath[:strings.LastIndex(rootPath, "/")+1], rootPath[strings.LastIndex(rootPath, "/")+1:]
	out := []map[string]any{}
	for p, m := range f.graph.msgs {
		if strings.HasPrefix(p, prefix) && m["replyToId"] == parent {
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b map[string]any) int { return compareIDs(b["id"].(string), a["id"].(string)) })
	return out
}

// serveDelta answers a read of the delta of the channel whose messages are at channelPath (fakeGraph).
func (f *fakeConnector) serveDelta(w http.ResponseWriter, r *http.Request, channelPath string) {
	q := r.URL.Query()
	self := "https://" + r.Host + strings.TrimSuffix(graphPrefix, "/") + channelPath + "/delta"
	f.mu.Lock()
	defer f.mu.Unlock()
	if q.Has("$deltatoken") && f.graph.deltaGone {
		w.WriteHeader(http.StatusGone)
		_, _ = io.WriteString(w, `{"error":{"code":"resyncRequired"}}`)
		return
	}
	since := -1
	if q.Has("$deltatoken") {
		since, _ = strconv.Atoi(q.Get("$deltatoken"))
	}
	page := map[string]any{"value": []map[string]any{}}
	if !q.Has("$deltatoken") && !q.Has("$skiptoken") {
		var roots []map[string]any
		for p, m := range f.graph.msgs {
			if strings.HasPrefix(p, channelPath+"/") && m["replyToId"] == nil {
				roots = append(roots, m)
			}
		}
		page["value"] = roots
		page["@odata.nextLink"] = self + "?$skiptoken=1"
		_ = json.NewEncoder(w).Encode(page)
		return
	}
	var roots []map[string]any
	if since >= 0 {
		for p, m := range f.graph.msgs {
			if strings.HasPrefix(p, channelPath+"/") && m["replyToId"] == nil && f.graph.changed[p] > since {
				roots = append(roots, m)
			}
		}
	}
	slices.SortFunc(roots, func(a, b map[string]any) int { return compareIDs(a["id"].(string), b["id"].(string)) })
	page["value"] = roots
	page["@odata.deltaLink"] = fmt.Sprintf("%s?$deltatoken=%d", self, f.graph.seq)
	_ = json.NewEncoder(w).Encode(page)
}

// made is m with its createdDateTime at t.
func made(m map[string]any, t time.Time) map[string]any {
	m["createdDateTime"] = t.UTC().Format(time.RFC3339Nano)
	return m
}

// person is a person's message with the HTML body html, by the Microsoft Entra user user; a reply to root unless root is "".
func person(id, root, user, html string, mentions ...map[string]any) map[string]any {
	return message(id, root, map[string]any{"application": nil, "device": nil, "user": map[string]any{
		"@odata.type": "#microsoft.graph.teamworkUserIdentity", "id": user, "displayName": "Example Person", "userIdentityType": "aadUser"}},
		html, nil, mentions)
}

// app is a message of the application appID with the HTML body html and the Adaptive Cards cards as its attachments.
func app(id, root, appID, html string, cards ...string) map[string]any {
	var atts []map[string]any
	for i, c := range cards {
		atts = append(atts, map[string]any{"id": strconv.Itoa(i), "contentType": "application/vnd.microsoft.card.adaptive", "content": c})
	}
	return message(id, root, map[string]any{"application": map[string]any{"@odata.type": "#microsoft.graph.teamworkApplicationIdentity",
		"id": appID, "displayName": "An App", "applicationIdentityType": "bot"}, "device": nil, "user": nil}, html, atts, nil)
}

func message(id, root string, from map[string]any, html string, atts, mentions []map[string]any) map[string]any {
	if atts == nil {
		atts = []map[string]any{}
	}
	if mentions == nil {
		mentions = []map[string]any{}
	}
	m := map[string]any{"id": id, "replyToId": root, "messageType": "message", "deletedDateTime": nil, "from": from,
		"body": map[string]any{"contentType": "html", "content": html}, "attachments": atts, "mentions": mentions}
	if root == "" {
		m["replyToId"] = nil
	}
	return m
}

// mentionOfBot is the mention of the bot's app, as Graph lists it in a message's mentions.
func mentionOfBot() map[string]any {
	return map[string]any{"id": 0, "mentionText": "Agent Kourier", "mentioned": map[string]any{"application": map[string]any{
		"id": testAppID, "displayName": "Agent Kourier", "applicationIdentityType": "bot"}, "user": nil}}
}
