package teams

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/chat/chattest"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// connRequest is one call that the fake Bot Connector took.
type connRequest struct {
	method, path string // path as it came on the wire, escaped
	query        string // the query as it came on the wire
	auth         string
	activity     outActivity
	raw          map[string]any
}

// fakeConnector is the Bot Connector, Microsoft Graph (under /v1.0/, graph_test.go) and Entra's token endpoint on one TLS server. It
// records each call, answers a post with a new activity ID, an update with 200, a team's channels from teams, and a conversation's
// members from rosters, in pages of the size asked for and at most pageMax; fail, if set, answers a call instead. A token for Graph's
// scope is "fake-graph-token-N", and one for any other scope "fake-bot-token-N".
type fakeConnector struct {
	srv *httptest.Server

	mu      sync.Mutex
	reqs    []connRequest                                            // guarded by mu
	teams   map[string][]string                                      // guarded by mu: the channels of each team
	rosters map[string][]account                                     // guarded by mu: the members of each conversation
	pageMax int                                                      // guarded by mu: the most members a page holds, 0 for no cap
	fail    func(r *http.Request) (status int, body string, ok bool) // guarded by mu
	nextID  int                                                      // guarded by mu
	tokens  int                                                      // guarded by mu: the token grants taken
	graph   fakeGraph                                                // guarded by mu
}

func newFakeConnector(t testing.TB) *fakeConnector {
	t.Helper()
	f := &fakeConnector{teams: map[string][]string{}, rosters: map[string][]account{}, nextID: 1700000000000, graph: newFakeGraph()}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// serviceURL is the fake's Bot Connector, as a channel's record names it.
func (f *fakeConnector) serviceURL() string { return f.srv.URL + "/amer/" + testTenant + "/" }

func (f *fakeConnector) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	fail := f.fail
	f.mu.Unlock()
	if strings.HasPrefix(r.URL.Path, graphPrefix) {
		f.serveGraph(w, r)
		return
	}
	if r.URL.Path == "/token" {
		_ = r.ParseForm()
		f.mu.Lock()
		f.tokens++
		n := f.tokens
		f.graph.scopes = append(f.graph.scopes, r.PostForm.Get("scope"))
		f.mu.Unlock()
		if fail != nil {
			if status, body, ok := fail(r); ok {
				w.WriteHeader(status)
				_, _ = io.WriteString(w, body)
				return
			}
		}
		kind := "bot"
		if r.PostForm.Get("scope") == graphScope {
			kind = "graph"
		}
		_, _ = fmt.Fprintf(w, `{"token_type":"Bearer","expires_in":3599,"access_token":"fake-%s-token-%d"}`, kind, n)
		return
	}
	body, _ := io.ReadAll(r.Body)
	req := connRequest{method: r.Method, path: r.URL.EscapedPath(), query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
	if len(body) > 0 {
		_ = json.Unmarshal(body, &req.activity)
		_ = json.Unmarshal(body, &req.raw)
	}
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.nextID++
	id := f.nextID
	f.mu.Unlock()
	if fail != nil {
		if status, body, ok := fail(r); ok {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
	}
	prefix := "/amer/" + testTenant + "/v3/"
	rest, ok := strings.CutPrefix(r.URL.Path, prefix)
	switch {
	case !ok:
		http.Error(w, `{"error":{"code":"BadPath"}}`, http.StatusNotFound)
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "teams/"):
		team := strings.TrimSuffix(strings.TrimPrefix(rest, "teams/"), "/conversations")
		f.mu.Lock()
		chans, known := f.teams[team]
		f.mu.Unlock()
		if !known {
			http.Error(w, `{"error":{"code":"NotFound","message":"team"}}`, http.StatusNotFound)
			return
		}
		var out struct {
			Conversations []map[string]string `json:"conversations"`
		}
		for _, c := range chans {
			out.Conversations = append(out.Conversations, map[string]string{"id": c, "name": "Example Team"})
		}
		_ = json.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet && strings.HasSuffix(rest, "/pagedmembers"):
		f.members(w, r, strings.TrimSuffix(strings.TrimPrefix(rest, "conversations/"), "/pagedmembers"))
	case r.Method == http.MethodPost:
		w.WriteHeader(http.StatusCreated)
		_, _ = fmt.Fprintf(w, `{"id":"%d"}`, id)
	case r.Method == http.MethodPut:
		_, _ = fmt.Fprintf(w, `{"id":"%s"}`, rest[strings.LastIndex(rest, "/")+1:])
	default:
		http.Error(w, "unexpected", http.StatusMethodNotAllowed)
	}
}

// members answers a page of the roster of conv, as Microsoft documents GET /v3/conversations/{id}/pagedmembers: the members from
// the continuationToken on (here, an index), at most pageSize of them, and the token of the next page while there is one. A
// conversation with no roster is not found.
func (f *fakeConnector) members(w http.ResponseWriter, r *http.Request, conv string) {
	f.mu.Lock()
	roster, known := f.rosters[conv]
	pageMax := f.pageMax
	f.mu.Unlock()
	if !known {
		http.Error(w, `{"error":{"code":"ConversationNotFound"}}`, http.StatusNotFound)
		return
	}
	size, err := strconv.Atoi(r.URL.Query().Get("pageSize"))
	if err != nil || size < 1 {
		http.Error(w, `{"error":{"code":"BadArgument"}}`, http.StatusBadRequest)
		return
	}
	if pageMax > 0 {
		size = min(size, pageMax)
	}
	from := 0
	if tok := r.URL.Query().Get("continuationToken"); tok != "" {
		if from, err = strconv.Atoi(tok); err != nil || from > len(roster) {
			http.Error(w, `{"error":{"code":"BadArgument"}}`, http.StatusBadRequest)
			return
		}
	}
	page := struct {
		Members           []account `json:"members"`
		ContinuationToken string    `json:"continuationToken,omitempty"`
	}{Members: roster[from:min(from+size, len(roster))]}
	if from+size < len(roster) {
		page.ContinuationToken = strconv.Itoa(from + size)
	}
	_ = json.NewEncoder(w).Encode(page)
}

// setRoster lists members as the roster of the conversation conv.
func (f *fakeConnector) setRoster(conv string, members ...account) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rosters[conv] = members
}

