package teams

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/secret"
	"github.com/agentkourier/agent-kourier/internal/testutil"
)

// The stand-ins of the roster tests: two people of the test channel's team, an account of the roster with a bot's ID, and a person
// no roster lists.
const (
	alexID    = "00000000-0000-4000-8000-0000000000a1"
	samID     = "00000000-0000-4000-8000-0000000000a2"
	rosterBot = "00000000-0000-4000-8000-0000000000b1"
	stranger  = "00000000-0000-4000-8000-0000000000ff"
)

var (
	alex      = account{ID: "29:1stand-in-alex", Name: "Alex Example", AADObjectID: alexID}
	sam       = account{ID: "29:1stand-in-sam", Name: "Sam Example", AADObjectID: samID}
	botMember = account{ID: "28:" + testAppID, Name: "Agent Kourier", AADObjectID: rosterBot}
	testUserA = account{ID: "29:1test-user-not-a-real-id", Name: "Test User", AADObjectID: testUser}
)

// failRoster makes every roster read of the fake answer status.
func failRoster(f *fakeConnector, status int) {
	f.setFail(func(r *http.Request) (int, string, bool) {
		return status, "", strings.HasSuffix(r.URL.Path, "/pagedmembers")
	})
}

// rosterReads are the continuation tokens that the roster reads of conv among reqs asked for, "" for a first page; it fails the test
// for one that does not ask for pages of rosterPage.
func rosterReads(t *testing.T, reqs []connRequest, conv string) []string {
	t.Helper()
	var tokens []string
	for _, r := range reqs {
		if r.method != http.MethodGet || r.path != "/amer/"+testTenant+"/v3/conversations/"+escapeID(conv)+"/pagedmembers" {
			continue
		}
		q, err := url.ParseQuery(r.query)
		if err != nil || q.Get("pageSize") != fmt.Sprint(rosterPage) {
			t.Errorf("a roster read asked %q, want pages of %d", r.query, rosterPage)
		}
		tokens = append(tokens, q.Get("continuationToken"))
	}
	return tokens
}

// heardFrom records channel, in team, as the receiver records the channel of an activity, so that the reads of people that are given
// no channel (ResolveUser, IsBot) know the team.
func (ct *clientTest) heardFrom(t testing.TB, channel, team string) {
	t.Helper()
	ct.c.recv.remember(t.Context(), Conversation{Channel: channel, ServiceURL: ct.f.serviceURL(), TeamID: team, TenantID: testTenant})
}

// IsMember reads the roster of the channel's conversation, the team's members, at the channel's service URL, page by page as the
// continuation tokens lead, and stops at the person's Microsoft Entra object ID. It reads it again at each call: a person who left the
// team is not a member at the next press. A user ID of no person (not an Entra object ID) is no member, with no call.
func TestIsMemberReadsTheChannelsRoster(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.f.setPageMax(1)
	ct.f.setRoster(testChannel, sam, alex, testUserA)
	ctx := t.Context()

	if member, err := ct.c.IsMember(ctx, testChannel, alexID); err != nil || !member {
		t.Fatalf("a member on the second page: %t, %v", member, err)
	}
	if got := rosterReads(t, ct.f.take(), testChannel); fmt.Sprint(got) != "[ 1]" {
		t.Errorf("the pages read: %q, want the first two and no more", got)
	}
	if member, err := ct.c.IsMember(ctx, testChannel, stranger); err != nil || member {
		t.Errorf("a person the roster does not list: %t, %v", member, err)
	}
	if got := rosterReads(t, ct.f.take(), testChannel); fmt.Sprint(got) != "[ 1 2]" {
		t.Errorf("the pages read: %q, want every page", got)
	}
	ct.f.setRoster(testChannel, sam, testUserA)
	if member, err := ct.c.IsMember(ctx, testChannel, alexID); err != nil || member {
		t.Errorf("a person who left the team: %t, %v; want no member, as nothing is cached", member, err)
	}
	ct.f.take()
	for _, user := range []string{"", alex.ID, "28:" + testAppID, strings.ToUpper(alexID)} {
		if member, err := ct.c.IsMember(ctx, testChannel, user); err != nil || member {
			t.Errorf("IsMember(%q): %t, %v; want no member and no error", user, member, err)
		}
	}
	if member, err := ct.c.IsMember(ctx, "", alexID); member || !errors.Is(err, chat.ErrChannelNotFound) {
		t.Errorf("no channel: %t, %v", member, err)
	}
	if n := len(ct.f.take()); n != 0 {
		t.Errorf("%d calls for user IDs of no person or no channel", n)
	}
}

