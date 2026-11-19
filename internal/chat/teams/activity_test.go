package teams

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The fixtures in testdata. message_mention and invoke_action are payloads recorded on a test tenant on 2026-11-18, with the person, the
// tenant, the bot, the team's group, the channel and the team's name replaced by stand-ins of the same shape and length. message_plain and message_reply are that payload with the IDs, conversation and reply chain of activities
// recorded in summary the same day (a plain post with no mention, and a reply in an existing thread), whose text was not recorded.
// message_update, message_delete, conversation_update and installation_update follow the shapes of Microsoft's documentation and were
// not recorded: the spike saw none.
const (
	testChannel = "19:KourierStandInChannelForTestsOnly0000000000a@thread.tacv2"
	testGroup   = "33333333-4444-4555-8666-777777777777"
	testUser    = "00000000-0000-4000-8000-000000000001"
	testConn    = "ns/teams"
)

// fixture is the raw body of the testdata file name.json.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name+".json"))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return b
}

// editFixture is the fixture name with edit applied to it as a JSON object.
func editFixture(t testing.TB, name string, edit func(m map[string]any)) []byte {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(fixture(t, name), &m); err != nil {
		t.Fatal(err)
	}
	edit(m)
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decoded(t testing.TB, body []byte) activity {
	t.Helper()
	a, err := decodeActivity(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return a
}

// TestMessageOfRecordedActivities maps the recorded message activities to the neutral model: the channel and the thread root from
// conversation.id, whose ;messageid= names the root even for a new top-level post (which then has no Thread), the person by their
// Microsoft Entra object ID, the text verbatim, and the mention of the bot by its entity.
func TestMessageOfRecordedActivities(t *testing.T) {
	ref := func(id, thread string) chat.Ref {
		return chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: id, Thread: thread}
	}
	cases := []struct {
		fixture string
		want    chat.Inbound
	}{
		{"message_mention", chat.Inbound{At: ref("1794965322719", ""), User: testUser,
			Text: "<at>Agent Kourier</at> echo two-socket test", Mentioned: true}},
		{"message_plain", chat.Inbound{At: ref("1794965057510", ""), User: testUser, Text: "a plain post with no mention"}},
		{"message_reply", chat.Inbound{At: ref("1794965132871", "1794965033859"), User: testUser, Text: "a reply in the thread"}},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			a := decoded(t, fixture(t, tc.fixture))
			if got := a.kind(); got != kindMessage {
				t.Fatalf("kind %v, want a message", got)
			}
			m, v, why := a.message(testConn, testAppID)
			if v != forward {
				t.Fatalf("verdict %v (%s), want forward", v, why)
			}
			if m.At != tc.want.At || m.User != tc.want.User || m.Text != tc.want.Text || m.Mentioned != tc.want.Mentioned || m.Direct || m.Bot != nil {
				t.Errorf("got %+v, want %+v", m, tc.want)
			}
		})
	}
}

// TestMessageMentionIsTheEntity: the bot is mentioned when a mention entity names 28:<appID>, and never by the <at> text alone.
func TestMessageMentionIsTheEntity(t *testing.T) {
	cases := []struct {
		name string
		edit func(m map[string]any)
		want bool
	}{
		{"the recorded mention", func(map[string]any) {}, true},
		{"the <at> text with no entity", func(m map[string]any) { m["entities"] = []any{} }, false},
		{"an entity of another bot", func(m map[string]any) {
			m["entities"] = []any{map[string]any{"type": "mention", "text": "<at>Agent Kourier</at>",
				"mentioned": map[string]any{"id": "28:00000000-0000-4000-8000-0000000000b0", "name": "Agent Kourier"}}}
		}, false},
		{"an entity of a person", func(m map[string]any) {
			m["entities"] = []any{map[string]any{"type": "mention", "mentioned": map[string]any{"id": "29:someone", "name": "Agent Kourier"}}}
		}, false},
		{"the bot's ID in an entity of another type", func(m map[string]any) {
			m["entities"] = []any{map[string]any{"type": "clientInfo", "mentioned": map[string]any{"id": "28:" + testAppID}}}
		}, false},
		{"the entity with text that does not say so", func(m map[string]any) {
			m["text"] = "no tag here"
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m, v, why := decoded(t, editFixture(t, "message_mention", tc.edit)).message(testConn, testAppID)
			if v != forward {
				t.Fatalf("verdict %v (%s), want forward", v, why)
			}
			if m.Mentioned != tc.want {
				t.Errorf("Mentioned %v, want %v", m.Mentioned, tc.want)
			}
		})
	}
}

