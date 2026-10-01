package slack_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat/slack"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// TestPermanentCode posts to a Slack that answers every call with one error code.
func TestPermanentCode(t *testing.T) {
	tests := []struct{ code, want string }{
		{"channel_not_found", "channel_not_found"},
		{"is_archived", "is_archived"},
		{"not_in_channel", "not_in_channel"},
		{"msg_too_long", "msg_too_long"},
		{"invalid_blocks", "invalid_blocks"},
		{"invalid_blocks_format", "invalid_blocks_format"},
		{"ratelimited", ""}, // a try later may work
		{"internal_error", ""},
		{"invalid_auth", ""}, // a revoked token is the restart policy's, not a post's
	}
	for _, tt := range tests {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error": tt.code})
		}))
		cfg := clientConfig(newFake(t))
		cfg.APIURL = srv.URL + "/"
		_, err := slack.New(cfg).Post(t.Context(), "C1", "", slack.Content{Text: "hi"}, "ev1")
		srv.Close()
		if err == nil {
			t.Fatalf("%s: Post succeeded", tt.code)
		}
		if got := slack.PermanentCode(err); got != tt.want {
			t.Errorf("PermanentCode(%s) = %q; want %q", tt.code, got, tt.want)
		}
		if got := slack.PermanentCode(fmt.Errorf("deliver ev1: post: %w", err)); got != tt.want { // however it is wrapped
			t.Errorf("PermanentCode(wrapped %s) = %q; want %q", tt.code, got, tt.want)
		}
	}
}

func TestPermanentCodeOfOtherErrors(t *testing.T) {
	for _, err := range []error{nil, errors.New("channel_not_found"), fmt.Errorf("slack: %w", errors.New("connection reset"))} {
		if got := slack.PermanentCode(err); got != "" {
			t.Errorf("PermanentCode(%v) = %q; want none: only Slack's own code counts", err, got)
		}
	}
}

// TestPostBadBlocksAreInvalid: blocks that are not blocks will never post, so the error says so.
func TestPostBadBlocksAreInvalid(t *testing.T) {
	_, c := newPoster(t)
	_, err := c.Post(t.Context(), "C1", "", slack.Content{Text: "hi", Blocks: json.RawMessage("{")}, "ev1")
	if !errors.Is(err, store.ErrInvalid) {
		t.Fatalf("Post with bad blocks = %v; want ErrInvalid", err)
	}
}