// setPageMax caps a page of a roster at n members.
func (f *fakeConnector) setPageMax(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pageMax = n
}

// setFail sets fail.
func (f *fakeConnector) setFail(fail func(r *http.Request) (status int, body string, ok bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

// setTeam lists channels as the channels of team.
func (f *fakeConnector) setTeam(team string, channels ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.teams[team] = channels
}

// take returns, and forgets, the calls taken since the last.
func (f *fakeConnector) take() []connRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.reqs
	f.reqs = nil
	return out
}

// clientTest is a Client of the test connection on the fake, with its store, and the Bot Framework's key that signs the activities a
// test sends it.
type clientTest struct {
	c     *Client
	f     *fakeConnector
	convs *MemoryConversations
	key   signingKey
}

func newClientTest(t testing.TB) *clientTest {
	t.Helper()
	f := newFakeConnector(t)
	key := newSigningKey(t, "k", "msteams")
	bf := newFakeBotFramework(t, key)
	convs := NewMemoryConversations()
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		TokenURL: f.srv.URL + "/token", MetadataURL: bf.metadataURL(), GraphURL: f.graphURL(), HTTPClient: f.srv.Client(),
		Conversations: convs, Logger: testutil.DiscardLogger()})
	if err != nil {
		t.Fatal(err)
	}
	return &clientTest{c: c, f: f, convs: convs, key: key}
}

// record gives channel a record at the fake's service URL, in team, whose Microsoft Entra group is testGroup.
func (ct *clientTest) record(t testing.TB, channel, team string) {
	t.Helper()
	if err := ct.convs.Remember(t.Context(), testConn, Conversation{Channel: channel, ServiceURL: ct.f.serviceURL(), TeamID: team,
		AADGroupID: testGroup, TenantID: testTenant}); err != nil {
		t.Fatal(err)
	}
}

