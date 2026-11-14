package chart_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
)

// TestDefaultMemoryRequest: the chart requests 64Mi of memory and limits it to 256Mi. Helm merges maps, so a values file that sets other
// resources keys (the limit, the CPU request) keeps the default request, and only one that sets requests.memory changes it.
func TestDefaultMemoryRequest(t *testing.T) {
	t.Parallel()
	helmPath(t)
	for _, tc := range []struct {
		name         string
		set          []string
		request, lim string
	}{
		{"the defaults", nil, "64Mi", "256Mi"},
		{"a limit alone keeps the default request", []string{"--set", "resources.limits.memory=512Mi"}, "64Mi", "512Mi"},
		{"a CPU request alone keeps the default memory request", []string{"--set", "resources.requests.cpu=100m"}, "64Mi", "256Mi"},
		{"its own memory request wins", []string{"--set", "resources.requests.memory=96Mi"}, "96Mi", "256Mi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var d appsv1.Deployment
			args := append([]string{"--set", "image.repository=agent-kourier,config.source=files"}, tc.set...)
			find(t, mustRender(t, "agent-kourier", "agent-kourier", args...), "Deployment", "agent-kourier", &d)
			r := d.Spec.Template.Spec.Containers[0].Resources
			if got := r.Requests.Memory().String(); got != tc.request {
				t.Errorf("memory request %s; want %s", got, tc.request)
			}
			if got := r.Limits.Memory().String(); got != tc.lim {
				t.Errorf("memory limit %s; want %s", got, tc.lim)
			}
		})
	}
}