// roundTrip is an http.RoundTripper of a function.
type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A channel that no activity has come from is read at the default service URL, and the error of that read is no "no".
func TestIsMemberOfAChannelWithNoRecordGoesToTheDefaultServiceURL(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	var asked string
	ct.c.api.client = &http.Client{Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
		asked = r.URL.Scheme + "://" + r.URL.Host + "/"
		return nil, errors.New("not reached")
	})}
	if member, err := ct.c.IsMember(t.Context(), testChannel, alexID); err == nil || member {
		t.Errorf("IsMember: %t, %v; want no member and an error", member, err)
	}
	if asked != "https://smba.trafficmanager.net/" {
		t.Errorf("asked %q, want the default service URL's host", asked)
	}
}

// A roster read fails closed: any error is no member, and the error says what Classify makes of it. A rate limit may pass on another
// try, after its wait, and so may a 5xx; a removed bot or a conversation that is gone will not; a rejected token is FatalAuth. A
// roster that leads back to the page it read is an error, and so are a page that is not JSON and a read past the bound.
func TestIsMemberFailsClosed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   chat.Verdict
	}{
		{"a rate limit", http.StatusTooManyRequests, `{"error":{"code":"Throttled"}}`, chat.Verdict{Class: chat.Transient, RetryAfter: 3 * time.Second}},
		{"a server error", http.StatusBadGateway, ``, chat.Verdict{Class: chat.Transient}},
		{"a bot removed from the team", http.StatusForbidden, `{"error":{"code":"BotNotInConversationRoster"}}`,
			chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "BotNotInConversationRoster"}},
		{"a conversation that is not there", http.StatusNotFound, `{"error":{"code":"ConversationNotFound"}}`,
			chat.Verdict{Class: chat.Permanent, Reason: chat.ReasonChannelGone, Code: "ConversationNotFound"}},
		{"a rejected token", http.StatusUnauthorized, `{"error":{"code":"Unauthorized"}}`, chat.Verdict{Class: chat.FatalAuth, Code: "Unauthorized"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct := newClientTest(t)
			ct.record(t, testChannel, testChannel)
			ct.f.setRoster(testChannel, alex)
			ct.f.setFail(func(r *http.Request) (int, string, bool) {
				return tc.status, tc.body, strings.HasSuffix(r.URL.Path, "/pagedmembers")
			})
			member, err := ct.c.IsMember(t.Context(), testChannel, alexID)
			if member || err == nil {
				t.Fatalf("IsMember: %t, %v; want no member and an error", member, err)
			}
			if v := ct.c.Classify(chat.OpOther, err); v != tc.want {
				t.Errorf("verdict %+v, want %+v", v, tc.want)
			}
		})
	}

	t.Run("a page that leads back", func(t *testing.T) {
		t.Parallel()
		ct := newClientTest(t)
		ct.record(t, testChannel, testChannel)
		ct.f.setFail(func(r *http.Request) (int, string, bool) {
			return http.StatusOK, `{"members":[{"id":"29:1stand-in-sam","aadObjectId":"` + samID + `"}],"continuationToken":"again"}`,
				strings.HasSuffix(r.URL.Path, "/pagedmembers")
		})
		if member, err := ct.c.IsMember(t.Context(), testChannel, alexID); member || err == nil {
			t.Errorf("IsMember: %t, %v; want no member and an error", member, err)
		}
		if n := len(rosterReads(t, ct.f.take(), testChannel)); n != 2 {
			t.Errorf("%d pages read, want 2: the second named the page it was", n)
		}
	})

	for _, tc := range []struct {
		name string
		next func(token string) string // the continuation token of the page after the one of token
		want []string                  // the pages read
		err  error                     // the error the read ends with, or nil for any
	}{
		{"a cycle of two pages", func(tok string) string { return map[string]string{"": "a", "a": "b", "b": "a"}[tok] }, []string{"", "a", "b"}, nil},
		{"tokens that never end", func(tok string) string { return tok + "x" }, nil, errRosterTooLong},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct := newClientTest(t)
			ct.record(t, testChannel, testChannel)
			ct.f.setFail(endlessRoster(tc.next))
			member, err := ct.c.IsMember(t.Context(), testChannel, alexID)
			if member || err == nil || (tc.err != nil && !errors.Is(err, tc.err)) {
				t.Errorf("IsMember: %t, %v; want no member and an error", member, err)
			}
			got := rosterReads(t, ct.f.take(), testChannel)
			want := tc.want
			if want == nil {
				for tok := ""; len(want) < rosterMaxPages; tok = tc.next(tok) {
					want = append(want, tok)
				}
			}
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("%d pages read, %q; want %d, %q", len(got), got, len(want), want)
			}
		})
	}

	t.Run("a page that is not JSON", func(t *testing.T) {
		t.Parallel()
		ct := newClientTest(t)
		ct.record(t, testChannel, testChannel)
		ct.f.setFail(func(r *http.Request) (int, string, bool) {
			return http.StatusOK, `<html>`, strings.HasSuffix(r.URL.Path, "/pagedmembers")
		})
		if member, err := ct.c.IsMember(t.Context(), testChannel, alexID); member || err == nil {
			t.Errorf("IsMember: %t, %v; want no member and an error", member, err)
		}
	})

	t.Run("a read past the bound", func(t *testing.T) {
		t.Parallel()
		ct := newClientTest(t)
		ct.c.rosterTimeout = 50 * time.Millisecond
		ct.record(t, testChannel, testChannel)
		ct.f.setFail(func(r *http.Request) (int, string, bool) {
			if strings.HasSuffix(r.URL.Path, "/pagedmembers") {
				select { // past the bound, or answered at last: a Client with no bound gets the 404 of a roster it does not have
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			}
			return 0, "", false
		})
		start := time.Now()
		member, err := ct.c.IsMember(t.Context(), testChannel, alexID)
		if member || !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("IsMember: %t, %v; want no member and the bound's error", member, err)
		}
		if took := time.Since(start); took > 5*time.Second {
			t.Errorf("IsMember took %s, past its bound", took)
		}
		if v := ct.c.Classify(chat.OpOther, err); v.Class != chat.Transient {
			t.Errorf("verdict %+v, want Transient", v)
		}
	})
}

