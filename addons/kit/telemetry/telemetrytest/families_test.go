package telemetrytest

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// recorder is a testing.TB that keeps what Families reports; its Fatalf ends the goroutine, as testing's does.
type recorder struct {
	testing.TB
	mu     sync.Mutex
	errors []string
}

func (r *recorder) Helper() {}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errors = append(r.errors, fmt.Sprintf(format, args...))
}

func (r *recorder) Fatal(args ...any) {
	r.Errorf("%s", fmt.Sprint(args...))
	runtime.Goexit()
}

func (r *recorder) Fatalf(format string, args ...any) {
	r.Errorf(format, args...)
	runtime.Goexit()
}

// run calls Families with a recorder, on a goroutine of its own so that a Fatal can end it, and returns what it
// reported.
func run(t *testing.T, reg *prometheus.Registry, golden string) []string {
	t.Helper()
	r := &recorder{TB: t}
	var wg sync.WaitGroup
	wg.Go(func() { Families(r, reg, "test_", golden) })
	wg.Wait()
	return r.errors
}

func registry(t *testing.T) (*prometheus.Registry, *prometheus.CounterVec) {
	t.Helper()
	reg := prometheus.NewRegistry()
	c := prometheus.NewCounterVec(prometheus.CounterOpts{Name: "test_things_total", Help: "Things."}, []string{"kind"})
	reg.MustRegister(c, prometheus.NewGauge(prometheus.GaugeOpts{Name: "other_gauge", Help: "Not under the prefix."}))
	return reg, c
}

const golden = "# HELP test_things_total Things.\n# TYPE test_things_total counter\ntest_things_total{kind=\"a\"}\ntest_things_total{kind=\"b\"}\n"

// The rendering holds the help, the type and each label set of a family under the prefix, sorted, and nothing else.
func TestFamiliesMatchesItsGolden(t *testing.T) {
	reg, c := registry(t)
	c.WithLabelValues("b").Add(7)
	c.WithLabelValues("a").Inc()
	path := filepath.Join(t.TempDir(), "metrics.golden")
	if err := os.WriteFile(path, []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	if errs := run(t, reg, path); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
}

// A label value, a help text or a family that differs from the golden file fails, and so does a family that is
// registered but has no series, whose type and labels the rendering cannot show.
func TestFamiliesFailsOnADifference(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metrics.golden")
	if err := os.WriteFile(path, []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, c := registry(t)
	c.WithLabelValues("a").Inc()
	c.WithLabelValues("c").Inc()
	if errs := run(t, reg, path); len(errs) != 1 || !strings.Contains(errs[0], `test_things_total{kind="c"}`) {
		t.Fatalf("errors = %q, want one that shows the new label value", errs)
	}
	reg, _ = registry(t)
	if errs := run(t, reg, path); len(errs) != 2 || !strings.Contains(errs[0], "test_things_total is registered but the test does not make it count") {
		t.Fatalf("errors = %q, want the family without series named, then the difference", errs)
	}
}

// With UpdateEnv set, Families writes the golden file, creating its directory.
func TestFamiliesWritesItsGoldenOnRequest(t *testing.T) {
	t.Setenv(UpdateEnv, "1")
	reg, c := registry(t)
	c.WithLabelValues("a").Inc()
	c.WithLabelValues("b").Inc()
	path := filepath.Join(t.TempDir(), "testdata", "metrics.golden")
	if errs := run(t, reg, path); len(errs) != 0 {
		t.Fatalf("errors = %q", errs)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != golden {
		t.Fatalf("golden = %q, %v", got, err)
	}
}
