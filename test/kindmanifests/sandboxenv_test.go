package kindmanifests_test

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The sandbox tier's secrets file is checked by hack/kind/sandbox/check-env.sh (env.sh holds the rules). These
// tests run it on fake files, never on the real one, and each refusal must name the key and never the value.
// Every secret below is an obviously fake constant, distinct per key so that a leak of one is found by name.
const (
	sandboxDir = "../../hack/kind/sandbox/"

	fakeBot     = "xoxb-FAKE-LEAKCHECK-0001"
	fakeApp     = "xapp-1-FAKE-LEAKCHECK-0002"
	fakeChannel = "C0FAKELEAK1"
	fakeModel   = "claude-fake-leakcheck"
	fakeKey     = "sk-ant-FAKE-LEAKCHECK-key-0003"

	// The other fake keys: an OpenAI key and a Bedrock key, each in its own shape.
	fakeOpenAIKey  = "sk-proj-FAKE-LEAKCHECK-key-0005"
	fakeBedrockKey = "ABSKFAKE-LEAKCHECK-key-0006"
)

// goodEnv is a file that passes, in the order of the example file.
func goodEnv() []string {
	return []string{
		"SLACK_BOT_TOKEN=" + fakeBot,
		"SLACK_APP_TOKEN=" + fakeApp,
		"SLACK_CHANNEL_ID=" + fakeChannel,
		"MODEL_PROVIDER=anthropic",
		"MODEL_NAME=" + fakeModel,
		"MODEL_API_KEY=" + fakeKey,
	}
}

// withLine returns lines with the one for key replaced by line, or with it dropped when line is empty.
func withLine(lines []string, key, line string) []string {
	var out []string
	for _, l := range lines {
		switch {
		case !strings.HasPrefix(l, key+"="):
			out = append(out, l)
		case line != "":
			out = append(out, line)
		}
	}
	return out
}

// envFile writes lines to a new file of the given mode in a fresh temp directory and returns its path.
func envFile(t *testing.T, mode os.FileMode, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sandbox.env")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	return path
}

// check runs check-env.sh on path, with extra environment, and returns its exit status and both outputs.
func check(t *testing.T, path string, env ...string) (code int, stdout, stderr string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", sandboxDir+"check-env.sh", path)
	cmd.Env = append(os.Environ(), env...)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		code = exit.ExitCode()
	default:
		t.Fatal(err)
	}
	return code, out.String(), errOut.String()
}

// noLeak fails the test if any fake secret, which no output may hold, is in out.
func noLeak(t *testing.T, what, out string) {
	t.Helper()
	for _, secret := range []string{fakeBot, fakeApp, fakeKey, fakeOpenAIKey, fakeBedrockKey, fakeOpenRouterKey, "LEAKCHECK"} {
		if strings.Contains(out, secret) {
			t.Errorf("%s holds %q; a value must never be printed", what, secret)
		}
	}
}

func TestEnvCheckAcceptsAGoodFile(t *testing.T) {
	code, stdout, stderr := check(t, envFile(t, 0o600, goodEnv()...))
	if code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
	for _, key := range []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_CHANNEL_ID", "MODEL_PROVIDER", "MODEL_NAME", "MODEL_API_KEY"} {
		if !strings.Contains(stdout, key) {
			t.Errorf("stdout %q does not name %s", stdout, key)
		}
	}
	noLeak(t, "stdout", stdout)
	noLeak(t, "stderr", stderr)
}

// TestEnvCheckAcceptsTheShapesOfARealFile: comments, blank lines, Windows line endings, export, quotes, a
// provider of either kind, and a group channel (G...) are all fine.
func TestEnvCheckAcceptsTheShapesOfARealFile(t *testing.T) {
	lines := []string{"# the sandbox", "", "  # indented comment", "export SLACK_BOT_TOKEN=\"" + fakeBot + "\"", "SLACK_APP_TOKEN='" + fakeApp + "'  ",
		"SLACK_CHANNEL_ID=G0FAKELEAK1", "MODEL_PROVIDER=openai", "MODEL_NAME=gpt-4.1", "MODEL_API_KEY=" + fakeOpenAIKey}
	path := filepath.Join(t.TempDir(), "crlf.env")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\r\n")), 0o600); err != nil { // no final newline either
		t.Fatal(err)
	}
	if code, _, stderr := check(t, path); code != 0 {
		t.Fatalf("exit %d, stderr %q; want 0", code, stderr)
	}
}

