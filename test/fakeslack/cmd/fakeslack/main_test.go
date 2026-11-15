package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// lineWriter sends each line written to it on a channel. The tests make it with a buffer of
// one because run writes exactly one line (the listening line) and must not block on it.
type lineWriter chan string

func (l lineWriter) Write(p []byte) (int, error) {
	l <- strings.TrimSpace(string(p))
	return len(p), nil
}

func call(t *testing.T, method, url string, body io.Reader, header http.Header) map[string]any {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, url, body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header = header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("%s %s: body is not JSON: %v", method, url, err)
	}
	return out
}

func TestRunServesSlackAndTheControlSurfaceUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-addr", "127.0.0.1:0"}, out) }()
	addr, ok := strings.CutPrefix(<-out, "fakeslack: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}

	auth := call(t, http.MethodPost, "http://"+addr+"/auth.test", nil, http.Header{"Authorization": {"Bearer xoxb-fake-token"}})
	if auth["ok"] != true || auth["user_id"] == "" {
		t.Errorf("auth.test = %v, want ok with a user_id", auth)
	}
	conns := call(t, http.MethodGet, "http://"+addr+"/_control/connections", nil, nil)
	if conns["connections"] != float64(0) {
		t.Errorf("/_control/connections = %v, want zero connections", conns)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}

func TestRunRejectsUnknownFlags(t *testing.T) {
	if err := run(t.Context(), []string{"-nope"}, io.Discard); err == nil {
		t.Fatal("run with an unknown flag = nil, want an error")
	}
}

func TestRunHostsTheAppsItIsGiven(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-strict-membership", "-strict-tokens", "-socket-spread",
			"-app", "app_id=ATEAM,bot_user=UTEAM,bot_id=BTEAM,team_id=TTEAM,team_name=team,bot_token=xoxb-team-token,app_token=xapp-team-token",
		}, out)
	}()
	addr, ok := strings.CutPrefix(<-out, "fakeslack: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}

	auth := call(t, http.MethodPost, "http://"+addr+"/auth.test", nil, http.Header{"Authorization": {"Bearer xoxb-team-token"}})
	if auth["ok"] != true || auth["user_id"] != "UTEAM" {
		t.Errorf("auth.test with the app's token = %v, want user_id UTEAM", auth)
	}
	auth = call(t, http.MethodPost, "http://"+addr+"/auth.test", nil, http.Header{"Authorization": {"Bearer xoxb-someone-else"}})
	if auth["ok"] != false || auth["error"] != "invalid_auth" {
		t.Errorf("auth.test with another token under -strict-tokens = %v, want invalid_auth", auth)
	}
	apps := func() []any {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "http://"+addr+"/_control/apps", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out []any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}()
	if len(apps) != 2 {
		t.Errorf("/_control/apps = %v, want the default app and the one given", apps)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}

func TestRunDeliversOverHTTPToTheAppsGivenARequestURL(t *testing.T) {
	type request struct{ path, signature, retry string }
	got := make(chan request, 16)
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- request{r.URL.Path, r.Header.Get("X-Slack-Signature"), r.Header.Get("X-Slack-Retry-Num")}
		if r.Header.Get("X-Slack-Retry-Num") == "" {
			w.WriteHeader(http.StatusServiceUnavailable) // the first attempt of each fails, and its retry is taken
		}
	}))
	defer broker.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	out := make(lineWriter, 1)
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, []string{
			"-addr", "127.0.0.1:0", "-http-retries", "10ms,10ms", "-http-timeout", "2s",
			"-request-url", broker.URL + "/chat/slack/agent-kourier/slack-fast", "-signing-secret", "fake-platform-secret",
			"-app", "app_id=ATEAM,bot_user=UTEAM,bot_id=BTEAM,request_url=" + broker.URL + "/chat/slack/team-a/team-a,signing_secret=fake-team-secret",
		}, out)
	}()
	addr, ok := strings.CutPrefix(<-out, "fakeslack: listening on ")
	if !ok {
		t.Fatal("run did not announce its address")
	}
	if s := call(t, http.MethodPost, "http://"+addr+"/_control/join", strings.NewReader(`{"channel":"C1","app":"ATEAM"}`), nil); s["error"] != nil {
		t.Fatalf("join = %v", s)
	}
	if m := call(t, http.MethodPost, "http://"+addr+"/_control/mention", strings.NewReader(`{"channel":"C1","user":"U1","text":"<@UFAKEBOT> hi"}`), nil); m["error"] != nil {
		t.Fatalf("mention = %v", m)
	}
	// The default app is sent the app_mention and the message event, and team-a, whose bot the text does not mention, the message
	// event alone: each request twice, its first attempt answered 503.
	want := map[string][]string{"/chat/slack/agent-kourier/slack-fast": {"", "1", "", "1"}, "/chat/slack/team-a/team-a": {"", "1"}}
	paths := map[string][]string{}
	for range 6 {
		select {
		case r := <-got:
			if !strings.HasPrefix(r.signature, "v0=") {
				t.Errorf("a request to %s is not signed: %q", r.path, r.signature)
			}
			paths[r.path] = append(paths[r.path], r.retry)
		case <-time.After(10 * time.Second):
			t.Fatalf("the broker got %q; want %q", paths, want)
		}
	}
	if !reflect.DeepEqual(paths, want) {
		t.Errorf("the requests at each URL carried X-Slack-Retry-Num %q; want %q", paths, want)
	}
	conns := call(t, http.MethodGet, "http://"+addr+"/_control/connections?app=ATEAM", nil, nil)
	if conns["connections"] != float64(0) {
		t.Errorf("connections of an app whose URL does not echo the challenge = %v; want 0", conns)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel = %v, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("run did not return after its context ended")
	}
}

