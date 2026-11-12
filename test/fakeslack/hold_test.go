package fakeslack_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/slack-go/slack"

	"github.com/agentkourier/agent-kourier/internal/testutil"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// postAsync posts text in C1 as the app, in a goroutine, and returns where the result will be.
func postAsync(ctx context.Context, fake *fakeslack.Server, app fakeslack.App, text string) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, _, err := webAs(fake, app).PostMessageContext(ctx, "C1", slack.MsgOptionText(text, false))
		done <- err
	}()
	return done
}

func TestHeldPostsWaitForTheReleaseAndHoldUpNoOtherApp(t *testing.T) {
	fake := twoApps(t)
	if err := fake.HoldPosts("AAPPA"); err != nil {
		t.Fatal(err)
	}
	held := postAsync(t.Context(), fake, appA, "from a")
	testutil.Eventually(t, "a's post to wait", func() bool { return fake.HeldPosts("AAPPA") == 1 }, testutil.Every(5*time.Millisecond))
	if n := len(fake.Messages("C1")); n != 0 {
		t.Errorf("the channel holds %d messages while a's post waits; want none: nothing is stored until the release", n)
	}
	// Another app, and the same app's other methods, are not held.
	if _, _, err := webAs(fake, appB).PostMessage("C1", slack.MsgOptionText("from b", false)); err != nil {
		t.Errorf("b's post while a's waits = %v", err)
	}
	if _, err := webAs(fake, appA).AuthTest(); err != nil {
		t.Errorf("a's auth.test while its posts are held = %v", err)
	}
	select {
	case err := <-held:
		t.Fatalf("a's post ended (%v) before the release", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := fake.ReleasePosts("AAPPA"); err != nil {
		t.Fatal(err)
	}
	if err := <-held; err != nil {
		t.Errorf("a's post after the release = %v", err)
	}
	if n := fake.HeldPosts("AAPPA"); n != 0 {
		t.Errorf("%d posts held after the release", n)
	}
	if _, _, err := webAs(fake, appA).PostMessage("C1", slack.MsgOptionText("again", false)); err != nil {
		t.Errorf("a post after the release = %v; want it not held", err)
	}
	if n := len(fake.Messages("C1")); n != 3 {
		t.Errorf("the channel holds %d messages, want a's two and b's one", n)
	}
}

// A call whose client has gone is abandoned: the message is never stored, and the call is no longer counted, as for a leader that is killed while its post waits.
func TestAHeldPostWhoseClientWentIsAbandoned(t *testing.T) {
	fake := twoApps(t)
	if err := fake.HoldPosts("AAPPA"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	gone := postAsync(ctx, fake, appA, "never")
	testutil.Eventually(t, "the post to wait", func() bool { return fake.HeldPosts("AAPPA") == 1 }, testutil.Every(5*time.Millisecond))
	cancel()
	if err := <-gone; err == nil {
		t.Error("the post of a cancelled client succeeded")
	}
	testutil.Eventually(t, "the abandoned post to leave the count", func() bool { return fake.HeldPosts("AAPPA") == 0 }, testutil.Every(5*time.Millisecond))
	if err := fake.ReleasePosts("AAPPA"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(fake.Messages("C1")); n != 0 {
		t.Errorf("the channel holds %d messages; want none: the abandoned post is never applied", n)
	}
}

func TestHoldAndReleaseRefuseAnAppTheFakeDoesNotHost(t *testing.T) {
	fake := twoApps(t)
	if err := fake.HoldPosts("ANOBODY"); !errors.Is(err, fakeslack.ErrNotFound) {
		t.Errorf("HoldPosts of an app the fake does not host = %v, want ErrNotFound", err)
	}
	if err := fake.ReleasePosts("ANOBODY"); !errors.Is(err, fakeslack.ErrNotFound) {
		t.Errorf("ReleasePosts of an app the fake does not host = %v, want ErrNotFound", err)
	}
	if err := fake.ReleasePosts("AAPPA"); err != nil {
		t.Errorf("ReleasePosts of an app that is not held = %v, want nil", err)
	}
}

// Close lets the posts that wait go, so that a test that ends with a hold on does not hang.
func TestCloseReleasesHeldPosts(t *testing.T) {
	fake := fakeslack.New(fakeslack.WithApp(appA))
	if err := fake.HoldPosts("AAPPA"); err != nil {
		t.Fatal(err)
	}
	held := postAsync(context.Background(), fake, appA, "x")
	testutil.Eventually(t, "the post to wait", func() bool { return fake.HeldPosts("AAPPA") == 1 }, testutil.Every(5*time.Millisecond))
	closed := make(chan struct{})
	go func() { fake.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return with a post held")
	}
	_ = <-held // released with the Close: whatever it ended with
}

// The control surface lists the envelopes of the events another app was sent for a bot's post, which a case acks to know the app took them in.
func TestTheControlSurfaceListsTheEnvelopesOfAPostsEvents(t *testing.T) {
	fake := twoApps(t)
	socketAs(t, fake, appA)
	socketAs(t, fake, appB)
	fake.SetMembers("C1", "UBOTA", "UBOTB")
	_, ts, err := webAs(fake, appA).PostMessage("C1", slack.MsgOptionText("a speaks", false))
	if err != nil {
		t.Fatal(err)
	}
	want := fake.EventEnvelopes("AAPPB", "C1", ts)
	if len(want) != 1 {
		t.Fatalf("b was sent %v; want one event", want)
	}
	get := func(query string) (int, string) {
		resp, err := http.Get(fake.URL() + "/_control/own-events?" + query) //nolint:noctx // a test of a loopback server
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, strings.TrimSpace(string(b))
	}
	if code, body := get("app=AAPPB&channel=C1&ts=" + ts); code != 200 || body != `{"envelopes":["`+want[0]+`"]}` {
		t.Errorf("own-events for b = %d %s; want its one envelope", code, body)
	}
	if code, body := get("app=AAPPB&channel=C1&ts=1.000001"); code != 200 || body != `{"envelopes":[]}` {
		t.Errorf("own-events for a message nobody posted = %d %s; want an empty list", code, body)
	}
	if code, _ := get("app=ANOBODY&channel=C1&ts=" + ts); code != 404 {
		t.Errorf("own-events for an app the fake does not host = %d; want 404", code)
	}
}