// shownBy is what Teams shows of the activities the fake took, as CommonMark reads their text (Teams' markdown is unmeasured, so
// code is not taken for inert): each mention entity's text, found in the text, is the mention of its user, shown as its name; the
// adapter's own step list is read as its lines (unlisted); and the text is split into its paragraphs, each a piece that a place's
// limit holds. What the reading finds acting is put on the first
// piece that holds the conformance canary (or the first piece), so that every construct is checked once, against a piece that
// carries hostile text.
func shownBy(reqs []connRequest) []chattest.Shown {
	var out []chattest.Shown
	for _, r := range reqs {
		if r.method != http.MethodPost && r.method != http.MethodPut {
			continue
		}
		text := uncarried(r.activity.Text)
		var active []chattest.Active
		for _, e := range r.activity.Entities {
			if e.Type != "mention" {
				active = append(active, chattest.Active{Kind: chattest.KindHTML, Text: "entity " + e.Type})
				continue
			}
			i := strings.Index(text, e.Text)
			if i < 0 || e.Text == "" {
				continue // an entity whose text is not there mentions nobody
			}
			text = text[:i] + e.Mentioned.Name + text[i+len(e.Text):]
			active = append(active, chattest.Active{Kind: chattest.KindMention, Text: e.Mentioned.ID})
		}
		if text != "" || len(active) > 0 {
			scan := chattest.ScanCommonMark(unlisted(text))
			active = append(active, scan.Active...)
			pieces := strings.Split(scan.Displayed, "\n\n")
			at := 0
			for i, p := range pieces {
				if strings.Contains(p, "zq7kx3") {
					at = i
					break
				}
			}
			for i, p := range pieces {
				s := chattest.Shown{Displayed: p}
				if i == at {
					s.Active = active
				}
				out = append(out, s)
			}
		}
		for _, a := range r.activity.Attachments {
			out = append(out, cardShown(a.Content)...)
		}
	}
	return out
}

// cardShown is what Teams shows of a card, one piece for each text in it: a TextRun's text, a choice's title and an action's title as
// the characters they hold (as Teams showed them on a test tenant, web client, channel thread, November 2026); a TextBlock's text, which takes markdown, with the card's mention entities found in it as in a message's text, and read
// as CommonMark; and the fallbackText as CommonMark, since where Teams shows it is not known. inert holds a card to the elements and
// keys read here.
func cardShown(card map[string]any) []chattest.Shown {
	var mentions []mentionEntity
	if teams, ok := card["msteams"].(map[string]any); ok {
		raw, _ := json.Marshal(teams["entities"])
		_ = json.Unmarshal(raw, &mentions)
	}
	var out []chattest.Shown
	if s, ok := card["fallbackText"].(string); ok {
		out = append(out, chattest.ScanCommonMark(s))
	}
	var walk func(v any)
	walk = func(v any) {
		switch v := v.(type) {
		case []any:
			for _, e := range v {
				walk(e)
			}
		case map[string]any:
			typ, _ := v["type"].(string)
			text, _ := v["text"].(string)
			title, isTitled := v["title"].(string)
			switch {
			case typ == "TextRun":
				out = append(out, chattest.Shown{Displayed: text})
			case typ == "TextBlock":
				var active []chattest.Active
				for _, e := range mentions {
					if i := strings.Index(text, e.Text); i >= 0 && e.Text != "" {
						text = text[:i] + e.Mentioned.Name + text[i+len(e.Text):]
						active = append(active, chattest.Active{Kind: chattest.KindMention, Text: e.Mentioned.ID})
					}
				}
				scan := chattest.ScanCommonMark(text)
				out = append(out, chattest.Shown{Displayed: scan.Displayed, Active: append(active, scan.Active...)})
			case isTitled:
				out = append(out, chattest.Shown{Displayed: title})
			}
			for _, k := range []string{"body", "inlines", "choices", "actions"} {
				walk(v[k])
			}
		}
	}
	walk(card)
	return out
}

// trailingCarrier is the paragraph of Agent Kourier's carrier of a post's tag at the end of a text (withCarrier).
var trailingCarrier = regexp.MustCompile(`\n\n<span title="kourier[0-9a-f]{32}"></span>$`)

