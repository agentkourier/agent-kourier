package kindmanifests_test

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"k8s.io/client-go/util/jsonpath"
)

// kubectl's -o jsonpath is client-go's JSONPath, which is not the full language: it has no `null`, for one, and an expression it cannot
// parse or run fails only when the script runs against a cluster. These tests run every jsonpath expression the sandbox's scripts and
// README give kubectl through client-go's own parser and evaluator, on a sample of what they are asked of, so that one it cannot run
// is found here.

// jsonPathSample is shaped as the objects the sandbox's expressions are run against: an Alertmanager, a Pod, a List of pods.
const jsonPathSample = `{
  "items": [
    {"metadata": {"name": "checkout-old", "deletionTimestamp": "2026-10-04T00:00:00Z"}},
    {"metadata": {"name": "checkout-live"}}
  ],
  "spec": {
    "configSecret": "alertmanager-kind-config",
    "externalUrl": "http://alertmanager.kind.local",
    "containers": [{"name": "alertmanager", "args": ["--web.external-url=http://alertmanager.kind.local"]}]
  }
}`

// runJSONPath parses expr as kubectl would and evaluates it on the sample, with missing keys allowed as kubectl's -o jsonpath allows
// them.
func runJSONPath(expr string) error {
	var sample any
	if err := json.Unmarshal([]byte(jsonPathSample), &sample); err != nil {
		return err
	}
	jp := jsonpath.New("sandbox").AllowMissingKeys(true)
	if err := jp.Parse(expr); err != nil {
		return err
	}
	return jp.Execute(io.Discard, sample)
}

// jsonPathUses finds the expressions given to kubectl as `jsonpath=...` in text: in single quotes either round the whole option
// ('jsonpath={...}') or round the expression (jsonpath='{...}').
func jsonPathUses(text string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`jsonpath='?(\{[^']*)'`).FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

func TestSandboxJSONPathsRunInKubectl(t *testing.T) {
	files, err := filepath.Glob(sandboxDir + "*.sh")
	if err != nil {
		t.Fatal(err)
	}
	files = append(files, sandboxDir+"README.md")
	var found int
	for _, file := range files {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, expr := range jsonPathUses(string(raw)) {
			found++
			if err := runJSONPath(expr); err != nil {
				t.Errorf("%s: kubectl cannot run -o jsonpath=%s: %v", file, expr, err)
			}
		}
	}
	if found < 4 {
		t.Errorf("found %d jsonpath expressions in the sandbox's scripts and README; want at least four (alert.sh, down.sh twice, the README): the search is not finding them", found)
	}
}

// TestJSONPathCheckRefusesWhatKubectlRefuses: the check is not vacuous. The filter that alert.sh once had, which kubectl refuses with
// "unrecognized identifier null", fails it, and ones that kubectl runs pass; the finder reads both quotings.
func TestJSONPathCheckRefusesWhatKubectlRefuses(t *testing.T) {
	bad := `{.items[?(@.metadata.deletionTimestamp==null)].metadata.name}`
	if err := runJSONPath(bad); err == nil {
		t.Errorf("%s was accepted: the check would not have found the bug it is for", bad)
	}
	for _, good := range []string{`{.spec.configSecret}`, `{.spec.containers[*].args}`, `{.items[*].metadata.name}`, `{.spec.configSecret} {.spec.externalUrl}{"\n"}`} {
		if err := runJSONPath(good); err != nil {
			t.Errorf("%s was refused: %v", good, err)
		}
	}
	got := jsonPathUses(`kubectl get x -o 'jsonpath={.a.b}' and kubectl get y -o jsonpath='{.c} {.d}{"\n"}' and "jsonpath=skipped"`)
	if len(got) != 2 || got[0] != `{.a.b}` || got[1] != `{.c} {.d}{"\n"}` {
		t.Errorf("found %q", got)
	}
}