// endlessRoster answers every roster read with a page that lists no one sought, and whose next page is next of its own token.
func endlessRoster(next func(token string) string) func(r *http.Request) (int, string, bool) {
	return func(r *http.Request) (int, string, bool) {
		n := next(r.URL.Query().Get("continuationToken"))
		return http.StatusOK, `{"members":[{"id":"29:1stand-in-sam","aadObjectId":"` + samID + `"}],"continuationToken":"` + n + `"}`,
			strings.HasSuffix(r.URL.Path, "/pagedmembers")
	}
}

// The error of a roster read names no conversation and no person: it says only which kind of call failed.
func TestARosterErrorNamesNoConversation(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.c.api.client = &http.Client{Transport: roundTrip(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection reset")
	})}
	_, err := ct.c.IsMember(t.Context(), testChannel, alexID)
	if err == nil {
		t.Fatal("no error")
	}
	for _, leak := range []string{"19:", "thread.tacv2", alexID, "pageSize"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the error %q holds %q", err, leak)
		}
	}
	if !strings.Contains(err.Error(), "v3/conversations/…/pagedmembers") {
		t.Errorf("the error %q does not say which call failed", err)
	}
}

// IsBot says a 28: ID is a bot, with no call. A Microsoft Entra object ID is read as ResolveUser reads it: the account a roster lists
// for it is a bot when its Teams ID is 28:, and a person when it is anything else. One that no roster lists, or that cannot be read,
// is a bot with an error (it fails closed), and so is an ID of no Teams shape.
func TestIsBot(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.heardFrom(t, testChannel, testChannel)
	ct.f.setRoster(testChannel, alex, botMember)
	ctx := t.Context()
	if bot, err := ct.c.IsBot(ctx, "28:"+testAppID); err != nil || !bot {
		t.Errorf("a 28: ID: %t, %v", bot, err)
	}
	if n := len(ct.f.take()); n != 0 {
		t.Errorf("%d calls for a 28: ID", n)
	}
	if bot, err := ct.c.IsBot(ctx, alexID); err != nil || bot {
		t.Errorf("a person: %t, %v", bot, err)
	}
	if bot, err := ct.c.IsBot(ctx, rosterBot); err != nil || !bot {
		t.Errorf("an account of the roster with a 28: ID: %t, %v", bot, err)
	}
	for _, user := range []string{samID, "", sam.ID, "B0123456789"} {
		if bot, err := ct.c.IsBot(ctx, user); !bot || err == nil {
			t.Errorf("IsBot(%q): %t, %v; want a bot and an error", user, bot, err)
		}
	}

	unreadable := newClientTest(t)
	unreadable.heardFrom(t, testChannel, testChannel)
	unreadable.f.setRoster(testChannel, alex)
	failRoster(unreadable.f, http.StatusBadGateway)
	if bot, err := unreadable.c.IsBot(ctx, alexID); !bot || err == nil {
		t.Errorf("a roster that cannot be read: %t, %v; want a bot and an error", bot, err)
	}
}

