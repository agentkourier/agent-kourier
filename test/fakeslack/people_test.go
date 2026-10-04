package fakeslack_test

import (
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func TestUsersInfo(t *testing.T) {
	fake, api := newFake(t)
	fake.AddUser(fakeslack.User{ID: "U1", Name: "ada", RealName: "Ada Lovelace", Email: "ada@example.com"})
	fake.AddUser(fakeslack.User{ID: "B1", Name: "helper", IsBot: true})

	u, err := api.GetUserInfo("U1")
	if err != nil || u.ID != "U1" || u.Name != "ada" || u.RealName != "Ada Lovelace" ||
		u.Profile.Email != "ada@example.com" || u.IsBot {
		t.Fatalf("GetUserInfo(U1) = %+v, %v", u, err)
	}
	if b, err := api.GetUserInfo("B1"); err != nil || !b.IsBot || b.Profile.Email != "" {
		t.Fatalf("GetUserInfo(B1) = %+v, %v; want a bot without an email", b, err)
	}
	for user, want := range map[string]string{"U404": "user_not_found", "": "invalid_arguments"} {
		if _, err := api.GetUserInfo(user); err == nil || err.Error() != want {
			t.Errorf("GetUserInfo(%q) = %v; want %s", user, err, want)
		}
	}
}

func TestUsersInfoWithoutDisplayName(t *testing.T) {
	fake, api := newFake(t)
	fake.AddUser(fakeslack.User{ID: "U2", Name: "grace"})
	u, err := api.GetUserInfo("U2")
	if err != nil || u.Name != "grace" || u.Profile.DisplayName != "" {
		t.Fatalf("GetUserInfo(U2) = %+v, %v; want a name and an empty display name", u, err)
	}
	fake.AddUser(fakeslack.User{ID: "U3", Name: "linus", DisplayName: "Linus T"})
	if u, err = api.GetUserInfo("U3"); err != nil || u.Profile.DisplayName != "Linus T" {
		t.Fatalf("GetUserInfo(U3) = %+v, %v", u, err)
	}
}

func TestSetMembersDedupesCopiesAndAllowsEmpty(t *testing.T) {
	fake, api := newFake(t)
	members := func(channel string) []string {
		got, _, err := api.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: channel, Limit: 2})
		if err != nil {
			t.Fatalf("members of %s: %v", channel, err)
		}
		return got
	}

	mine := []string{"U1", "U2", "U1", "U3", "U2"}
	fake.SetMembers("C1", mine...)
	mine[0] = "tampered"
	// Duplicates would repeat under a cursor keyed on the ID; they must not.
	var all []string
	cursor := ""
	for range 5 {
		page, next, err := api.GetUsersInConversation(&slack.GetUsersInConversationParameters{
			ChannelID: "C1", Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatalf("page: %v", err)
		}
		all, cursor = append(all, page...), next
		if cursor == "" {
			break
		}
	}
	if fmt.Sprint(all) != "[U1 U2 U3]" {
		t.Fatalf("members = %v; want [U1 U2 U3], deduped, in order, unaffected by the caller's slice", all)
	}

	fake.SetMembers("C2")
	if got := members("C2"); len(got) != 0 {
		t.Fatalf("empty channel members = %v; want none", got)
	}
	body := rawBody(t, fake, "conversations.members", url.Values{"token": {testToken}, "channel": {"C2"}})
	if !strings.Contains(body, `"members":[]`) {
		t.Fatalf("body = %s; want \"members\":[] rather than null", body)
	}
}

func TestConversationsMembers(t *testing.T) {
	fake, api := newFake(t)
	fake.SetMembers("C1", "U1", "U2", "U3")

	var got []string
	cursor := ""
	for page := 1; page <= 2; page++ {
		members, next, err := api.GetUsersInConversation(&slack.GetUsersInConversationParameters{
			ChannelID: "C1", Limit: 2, Cursor: cursor})
		if err != nil || (next != "") != (page == 1) {
			t.Fatalf("page %d = %v, cursor %q, %v", page, members, next, err)
		}
		got, cursor = append(got, members...), next
	}
	if fmt.Sprint(got) != "[U1 U2 U3]" {
		t.Fatalf("members = %v; want [U1 U2 U3]", got)
	}

	fake.SetMembers("C1", "U9") // replaces the membership
	if members, _, err := api.GetUsersInConversation(&slack.GetUsersInConversationParameters{ChannelID: "C1"}); err != nil ||
		fmt.Sprint(members) != "[U9]" {
		t.Fatalf("members after SetMembers = %v, %v", members, err)
	}
	for _, p := range []slack.GetUsersInConversationParameters{{ChannelID: "C404"}, {}} {
		if _, _, err := api.GetUsersInConversation(&p); err == nil || err.Error() != "channel_not_found" {
			t.Errorf("members of %q = %v; want channel_not_found", p.ChannelID, err)
		}
	}
	for want, p := range map[string]slack.GetUsersInConversationParameters{
		"invalid_cursor": {ChannelID: "C1", Cursor: "bogus"},
		"invalid_limit":  {ChannelID: "C1", Limit: -1},
	} {
		if _, _, err := api.GetUsersInConversation(&p); err == nil || err.Error() != want {
			t.Errorf("members with %+v = %v; want %s", p, err, want)
		}
	}
}

