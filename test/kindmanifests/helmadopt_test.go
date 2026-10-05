package kindmanifests_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// kubectlStub is a kubectl that logs its arguments and answers the three reads lib.sh's migration helpers make, from the
// environment: STUB_EXISTS is what `get ... -o name` prints ("" for an object that is not there), and STUB_OWNER is the
// Helm release annotation `get ... -o jsonpath` prints ("" for an object no release owns).
const kubectlStub = `#!/bin/sh
echo "$*" >> "$STUB_LOG/argv"
case "$*" in
  *"get "*"-o name"*) printf '%s' "$STUB_EXISTS" ;;
  *"get "*"-o jsonpath="*) printf '%s' "$STUB_OWNER" ;;
esac
`

// runMigrationHelper runs one of lib.sh's helpers against the stub and returns what kubectl was asked, one call a line.
func runMigrationHelper(t *testing.T, call, exists, owner string) string {
	t.Helper()
	bin, logs := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "kubectl"), []byte(kubectlStub), 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "bash", "-c", `set -eu; . "$1"; K=(kubectl --context kind-courier-full); `+call, "bash", "../../hack/kind/lib.sh")
	cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "STUB_LOG="+logs, "STUB_EXISTS="+exists, "STUB_OWNER="+owner)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%s: %v\n%s", call, err, out)
	}
	return readLog(t, logs, "argv")
}

// TestReplaceUnownedDeployment: a courier Deployment that no Helm release owns (the hand-made one of a tier brought up before the
// chart) is deleted, in the foreground so that its pod has released the SQLite volume before the chart's starts; one that a release
// owns, or none, is left alone.
func TestReplaceUnownedDeployment(t *testing.T) {
	const del = "--context kind-courier-full -n courier delete deploy/courier --cascade=foreground --wait"
	for name, tc := range map[string]struct {
		exists, owner string
		deleted       bool
	}{
		"hand-made":                {"deployment.apps/courier", "", true},
		"owned by a release":       {"deployment.apps/courier", "courier", false},
		"owned by another release": {"deployment.apps/courier", "other", false},
		"not there":                {"", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			argv := runMigrationHelper(t, "replace_unowned_deployment courier", tc.exists, tc.owner)
			if got := strings.Contains(argv, del); got != tc.deleted {
				t.Errorf("kubectl was asked:\n%s\nwant a foreground delete of the Deployment: %t", argv, tc.deleted)
			}
		})
	}
}

// TestAdoptIntoRelease: an object that is there and that no release owns gets the annotations and the label that make Helm manage
// it; one that is not there, or that a release owns, is not touched.
func TestAdoptIntoRelease(t *testing.T) {
	const (
		annotate = "-n courier annotate --overwrite pvc/courier-data meta.helm.sh/release-name=courier meta.helm.sh/release-namespace=courier"
		label    = "-n courier label --overwrite pvc/courier-data app.kubernetes.io/managed-by=Helm"
	)
	for name, tc := range map[string]struct {
		exists, owner string
		adopted       bool
	}{
		"hand-made":          {"persistentvolumeclaim/courier-data", "", true},
		"owned by a release": {"persistentvolumeclaim/courier-data", "courier", false},
		"not there":          {"", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			argv := runMigrationHelper(t, "adopt_into_release courier pvc/courier-data", tc.exists, tc.owner)
			if got := strings.Contains(argv, annotate) && strings.Contains(argv, label); got != tc.adopted {
				t.Errorf("kubectl was asked:\n%s\nwant the object adopted: %t", argv, tc.adopted)
			}
			if !tc.adopted && (strings.Contains(argv, "annotate") || strings.Contains(argv, "label")) {
				t.Errorf("kubectl was asked:\n%s\nwant nothing annotated or labelled", argv)
			}
		})
	}
}

// valuesFile finds the chart's ci values file that a line names.
var valuesFile = regexp.MustCompile(`ci/[a-z-]+-values\.yaml`)

// TestEveryTierInstallsTheChart: each tier's script installs courier with a values file of the chart's ci directory, and the files
// exist; none applies a hand-made courier manifest any more.
func TestEveryTierInstallsTheChart(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		{"lib.sh", `install_courier "${COURIER_VALUES:-${ROOT}/charts/courier/ci/fast-values.yaml}"`}, // the fast and full tiers (deploy_courier)
		{"up-fast.sh", `COURIER_VALUES=${ROOT}/charts/courier/ci/fast-postgres-values.yaml`},          // E2E_STORE=postgres
		{"up-ax.sh", `install_courier "${ROOT}/charts/courier/ci/ax-values.yaml"`},
		{"sandbox/up.sh", `helm_install_courier "${SANDBOX_RELEASE}" "${ROOT}/charts/courier/ci/sandbox-values.yaml"`},
	} {
		raw, err := os.ReadFile("../../hack/kind/" + tc.script)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(raw), tc.want) {
			t.Errorf("%s does not run %s", tc.script, tc.want)
		}
		if file := valuesFile.FindString(tc.want); file != "" {
			if _, err := os.Stat("../../charts/courier/" + file); err != nil {
				t.Errorf("%s names %s: %v", tc.script, file, err)
			}
		}
	}
	for _, gone := range []string{"fast/courier/deployment.yaml", "fast/courier/pvc.yaml", "fast/courier/rbac.yaml", "fast/courier/service.yaml", "sandbox/courier/deployment.yaml", "sandbox/token-refresh.yaml"} {
		if _, err := os.Stat("../../hack/kind/" + gone); err == nil {
			t.Errorf("hack/kind/%s is back: the chart makes what it held", gone)
		}
	}
}