// ResolveUser gives a person's name from the roster of each team the bot has heard from in this run, in order, and keeps it for the
// names' TTL: a second call makes none, and one after the TTL reads again. An ID that no roster lists, an ID of no person, and a
// connection that has heard from no team are errors. A team whose roster cannot be read does not hide a person another lists.
func TestResolveUserReadsTheRostersOfTheTeams(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	f := newFakeConnector(t)
	bf := newFakeBotFramework(t, newSigningKey(t, "k", "msteams"))
	c, err := New(Config{Connection: testConn, TenantID: testTenant, AppID: testAppID, ClientSecret: secret.New(testClientSecret),
		TokenURL: f.srv.URL + "/token", MetadataURL: bf.metadataURL(), HTTPClient: f.srv.Client(), Logger: testutil.DiscardLogger(),
		Now: clock.now})
	if err != nil {
		t.Fatal(err)
	}
	ct := &clientTest{c: c, f: f}
	ctx := t.Context()
	if _, err := c.ResolveUser(ctx, alexID); err == nil {
		t.Error("no team heard from: no error")
	}
	otherTeam := "19:KourierStandInOtherTeamChannel00000000000a@thread.tacv2"
	ct.heardFrom(t, otherTeam, otherTeam)
	ct.heardFrom(t, testChannel, testChannel)
	ct.f.setRoster(testChannel, sam)
	ct.f.setRoster(otherTeam, alex)
	ct.f.setFail(func(r *http.Request) (int, string, bool) { // the first team's roster, in order, cannot be read
		return http.StatusBadGateway, "", strings.Contains(r.URL.EscapedPath(), "/"+escapeID(testChannel)+"/pagedmembers")
	})
	if u, err := c.ResolveUser(ctx, alexID); err != nil || u != (chat.User{ID: alexID, Name: "Alex Example"}) {
		t.Fatalf("a person of the second team, the first unreadable: %+v, %v", u, err)
	}
	if got := rosterReads(t, ct.f.take(), testChannel); len(got) != 1 {
		t.Errorf("the first team was read %d times, want once, first", len(got))
	}
	ct.f.setFail(nil)
	if u, err := c.ResolveUser(ctx, alexID); err != nil || u.Name != "Alex Example" {
		t.Errorf("again: %+v, %v", u, err)
	}
	if n := len(ct.f.take()); n != 0 {
		t.Errorf("%d calls for a name it holds", n)
	}
	clock.advance(namesTTL)
	ct.f.setRoster(otherTeam, account{ID: alex.ID, Name: "Alex Renamed", AADObjectID: alexID})
	if u, err := c.ResolveUser(ctx, alexID); err != nil || u.Name != "Alex Renamed" {
		t.Errorf("after the TTL: %+v, %v", u, err)
	}
	if u, err := c.ResolveUser(ctx, samID); err != nil || u.Name != "Sam Example" {
		t.Errorf("a person of the first team: %+v, %v", u, err)
	}
	for _, id := range []string{stranger, "28:" + testAppID, sam.ID, ""} {
		if u, err := c.ResolveUser(ctx, id); err == nil {
			t.Errorf("ResolveUser(%q): %+v and no error", id, u)
		}
	}
}

// ResolveUser reads the teams in the order of their IDs, whatever the order they were heard from, and the read of one ID asks for
// at most rosterMaxPages pages in all, across every team: a roster whose tokens never end leaves none for the next team.
func TestResolveUserReadsTheTeamsInOrderWithinThePages(t *testing.T) {
	t.Parallel()
	teams := []string{
		"19:KourierStandInTeamC00000000000000000000000a@thread.tacv2",
		"19:KourierStandInTeamA00000000000000000000000a@thread.tacv2",
		"19:KourierStandInTeamB00000000000000000000000a@thread.tacv2",
	}
	ct := newClientTest(t)
	for _, team := range teams {
		ct.heardFrom(t, team, team)
		ct.f.setRoster(team, sam)
	}
	if _, err := ct.c.ResolveUser(t.Context(), stranger); !errors.Is(err, errUnknownPerson) {
		t.Errorf("a person no team lists: %v", err)
	}
	var order []string
	for _, r := range ct.f.take() {
		if conv, ok := strings.CutSuffix(strings.TrimPrefix(r.path, "/amer/"+testTenant+"/v3/conversations/"), "/pagedmembers"); ok {
			order = append(order, conv)
		}
	}
	if want := []string{escapeID(teams[1]), escapeID(teams[2]), escapeID(teams[0])}; fmt.Sprint(order) != fmt.Sprint(want) {
		t.Errorf("the teams were read in the order %q, want %q", order, want)
	}

	ct.f.setFail(endlessRoster(func(tok string) string { return tok + "x" }))
	if _, err := ct.c.ResolveUser(t.Context(), stranger); !errors.Is(err, errRosterTooLong) {
		t.Errorf("rosters that never end: %v", err)
	}
	if n := len(ct.f.take()); n != rosterMaxPages {
		t.Errorf("%d pages read across the teams, want %d", n, rosterMaxPages)
	}
}