func TestOldestLatestAndInclusive(t *testing.T) {
	_, api := newFake(t)
	a, b, c := post(t, api, text("a")), post(t, api, text("b")), post(t, api, text("c"))
	r1 := post(t, api, text("r1"), slack.MsgOptionTS(b))
	r2 := post(t, api, text("r2"), slack.MsgOptionTS(b))

	for name, tc := range map[string]struct {
		p    slack.GetConversationHistoryParameters
		want []string
	}{
		"oldest exclusive": {slack.GetConversationHistoryParameters{Oldest: b}, []string{c}},
		"oldest inclusive": {slack.GetConversationHistoryParameters{Oldest: b, Inclusive: true}, []string{c, b}},
		"latest exclusive": {slack.GetConversationHistoryParameters{Latest: b}, []string{a}},
		"latest inclusive": {slack.GetConversationHistoryParameters{Latest: b, Inclusive: true}, []string{b, a}},
		"window":           {slack.GetConversationHistoryParameters{Oldest: a, Latest: c}, []string{b}},
	} {
		wantTS(t, name, history(t, api, tc.p).Messages, tc.want...)
	}

	// Replies always lead with the thread parent; the window applies to the replies.
	msgs, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: "C1", Timestamp: b, Oldest: r1})
	if err != nil {
		t.Fatalf("GetConversationReplies: %v", err)
	}
	wantTS(t, "replies oldest", msgs, b, r2)

	for want, p := range map[string]slack.GetConversationHistoryParameters{
		"invalid_ts_oldest": {ChannelID: "C1", Oldest: "soon"},
		"invalid_ts_latest": {ChannelID: "C1", Latest: "later"},
	} {
		if _, err := api.GetConversationHistory(&p); err == nil || err.Error() != want {
			t.Errorf("history with %+v = %v; want %s", p, err, want)
		}
	}
	// Confirmed in the dogfood of 2026-10-04: an oldest that is not a ts after the epoch is refused, whatever it is
	// wrong with. "0.000000" is what the dry run sent for "from the start", and the zero time.Time formats as the
	// negative one; sending no oldest is the way to read it all.
	for _, oldest := range []string{"0", "0.000000", "-1", "-62135596800.000000", "1.2.3", "1e9", "1.1234567", ".5", " 1"} {
		p := slack.GetConversationHistoryParameters{ChannelID: "C1", Oldest: oldest}
		if _, err := api.GetConversationHistory(&p); err == nil || err.Error() != "invalid_ts_oldest" {
			t.Errorf("history with oldest %q = %v; want invalid_ts_oldest", oldest, err)
		}
	}
	if r := history(t, api, slack.GetConversationHistoryParameters{ChannelID: "C1"}); len(r.Messages) == 0 {
		t.Errorf("history with no oldest returned nothing; want the whole channel")
	}
	for want, p := range map[string]slack.GetConversationRepliesParameters{
		"invalid_ts_oldest": {ChannelID: "C1", Timestamp: b, Oldest: "soon"},
		"invalid_ts_latest": {ChannelID: "C1", Timestamp: b, Latest: "later"},
	} {
		if _, _, _, err := api.GetConversationReplies(&p); err == nil || err.Error() != want {
			t.Errorf("replies with %+v = %v; want %s", p, err, want)
		}
	}
	// With only oldest set, paging starts at the newest message and walks back
	// toward oldest. A limit of 1 shows which end it starts from.
	page := history(t, api, slack.GetConversationHistoryParameters{Oldest: a, Limit: 1})
	wantTS(t, "only oldest, page 1", page.Messages, c)
	page = history(t, api, slack.GetConversationHistoryParameters{Oldest: a, Limit: 1,
		Cursor: page.ResponseMetaData.NextCursor})
	wantTS(t, "only oldest, page 2", page.Messages, b)
	if page.ResponseMetaData.NextCursor != "" {
		t.Fatalf("cursor after the last page = %q; want none", page.ResponseMetaData.NextCursor)
	}
}
