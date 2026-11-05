package kindmanifests_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dexKubectl is a kubectl for refresh_dex_token: it appends its arguments to calls, and plays one line of the scenario file per pod made (the
// n-th `run` is the n-th line), each "<run succeeds 1|0>|<phase>|<exit code>|<termination message>". `get pod` answers with that line's phase,
// or with the exit code and message in the form the script's template makes; the Secret's stdin is kept in secret-token.
const dexKubectl = `#!/bin/sh
echo "$*" >> "$STUB_LOG/calls"
line() { sed -n "$(cat "$STUB_LOG/runs")p" "$STUB_LOG/scenario"; }
case "$*" in
  *" run "*)
    echo $(( $(cat "$STUB_LOG/runs" 2>/dev/null || echo 0) + 1 )) > "$STUB_LOG/runs"
    [ "$(line | cut -d'|' -f1)" = 1 ] || { echo "error: forbidden" >&2; exit 1; }
    echo "pod/x created" ;;
  *"jsonpath={.status.phase}"*) line | cut -d'|' -f2 ;;
  *"terminated.exitCode"*) printf '%s %s' "$(line | cut -d'|' -f3)" "$(line | cut -d'|' -f4-)" ;;
  *"create secret"*) cat > "$STUB_LOG/secret-token"; echo "kind: Secret" ;;
  *) cat > /dev/null 2>&1 || true ;;
esac
`

// dexJWT is the shape of a token (three base64url parts) that dex_token_from accepts, and is obviously fake.
const dexJWT = "aaaaaaaaaa.bbbbbbbbbb-_.cccccccccc"

// refreshWith runs lib.sh's refresh_dex_token against dexKubectl with the scenario, optionally under `bash -x`, with no waits between looks or
// tries and two looks per pod. It returns the calls one to a line, the token the Secret was given ("" if none), and the output with the error.
func refreshWith(t *testing.T, scenario string, xtrace bool) (calls []string, secret, out string, err error) {
	t.Helper()
	bin, logs := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(dexKubectl), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(logs, "scenario"), []byte(scenario), 0o600); err != nil {
		t.Fatal(err)
	}
	lib, err := filepath.Abs("../../hack/kind/lib.sh")
	if err != nil {
		t.Fatal(err)
	}
	args := []string{"-c", `set -euo pipefail; . "$1"; K=(kubectl --context c); refresh_dex_token`, "bash", lib}
	if xtrace {
		args = append([]string{"-x"}, args...)
	}
	cmd := exec.CommandContext(t.Context(), "bash", args...)
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs,
		"DEX_TOKEN_POLLS=2", "DEX_TOKEN_POLL_SLEEP=0", "DEX_TOKEN_RETRY_SLEEP=0")
	raw, err := cmd.CombinedOutput()
	for _, l := range strings.Split(strings.TrimSpace(readLog(t, logs, "calls")), "\n") {
		if l != "" {
			calls = append(calls, l)
		}
	}
	return calls, readLog(t, logs, "secret-token"), string(raw), err
}

func count(calls []string, parts ...string) int {
	n := 0
	for _, c := range calls {
		ok := true
		for _, p := range parts {
			ok = ok && strings.Contains(c, p)
		}
		if ok {
			n++
		}
	}
	return n
}

const dexGood = "1|Succeeded|0|TOKEN:" + dexJWT + ":END\n"

// TestTheDexTokenIsReadFromThePodsStatus: the pod runs to completion and its termination message is read from its status; nothing attaches to it or
// streams its logs, which is what came back empty on a loaded machine. The Secret is made with the token, and the pod is deleted.
func TestTheDexTokenIsReadFromThePodsStatus(t *testing.T) {
	calls, secret, out, err := refreshWith(t, dexGood, false)
	if err != nil {
		t.Fatalf("refresh_dex_token = %v\n%s", err, out)
	}
	if secret != dexJWT {
		t.Errorf("the Secret was given %q; want the token", secret)
	}
	if n := count(calls, " run "); n != 1 {
		t.Errorf("%d pods were made; want 1: %v", n, calls)
	}
	for _, c := range calls {
		for _, banned := range []string{" --rm", " -i ", " -it ", " logs ", " attach "} {
			if strings.Contains(c, banned) {
				t.Errorf("call %q uses %q: the token must be read from the pod's status, not streamed", c, banned)
			}
		}
	}
	if !strings.Contains(strings.Join(calls, "\n"), "terminated.message") {
		t.Errorf("no call reads the termination message: %v", calls)
	}
	if n := count(calls, " delete pod "); n != 1 {
		t.Errorf("%d pod deletes; want 1: %v", n, calls)
	}
}

