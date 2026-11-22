// Package telemetrytest holds an add-on's metrics to what it has served before: a test builds the add-on's registry
// as the add-on does, makes each family count, and Families compares the result with a golden file.
package telemetrytest

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// UpdateEnv is the variable that, set to anything, makes Families rewrite its golden file instead of failing.
const UpdateEnv = "UPDATE_GOLDEN"

// fqNameRE reads a family's name from a Desc's String, the only place a Desc gives it.
var fqNameRE = regexp.MustCompile(`fqName: "([^"]+)"`)

// Families renders the families of reg whose names begin with prefix, one block each (help, type, and the sorted
// label sets of its series, without values, as /metrics prints them), and compares them with the golden file. Each
// family that reg describes under prefix must be among those it gathered, so a family that the test does not make
// count, whose type and labels would go unchecked, fails it. With UpdateEnv set it writes the file instead.
func Families(t testing.TB, reg *prometheus.Registry, prefix, golden string) {
	t.Helper()
	descs := make(chan *prometheus.Desc)
	go func() { reg.Describe(descs); close(descs) }()
	described := map[string]bool{}
	for d := range descs {
		if m := fqNameRE.FindStringSubmatch(d.String()); m != nil && strings.HasPrefix(m[1], prefix) {
			described[m[1]] = true
		}
	}
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	for _, f := range families {
		if !strings.HasPrefix(f.GetName(), prefix) {
			continue
		}
		delete(described, f.GetName())
		fmt.Fprintf(&b, "# HELP %s %s\n# TYPE %s %s\n", f.GetName(), f.GetHelp(), f.GetName(), strings.ToLower(f.GetType().String()))
		var series []string
		for _, m := range f.GetMetric() {
			var labels []string
			for _, l := range m.GetLabel() {
				labels = append(labels, fmt.Sprintf("%s=%q", l.GetName(), l.GetValue()))
			}
			s := f.GetName()
			if len(labels) > 0 {
				s += "{" + strings.Join(labels, ",") + "}"
			}
			series = append(series, s)
		}
		slices.Sort(series)
		for _, s := range series {
			b.WriteString(s + "\n")
		}
	}
	for _, name := range slices.Sorted(maps.Keys(described)) {
		t.Errorf("%s is registered but the test does not make it count, so its type and labels are unchecked", name)
	}
	if os.Getenv(UpdateEnv) != "" {
		if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(golden, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != string(want) {
		t.Fatalf("the metric families differ from %s (%s=1 rewrites it):\n--- got\n%s--- want\n%s", golden, UpdateEnv, got, want)
	}
}
