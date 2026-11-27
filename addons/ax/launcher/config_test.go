package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
)

type workspaceAX struct {
	ax.AXClient
}

func (workspaceAX) GetWorkspace(_ context.Context, req *ax.GetWorkspaceRequest, _ ...grpc.CallOption) (*ax.Workspace, error) {
	return &ax.Workspace{Spec: &ax.WorkspaceSpec{Profile: &ax.Profile{
		Image: "registry.test/" + req.Name + "@sha256:" + strings.Repeat("a", 64),
	}}}, nil
}

func TestTaskTemplateValidatesTheRunnerAndResolvesProfiles(t *testing.T) {
	const template = `apiVersion: ax.io/v1alpha1
kind: Task
spec:
  gateway: {name: results}
  commands: [agent-kourier-ax-result, --, agent-command]
  workspace: {name: test-workspace}
`
	for _, test := range []struct {
		name string
		body string
		want string
	}{
		{"workspace digest", template, ""},
		{"mutable tag", template + "  profile: {image: 'registry.test/agent:latest'}\n", "digest-pinned"},
		{"reserved environment", template + "  env: [{name: AGENT_KOURIER_AX_RESULT_ID, value: bogus}]\n", "reserved"},
		{"debug", template + "  debug: true\n", "debug disabled"},
		{"retry", template + "  retry: {}\n", "no retry"},
		{"wrapper", strings.Replace(template, "agent-kourier-ax-result", "other", 1), "must begin"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := config{template: filepath.Join(t.TempDir(), "task.yaml"), atespace: "default",
				identity: "test", promptKey: "prompt", url: "https://callback.test/ax/results/v1"}
			if err := os.WriteFile(cfg.template, []byte(test.body), 0o600); err != nil {
				t.Fatal(err)
			}
			task, err := readTemplate(t.Context(), cfg, workspaceAX{})
			if test.want != "" {
				if err == nil || !strings.Contains(err.Error(), test.want) {
					t.Fatalf("error = %v, want %q", err, test.want)
				}
				return
			}
			if err != nil || !strings.HasPrefix(task.GetSpec().GetProfile().GetImage(), "registry.test/test-workspace@sha256:") {
				t.Fatalf("resolved profile = %v, error = %v", task.GetSpec().GetProfile(), err)
			}
		})
	}
}