// TestADexTryThatLeavesNoTokenIsTriedAgain: a pod that ends with nothing in its termination message, with a message that is not wholly a token line, or
// that could not be made, is a lost try and not a failure; the next pod's token is stored, and each pod is deleted.
func TestADexTryThatLeavesNoTokenIsTriedAgain(t *testing.T) {
	for name, scenario := range map[string]string{
		"an empty message":    "1|Succeeded|0|\n" + dexGood,
		"a truncated message": "1|Succeeded|0|TOKEN:aaaaaaaaaa.bbbbbbbbbb-_.ccc\n" + dexGood,
		"a glued message":     "1|Succeeded|0|TOKEN:" + dexJWT + ":ENDTOKEN:" + dexJWT + ":END\n" + dexGood,
		"no container status": "1|Failed||\n" + dexGood,
		"a pod that failed":   "1|Failed|1|\n" + dexGood,
		"no pod made":         "0|||\n" + dexGood,
	} {
		t.Run(name, func(t *testing.T) {
			calls, secret, out, err := refreshWith(t, scenario, false)
			if err != nil {
				t.Fatalf("refresh_dex_token = %v\n%s", err, out)
			}
			if secret != dexJWT {
				t.Errorf("the Secret was given %q; want the token of the second pod", secret)
			}
			if runs, deletes := count(calls, " run "), count(calls, " delete pod "); runs != 2 || deletes != 2 {
				t.Errorf("%d pods made and %d deleted; want 2 and 2: %v", runs, deletes, calls)
			}
		})
	}
}

// TestADexTokenThatNeverComesIsBoundedAndSaysWhatWasTried: three pods, then the error, which names what each saw (a pod that could not be made, one that
// failed with its exit code, one that never finished) and the way the token was asked for. No Secret is made, and no pod is left. Nothing of a
// partial token reaches the output.
func TestADexTokenThatNeverComesIsBoundedAndSaysWhatWasTried(t *testing.T) {
	calls, secret, out, err := refreshWith(t, "0|||\n1|Failed|22|\n1|Pending||\n", false)
	if err == nil {
		t.Fatalf("refresh_dex_token succeeded with a third pod that never finished:\n%s", out)
	}
	if secret != "" {
		t.Errorf("a Secret was made with %q", secret)
	}
	if runs, deletes := count(calls, " run "), count(calls, " delete pod "); runs != 3 || deletes != 3 {
		t.Errorf("%d pods made and %d deleted; want 3 and 3 (a fourth try must not start): %v", runs, deletes, calls)
	}
	for _, want := range []string{
		"after 3 tries",
		"kubectl run could not make the pod",
		"the pod ended Failed with exit code 22",
		"the pod was Pending after 2 looks",
		"termination message",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the error does not say %q:\n%s", want, out)
		}
	}
}

// TestTheDexTokenIsNotTracedOrPrinted: under `bash -x` the variables that hold the token are not traced, and a token that was cut short, or
// a good one, is not in the output of a failure.
func TestTheDexTokenIsNotTracedOrPrinted(t *testing.T) {
	const partial = "TOKEN:aaaaaaaaaa.bbbbbbbbbb-_.ccc"
	_, secret, out, err := refreshWith(t, dexGood, true)
	if err != nil || secret != dexJWT {
		t.Fatalf("refresh_dex_token = %v, Secret %q\n%s", err, secret, out)
	}
	if strings.Contains(out, "aaaaaaaaaa") || strings.Contains(out, "cccccccccc") {
		t.Errorf("the token is in the trace of a run that worked:\n%s", out)
	}
	_, _, out, err = refreshWith(t, strings.Repeat("1|Succeeded|0|"+partial+"\n", 3), true)
	if err == nil {
		t.Fatal("refresh_dex_token succeeded on a token cut short")
	}
	if strings.Contains(out, "aaaaaaaaaa") {
		t.Errorf("part of a token is in the output of a failed run:\n%s", out)
	}
}