// A team's service URL is the same at every call when its channels' records name more than one: the least of them.
func TestAKnownTeamHasOneServiceURL(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	other := "19:KourierStandInOtherTeamChannel00000000000a@thread.tacv2"
	ct.heardFrom(t, testChannel, testChannel)
	ct.c.recv.remember(t.Context(), Conversation{Channel: other, ServiceURL: "https://zz.example.test/", TeamID: testChannel, TenantID: testTenant})
	ct.c.recv.remember(t.Context(), Conversation{Channel: "19:KourierStandInNoServiceChannel000000000a@thread.tacv2", TeamID: testChannel})
	for range 20 {
		if got := ct.c.knownTeams(); len(got) != 1 || got[0].ServiceURL != ct.f.serviceURL() || got[0].TeamID != testChannel {
			t.Fatalf("knownTeams: %+v, want the one team at %s", got, ct.f.serviceURL())
		}
	}
}

// The names are a bounded cache: past namesMax the expired go first, then any, so that it never holds more; a name as old as the TTL
// is not given.
func TestTheNamesAreBounded(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	n := newTTLCache[string, account](clock.now, namesTTL, namesMax)
	for i := range namesMax {
		n.put(fmt.Sprintf("id-%d", i), account{ID: fmt.Sprintf("29:%d", i), Name: "x", AADObjectID: fmt.Sprintf("id-%d", i)})
	}
	clock.advance(namesTTL / 2)
	n.put("id-new", account{ID: "29:new", Name: "y", AADObjectID: "id-new"})
	if got := n.len(); got != namesMax {
		t.Errorf("%d names held, want %d", got, namesMax)
	}
	if a, ok := n.get("id-new"); !ok || a.Name != "y" {
		t.Errorf("the newest: %+v, %t", a, ok)
	}
	clock.advance(namesTTL / 2)
	if _, ok := n.get("id-new"); !ok {
		t.Error("a name younger than the TTL is not given")
	}
	held := 0
	for i := range namesMax {
		if _, ok := n.get(fmt.Sprintf("id-%d", i)); ok {
			held++
		}
	}
	if held != 0 {
		t.Errorf("%d names as old as the TTL are given", held)
	}
	n.put("id-newer", account{ID: "29:newer", Name: "z", AADObjectID: "id-newer"})
	if got := n.len(); got != 2 {
		t.Errorf("%d names held once the expired went, want 2", got)
	}
}

// A name is shown as its letters, marks, digits and the punctuation of names (. - '), cut at nameUnits; anything else, markup and
// controls among it, is a space between words, so no name ends the mention's tag, styles the line or makes a link.
func TestMentionName(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		"Alex Example":                  "Alex Example",
		"  Zoë   O'Brien-Smith  ":       "Zoë O'Brien-Smith",
		"李 小龍":                          "李 小龍",
		"मनीष":                          "मनीष",
		"</at><at>x</at> **b** _i_":     "at at x at b i",
		"[x](https://evil.test) {a}":    "x https evil.test a",
		"a\u202eb\u200bc\nd\te&lt;f":    "a b c d e lt f",
		"Jo \\* \\[x\\]":                "Jo x",
		"<script>alert(1)</script> Ann": "script alert 1 script Ann",
		"🙂":                             "",
		"":                              "",
		strings.Repeat("a", 200):        chat.Truncate(strings.Repeat("a", 200), nameUnits),
	} {
		if got := mentionName(in); got != want {
			t.Errorf("mentionName(%q) = %q, want %q", in, got, want)
		}
		if chat.Units(mentionName(in)) > nameUnits {
			t.Errorf("mentionName(%q) is over %d units", in, nameUnits)
		}
	}
}

