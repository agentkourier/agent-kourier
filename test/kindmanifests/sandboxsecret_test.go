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
K=(kubectl --context kind-agent-kourier-full)
sandbox_env_load "$2"
apply_secret agent-kourier sandbox-slack botToken=SLACK_BOT_TOKEN appToken=SLACK_APP_TOKEN
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
	want := "--context kind-agent-kourier-full apply --server-side --force-conflicts --field-manager=agent-kourier-sandbox -f -"
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
	for _, line := range []string{"kind: Secret", "  name: sandbox-slack", "  namespace: agent-kourier", "  botToken: ", "  appToken: ", "  name: sandbox-model", "  namespace: kagent", "  apiKey: "} {
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
MODEL_NAME=model-x SLACK_CHANNEL_ID=C0RENDER01 render "$2"`, "bash", sandboxDir+"env.sh", sandboxDir+"agent-kourier/config/binding.yaml")
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
		"the Deployment is there":   {"echo deployment.apps/agent-kourier-sandbox", true, "make sandbox-down first"},
		"no Deployment":             {"exit 0", false, ""},
		"the API server cannot say": {"echo 'Unable to connect' >&2; exit 1", false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			bin, logs := t.TempDir(), t.TempDir()
			stub := "#!/bin/sh\necho \"$*\" >> " + logs + "/argv\n" + tc.stub + "\n"
			if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(stub), 0o700); err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(t.Context(), "bash", "-c", `. "$1"; K=(kubectl --context kind-agent-kourier-full); refuse_while_sandbox_up`, "bash", "../../hack/kind/lib.sh")
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			var errOut bytes.Buffer
			cmd.Stderr = &errOut
			err := cmd.Run()
			if (err != nil) != tc.refuse || !strings.Contains(errOut.String(), tc.wantInfo) {
				t.Errorf("err %v, stderr %q; want a refusal: %t, saying %q", err, errOut.String(), tc.refuse, tc.wantInfo)
			}
			if argv := readLog(t, logs, "argv"); !strings.Contains(argv, "--context kind-agent-kourier-full -n agent-kourier get deploy agent-kourier-sandbox") {
				t.Errorf("kubectl was asked %q; want it to name the context and the Deployment", argv)
			}
		})
	}
}

// TestTheTierScriptsRefuseWhileSandboxUp: e2e-run.sh refuses on the context it has checked and before it makes a token or
// starts the Job; up-full.sh refuses before it changes anything in an existing cluster; deploy_agentkourier refuses first of all.
func TestTheTierScriptsRefuseWhileSandboxUp(t *testing.T) {
	guard := "refuse_while_sandbox_up"
	for _, tc := range []struct{ file, before, after string }{
		{"e2e-run.sh", `assert_context "${tier}"`, "refresh_dex_token"},
		{"up-full.sh", "\nassert_context full\n", "Agent Substrate"},
		{"lib.sh", "deploy_agentkourier() {", "mktemp -d"},
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

// TestUpMakesTheFirstTokenWithTheCronJob: one path makes a token: up.sh installs the chart (whose CronJob it is) with no replicas, runs the
// CronJob as a Job, waits for it, and deletes it, and only then takes the full tier's Agent Kourier down and starts the sandbox's on the real
// Slack: on a first run, a Job that fails leaves both Agent Kourier deployments as they were; on a re-run the sandbox's is at zero until the next run or
// sandbox-down. It does not use lib.sh's refresh_dex_token, whose throwaway pod leaves the
// token in its log.
func TestUpMakesTheFirstTokenWithTheCronJob(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	install := strings.Index(s, `helm_install_agentkourier "${SANDBOX_RELEASE}" "${ROOT}/charts/agent-kourier/ci/sandbox-values.yaml" --set replicaCount=0`)
	create := strings.Index(s, `create job agent-kourier-token-refresh-bringup --from="cronjob/${SANDBOX_RELEASE}-token-refresh"`)
	wait := strings.Index(s, `wait --for=condition=Complete job/agent-kourier-token-refresh-bringup`)
	del := strings.Index(s, `delete job agent-kourier-token-refresh-bringup >/dev/null`)
	scale := strings.Index(s, `scale deploy/agent-kourier --replicas=0`)
	start := strings.Index(s, `helm_install_agentkourier "${SANDBOX_RELEASE}" "${ROOT}/charts/agent-kourier/ci/sandbox-values.yaml"
`)
	if install <= 0 || install >= create || create >= wait || wait >= del || del >= scale || scale >= start {
		t.Errorf("up.sh: install with no replicas %d, create the Job %d, wait %d, delete %d, scale the tier's Agent Kourier down %d, start the sandbox's %d; want that order", install, create, wait, del, scale, start)
	}
	if strings.Contains(s, "refresh_dex_token") {
		t.Error("up.sh uses refresh_dex_token: the CronJob's script is the one path that makes the token")
	}
}

// TestUpReplacesAHandMadeSandboxBeforeItWaitsForTheTiersPod: a sandbox brought up before the chart has a pod that is not the release's
// (instance=sandbox), which the wait for the tier's own pod to go would never see go; so the hand-made Deployment is replaced first, and
// the wait for the tier's Agent Kourier comes after every step that deletes the sandbox's.
func TestUpReplacesAHandMadeSandboxBeforeItWaitsForTheTiersPod(t *testing.T) {
	raw, err := os.ReadFile(sandboxDir + "up.sh")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	replace := strings.Index(s, `replace_unowned_deployment "${SANDBOX_RELEASE}"`)
	wait := strings.Index(s, `wait --for=delete pod -l "app.kubernetes.io/name=agent-kourier,app.kubernetes.io/instance!=${SANDBOX_RELEASE}"`)
	if replace <= 0 || wait <= replace {
		t.Errorf("up.sh: replace the hand-made Deployment %d, wait for the tier's pod to go %d; want that order", replace, wait)
	}
}

// TestEveryUpScriptRequiresHelm4: helm_install_agentkourier needs --force-conflicts, which Helm 3 lacks, and it would fail only after the
// hand-made Deployment was deleted; so each script checks the version in its preflight, before it changes anything.
func TestEveryUpScriptRequiresHelm4(t *testing.T) {
	for _, tc := range []struct{ script, after, before string }{
		{"up-fast.sh", `die "the Docker daemon is not reachable"`, "kind create cluster"},
		{"up-full.sh", `die "the Docker daemon is not reachable"`, "kind create cluster"},
		{"up-ax.sh", `die "Docker is not reachable"`, "kind create cluster"},
		{"sandbox/up.sh", `die "the Docker daemon is not reachable"`, `"${HERE}/up-full.sh"`},
	} {
		raw, err := os.ReadFile("../../hack/kind/" + tc.script)
		if err != nil {
			t.Fatal(err)
		}
		s := string(raw)
		a, r, b := strings.Index(s, tc.after), strings.Index(s, "\nrequire_helm4\n"), strings.Index(s, tc.before)
		if a < 0 || r < a || b < r {
			t.Errorf("%s: preflight %d, require_helm4 %d, first change %d; want the check between them", tc.script, a, r, b)
		}
	}
	for version, ok := range map[string]bool{"v4.3.0": true, "v4.0.0-rc.1": true, "v3.17.2": false, "": false} {
		bin := t.TempDir()
		stub := "#!/bin/sh\n"
		if version == "" {
			stub += "exit 1\n"
		} else {
			stub += "echo " + version + "\n"
		}
		if err := os.WriteFile(filepath.Join(bin, "helm"), []byte(stub), 0o700); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "bash", "-c", `. ../../hack/kind/lib.sh; require_helm4`)
		cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
		if err := cmd.Run(); (err == nil) != ok {
			t.Errorf("helm %q: require_helm4 err %v; want it to pass: %t", version, err, ok)
		}
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

// secretName matches the three variables that hold a secret.
var secretName = regexp.MustCompile(`SLACK_BOT_TOKEN|SLACK_APP_TOKEN|MODEL_API_KEY`)

// TestSecretVariablesAreOnlyEverNamesForApplySecret: no script expands a secret's variable. In up.sh, down.sh and check-env.sh
// the three names appear only as the `=NAME` arguments of apply_secret calls (and in comments, and in the one message that
// tells the person which keys to fix), never as `$NAME` or `${NAME...}`, and nothing in them expands a variable indirectly or
// evals, which could reach a secret by a name held in another. In env.sh no secret is expanded by name either, and the only
// indirect expansions are the loader's own check of each key and apply_secret's one.
func TestSecretVariablesAreOnlyEverNamesForApplySecret(t *testing.T) {
	read := func(name string) string {
		raw, err := os.ReadFile(sandboxDir + name)
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	expansion := regexp.MustCompile(`\$\{?!?(SLACK_BOT_TOKEN|SLACK_APP_TOKEN|MODEL_API_KEY)\b`)
	for _, name := range []string{"up.sh", "down.sh", "check-env.sh", "env.sh"} {
		src := read(name)
		if m := expansion.FindString(src); m != "" {
			t.Errorf("%s expands %q: a secret's variable is only ever named, as an argument to apply_secret", name, m)
		}
		if name == "env.sh" {
			continue
		}
		if strings.Contains(src, "${!") || regexp.MustCompile(`\beval\b`).MatchString(src) {
			t.Errorf("%s expands a variable indirectly or evals: that could reach a secret by a name held elsewhere", name)
		}
		for i, line := range strings.Split(src, "\n") {
			trim := strings.TrimSpace(line)
			if strings.HasPrefix(trim, "#") || !secretName.MatchString(trim) {
				continue
			}
			if strings.HasPrefix(trim, "apply_secret ") {
				// Every use on the line is the right-hand side of KEY=NAME.
				for _, loc := range secretName.FindAllStringIndex(trim, -1) {
					if loc[0] == 0 || trim[loc[0]-1] != '=' {
						t.Errorf("%s:%d: %q is not the =NAME argument of apply_secret", name, i+1, trim)
					}
				}
				continue
			}
			if strings.HasPrefix(trim, `echo "         Fix SLACK_BOT_TOKEN and SLACK_APP_TOKEN in `) {
				continue
			}
			t.Errorf("%s:%d: %q names a secret's variable outside apply_secret", name, i+1, trim)
		}
	}
	if n := strings.Count(read("env.sh"), "${!"); n != 2 {
		t.Errorf("env.sh expands %d variables indirectly; want 2 (the loader's check of each key, and apply_secret's)", n)
	}
}
