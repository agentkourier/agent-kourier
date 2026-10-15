//go:build liveprobe

package slack

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type liveProbeConfig struct {
	Bot, App, Channel string
}

func readLiveProbeConfig(t *testing.T) liveProbeConfig {
	t.Helper()
	envFile := os.Getenv("LIVE_SLACK_ENV")
	channel := os.Getenv("LIVE_SLACK_CHANNEL")
	if envFile == "" {
		t.Fatal("LIVE_SLACK_ENV must name an authorized sandbox env file")
	}
	script := filepath.Join("..", "..", "..", "hack", "kind", "sandbox", "env.sh")
	// The existing parser never sources the env file or exports its tokens. Its stdout is consumed only in memory here.
	raw, err := exec.CommandContext(t.Context(), "bash", "-c", `. "$1"
sandbox_env_load "$2"
printf '{"bot":"%s","app":"%s","channel":"%s"}' "$SLACK_BOT_TOKEN" "$SLACK_APP_TOKEN" "$SLACK_CHANNEL_ID"`, "bash", script, envFile).Output()
	if err != nil {
		t.Fatal("sandbox credential file failed validation")
	}
	var cfg liveProbeConfig
	if json.Unmarshal(raw, &cfg) != nil {
		t.Fatal("could not read checked sandbox settings")
	}
	if channel != "" {
		cfg.Channel = channel
	}
	if !strings.HasPrefix(cfg.Channel, "C") && !strings.HasPrefix(cfg.Channel, "G") {
		t.Fatal("LIVE_SLACK_CHANNEL must be a channel ID")
	}
	return cfg
}

func liveChannelCode(channel string) string {
	if strings.HasPrefix(channel, "G") {
		return "G"
	}
	return "C"
}