// The ending of a question names its decider, read from the roster of the prompt's channel: the mention's tag and its entity hold the
// person's name, cleaned. A name that cannot be read, or that has nothing left once cleaned, is "someone", the mention is kept, and the
// edit is made either way.
func TestAnEndingNamesItsDecider(t *testing.T) {
	t.Parallel()
	ending := chat.End(chat.Ending{Outcome: chat.Answered, By: alexID, Prompt: chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "Why?"}}},
		Answers: [][]string{{"(answered)"}}})
	ref := chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1790000000200", Thread: "1790000000100"}
	for _, tc := range []struct {
		name   string
		roster []account
		fail   bool
		want   string
	}{
		{"a member", []account{sam, alex}, false, "Alex Example"},
		{"a hostile name", []account{{ID: alex.ID, AADObjectID: alexID, Name: "</at>[x](https://evil.test) **b**"}}, false, "at x https evil.test b"},
		{"a name of nothing shown", []account{{ID: alex.ID, AADObjectID: alexID, Name: "🙂"}}, false, mentionLabel},
		{"no longer a member", []account{sam}, false, mentionLabel},
		{"a roster that cannot be read", []account{alex}, true, mentionLabel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ct := newClientTest(t)
			ct.record(t, testChannel, testChannel)
			ct.f.setRoster(testChannel, tc.roster...)
			if tc.fail {
				failRoster(ct.f, http.StatusServiceUnavailable)
			}
			if err := ct.c.Update(t.Context(), ref, ending); err != nil {
				t.Fatalf("Update: %v", err)
			}
			card := editedCard(t, ct.f.take())
			tag := "<at>" + tc.want + "</at>"
			if lines := outline(t, card); lines[0] != "block:"+tag+" answered." {
				t.Errorf("the ending says %q, want %q", lines[0], tag+" answered.")
			}
			entities := card["msteams"].(map[string]any)["entities"].([]any)
			e := entities[0].(map[string]any)
			if m := e["mentioned"].(map[string]any); len(entities) != 1 || e["text"] != tag || m["id"] != alexID || m["name"] != tc.want {
				t.Errorf("entities %v", entities)
			}
		})
	}
}

// editedCard is the card of the one edit among reqs, and fails the test unless there is one.
func editedCard(t *testing.T, reqs []connRequest) map[string]any {
	t.Helper()
	var cards []map[string]any
	for _, r := range reqs {
		if r.method == http.MethodPut && len(r.activity.Attachments) == 1 {
			cards = append(cards, r.activity.Attachments[0].Content)
		}
	}
	if len(cards) != 1 {
		t.Fatalf("%d edits with a card, want 1", len(cards))
	}
	return cards[0]
}

// A decider whose name the Client holds is named with no roster read, and a decider who is no Entra object ID, or none, is not looked
// up.
func TestAnEndingNamesAKnownDeciderWithNoRead(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.f.setRoster(testChannel, alex)
	ref := chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1790000000200", Thread: "1790000000100"}
	ctx := t.Context()
	if member, err := ct.c.IsMember(ctx, testChannel, alexID); err != nil || !member {
		t.Fatalf("IsMember: %t, %v", member, err)
	}
	ct.f.take()
	for by, want := range map[string]string{alexID: "block:<at>Alex Example</at> cancelled the question.",
		"U0FORGED": "text:someone cancelled the question.", "": "text:The question was cancelled."} {
		e := chat.End(chat.Ending{Outcome: chat.Cancelled, By: by, Prompt: chat.Prompt{ID: "ix-1", Questions: []chat.Question{{Text: "Why?"}}}})
		if err := ct.c.Update(ctx, ref, e); err != nil {
			t.Fatalf("Update by %q: %v", by, err)
		}
		reqs := ct.f.take()
		if reads := rosterReads(t, reqs, testChannel); len(reads) != 0 {
			t.Errorf("an ending by %q read the roster %d times", by, len(reads))
		}
		if line := outline(t, editedCard(t, reqs))[0]; line != want {
			t.Errorf("an ending by %q says %q, want %q", by, line, want)
		}
	}
}

