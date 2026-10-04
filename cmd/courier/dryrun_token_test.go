package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canaryToken is a fake bot token that the tests look for in every output and error.
const canaryToken = "xoxb-CANARY-0123456789-abcdef"

// writeSecretFile writes content to a new file with the given mode in a temporary directory and returns its path.
func writeSecretFile(t *testing.T, mode os.FileMode, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil { // WriteFile is subject to the umask
		t.Fatal(err)
	}
	return path
}

// The token comes from the environment, or from a file that holds only the token or that is a secrets file of the sandbox
// (KEY=VALUE lines, as hack/kind/sandbox/sandbox.env.example); a file is preferred to the environment, so that the
// one named on the command line is the one used.
func TestLoadBotToken(t *testing.T) {
	sandboxEnv := "# the sandbox's secrets\nSLACK_APP_TOKEN=xapp-OTHER-0123456789\n\nexport SLACK_BOT_TOKEN=" + canaryToken + "\nMODEL_API_KEY=not-this-one\n"
	for _, tc := range []struct {
		name, file string
		env        env
	}{
		{"the environment", "", env{"SLACK_BOT_TOKEN": canaryToken}},
		{"a file of the token alone", writeSecretFile(t, 0o600, canaryToken+"\n"), nil},
		{"a secrets file", writeSecretFile(t, 0o600, sandboxEnv), nil},
		{"the file over the environment", writeSecretFile(t, 0o400, canaryToken), env{"SLACK_BOT_TOKEN": "xoxb-ENVIRONMENT-0123456789"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := loadBotToken(tc.file, tc.env.get)
			if err != nil {
				t.Fatal(err)
			}
			if got.Reveal() != canaryToken {
				t.Errorf("token = %q; want the canary", got)
			}
		})
	}
}

// A token that cannot be used is refused with a message that names where it looked and what is wrong, and never
// holds the token, nor any other line of the file.
func TestLoadBotTokenRefusals(t *testing.T) {
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(writeSecretFile(t, 0o600, canaryToken), link); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, file string
		env        env
		want       string
	}{
		{"nothing given", "", nil, "SLACK_BOT_TOKEN"},
		{"a file that does not exist", filepath.Join(t.TempDir(), "absent"), nil, "absent"},
		{"a file the group can read", writeSecretFile(t, 0o640, canaryToken), nil, "0640"},
		{"a file anyone can read", writeSecretFile(t, 0o644, canaryToken), nil, "0644"},
		{"a link", link, nil, "link"},
		{"a directory", t.TempDir(), nil, "regular file"},
		{"a secrets file with no bot token", writeSecretFile(t, 0o600, "SLACK_APP_TOKEN=xapp-OTHER-0123456789\n"), nil, "SLACK_BOT_TOKEN"},
		{"a bot token given twice", writeSecretFile(t, 0o600, "SLACK_BOT_TOKEN="+canaryToken+"\nSLACK_BOT_TOKEN="+canaryToken+"\n"), nil, "twice"},
		{"an empty file", writeSecretFile(t, 0o600, "\n# nothing\n"), nil, "no token"},
		{"two lines that are not KEY=VALUE", writeSecretFile(t, 0o600, canaryToken+"\n"+canaryToken+"\n"), nil, "no token"},
		{"a value that is no bot token", writeSecretFile(t, 0o600, "SLACK_BOT_TOKEN=CANARY-not-a-bot-token\n"), nil, "xoxb-"},
		{"an app token in the environment", "", env{"SLACK_BOT_TOKEN": "xapp-CANARY-0123456789"}, "xoxb-"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadBotToken(tc.file, tc.env.get)
			if err == nil {
				t.Fatal("loadBotToken accepted it")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not say %q", err, tc.want)
			}
			for _, secretText := range []string{"CANARY", "OTHER-0123456789", "not-this-one"} {
				if strings.Contains(err.Error(), secretText) {
					t.Errorf("error %q holds %q from the file or environment", err, secretText)
				}
			}
		})
	}
}