func TestRunRejectsAnHTTPSettingItCannotUseWithoutRepeatingTheSecret(t *testing.T) {
	for name, args := range map[string][]string{
		"a URL without a secret":       {"-request-url", "http://broker.example/x"},
		"a secret without a URL":       {"-signing-secret", "fake-secret-value"},
		"a relative URL":               {"-request-url", "/x", "-signing-secret", "fake-secret-value"},
		"an app's URL without secret":  {"-app", "app_id=A1,bot_user=U1,bot_id=B1,request_url=http://broker.example/x"},
		"an app's secret without URL":  {"-app", "app_id=A1,bot_user=U1,bot_id=B1,signing_secret=fake-secret-value"},
		"a retry wait that is no time": {"-http-retries", "1s,soon"},
		"a negative retry wait":        {"-http-retries", "-1s"},
		"no timeout":                   {"-http-timeout", "0s"},
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t.Context(), append([]string{"-addr", "127.0.0.1:0"}, args...), io.Discard)
			if err == nil {
				t.Fatal("run = nil, want an error")
			}
			if strings.Contains(err.Error(), "fake-secret-value") {
				t.Errorf("error %q repeats the signing secret", err)
			}
		})
	}
}

func TestRunRejectsAnAppItCannotUseWithoutRepeatingIt(t *testing.T) {
	for name, app := range map[string]string{
		"no key=value":    "xoxb-secret-token",
		"unknown key":     "app_id=A1,bot_user=U1,bot_id=B1,colour=xoxb-secret-token",
		"missing ids":     "bot_token=xoxb-secret-token",
		"wrong token tag": "app_id=A1,bot_user=U1,bot_id=B1,bot_token=secret-token",
		"secret, no URL":  "app_id=A1,bot_user=U1,bot_id=B1,signing_secret=secret-token",
	} {
		t.Run(name, func(t *testing.T) {
			err := run(t.Context(), []string{"-addr", "127.0.0.1:0", "-app", app}, io.Discard)
			if err == nil {
				t.Fatal("run = nil, want an error")
			}
			if strings.Contains(err.Error(), "secret-token") {
				t.Errorf("error %q repeats the token", err)
			}
		})
	}
}