// TestMessageDropsWhatIsNoPersonsPost: Agent Kourier's own posts (which Teams does not send, but a 28: sender is a bot whatever it is),
// other bots' posts (whose alerts come through Graph), a post with no Entra user or no text, and a conversation that is not a channel
// are not forwarded; an ID that holds ':' is invalid, because chat.MessageID joins on it.
func TestMessageDropsWhatIsNoPersonsPost(t *testing.T) {
	cases := []struct {
		name string
		edit func(m map[string]any)
		want verdict
	}{
		{"Agent Kourier's own post", func(m map[string]any) {
			m["from"] = map[string]any{"id": "28:" + testAppID, "name": "Agent Kourier"}
		}, ownPost},
		{"another bot's post", func(m map[string]any) {
			m["from"] = map[string]any{"id": "28:00000000-0000-4000-8000-0000000000b0", "name": "Workflows"}
		}, dropped},
		{"a bot's post that names an Entra object", func(m map[string]any) {
			m["from"] = map[string]any{"id": "28:00000000-0000-4000-8000-0000000000b0", "aadObjectId": testUser}
		}, dropped},
		{"no Entra object ID", func(m map[string]any) { m["from"] = map[string]any{"id": "29:someone", "name": "Someone"} }, dropped},
		{"no text", func(m map[string]any) { m["text"] = "" }, dropped},
		{"a personal conversation", func(m map[string]any) {
			m["conversation"] = map[string]any{"id": "a:1personal", "conversationType": "personal", "tenantId": testTenant}
		}, dropped},
		{"a group chat", func(m map[string]any) {
			m["conversation"] = map[string]any{"id": "19:chat@thread.v2", "conversationType": "groupChat", "tenantId": testTenant}
		}, dropped},
		{"an ID with a colon", func(m map[string]any) { m["id"] = "1:794965322719" }, invalid},
		{"no thread marker: the post is its own root", func(m map[string]any) {
			m["conversation"].(map[string]any)["id"] = testChannel
		}, forward},
		{"no ID", func(m map[string]any) { delete(m, "id") }, invalid},
		{"a conversation with no channel", func(m map[string]any) {
			m["conversation"].(map[string]any)["id"] = ";messageid=1794965322719"
		}, invalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, v, _ := decoded(t, editFixture(t, "message_mention", tc.edit)).message(testConn, testAppID)
			if v != tc.want {
				t.Errorf("verdict %v, want %v", v, tc.want)
			}
		})
	}
}

// TestKindOfActivities: edits and deletions are ignored, as the Slack adapter ignores a message's edits and deletions; the install
// events are conversation records; an invoke goes to its own seam; anything else is ignored.
func TestKindOfActivities(t *testing.T) {
	cases := []struct {
		fixture string
		want    activityKind
	}{
		{"message_mention", kindMessage},
		{"message_plain", kindMessage},
		{"message_reply", kindMessage},
		{"message_update", kindIgnored},
		{"message_delete", kindIgnored},
		{"conversation_update", kindIgnored},
		{"installation_update", kindIgnored},
		{"invoke_action", kindInvoke},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			if got := decoded(t, fixture(t, tc.fixture)).kind(); got != tc.want {
				t.Errorf("kind %v, want %v", got, tc.want)
			}
		})
	}
	for _, typ := range []string{"typing", "messageReaction", "event", ""} {
		body := editFixture(t, "message_mention", func(m map[string]any) { m["type"] = typ })
		if got := decoded(t, body).kind(); got != kindIgnored {
			t.Errorf("type %q: kind %v, want ignored", typ, got)
		}
	}
}