// TestEnvCheckRefuses: each way a file can be wrong is refused with a non-zero status, a message that names
// the key (or the line, when the line is not KEY=VALUE) and none of the values.
func TestEnvCheckRefuses(t *testing.T) {
	good := goodEnv()
	for name, tc := range map[string]struct {
		lines []string
		want  string // what the message must say
	}{
		"bot token with the wrong prefix":    {withLine(good, "SLACK_BOT_TOKEN", "SLACK_BOT_TOKEN=xoxp-FAKE-LEAKCHECK-0001"), "SLACK_BOT_TOKEN is malformed"},
		"bot token that is only a prefix":    {withLine(good, "SLACK_BOT_TOKEN", "SLACK_BOT_TOKEN=xoxb-"), "SLACK_BOT_TOKEN is malformed"},
		"bot token with a space in it":       {withLine(good, "SLACK_BOT_TOKEN", "SLACK_BOT_TOKEN=xoxb-FAKE LEAKCHECK-0001"), "SLACK_BOT_TOKEN is malformed"},
		"app token that is a bot token":      {withLine(good, "SLACK_APP_TOKEN", "SLACK_APP_TOKEN="+fakeBot), "SLACK_APP_TOKEN is malformed"},
		"channel that is a name":             {withLine(good, "SLACK_CHANNEL_ID", "SLACK_CHANNEL_ID=#general"), "SLACK_CHANNEL_ID is malformed"},
		"channel in lower case":              {withLine(good, "SLACK_CHANNEL_ID", "SLACK_CHANNEL_ID=c0fakeleak1"), "SLACK_CHANNEL_ID is malformed"},
		"channel that is a user":             {withLine(good, "SLACK_CHANNEL_ID", "SLACK_CHANNEL_ID=U0FAKELEAK1"), "SLACK_CHANNEL_ID is malformed"},
		"provider that is not supported":     {withLine(good, "MODEL_PROVIDER", "MODEL_PROVIDER=gemini"), "MODEL_PROVIDER is malformed"},
		"model name with a slash":            {withLine(good, "MODEL_NAME", "MODEL_NAME=a/b-leakcheck"), "MODEL_NAME is malformed"},
		"api key that is too short":          {withLine(good, "MODEL_API_KEY", "MODEL_API_KEY=sk-short"), "MODEL_API_KEY is malformed"},
		"api key with a quote inside":        {withLine(good, "MODEL_API_KEY", "MODEL_API_KEY=sk-fake-leak\"check-key-0003"), "MODEL_API_KEY is malformed"},
		"bot token still REPLACE":            {withLine(good, "SLACK_BOT_TOKEN", "SLACK_BOT_TOKEN=xoxb-REPLACE"), "SLACK_BOT_TOKEN is still a placeholder"},
		"app token still replace_me":         {withLine(good, "SLACK_APP_TOKEN", "SLACK_APP_TOKEN=xapp-replace_me"), "SLACK_APP_TOKEN is still a placeholder"},
		"channel still REPLACE":              {withLine(good, "SLACK_CHANNEL_ID", "SLACK_CHANNEL_ID=CREPLACE"), "SLACK_CHANNEL_ID is still a placeholder"},
		"model name still REPLACE":           {withLine(good, "MODEL_NAME", "MODEL_NAME=REPLACE"), "MODEL_NAME is still a placeholder"},
		"api key still REPLACE":              {withLine(good, "MODEL_API_KEY", "MODEL_API_KEY=REPLACE"), "MODEL_API_KEY is still a placeholder"},
		"api key CHANGEME in the middle":     {withLine(good, "MODEL_API_KEY", "MODEL_API_KEY=sk-ChangeMe-leakcheck-0003"), "MODEL_API_KEY is still a placeholder"},
		"bot token empty":                    {withLine(good, "SLACK_BOT_TOKEN", "SLACK_BOT_TOKEN="), "SLACK_BOT_TOKEN is empty"},
		"a key set twice":                    {append(append([]string{}, good...), "MODEL_NAME=again"), "MODEL_NAME is set twice"},
		"a key of no meaning":                {append(append([]string{}, good...), "EXTRA_KEY=1"), "EXTRA_KEY is not a key of this file"},
		"a line that is not KEY=VALUE":       {append(append([]string{}, good...), "xoxb-FAKE-LEAKCHECK-9999"), "line 7 is not KEY=VALUE"},
		"a line with spaces around the =":    {withLine(good, "MODEL_NAME", "MODEL_NAME = "+fakeModel), "line 5 is not KEY=VALUE"},
		"a command in place of the assignee": {append([]string{"touch pwned-leakcheck"}, good...), "line 1 is not KEY=VALUE"},
	} {
		t.Run(name, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, tc.lines...))
			if code == 0 {
				t.Fatalf("exit 0 (stdout %q); want a refusal", stdout)
			}
			if !strings.Contains(stderr, tc.want) {
				t.Errorf("stderr %q does not say %q", stderr, tc.want)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
}

func TestEnvCheckRefusesAMissingKey(t *testing.T) {
	for _, key := range []string{"SLACK_BOT_TOKEN", "SLACK_APP_TOKEN", "SLACK_CHANNEL_ID", "MODEL_PROVIDER", "MODEL_NAME", "MODEL_API_KEY"} {
		t.Run(key, func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, 0o600, withLine(goodEnv(), key, "")...))
			if code == 0 || !strings.Contains(stderr, key+" is missing") {
				t.Errorf("exit %d, stderr %q; want a refusal that says %s is missing", code, stderr, key)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
}

// TestEnvCheckRefusesTheExampleFile: copied and not filled in, the example is all placeholders.
func TestEnvCheckRefusesTheExampleFile(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "sandbox.env.example")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "copy.env")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := check(t, path); code == 0 || !strings.Contains(stderr, "is still a placeholder") {
		t.Errorf("exit %d, stderr %q; want a refusal that says a value is still a placeholder", code, stderr)
	}
}