// A press on Microsoft Teams is decided end to end on the fake Bot Connector: a signed invoke comes to the route and is recorded with
// the presser's Entra object ID; the two checks that the press worker makes of the presser (interact and feedback: a member of the
// prompt's channel, then no bot) pass for a person of the team's roster and refuse a stranger; and the ending that the decision puts
// on the card names the presser.
func TestAPressIsDecidedEndToEnd(t *testing.T) {
	ct := newClientTest(t)
	ct.f.setRoster(testChannel, sam, testUserA)
	sink := &pressSink{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ct.c.recv.Serve(ctx, sink) }()
	t.Cleanup(func() { cancel(); _ = <-done }) // Serve says only that it was cancelled
	testutil.Eventually(t, "the receiver takes requests", func() bool { return ct.c.recv.live.Load() != nil })

	press := func(id, user string) {
		t.Helper()
		body := editFixture(t, "invoke_action", func(m map[string]any) {
			m["id"] = id
			m["serviceUrl"] = ct.f.serviceURL()
			m["from"].(map[string]any)["aadObjectId"] = user
			m["value"].(map[string]any)["action"].(map[string]any)["data"] = promptData("submit")
		})
		claims := claimsAt(time.Now())
		claims["serviceurl"] = ct.f.serviceURL()
		req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/chat/teams/"+testConn, bytes.NewReader(body))
		req.Header.Set("Authorization", bearer(t, ct.key, claims))
		rec := httptest.NewRecorder()
		h, _ := ct.c.HTTPHandler()
		h.ServeHTTP(rec, req)
		if got := told(t, rec); got != textPromptReceived {
			t.Fatalf("the press was told %q", got)
		}
	}
	press("f:e2e-1", testUser)
	press("f:e2e-2", stranger)
	// The receiver records the channel as lane work after the press reached the sink: wait for it, or the roster read below
	// goes to the default service URL instead of the fake.
	testutil.Eventually(t, "the channel is recorded", func() bool {
		_, ok, _ := ct.convs.Lookup(t.Context(), testConn, testChannel)
		return ok
	})
	presses := sink.taken()
	if len(presses) != 2 {
		t.Fatalf("the sink was given %d presses", len(presses))
	}
	for i, want := range []bool{true, false} {
		p := presses[i]
		member, err := ct.c.IsMember(t.Context(), p.At.Channel, p.User)
		if err != nil || member != want {
			t.Fatalf("press %d: IsMember %t, %v; want %t", i, member, err, want)
		}
		if !member {
			continue
		}
		if bot, err := ct.c.IsBot(t.Context(), p.User); err != nil || bot {
			t.Errorf("press %d: IsBot %t, %v; want a person", i, bot, err)
		}
		ct.f.take()
		e := chat.End(chat.Ending{Outcome: chat.Answered, By: p.User, Prompt: chat.Prompt{ID: p.PromptID, Questions: []chat.Question{{Text: "Why?"}}}})
		if err := ct.c.Update(t.Context(), p.At, e); err != nil {
			t.Fatalf("press %d: the ending: %v", i, err)
		}
		if line := outline(t, editedCard(t, ct.f.take()))[0]; line != "block:<at>Test User</at> answered." {
			t.Errorf("press %d: the ending says %q", i, line)
		}
	}
}

// A message's mention (chat.Mention) names its person as the ending names its decider: the tag and the entity hold the name that the
// roster of the message's channel gives, cleaned, and "someone" when it cannot be read; the post or the edit is made either way.
func TestAMessagesMentionNamesItsPerson(t *testing.T) {
	t.Parallel()
	msg := chat.Say(chat.Message{Fallback: "Alex was named in the reply above.",
		Parts: []chat.Part{chat.Mention{User: alexID, Text: "was named as alex in the reply above."}}})
	for _, tc := range []struct {
		name   string
		roster []account
		fail   bool
		want   string
	}{
		{"a member", []account{sam, alex}, false, "Alex Example"},
		{"a hostile name", []account{{ID: alex.ID, AADObjectID: alexID, Name: "</at>[x](https://evil.test) **b**"}}, false, "at x https evil.test b"},
		{"no longer a member", []account{sam}, false, mentionLabel},
		{"a roster that cannot be read", []account{alex}, true, mentionLabel},
	} {
		for _, op := range []string{"post", "update"} {
			t.Run(tc.name+", "+op, func(t *testing.T) {
				t.Parallel()
				ct := newClientTest(t)
				ct.record(t, testChannel, testChannel)
				ct.f.setRoster(testChannel, tc.roster...)
				if tc.fail {
					failRoster(ct.f, http.StatusServiceUnavailable)
				}
				var err error
				if op == "post" {
					_, err = ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel, Thread: "1790000000100"}, msg, findTag)
				} else {
					err = ct.c.Update(t.Context(), chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1790000000200",
						Thread: "1790000000100"}, msg)
				}
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
				calls := activityCalls(ct.f.take())
				if len(calls) != 1 {
					t.Fatalf("%d posts or edits, want 1", len(calls))
				}
				a := calls[0].activity
				tag := "<at>" + tc.want + "</at>"
				if !strings.HasPrefix(a.Text, tag+" was named as alex") {
					t.Errorf("the text is %q, want it to start with %q", a.Text, tag)
				}
				if len(a.Entities) != 1 || a.Entities[0].Text != tag || a.Entities[0].Mentioned.ID != alexID || a.Entities[0].Mentioned.Name != tc.want {
					t.Errorf("entities %+v", a.Entities)
				}
			})
		}
	}
}

// The people of one message are read in one read of the channel's roster, with one budget of pages: the read stops at the page that
// lists the last of them, and a person mentioned twice is looked up once.
func TestAMessagesMentionsAreReadInOneRosterRead(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.f.setRoster(testChannel, sam, alex, testUserA, botMember)
	ct.f.setPageMax(1)
	msg := chat.Say(chat.Message{Fallback: "Alex and the test user were named.", Parts: []chat.Part{
		chat.Mention{User: alexID, Text: "first."}, chat.Mention{User: testUser, Text: "second."}, chat.Mention{User: alexID, Text: "again."},
	}})
	if _, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, msg, findTag); err != nil {
		t.Fatal(err)
	}
	reqs := ct.f.take()
	if got := rosterReads(t, reqs, testChannel); strings.Join(got, ",") != ",1,2" {
		t.Errorf("roster pages read %q, want the first three once each", got)
	}
	calls := activityCalls(reqs)
	if len(calls) != 1 {
		t.Fatalf("%d posts, want 1", len(calls))
	}
	text := calls[0].activity.Text
	for _, want := range []string{"<at>Alex Example</at> first.", "<at>Test User</at> second.", "<at>Alex Example</at> again."} {
		if !strings.Contains(text, want) {
			t.Errorf("the text %q lacks %q", text, want)
		}
	}
}