// TestConversationOfActivities: every activity in a team tells the conversation store what Teams sends only inbound, and an activity
// that leaves a field out (an invoke has no aadGroupId) says nothing of it.
func TestConversationOfActivities(t *testing.T) {
	full := Conversation{Channel: testChannel, ServiceURL: testService, TeamID: testChannel, AADGroupID: testGroup, TenantID: testTenant}
	noGroup := full
	noGroup.AADGroupID = ""
	cases := []struct {
		fixture string
		want    Conversation
	}{
		{"conversation_update", full},
		{"installation_update", full},
		{"message_mention", full},
		{"message_reply", full},
		{"message_update", full},
		{"invoke_action", noGroup},
	}
	for _, tc := range cases {
		t.Run(tc.fixture, func(t *testing.T) {
			got, ok := decoded(t, fixture(t, tc.fixture)).conversationRecord()
			if !ok || got != tc.want {
				t.Errorf("got %+v, %v; want %+v", got, ok, tc.want)
			}
		})
	}
	personal := editFixture(t, "message_mention", func(m map[string]any) {
		m["conversation"] = map[string]any{"id": "a:1personal", "conversationType": "personal"}
		delete(m["channelData"].(map[string]any), "team")
	})
	if c, ok := decoded(t, personal).conversationRecord(); ok {
		t.Errorf("a personal conversation is recorded: %+v", c)
	}
}

// TestInvokeOfTheRecordedPress maps the recorded Action.Execute press to what the invoke seam is given: the card's message as the
// reference, the presser, the name and the raw value.
func TestInvokeOfTheRecordedPress(t *testing.T) {
	inv, ok := decoded(t, fixture(t, "invoke_action")).invoke(testConn)
	if !ok {
		t.Fatal("the recorded press is refused")
	}
	if _, ok := decoded(t, editFixture(t, "invoke_action", func(m map[string]any) { m["replyToId"] = "1:2" })).invoke(testConn); ok {
		t.Error("a replyToId with ':' is taken")
	}
	want := chat.Ref{Platform: PlatformName, Conn: testConn, Channel: testChannel, ID: "1790000000200", Thread: "1790000000100"}
	if inv.At != want || inv.User != testUser || inv.Name != "adaptiveCard/action" || inv.ID != "f:00000000-0000-4000-8000-0000000000c3" {
		t.Errorf("got %+v", inv)
	}
	if !strings.Contains(string(inv.Value), `"verb": "press"`) {
		t.Errorf("value %s lacks the verb", inv.Value)
	}
}

// TestDecodeActivityRefusesWhatIsNoActivity: a body that is not a JSON object, or an activity of another channel, is refused.
func TestDecodeActivityRefusesWhatIsNoActivity(t *testing.T) {
	if _, err := decodeActivity(editFixture(t, "invoke_action", func(m map[string]any) {
		m["id"] = "f:" + strings.Repeat("a", maxActivityID-2)
	})); err != nil {
		t.Errorf("an ID of maxActivityID bytes: %v", err)
	}
	for name, body := range map[string][]byte{
		"empty":         nil,
		"not JSON":      []byte("not json"),
		"an array":      []byte("[]"),
		"other channel": editFixture(t, "message_mention", func(m map[string]any) { m["channelId"] = "slack" }),
		"no channel":    editFixture(t, "message_mention", func(m map[string]any) { delete(m, "channelId") }),
		"an ID past maxActivityID": editFixture(t, "message_mention", func(m map[string]any) {
			m["id"] = strings.Repeat("1", maxActivityID+1)
		}),
	} {
		if _, err := decodeActivity(body); err == nil {
			t.Errorf("%s: decoded", name)
		}
	}
}
