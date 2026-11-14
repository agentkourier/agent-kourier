package fakeslack_test

import (
	"errors"
	"net/url"
	"reflect"
	"testing"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

func TestSetStatusIsAnsweredOkAndKeptInOrderPerChannel(t *testing.T) {
	fake, api := newFake(t)
	root := post(t, api, text("root"))
	for _, status := range []string{"is working…", "is working…", ""} {
		if err := api.SetAssistantThreadsStatus(slack.AssistantThreadsSetStatusParameters{ChannelID: "C1", ThreadTS: root, Status: status}); err != nil {
			t.Fatalf("SetAssistantThreadsStatus(%q) = %v", status, err)
		}
	}
	if err := api.SetAssistantThreadsStatus(slack.AssistantThreadsSetStatusParameters{ChannelID: "C2", ThreadTS: "1.1", Status: "other"}); err != nil {
		t.Fatal(err)
	}
	want := []fakeslack.ThreadStatus{{Channel: "C1", ThreadTS: root, Status: "is working…"}, {Channel: "C1", ThreadTS: root, Status: "is working…"}, {Channel: "C1", ThreadTS: root}}
	got := fake.ThreadStatuses("C1")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ThreadStatuses = %+v; want %+v, an empty status being a clear", got, want)
	}
	got[0].Status = "changed" // a copy
	if again := fake.ThreadStatuses("C1"); again[0].Status != "is working…" {
		t.Errorf("ThreadStatuses after a change to a copy = %+v", again)
	}
	if none := fake.ThreadStatuses("C3"); none == nil || len(none) != 0 {
		t.Errorf("ThreadStatuses of another channel = %#v; want an empty list", none)
	}
	var over []fakeslack.ThreadStatus
	if s := control(t, fake, "GET", "/_control/thread-statuses?"+url.Values{"channel": {"C1"}}.Encode(), nil, &over); s != 200 || !reflect.DeepEqual(over, want) {
		t.Errorf("/_control/thread-statuses = %d, %+v; want %+v", s, over, want)
	}
	if s := control(t, fake, "GET", "/_control/thread-statuses", nil, new(map[string]string)); s != 400 {
		t.Errorf("thread statuses with no channel = %d; want 400", s)
	}
}

func TestSetStatusRefusesACallWithoutItsArguments(t *testing.T) {
	fake, api := newFake(t)
	for name, p := range map[string]slack.AssistantThreadsSetStatusParameters{
		"no channel": {ThreadTS: "1.1", Status: "x"},
		"no thread":  {ChannelID: "C1", Status: "x"},
	} {
		var resp slack.SlackErrorResponse
		if err := api.SetAssistantThreadsStatus(p); !errors.As(err, &resp) || resp.Err != "invalid_arguments" {
			t.Errorf("%s: SetAssistantThreadsStatus = %v; want invalid_arguments", name, err)
		}
	}
	if n := len(fake.ThreadStatuses("C1")); n != 0 {
		t.Errorf("%d statuses kept for refused calls", n)
	}
}
