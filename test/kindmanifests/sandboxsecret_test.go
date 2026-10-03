package kindmanifests_test

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The value-handling guarantees of the sandbox's scripts: a secret is on no argument list, in no child's environment, in no
// output and in no last-applied annotation, and goes to the API server as a manifest on stdin. The tests run env.sh's
// apply_secret with stand-ins for kubectl, base64 and tr first in PATH, which record what they are given.

// recorders makes a directory of stand-ins and returns it with the directory their records go to. kubectl records its
// arguments, its environment and its stdin; base64 and tr record their arguments and then run the real ones.
func recorders(t *testing.T) (bin, logs string) {
	t.Helper()
	bin, logs = t.TempDir(), t.TempDir()
	stub := "#!/bin/sh\necho \"$*\" >> " + logs + "/kubectl.argv\nenv >> " + logs + "/kubectl.env\ncat >> " + logs + "/kubectl.stdin\n"
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"base64", "tr"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		shim := "#!/bin/sh\necho \"$*\" >> " + logs + "/" + tool + ".argv\nexec " + path + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(shim), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return bin, logs
}

// readLog reads a record, empty if the stand-in never ran.
func readLog(t *testing.T, logs, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(logs, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return string(raw)
}

// secretValues are what the file holds and the Secrets are to hold, each raw and as base64.
func secretValues() (raw, encoded []string) {
	for _, v := range []string{fakeBot, fakeApp, fakeKey} {
		raw = append(raw, v)
		encoded = append(encoded, base64.StdEncoding.EncodeToString([]byte(v)))
	}
	return raw, encoded
}

// TestApplySecretKeepsValuesOffArgvEnvAndOutput: with the six keys also exported in the caller's environment (which a
// load that did not unset them first would pass on to every child, since printf -v keeps a variable's export), a load of
// the file and two apply_secret calls leave no value, raw or base64, on any argument list, in kubectl's environment, or in
// the script's output; the manifest on kubectl's stdin holds each value as base64 and never raw, and kubectl is asked for a
// server-side apply, which keeps no last-applied copy.
func TestApplySecretKeepsValuesOffArgvEnvAndOutput(t *testing.T) {
	bin, logs := recorders(t)
	script := `set -eu
. "$1"
K=(kubectl --context kind-courier-full)
sandbox_env_load "$2"
apply_secret courier sandbox-slack botToken=SLACK_BOT_TOKEN appToken=SLACK_APP_TOKEN
apply_secret kagent sandbox-model apiKey=MODEL_API_KEY
`
	cmd := exec.CommandContext(t.Context(), "bash", "-c", script, "bash", sandboxDir+"env.sh", envFile(t, 0o600, goodEnv()...))
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SLACK_BOT_TOKEN="+fakeBot, "SLACK_APP_TOKEN="+fakeApp, "SLACK_CHANNEL_ID="+fakeChannel, "MODEL_PROVIDER=anthropic", "MODEL_NAME="+fakeModel, "MODEL_API_KEY="+fakeKey)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v\nstdout %q\nstderr %q", err, out.String(), errOut.String())
	}
	raw, encoded := secretValues()
	hold := func(what, text string, values []string) {
		for _, v := range values {
			if strings.Contains(text, v) {
				t.Errorf("%s holds %q; a value must not be there", what, v)
			}
		}
	}
	all := append(append([]string{fakeChannel, fakeModel}, raw...), encoded...)
	hold("stdout", out.String(), all)
	hold("stderr", errOut.String(), all)
	for _, name := range []string{"kubectl.argv", "base64.argv", "tr.argv", "kubectl.env"} {
		hold(name, readLog(t, logs, name), all)
	}
	if env := readLog(t, logs, "kubectl.env"); !strings.Contains(env, "PATH=") || strings.Contains(env, "LEAKCHECK") {
		t.Errorf("kubectl's environment %q: want a real environment (the stand-in ran) and no value in it", env)
	}
	// kubectl is asked for a server-side apply, twice, naming its context.
	argv := strings.Split(strings.TrimSpace(readLog(t, logs, "kubectl.argv")), "\n")
	want := "--context kind-courier-full apply --server-side --force-conflicts --field-manager=courier-sandbox -f -"
	if len(argv) != 2 || argv[0] != want || argv[1] != want {
		t.Errorf("kubectl was run as %q; want twice, as %q", argv, want)
	}
	// The manifests: each value as base64 on stdin, and never raw.
	stdin := readLog(t, logs, "kubectl.stdin")
	hold("kubectl's stdin", stdin, raw)
	for i, v := range encoded {
		if !strings.Contains(stdin, ": "+v+"\n") {
			t.Errorf("kubectl's stdin lacks the base64 of value %d", i)
		}
	}
	for _, line := range []string{"kind: Secret", "  name: sandbox-slack", "  namespace: courier", "  botToken: ", "  appToken: ", "  name: sandbox-model", "  namespace: kagent", "  apiKey: "} {
		if !strings.Contains(stdin, line) {
			t.Errorf("kubectl's stdin lacks %q:\n%s", line, redactBase64(stdin))
		}
	}
	if strings.Contains(stdin, "stringData") || strings.Contains(stdin, "annotations") {
		t.Error("the manifest carries stringData or annotations: values go as data, and kubectl is not to keep a copy")
	}
}