// uncarried is text without the paragraph of the carrier at its end, which Teams shows nothing of: the reading takes off that one
// paragraph of the exact shape, last, and reads any other span as the HTML it is.
func uncarried(text string) string { return trailingCarrier.ReplaceAllString(text, "") }

// unlisted is text with the step list that the adapter puts last (stepList) read as what it shows: the quote and the list are the
// adapter's own, so each of its lines is a paragraph of its own here, and only what is in the lines is scanned. escapeLine starts
// no line with ">" or "- ", so a last paragraph whose every line starts with ">" is a list of the adapter's, or a quote of the
// Body's, whose style a Body may have anyway.
func unlisted(text string) string {
	cut := strings.LastIndex(text, "\n\n") + 1
	if cut > 0 {
		cut++
	}
	last := strings.Split(text[cut:], "\n")
	var lines []string
	for _, l := range last {
		rest, quoted := strings.CutPrefix(l, ">")
		if !quoted {
			return text
		}
		rest = strings.TrimPrefix(strings.TrimPrefix(rest, " "), "- ")
		if rest != "" {
			lines = append(lines, rest)
		}
	}
	return text[:cut] + strings.Join(lines, "\n\n")
}

// inert checks that every activity is a plain message in markdown with no more than its text, its mentions and Agent Kourier's cards:
// no other attachment, and no suggested action, that could carry a link or a preview. A card holds only the elements and the keys
// that cardKeys lists, so none has an image, a link, a selectAction or an Action.OpenUrl; its schema is the one the adapter names.
func inert(reqs []connRequest) error {
	for _, r := range reqs {
		if r.raw == nil {
			continue
		}
		for k := range r.raw {
			switch k {
			case "type", "text", "textFormat", "entities", "attachments":
			default:
				return fmt.Errorf("%s %s carried %q", r.method, r.path, k)
			}
		}
		if r.activity.Type != "message" || (r.activity.Text != "" && r.activity.TextFormat != "markdown") {
			return fmt.Errorf("%s %s is a %q of the format %q", r.method, r.path, r.activity.Type, r.activity.TextFormat)
		}
		atts, _ := r.raw["attachments"].([]any)
		for _, a := range atts {
			att, _ := a.(map[string]any)
			if len(att) != 2 || att["contentType"] != cardContentType {
				return fmt.Errorf("%s %s carried the attachment %v", r.method, r.path, a)
			}
			card, _ := att["content"].(map[string]any)
			if card["$schema"] != cardSchema || card["version"] != cardVersion {
				return fmt.Errorf("%s %s carried a card of the schema %v, version %v", r.method, r.path, card["$schema"], card["version"])
			}
			if err := inertElement(card, "AdaptiveCard"); err != nil {
				return fmt.Errorf("%s %s: %w", r.method, r.path, err)
			}
		}
	}
	return nil
}

// cardKeys is each element a card may hold, by its type ("choice" for an Input.ChoiceSet's choice, "msteams" for the card's Teams
// object, "mention" for one of its entities), with the keys it may have and, for a key that holds elements, their type ("" for
// elements that name their own); a key with no type is a leaf, or the action's data, which Teams sends back and shows nowhere; and a
// key of the type "carrier" holds Agent Kourier's carrier of a post's tag and nothing else.
var cardKeys = map[string]map[string]string{
	"AdaptiveCard":    {"type": "-", "$schema": "-", "version": "-", "fallbackText": "-", "body": "", "actions": "Action.Execute", "msteams": "msteams"},
	"msteams":         {"entities": "mention"},
	"mention":         {"type": "-", "text": "-", "mentioned": "-"},
	"RichTextBlock":   {"type": "-", "id": "carrier", "inlines": "TextRun"},
	"TextRun":         {"type": "-", "text": "-", "weight": "-", "isSubtle": "-", "fontType": "-"},
	"TextBlock":       {"type": "-", "id": "carrier", "text": "-", "wrap": "-"},
	"Input.Text":      {"type": "-", "id": "-", "isMultiline": "-"},
	"Input.ChoiceSet": {"type": "-", "id": "-", "style": "-", "isMultiSelect": "-", "choices": "choice"},
	"choice":          {"title": "-", "value": "-"},
	"Action.Execute":  {"type": "-", "verb": "-", "title": "-", "style": "-", "data": "-"},
}

