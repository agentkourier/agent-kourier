package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"syscall"

	"github.com/agentkourier/agent-kourier/internal/secret"
)

// botTokenKey is the name of the bot token in the environment and in a secrets file: the key of the sandbox's
// (hack/kind/sandbox/sandbox.env.example).
const botTokenKey = "SLACK_BOT_TOKEN" //nolint:gosec // G101: the name of the variable, not a credential

// maxTokenFile bounds what is read of a token file.
const maxTokenFile = 64 << 10

// botTokenShape is what a bot token looks like, as the sandbox's secrets file checks it.
var botTokenShape = regexp.MustCompile(`^xoxb-[A-Za-z0-9-]{8,}$`)

// loadBotToken returns the Slack bot token for the dry run, which reads Slack with it. The token is never an
// argument: it comes from file if one is named, else from the environment variable SLACK_BOT_TOKEN.
//
// A file is refused unless it is a regular file, not a link, that the group and others cannot read, as the sandbox
// refuses its secrets file. It holds the token alone, or KEY=VALUE lines among which SLACK_BOT_TOKEN is set once
// (so the sandbox's own file works as it is); comments and blank lines are skipped.
//
// Every error names the file or the variable and what is wrong with it, and holds nothing of what was read.
func loadBotToken(file string, getenv func(string) string) (secret.Value, error) {
	value, from := getenv(botTokenKey), "the environment variable "+botTokenKey
	if file != "" {
		var err error
		if value, err = readTokenFile(file); err != nil {
			return secret.Value{}, err
		}
		from = file
	}
	if value == "" {
		return secret.Value{}, fmt.Errorf("no bot token: set %s, or name a file with --token-file", botTokenKey)
	}
	if !botTokenShape.MatchString(value) {
		return secret.Value{}, fmt.Errorf("the bot token in %s is not a bot token: want xoxb- and then letters, digits and dashes", from)
	}
	return secret.New(value), nil
}

// readTokenFile returns the bot token that the file holds. See loadBotToken.
func readTokenFile(path string) (string, error) {
	// A link is refused by name, and then not followed: the open itself refuses one that appears between the two
	// (O_NOFOLLOW), and what is checked is the open file, so the file that is read is the file that was checked.
	if fi, err := os.Lstat(path); err != nil {
		return "", fmt.Errorf("token file: %w", err)
	} else if fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("token file %s is a symbolic link; name the file itself", path)
	} else if !fi.Mode().IsRegular() { // before the open: a FIFO with no writer would block it for ever
		return "", fmt.Errorf("token file %s is not a regular file", path)
	}
	// O_NONBLOCK: a FIFO put in its place after the Lstat must not block the open either; the check of the open file
	// below then refuses it. It has no effect on reading a regular file.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // the operator names the token file
	if errors.Is(err, syscall.ELOOP) {
		return "", fmt.Errorf("token file %s is a symbolic link; name the file itself", path)
	}
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	switch fi, err := f.Stat(); {
	case err != nil:
		return "", fmt.Errorf("token file %s: %w", path, err)
	case !fi.Mode().IsRegular():
		return "", fmt.Errorf("token file %s is not a regular file", path)
	case fi.Mode().Perm()&0o077 != 0:
		return "", fmt.Errorf("token file %s has mode %04o; it holds a secret, so chmod 600 it", path, fi.Mode().Perm())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxTokenFile+1))
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if len(data) > maxTokenFile {
		return "", fmt.Errorf("token file %s is larger than %d bytes", path, maxTokenFile)
	}
	var bare []string // the lines that are no KEY=VALUE
	var token string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, isPair := strings.Cut(strings.TrimPrefix(line, "export "), "=")
		switch {
		case !isPair:
			bare = append(bare, line)
		case strings.TrimSpace(key) == botTokenKey && token != "":
			return "", fmt.Errorf("token file %s sets %s twice", path, botTokenKey)
		case strings.TrimSpace(key) == botTokenKey:
			token = strings.TrimSpace(value)
		}
	}
	switch {
	case token != "":
		return token, nil
	case len(bare) == 1:
		return bare[0], nil
	}
	return "", errors.New("token file " + path + " holds no token: want the token alone, or a " + botTokenKey + "=... line")
}