// redactBase64 hides what follows a key in a manifest, for a failure message.
func redactBase64(s string) string {
	return regexp.MustCompile(`(?m)^(  \w+): \S+$`).ReplaceAllString(s, "$1: <base64>")
}

// TestApplySecretUsesTheBuiltinForAValue: in the one function, a value's variable is expanded only as the argument of the
// printf builtin, which no process sees; any other command on it (an external echo, say) would put it on an argument list,
// where `ps` shows it and where a stand-in in PATH cannot see a command by its absolute path.
func TestApplySecretUsesTheBuiltinForAValue(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "env.sh")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "apply_secret() {")
	end := strings.Index(src[start:], "\n}\n")
	if start < 0 || end < 0 {
		t.Fatal("env.sh has no apply_secret")
	}
	body := src[start : start+end]
	uses := regexp.MustCompile(`\$\{![a-z]+\}`).FindAllStringIndex(body, -1)
	if len(uses) != 1 {
		t.Fatalf("apply_secret expands an indirect variable %d times; want once", len(uses))
	}
	if !strings.Contains(body, `printf %s "${!var}" | base64 | tr -d '\n'`) {
		t.Errorf("apply_secret does not expand the value as the argument of printf into base64: %s", body)
	}
	if strings.Contains(body, "/bin/") || strings.Contains(body, "/usr/") {
		t.Error("apply_secret runs a command by absolute path")
	}
}

// TestNoScriptExportsOrTracesAValue: no sandbox script exports a variable or turns on tracing, so that a value, once
// loaded, stays in the shell that loaded it: an exported one is in the environment of every child, and a trace prints it.
func TestNoScriptExportsOrTracesAValue(t *testing.T) {
	scripts, err := filepath.Glob(sandboxDir + "*.sh")
	if err != nil || len(scripts) < 4 {
		t.Fatalf("scripts %v, %v", scripts, err)
	}
	bad := regexp.MustCompile(`(?m)^\s*(export\b|declare\s+-\w*x|typeset\s+-\w*x|set\s+-\w*x|set\s+-o\s+xtrace|trap\s+.*DEBUG|BASH_XTRACEFD|readonly\s+\w+=)`)
	for _, script := range scripts {
		raw, err := os.ReadFile(script)
		if err != nil {
			t.Fatal(err)
		}
		if m := bad.FindString(string(raw)); m != "" {
			t.Errorf("%s has %q: values are never exported or traced", script, strings.TrimSpace(m))
		}
		// `set -x` in a compound command or after a ;, too.
		if regexp.MustCompile(`[;&|]\s*(export|set\s+-\w*x)\b`).MatchString(string(raw)) {
			t.Errorf("%s exports or traces after another command", script)
		}
	}
}

