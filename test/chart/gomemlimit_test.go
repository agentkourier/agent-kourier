package chart_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
)

// goMemLimit renders the chart with args and returns the values of the GOMEMLIMIT entries of the container's environment: none when
// the chart renders no variable, and more than one only if the chart duplicated one (which a server-side apply refuses).
func goMemLimit(t *testing.T, args ...string) []string {
	t.Helper()
	var d appsv1.Deployment
	find(t, mustRender(t, "agent-kourier", "agent-kourier", append([]string{"--set", "image.repository=agent-kourier,config.source=files"}, args...)...), "Deployment", "agent-kourier", &d)
	var got []string
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "GOMEMLIMIT" {
			if e.ValueFrom != nil {
				t.Errorf("GOMEMLIMIT comes from %+v; want a value", e.ValueFrom)
			}
			got = append(got, e.Value)
		}
	}
	return got
}

// TestGoMemLimitIsFourFifthsOfTheMemoryLimit: with a memory limit the container gets GOMEMLIMIT, in bytes, at 80% of it, so that the
// Go runtime collects harder before the kernel's OOM killer steps in. Every spelling of a valid quantity that the chart reads renders,
// whether it reaches the template as a string, a whole number (--set) or a float64 (a values file's 1000000000, which a naive
// printing turns into 1e+09). The default limit is 256Mi.
func TestGoMemLimitIsFourFifthsOfTheMemoryLimit(t *testing.T) {
	t.Parallel()
	helmPath(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "memory.yaml")
	if err := os.WriteFile(file, []byte("resources:\n  limits:\n    memory: 1000000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"the default, 256Mi", nil, "214748364"},
		{"512Mi", []string{"--set", "resources.limits.memory=512Mi"}, "429496729"},
		{"1Gi", []string{"--set", "resources.limits.memory=1Gi"}, "858993459"},
		{"2Ti", []string{"--set", "resources.limits.memory=2Ti"}, "1759218604441"},
		{"256M, decimal", []string{"--set", "resources.limits.memory=256M"}, "204800000"},
		{"1G, decimal", []string{"--set", "resources.limits.memory=1G"}, "800000000"},
		{"a number of bytes, from --set", []string{"--set", "resources.limits.memory=1000000000"}, "800000000"},
		{"a number of bytes, from a values file (a float64)", []string{"-f", file}, "800000000"},
		{"a quoted number of bytes", []string{"--set-string", "resources.limits.memory=1000000000"}, "800000000"},
		{"a fraction, 0.5Gi", []string{"--set-string", "resources.limits.memory=0.5Gi"}, "429496729"},
		{"a fraction, 1.5G", []string{"--set-string", "resources.limits.memory=1.5G"}, "1200000000"},
		{"the smallest it renders, 1Mi", []string{"--set", "resources.limits.memory=1Mi"}, "838860"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := goMemLimit(t, tc.args...); len(got) != 1 || got[0] != tc.want {
				t.Errorf("GOMEMLIMIT = %q; want the one value %q", got, tc.want)
			}
		})
	}
}

// TestNoMemoryLimitRendersNoGoMemLimit: without a limit, or with one this chart does not read, the render succeeds and sets no
// variable: the Go runtime then keeps its own default (no limit). A quantity that Kubernetes accepts is never a reason to fail the
// render; one it refuses is for the API server to refuse, not the template.
func TestNoMemoryLimitRendersNoGoMemLimit(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range []struct {
		name string
		args []string
	}{
		{"no resources at all", []string{"--set", "resources=null"}},
		{"no limits at all", []string{"--set", "resources.limits=null"}},
		{"limits without memory", []string{"--set", "resources.limits.memory=null,resources.limits.cpu=500m"}},
		{"no memory limit, only a request", []string{"--set", "resources.limits.memory=null"}},
		{"a limit under 1 MiB", []string{"--set", "resources.limits.memory=512Ki"}},
		{"a quantity of milli-bytes", []string{"--set-string", "resources.limits.memory=268435456000m"}},
		{"a quantity with an exponent", []string{"--set-string", "resources.limits.memory=256e6"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := goMemLimit(t, tc.args...); len(got) != 0 {
				t.Errorf("GOMEMLIMIT = %q; want none", got)
			}
		})
	}
	// Not quantities at all: the template does not fail on them either (it reads no more than it can), and the API server is
	// what refuses them. The test reads the render as text, for Kubernetes' own decoder refuses them too.
	for _, bad := range []string{"", "lots", "256MB", "-256Mi"} {
		t.Run("not a quantity: "+bad, func(t *testing.T) {
			t.Parallel()
			out, err := render(t.Context(), t, "agent-kourier", "agent-kourier", "--set", "image.repository=agent-kourier,config.source=files", "--set-string", "resources.limits.memory="+bad)
			if err != nil {
				t.Fatalf("a memory limit of %q failed the render: %v", bad, err)
			}
			if strings.Contains(out, "GOMEMLIMIT") {
				t.Errorf("a memory limit of %q rendered GOMEMLIMIT", bad)
			}
		})
	}
}

// TestGoMemLimitInExtraEnvWins: a GOMEMLIMIT the operator sets in extraEnv is the only one rendered, as they wrote it, whatever the memory
// limit is, and the rest of extraEnv is untouched. A second entry of the same name would be refused by a server-side apply, and
// would leave which one wins to the kubelet.
func TestGoMemLimitInExtraEnvWins(t *testing.T) {
	t.Parallel()
	helmPath(t)
	args := []string{"--set-json", `extraEnv=[{"name":"OTEL_TRACES_EXPORTER","value":"otlp"},{"name":"GOMEMLIMIT","value":"100MiB"}]`}
	if got := goMemLimit(t, args...); len(got) != 1 || got[0] != "100MiB" {
		t.Errorf("GOMEMLIMIT = %q; want only the operator's 100MiB", got)
	}
	var d appsv1.Deployment
	find(t, mustRender(t, "agent-kourier", "agent-kourier", append([]string{"--set", "image.repository=agent-kourier,config.source=files"}, args...)...), "Deployment", "agent-kourier", &d)
	var otel []corev1.EnvVar
	for _, e := range d.Spec.Template.Spec.Containers[0].Env {
		if e.Name == "OTEL_TRACES_EXPORTER" {
			otel = append(otel, e)
		}
	}
	if len(otel) != 1 || otel[0].Value != "otlp" {
		t.Errorf("OTEL_TRACES_EXPORTER entries = %+v; want the operator's, once", otel)
	}
	// An extraEnv that sets something else leaves the chart's value in place, before it.
	if got := goMemLimit(t, "--set-json", `extraEnv=[{"name":"OTEL_TRACES_EXPORTER","value":"otlp"}]`); len(got) != 1 || got[0] != "214748364" {
		t.Errorf("GOMEMLIMIT = %q; want the chart's value beside another entry of extraEnv", got)
	}
}

// TestNoRenderedEnvironmentRepeatsAName: no values file of the tiers renders a container environment with a name twice.
func TestNoRenderedEnvironmentRepeatsAName(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			for _, o := range mustRender(t, tc.release, tc.namespace, append([]string{"-f", values(tc.name)}, tc.args...)...) {
				if o.kind() != "Deployment" {
					continue
				}
				var d appsv1.Deployment
				find(t, []object{o}, "Deployment", o.name(), &d)
				seen := map[string]bool{}
				for _, e := range d.Spec.Template.Spec.Containers[0].Env {
					if seen[e.Name] {
						t.Errorf("the environment of %s names %s twice", o.name(), e.Name)
					}
					seen[e.Name] = true
				}
			}
		})
	}
}
