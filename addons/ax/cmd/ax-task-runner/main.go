// AX launches this path as PID 1. Keep its metadata server off the agent's port 80.
package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/ax/pkg/apis/v1alpha1"
	"github.com/google/ax/runner"
	"gopkg.in/yaml.v3"
)

func main() {
	if err := run(); err != nil {
		slog.Error("AX runner failed")
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	var task v1alpha1.Task
	if err := yaml.Unmarshal([]byte(os.Getenv("AX_TASK_YAML")), &task); err != nil {
		return errors.New("invalid task configuration")
	}
	var workspaces []*v1alpha1.Workspace
	dec := yaml.NewDecoder(strings.NewReader(os.Getenv("AX_WORKSPACES_YAML")))
	for {
		var ws v1alpha1.Workspace
		if err := dec.Decode(&ws); err != nil {
			if !errors.Is(err, io.EOF) {
				return errors.New("invalid workspace configuration")
			}
			break
		}
		workspaces = append(workspaces, &ws)
	}
	port := 8081
	if len(task.GetSpec().GetCommand()) == 0 {
		// AX's golden-template build has no agent command. Serve its readiness
		// probe on port 80; running Tasks leave that port to the A2A agent.
		port = 80
	}
	return runner.Run(ctx, runner.Config{Port: port, Task: &task, Workspaces: workspaces})
}