// TestRenderFillsPlaceholdersByExpansion: render gives the files with their values in, on stdout only.
func TestRenderFillsPlaceholdersByExpansion(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -eu
. "$1"
MODEL_NAME=model-x SLACK_CHANNEL_ID=C0RENDER01 render "$2"`, "bash", sandboxDir+"env.sh", sandboxDir+"courier/config/binding.yaml")
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "    channel: C0RENDER01\n") || strings.Contains(string(out), "__SLACK_CHANNEL_ID__\n    output") {
		t.Errorf("render gave %q; want the channel in", out)
	}
}

// TestEnvCheckRefusesASymbolicLink: a link is refused for what it is, not by a mode that chmod cannot fix; the file itself is
// accepted by the same path with no link.
func TestEnvCheckRefusesASymbolicLink(t *testing.T) {
	target := envFile(t, 0o600, goodEnv()...)
	link := filepath.Join(t.TempDir(), "link.env")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := check(t, link)
	if code == 0 || !strings.Contains(stderr, "is a symbolic link") || strings.Contains(stderr, "chmod") {
		t.Errorf("exit %d, stderr %q; want a refusal that says it is a symbolic link and gives no chmod hint", code, stderr)
	}
	noLeak(t, "stdout", stdout)
	noLeak(t, "stderr", stderr)
	dangling := filepath.Join(t.TempDir(), "dangling.env")
	if err := os.Symlink(filepath.Join(t.TempDir(), "nowhere"), dangling); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := check(t, dangling); code == 0 || !strings.Contains(stderr, "does not exist") {
		t.Errorf("a dangling link: exit %d, stderr %q; want it refused as not existing", code, stderr)
	}
}

// TestRefuseWhileSandboxUp: lib.sh's guard refuses when kubectl finds the sandbox's Deployment, and not when it finds none or
// cannot ask.
func TestRefuseWhileSandboxUp(t *testing.T) {
	for name, tc := range map[string]struct {
		stub     string
		refuse   bool
		wantInfo string
	}{
		"the Deployment is there":   {"echo deployment.apps/courier-sandbox", true, "make sandbox-down first"},
		"no Deployment":             {"exit 0", false, ""},
		"the API server cannot say": {"echo 'Unable to connect' >&2; exit 1", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			bin, logs := t.TempDir(), t.TempDir()
			stub := "#!/bin/sh\necho \"$*\" >> " + logs + "/argv\n" + tc.stub + "\n"
			if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-c", `. "$1"; K=(kubectl --context kind-courier-full); refuse_while_sandbox_up`, "bash", "../../hack/kind/lib.sh")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var errOut bytes.Buffer
			cmd.Stderr = &errOut
			err := cmd.Run()
			if (err != nil) != tc.refuse || !strings.Contains(errOut.String(), tc.wantInfo) {
				t.Errorf("err %v, stderr %q; want a refusal: %t, saying %q", err, errOut.String(), tc.refuse, tc.wantInfo)
			}
			if argv := readLog(t, logs, "argv"); !strings.Contains(argv, "--context kind-courier-full -n courier get deploy courier-sandbox") {
				t.Errorf("kubectl was asked %q; want it to name the context and the Deployment", argv)
			}
		})
	}
}

// TestTheTierScriptsRefuseWhileSandboxUp: e2e-run.sh refuses on the context it has checked and before it makes a token or
// starts the Job; up-full.sh refuses before it changes anything in an existing cluster; deploy_courier refuses first of all.
func TestTheTierScriptsRefuseWhileSandboxUp(t *testing.T) {
	guard := "refuse_while_sandbox_up"
	for _, tc := range []struct{ file, before, after string }{
		{"e2e-run.sh", `assert_context "${tier}"`, "refresh_dex_token"},
		{"up-full.sh", "\nassert_context full\n", "Agent Substrate"},
		{"lib.sh", "deploy_courier() {", "mktemp -d"},
	} {
		raw, err := os.ReadFile("../../hack/kind/" + tc.file)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		b := strings.Index(s, tc.before)
		g := strings.Index(s[b+1:], guard) + b + 1
		a := strings.Index(s[b:], tc.after) + b
		if b < 0 || g <= b || a <= b || g > a {
			t.Errorf("%s: %q at %d, %s at %d, %q at %d; want the guard between them", tc.file, tc.before, b, guard, g, tc.after, a)
		}
	}
}

// TestUpMakesTheFirstTokenWithTheCronJob: one path makes a token: up.sh runs the CronJob as a Job, waits for it, and deletes it,
// and does not use lib.sh's refresh_dex_token, whose throwaway pod leaves the token in its log.
func TestUpMakesTheFirstTokenWithTheCronJob(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	create := strings.Index(s, `create job courier-token-refresh-bringup --from=cronjob/courier-token-refresh`)
	wait := strings.Index(s, `wait --for=condition=Complete job/courier-token-refresh-bringup`)
	del := strings.Index(s, `delete job courier-token-refresh-bringup >/dev/null`)
	apply := strings.Index(s, `apply -f "${SBX}/token-refresh.yaml"`)
	deploy := strings.Index(s, `apply -f "${SBX}/courier/deployment.yaml"`)
	if apply <= 0 || apply >= create || create >= wait || wait >= del || del >= deploy {
		t.Errorf("up.sh: apply the CronJob %d, create the Job %d, wait %d, delete %d, courier's Deployment %d; want that order", apply, create, wait, del, deploy)
	}
	if strings.Contains(s, "refresh_dex_token") {
		t.Error("up.sh uses refresh_dex_token: the CronJob's script is the one path that makes the token")
	}
}

// TestUpExitsThreeWhenSlackRejectsTheTokens: the exit status the README documents is the one up.sh gives, after its checks.
func TestUpExitsThreeWhenSlackRejectsTheTokens(t *testing.T) {
	up, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	readme, err := os.ReadFile(sandboxDir + "README.md")
	if err != nil {
		t.Fatal(err)
	}
	s := string(up)
	exit3 := regexp.MustCompile(`(?m)^\s+exit 3$`).FindAllStringIndex(s, -1)
	i := strings.Index(s, `[ "${failed}" -eq 0 ] || die`)
	if len(exit3) != 1 || i < 0 || exit3[0][0] < i || !strings.Contains(s, "Slack rejected the tokens") {
		t.Errorf("up.sh: failure check at %d, exit 3 at %v; want the check first and one exit 3 after it", i, exit3)
	}
	if !strings.Contains(string(readme), "Exit 3: deployed, but Slack rejected the tokens") {
		t.Error("the README does not document exit 3")
	}
}