// A mention that could not be named is not read again for namesMissTTL: a person the channel's roster does not list is named as
// someone with no read in that channel, and a roster that could not be read names anyone mentioned in its channel so. A person of the
// channel whom no miss covers is read; past the TTL, the missed are read again. A read that the caller ended keeps no miss.
func TestAMentionThatCannotBeNamedIsNotReadAgainForAWhile(t *testing.T) {
	t.Parallel()
	mention := func(user string) chat.Outgoing {
		return chat.Say(chat.Message{Fallback: "named.", Parts: []chat.Part{chat.Mention{User: user, Text: "named."}}})
	}
	for _, tc := range []struct {
		name string
		fail bool
		// otherRead says whether a mention of another person of the channel, sam, reads the roster while the miss is kept.
		otherRead bool
	}{
		{"a person the roster does not list", false, true},
		{"a roster that cannot be read", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newFakeClock()
			ct := newClientTest(t)
			ct.c.misses = newTTLCache[missKey, struct{}](clock.now, namesMissTTL, namesMissMax)
			ct.record(t, testChannel, testChannel)
			ct.f.setRoster(testChannel, sam)
			if tc.fail {
				failRoster(ct.f, http.StatusServiceUnavailable)
			}
			post := func(user string) (reads int, text string) {
				t.Helper()
				if _, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, mention(user), findTag); err != nil {
					t.Fatal(err)
				}
				reqs := ct.f.take()
				calls := activityCalls(reqs)
				if len(calls) != 1 {
					t.Fatalf("%d posts, want 1", len(calls))
				}
				return len(rosterReads(t, reqs, testChannel)), calls[0].activity.Text
			}
			if reads, _ := post(alexID); reads == 0 {
				t.Fatal("the first mention read no roster")
			}
			reads, text := post(alexID)
			if reads != 0 {
				t.Errorf("a missed mention read the roster %d times again within the TTL", reads)
			}
			if !strings.HasPrefix(text, "<at>"+mentionLabel+"</at>") {
				t.Errorf("a missed mention shows %q, want it named as %s", text, mentionLabel)
			}
			if reads, _ := post(samID); (reads > 0) != tc.otherRead {
				t.Errorf("another person's mention read the roster %d times, want a read: %t", reads, tc.otherRead)
			}
			clock.advance(namesMissTTL)
			if reads, _ := post(alexID); reads == 0 {
				t.Error("a mention past the miss's TTL read no roster")
			}
		})
	}
	t.Run("a read the caller ended", func(t *testing.T) {
		t.Parallel()
		ct := newClientTest(t)
		ct.record(t, testChannel, testChannel)
		ct.f.setRoster(testChannel, sam)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if names := ct.c.mentionNames(ctx, testChannel, mention(alexID)); len(names) != 0 {
			t.Errorf("names %v from an ended read", names)
		}
		if n := ct.c.misses.len(); n != 0 {
			t.Errorf("%d misses kept from a read the caller ended, want 0", n)
		}
	})
}

// A post or an edit that is not valid is refused before the names of its mentions are read: it reads no roster.
func TestAnInvalidMessageReadsNoRoster(t *testing.T) {
	t.Parallel()
	ct := newClientTest(t)
	ct.record(t, testChannel, testChannel)
	ct.f.setRoster(testChannel, alex)
	bad := chat.Outgoing{Message: &chat.Message{Fallback: "no version.", Parts: []chat.Part{chat.Mention{User: alexID, Text: "named."}}}}
	if _, err := ct.c.Post(t.Context(), chat.Target{Conn: testConn, Channel: testChannel}, bad, findTag); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("post: %v, want chat.ErrInvalid", err)
	}
	if err := ct.c.Update(t.Context(), chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1790000000200"}, bad); !errors.Is(err, chat.ErrInvalid) {
		t.Errorf("update: %v, want chat.ErrInvalid", err)
	}
	if reqs := ct.f.take(); len(reqs) != 0 {
		t.Errorf("%d calls for a message that is not valid, want none: %+v", len(reqs), reqs)
	}
}
