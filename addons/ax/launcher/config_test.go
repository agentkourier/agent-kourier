package launcher

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type workspaceAX struct {
	ax.AXClient
}

func (workspaceAX) GetWorkspace(_ context.Context, req *ax.GetWorkspaceRequest, _ ...grpc.CallOption) (*ax.Workspace, error) {
	if req.Name == "missing" {
		return nil, status.Error(codes.NotFound, "fixture workspace absent")
	}
	return &ax.Workspace{Spec: &ax.WorkspaceSpec{Profile: &ax.Profile{
		Image: "registry.test/" + req.Name + "@sha256:" + strings.Repeat("a", 64),
	}}}, nil
}

func TestTaskTemplateValidatesTheRunnerAndResolvesProfiles(t *testing.T) {
	const template = `apiVersion: ax.io/v1alpha1
kind: Task
spec:
  gateway: {name: results}
  commands: [/usr/local/bin/agent-kourier-ax-result, --, agent-command]
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
		{"relative wrapper", strings.Replace(template, "/usr/local/bin/", "", 1), "absolute path"},
		{"relative workspace path", strings.Replace(template, "name: test-workspace}", "name: test-workspace, path: relative}", 1), "invalid AX Workspace binding"},
		{"unresolved workspace", strings.Replace(template, "test-workspace", "missing", 1), "resolve AX Workspace profile"},
		{"wrong atespace", strings.Replace(template, "spec:", "metadata: {atespace: other}\nspec:", 1), "differs"},
		{"multiple documents", template + "---\n" + template, "one AX v1alpha1 Task"},
		{"oversized template", template + strings.Repeat(" ", 128<<10), "exceeds the size limit"},
		{"ate template", template + "  profile: {ateTemplate: {name: legacy}}\n", "digest-pinned"},
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
