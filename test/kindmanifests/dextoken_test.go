package kindmanifests_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// extract runs lib.sh's dex_token_from over out, as refresh_dex_token does over what its pod printed.
func extract(t *testing.T, out string) string {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `. ../../hack/kind/lib.sh && dex_token_from`)
	cmd.Stdin = strings.NewReader(out)
	got, err := cmd.Output()
	if err != nil {
		t.Fatalf("dex_token_from: %v", err)
	}
	return strings.TrimSpace(string(got))
}

// TestDexTokenIsTheSentinelLine: the token is what a line that is exactly TOKEN:<jwt>:END holds. kubectl, when it
// cannot attach to the pod under load, falls back to streaming its logs and can print them twice, or cut one copy
// short and glue it to the next: whatever surrounds a good line, the token is that line's, and a line that is
// not wholly good is no token, so the caller tries again and does not store a mangled one.
func TestDexTokenIsTheSentinelLine(t *testing.T) {
	const jwt = "aaaaaaaaaa.bbbbbbbbbb-_.cccccccccc"
	good := "TOKEN:" + jwt + ":END"
	for name, tc := range map[string]struct{ out, want string }{
		"clean":                     {good + "\n", jwt},
		"printed twice":             {good + "\n" + good + "\n", jwt},
		"warning before":            {"warning: couldn't attach to pod/x, falling back to streaming logs\n" + good + "\n", jwt},
		"a truncated copy after":    {good + "\n" + good[:len(good)-12] + "\n", jwt},
		"a truncated copy glued":    {good + "\n" + good + good[:20], jwt},
		"the first line is glued":   {good + good + "\n", ""},
		"cut short, no sentinel":    {good[:len(good)-4] + "\n", ""},
		"a JWT with no sentinel":    {jwt + "\n", ""},
		"a part is missing":         {"TOKEN:aaaaaaaaaa.bbbbbbbbbb:END\n", ""},
		"nothing":                   {"", ""},
		"another token after a bad": {good + good + "\n" + good + "\n", jwt},
	} {
		t.Run(name, func(t *testing.T) {
			if got := extract(t, tc.out); got != tc.want {
				t.Errorf("token = %q; want %q", got, tc.want)
			}
		})
	}
}

// TestARunInTheFullTierStartsFromAFreshToken: the Binding's Dex token lasts 24 hours, so e2e-run.sh makes a new one
// for a full-tier run, after it has checked the context it will act on and before it starts the Job. A fast-tier
// run, whose token is not Dex's, does not.
func TestARunInTheFullTierStartsFromAFreshToken(t *testing.T) {
	raw, err := os.ReadFile("../../hack/kind/e2e-run.sh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(raw)
	check, refresh, job := `assert_context "${tier}"`, `[ "${tier}" != full ] || refresh_dex_token`, `echo "==> run the Job in`
	i, j, k := strings.Index(script, check), strings.Index(script, refresh), strings.Index(script, job)
	if i < 0 || j < 0 || k < 0 || i >= j || j >= k {
		t.Errorf("e2e-run.sh has the context check at %d, the full-tier refresh at %d and the Job at %d; want that order, so the token is made on the checked context, before the run", i, j, k)
	}
}

// TestAnInterruptedRunLeavesNoTokenPod: a Ctrl-C during the pod's `kubectl run` skips the function's RETURN trap, so
// the scripts that make a token delete the pod, by the name lib.sh keeps in DEX_TOKEN_POD, in their EXIT traps.
func TestAnInterruptedRunLeavesNoTokenPod(t *testing.T) {
	for _, file := range []string{"up-full.sh", "e2e-run.sh"} {
		raw, err := os.ReadFile("../../hack/kind/" + file)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "trap ") && strings.HasSuffix(line, " EXIT") && strings.Contains(line, `delete_courier_pod "${DEX_TOKEN_POD:-}"`) {
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no EXIT trap that deletes the token pod (delete_courier_pod \"${DEX_TOKEN_POD:-}\")", file)
		}
	}
}