// bodyTypes are the elements a card's body may hold.
var bodyTypes = []string{"RichTextBlock", "TextBlock", "Input.Text", "Input.ChoiceSet"}

// inertElement checks that v is an element of the type want (for "", one of bodyTypes) with only the keys cardKeys gives that type,
// and so for every element it holds. An element whose keys name a type must be of it.
func inertElement(v any, want string) error {
	el, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("a card element %v is not an object", v)
	}
	typ := want
	if want == "" {
		typ, _ = el["type"].(string)
		if !slices.Contains(bodyTypes, typ) {
			return fmt.Errorf("a card's body holds the element %q", typ)
		}
	}
	keys := cardKeys[typ]
	if _, typed := keys["type"]; typed && el["type"] != typ {
		return fmt.Errorf("a card element %v is not a %q", v, typ)
	}
	for k, sub := range el {
		inner, allowed := keys[k]
		switch {
		case !allowed:
			return fmt.Errorf("a card's %q carried %q", typ, k)
		case inner == "-":
		case inner == "carrier": // the id of a text: Agent Kourier's carrier alone (withCarrier), which shows nothing
			if id, _ := sub.(string); !carrierShape.MatchString(id) {
				return fmt.Errorf("a card's %q has the id %v, which is no carrier", typ, sub)
			}
		case inner == "msteams":
			if err := inertElement(sub, inner); err != nil {
				return err
			}
		default:
			list, isList := sub.([]any)
			if !isList {
				return fmt.Errorf("a card's %q holds %q that is not a list", typ, k)
			}
			for _, e := range list {
				if err := inertElement(e, inner); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// The groups and the channel states that Teams has no such thing as, with why.
var (
	absentChannels = map[chattest.ChannelState]string{
		chattest.ChannelNotJoined: "the Bot Connector lists the team's channels for the bot, and has no channel that is there and that " +
			"the bot is not in: one it does not list is not found",
		chattest.ChannelArchived:     "the Bot Connector says nothing of an archive, and ChannelAccess makes no Microsoft Graph read",
		chattest.ChannelScopeMissing: "the bot's token has no permission of its own for the read: there is no missing scope to report",
	}
	absentGroups = map[chattest.GroupState]string{
		chattest.GroupReadable:     "Microsoft Teams has no approver groups: a Binding of a Teams connection takes none",
		chattest.GroupScopeMissing: "Microsoft Teams has no approver groups to lack a permission for",
	}
)

// teamsSubject is the conformance Subject of a Client on the fake.
func teamsSubject(t *testing.T) chattest.Subject {
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	unknown := "19:KourierStandInUnlistedChannel000000000000a@thread.tacv2"
	ct.record(t, unknown, testChannel)
	ct.f.setTeam(testChannel, testChannel)
	var pending []connRequest
	var pmu sync.Mutex
	drain := func() []connRequest {
		pmu.Lock()
		defer pmu.Unlock()
		pending = append(pending, ct.f.take()...)
		out := pending
		pending = nil
		return out
	}
	var forInert []connRequest
	tooLarge := chat.Prompt{ID: "ix-large"}
	for range 40 {
		tooLarge.Questions = append(tooLarge.Questions, chat.Question{Text: strings.Repeat("q", maxTextUnits)})
	}
	tooLargeTools := chat.Prompt{ID: "ix-large-tools", Tools: twentyTools(strings.Repeat("中", capabilities().Limit(chat.WhereToolArgs)))}
	return chattest.Subject{
		Platform: ct.c,
		Sent: func() []chattest.Shown {
			reqs := drain()
			pmu.Lock()
			forInert = append(forInert, reqs...)
			pmu.Unlock()
			return shownBy(reqs)
		},
		Inert: func() error {
			reqs := drain()
			pmu.Lock()
			all := slices.Concat(forInert, reqs)
			forInert = nil
			pmu.Unlock()
			return inert(all)
		},
		AllowAutolink: true,
		Target:        chat.Target{Conn: testConn, Channel: testChannel, Thread: "1700000000000"},
		TooLarge:      &tooLarge,
		ToolApprovals: true,
		TooLargeTools: &tooLargeTools,
		MentionUser:   testUser,
		Channels: func(st chattest.ChannelState) string {
			if st == chattest.ChannelJoined {
				return testChannel
			}
			return unknown
		},
		Groups:         func(chattest.GroupState) string { return "S0UNKNOWN" },
		AbsentChannels: absentChannels,
		AbsentGroups:   absentGroups,
	}
}

func TestTeamsAdapterPassesConformanceAgainstTheFakeBotConnector(t *testing.T) {
	chattest.ConformanceSharded(t, 8, teamsSubject)
}

// The reading of the wire is itself tested: what it says of text that Teams would act on must be what it is.
func TestTheTeamsReadingOfTheWireFindsWhatTeamsWouldActOn(t *testing.T) {
	t.Parallel()
	post := func(text string, entities ...mentionEntity) connRequest {
		return connRequest{method: http.MethodPost, activity: outActivity{Type: "message", Text: text, TextFormat: "markdown", Entities: entities}}
	}
	mention := mentionEntity{Type: "mention", Text: "<at>someone</at>", Mentioned: mentioned{ID: testUser, Name: "someone"}}
	for _, tc := range []struct {
		name string
		req  connRequest
		want []chattest.Kind
	}{
		{"a raw at tag with no entity", post("<at>x</at> hi"), []chattest.Kind{chattest.KindHTML, chattest.KindHTML}},
		{"a mention entity", post("<at>someone</at> hi", mention), []chattest.Kind{chattest.KindMention}},
		{"a second at tag beside a mention", post("<at>someone</at> <at>someone</at>", mention), []chattest.Kind{chattest.KindMention, chattest.KindHTML, chattest.KindHTML}},
		{"an entity whose text is not there", post("hi", mention), nil},
		{"a markdown link", post("[x](https://evil.test)"), []chattest.Kind{chattest.KindLink}},
		{"escaped text", post("&lt;at&gt;x&lt;/at&gt; &#91;x&#93;(y) \\*b\\*"), nil},
		{"the carrier last", post("hi\n\n<span title=\"kourier0123456789abcdef0123456789abcdef\"></span>"), nil},
		{"a carrier not last", post("<span title=\"kourier0123456789abcdef0123456789abcdef\"></span>\n\nhi"), []chattest.Kind{chattest.KindHTML, chattest.KindHTML}},
		{"a span of another title last", post("hi\n\n<span title=\"x\"></span>"), []chattest.Kind{chattest.KindHTML, chattest.KindHTML}},
	} {
		var kinds []chattest.Kind
		for _, s := range shownBy([]connRequest{tc.req}) {
			for _, a := range s.Active {
				kinds = append(kinds, a.Kind)
			}
		}
		if fmt.Sprint(kinds) != fmt.Sprint(tc.want) {
			t.Errorf("%s: found %v, want %v", tc.name, kinds, tc.want)
		}
	}
}

// The reading of a card is itself tested: a TextBlock takes markdown and the card's mentions, and a TextRun and a title are the
// characters they hold; inert takes Agent Kourier's cards and nothing that could carry a link.
func TestTheTeamsReadingOfACard(t *testing.T) {
	t.Parallel()
	mention := map[string]any{"type": "mention", "text": "<at>someone</at>", "mentioned": map[string]any{"id": testUser, "name": "someone"}}
	cardOfBody := func(body ...any) map[string]any {
		return map[string]any{"type": "AdaptiveCard", "$schema": cardSchema, "version": cardVersion, "body": body,
			"msteams": map[string]any{"entities": []any{mention}}}
	}
	block := func(text string) map[string]any {
		return map[string]any{"type": "TextBlock", "text": text, "wrap": true}
	}
	run := func(text string) map[string]any {
		return map[string]any{"type": "RichTextBlock", "inlines": []any{map[string]any{"type": "TextRun", "text": text}}}
	}
	for _, tc := range []struct {
		name string
		card map[string]any
		want []chattest.Kind
	}{
		{"a mention in a TextBlock", cardOfBody(block("<at>someone</at> answered.")), []chattest.Kind{chattest.KindMention}},
		{"a link in a TextBlock", cardOfBody(block("[x](https://evil.test)")), []chattest.Kind{chattest.KindLink}},
		{"markup in a TextRun", cardOfBody(run("[x](https://evil.test) *b* <at>someone</at>")), nil},
	} {
		var kinds []chattest.Kind
		for _, s := range cardShown(tc.card) {
			for _, a := range s.Active {
				kinds = append(kinds, a.Kind)
			}
		}
		if fmt.Sprint(kinds) != fmt.Sprint(tc.want) {
			t.Errorf("%s: found %v, want %v", tc.name, kinds, tc.want)
		}
	}
	if got := cardShown(cardOfBody(run("a *b*"))); len(got) != 1 || got[0].Displayed != "a *b*" {
		t.Errorf("a TextRun is shown as %+v, want its characters", got)
	}

	posted := func(att map[string]any) connRequest {
		return connRequest{method: http.MethodPost, activity: outActivity{Type: "message"}, raw: map[string]any{"type": "message", "attachments": []any{att}}}
	}
	ours := func(card map[string]any) map[string]any {
		return map[string]any{"contentType": cardContentType, "content": card}
	}
	press := map[string]any{"type": "Action.Execute", "title": "Submit", "data": map[string]any{dataKey: map[string]any{}}}
	good := cardOfBody(run("x"), block("<at>someone</at> answered."), map[string]any{"type": "Input.Text", "id": "t0", "isMultiline": true},
		map[string]any{"type": "Input.ChoiceSet", "id": "r1", "style": "expanded", "isMultiSelect": false,
			"choices": []any{map[string]any{"title": "a", "value": "0"}}})
	good["actions"] = []any{press}
	good["body"].([]any)[0].(map[string]any)["id"] = "kourier0123456789abcdef0123456789abcdef"
	if err := inert([]connRequest{posted(ours(good))}); err != nil {
		t.Errorf("Agent Kourier's card is not inert: %v", err)
	}
	with := func(edit func(map[string]any)) map[string]any {
		raw, _ := json.Marshal(good)
		var c map[string]any
		_ = json.Unmarshal(raw, &c)
		edit(c)
		return ours(c)
	}
	for name, att := range map[string]map[string]any{
		"another attachment": {"contentType": "application/vnd.microsoft.card.hero", "content": map[string]any{}},
		"a content URL":      {"contentType": cardContentType, "content": good, "contentUrl": "https://evil.test"},
		"an image": with(func(c map[string]any) {
			c["body"] = []any{map[string]any{"type": "Image", "url": "https://evil.test/x.png"}}
		}),
		"an Action.OpenUrl": with(func(c map[string]any) {
			c["actions"] = []any{map[string]any{"type": "Action.OpenUrl", "url": "https://evil.test"}}
		}),
		"an id on a text that is no carrier": with(func(c map[string]any) {
			c["body"].([]any)[0].(map[string]any)["id"] = "https://evil.test"
		}),
		"a selectAction": with(func(c map[string]any) {
			c["body"].([]any)[1].(map[string]any)["selectAction"] = map[string]any{"type": "Action.OpenUrl", "url": "https://evil.test"}
		}),
		"a background image": with(func(c map[string]any) { c["backgroundImage"] = "https://evil.test/x.png" }),
		"another version":    with(func(c map[string]any) { c["version"] = "1.6" }),
		"a TextRun's link": with(func(c map[string]any) {
			c["body"].([]any)[0].(map[string]any)["inlines"].([]any)[0].(map[string]any)["selectAction"] = "x"
		}),
		"a choice's type": with(func(c map[string]any) {
			c["body"].([]any)[3].(map[string]any)["choices"].([]any)[0].(map[string]any)["type"] = "Image"
		}),
	} {
		if err := inert([]connRequest{posted(att)}); err == nil {
			t.Errorf("%s is taken for inert", name)
		}
	}
}