func TestEnvCheckRefusesAMissingFile(t *testing.T) {
	code, _, stderr := check(t, filepath.Join(t.TempDir(), "nowhere.env"))
	if code == 0 || !strings.Contains(stderr, "does not exist") {
		t.Errorf("exit %d, stderr %q; want a refusal that says the file does not exist", code, stderr)
	}
}

func TestEnvCheckRefusesADirectory(t *testing.T) {
	code, _, stderr := check(t, t.TempDir())
	if code == 0 || !strings.Contains(stderr, "not a regular file") {
		t.Errorf("exit %d, stderr %q; want a refusal that says it is not a regular file", code, stderr)
	}
}

// TestEnvCheckRefusesAFileOthersCanReadOrWrite: anything of the group or others, in any of the three bits.
func TestEnvCheckRefusesAFileOthersCanReadOrWrite(t *testing.T) {
	for _, mode := range []os.FileMode{0o640, 0o644, 0o604, 0o660, 0o606, 0o620, 0o602, 0o610, 0o601, 0o666, 0o710} {
		t.Run(mode.String(), func(t *testing.T) {
			code, stdout, stderr := check(t, envFile(t, mode, goodEnv()...))
			if code == 0 || !strings.Contains(stderr, "chmod 600") {
				t.Errorf("mode %o: exit %d, stderr %q; want a refusal that says to chmod 600 it", mode, code, stderr)
			}
			noLeak(t, "stdout", stdout)
			noLeak(t, "stderr", stderr)
		})
	}
	// Tighter than 600 is fine.
	for _, mode := range []os.FileMode{0o400, 0o600, 0o500} {
		if code, _, stderr := check(t, envFile(t, mode, goodEnv()...)); code != 0 {
			t.Errorf("mode %o: exit %d, stderr %q; want it accepted", mode, code, stderr)
		}
	}
}

func TestEnvCheckRefusesAnUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	code, stdout, stderr := check(t, envFile(t, 0o000, goodEnv()...))
	if code == 0 || !strings.Contains(stderr, "not readable") {
		t.Errorf("exit %d, stderr %q; want a refusal that says it is not readable", code, stderr)
	}
	noLeak(t, "stdout", stdout)
}

// TestEnvCheckRefusesAFileOfAnotherUser: the file is the test user's, and the script is told by a stand-in
// for id that the user is someone else.
func TestEnvCheckRefusesAFileOfAnotherUser(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "id"), []byte("#!/bin/sh\necho 4242\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := check(t, envFile(t, 0o600, goodEnv()...), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if code == 0 || !strings.Contains(stderr, "not by you") {
		t.Errorf("exit %d, stderr %q; want a refusal that says the file is not the user's", code, stderr)
	}
	noLeak(t, "stdout", stdout)
	noLeak(t, "stderr", stderr)
}

// TestEnvCheckNeverRunsTheFile: the file is parsed and never sourced, so what looks like a command or an
// expansion in it is neither run nor expanded: a refused line, or a value that fails its pattern.
func TestEnvCheckNeverRunsTheFile(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "pwned")
	for name, lines := range map[string][]string{
		"a command substitution as a value":     withLine(goodEnv(), "MODEL_NAME", "MODEL_NAME=$(touch "+marker+")"),
		"a command substitution as an endpoint": append(openRouterEnv(), "MODEL_BASE_URL=$(touch "+marker+")"),
		"a backtick as a value":                 withLine(goodEnv(), "MODEL_NAME", "MODEL_NAME=`touch "+marker+"`"),
		"a second command after a ;":            withLine(goodEnv(), "SLACK_CHANNEL_ID", "SLACK_CHANNEL_ID=C0FAKELEAK1; touch "+marker),
		"a command line":                        append([]string{"touch " + marker}, goodEnv()...),
		"an expansion in a value":               withLine(goodEnv(), "MODEL_API_KEY", "MODEL_API_KEY=${HOME}-leakcheck-0003-padding"),
	} {
		t.Run(name, func(t *testing.T) {
			code, _, stderr := check(t, envFile(t, 0o600, lines...))
			if code == 0 {
				t.Errorf("exit 0; want a refusal of what is not a plain value")
			}
			noLeak(t, "stderr", stderr)
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the file's contents were run")
			}
		})
	}
}

// TestEnvCheckIgnoresWhatTheEnvironmentHolds: only the file speaks: a key set in the environment is not the file's.
func TestEnvCheckIgnoresWhatTheEnvironmentHolds(t *testing.T) {
	code, _, stderr := check(t, envFile(t, 0o600, withLine(goodEnv(), "MODEL_NAME", "")...), "MODEL_NAME="+fakeModel)
	if code == 0 || !strings.Contains(stderr, "MODEL_NAME is missing") {
		t.Errorf("exit %d, stderr %q; want MODEL_NAME missing, since the file lacks it", code, stderr)
	}
}
