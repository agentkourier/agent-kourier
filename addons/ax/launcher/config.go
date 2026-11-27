// Package launcher runs AX sandboxes for authenticated A2A turns.
package launcher

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	ax "github.com/google/ax/pkg/apis/v1alpha1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"
	"gopkg.in/yaml.v3"

	"github.com/agentkourier/agent-kourier/addons/ax/internal/result"
	"github.com/agentkourier/agent-kourier/addons/kit/settings"
)

const (
	maxTaskSize   = (32 << 10) - (1 << 10) // rc.2 MaxTaskYAMLSize
	maxConfigSize = 16 << 10               // rc.2 MaxTaskConfigSize
	maxDeadline   = 24 * time.Hour
	nameLabel     = "agentkourier.dev/launcher"
	turnLabel     = "agentkourier.dev/turn"
)

var (
	addr         = settings.Required("AX_ADDR")
	atespace     = settings.Func("AX_ATESPACE", "default", labelSetting("AX_ATESPACE"))
	identity     = settings.Func("AX_LAUNCHER_ID", "", labelSetting("AX_LAUNCHER_ID"))
	templatePath = settings.Required("AX_TASK_TEMPLATE")
	callbackURL  = settings.Func("AX_RESULT_URL", "", func(s string, _ settings.Values) (string, error) {
		if _, err := result.URL(s); err != nil {
			return "", fmt.Errorf("AX_RESULT_URL: %w", err)
		}
		return s, nil
	})
	callbackTokenPath = settings.Required("AX_RESULT_TOKEN_FILE")
	promptKey         = settings.Func("AX_PROMPT_KEY", "prompt", func(s string, _ settings.Values) (string, error) {
		return s, nil
	})
	history = settings.Func("AX_HISTORY", "true", func(s string, _ settings.Values) (bool, error) {
		v, err := strconv.ParseBool(s)
		if err != nil {
			return false, errors.New("AX_HISTORY must be a boolean")
		}
		return v, nil
	})
	running = settings.Func("AX_MAX_RUNNING", "10", func(s string, _ settings.Values) (int, error) {
		v, err := strconv.Atoi(s)
		if err != nil || v < 1 {
			return 0, errors.New("AX_MAX_RUNNING must be a positive integer")
		}
		return v, nil
	})
	poll        = durationSetting("AX_POLL_INTERVAL", "2s", time.Millisecond, maxDeadline)
	timeout     = durationSetting("AX_TURN_TIMEOUT", "30m", time.Second, maxDeadline)
	keep        = durationSetting("AX_KEEP_FAILED", "24h", 0, 30*24*time.Hour)
	dnsLabel    = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
	digestImage = regexp.MustCompile(`^[^@\s]+@sha256:[a-f0-9]{64}$`)
)

type config struct {
	addr      string
	atespace  string
	identity  string
	template  string
	url       string
	tokenFile string
	promptKey string
	history   bool
	running   int
	poll      time.Duration
	timeout   time.Duration
	keep      time.Duration
}

func vars() []settings.Var {
	return []settings.Var{addr, atespace, identity, templatePath, callbackURL, callbackTokenPath, promptKey, history, running, poll, timeout, keep}
}

func readConfig(v settings.Values) config {
	return config{
		addr:      addr.From(v),
		atespace:  atespace.From(v),
		identity:  identity.From(v),
		template:  templatePath.From(v),
		url:       callbackURL.From(v),
		tokenFile: callbackTokenPath.From(v),
		promptKey: promptKey.From(v),
		history:   history.From(v),
		running:   running.From(v),
		poll:      poll.From(v),
		timeout:   timeout.From(v),
		keep:      keep.From(v),
	}
}

func labelSetting(key string) func(string, settings.Values) (string, error) {
	return func(s string, _ settings.Values) (string, error) {
		if !isLabel(s) {
			return "", fmt.Errorf("%s must be a lowercase DNS label", key)
		}
		return s, nil
	}
}

func isLabel(s string) bool { return len(s) <= 63 && dnsLabel.MatchString(s) }

func durationSetting(key, def string, low, high time.Duration) settings.Of[time.Duration] {
	return settings.Func(key, def, func(s string, _ settings.Values) (time.Duration, error) {
		d, err := time.ParseDuration(s)
		if err != nil || d < low || d > high {
			return 0, fmt.Errorf("%s must be a duration from %s through %s", key, low, high)
		}
		return d, nil
	})
}

func readTemplate(ctx context.Context, cfg config, client ax.AXClient) (*ax.Task, error) {
	file, err := os.Open(cfg.template)
	if err != nil {
		return nil, fmt.Errorf("open AX_TASK_TEMPLATE: %w", err)
	}
	defer func() { _ = file.Close() }()
	const limit = 128 << 10
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read AX_TASK_TEMPLATE: %w", err)
	}
	if len(data) > limit {
		return nil, errors.New("AX_TASK_TEMPLATE exceeds the size limit")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var task ax.Task
	if decoder.Decode(&task) != nil || !errors.Is(decoder.Decode(new(yaml.Node)), io.EOF) ||
		task.GetApiVersion() != "ax.io/v1alpha1" || task.GetKind() != "Task" || task.GetSpec() == nil {
		return nil, errors.New("AX_TASK_TEMPLATE must contain one AX v1alpha1 Task")
	}
	spec := task.Spec
	if spec.Debug || spec.Retry != nil || !isLabel(spec.GetGateway().GetName()) {
		return nil, errors.New("AX Task template requires a Gateway, debug disabled, and no retry")
	}
	if ws := spec.GetWorkspace(); ws != nil &&
		(!isLabel(ws.GetName()) || (ws.GetPath() != "" && !path.IsAbs(ws.GetPath()))) {
		return nil, errors.New("invalid AX Workspace binding")
	}
	if len(spec.Commands) < 3 || !path.IsAbs(spec.Commands[0]) || path.Base(spec.Commands[0]) != "agent-kourier-ax-result" ||
		spec.Commands[1] != "--" || spec.Commands[2] == "" {
		return nil, errors.New("AX Task command must begin with an absolute path to agent-kourier-ax-result and -- followed by the agent command")
	}
	for _, e := range spec.Env {
		switch e.GetName() {
		case result.URLEnv, result.IDEnv, result.DeadlineEnv:
			return nil, errors.New("AX Task template overrides a reserved result environment variable")
		}
	}
	if scoped := task.GetMetadata().GetAtespace(); scoped != "" && scoped != cfg.atespace {
		return nil, errors.New("AX Task template atespace differs from AX_ATESPACE")
	}
	profile := spec.GetProfile()
	if profile == nil || proto.Equal(profile, &ax.Profile{}) {
		if spec.GetWorkspace().GetName() == "" {
			return nil, errors.New("AX Task template requires an image or a Workspace profile")
		}
		c, cancel := context.WithTimeout(ctx, rpcTimeout)
		defer cancel()
		workspace, err := client.GetWorkspace(c, &ax.GetWorkspaceRequest{Atespace: cfg.atespace, Name: spec.Workspace.Name})
		if err != nil {
			return nil, fmt.Errorf("resolve AX Workspace profile: %w", err)
		}
		profile = workspace.GetSpec().GetProfile()
	}
	if !digestImage.MatchString(profile.GetImage()) || profile.GetAteTemplate() != nil {
		return nil, errors.New("AX Task template requires a digest-pinned resolved image")
	}
	// Pin the checked profile even if it came from the Workspace so subsequent
	// edits to that Workspace cannot change the image of an admitted turn.
	spec.Profile = proto.Clone(profile).(*ax.Profile)
	if _, err := fillTask(cfg, &task, strings.Repeat("0", 32), strings.Repeat("0", 32), strings.Repeat("0", 32), time.Now(), ""); err != nil {
		return nil, fmt.Errorf("AX Task template: %w", err)
	}
	return &task, nil
}

func fillTask(cfg config, template *ax.Task, turn, session, resultID string, deadline time.Time, prompt string) (*ax.Task, error) {
	task := proto.Clone(template).(*ax.Task)
	task.Status = nil
	meta := task.GetMetadata()
	task.Metadata = &ax.ObjectMeta{Name: taskName(cfg, turn), Atespace: cfg.atespace, Labels: map[string]string{}}
	for k, v := range meta.GetLabels() {
		task.Metadata.Labels[k] = v
	}
	task.Metadata.Labels[nameLabel], task.Metadata.Labels[turnLabel] = cfg.identity, turn
	task.Spec.SessionId = session
	task.Spec.Goal = &ax.Goal{Text: prompt}
	if task.Spec.Config == nil {
		task.Spec.Config = &structpb.Struct{}
	}
	if task.Spec.Config.Fields == nil {
		task.Spec.Config.Fields = make(map[string]*structpb.Value)
	}
	task.Spec.Config.Fields[cfg.promptKey] = structpb.NewStringValue(prompt)
	task.Spec.Env = append(task.Spec.Env,
		&ax.EnvVar{Name: result.URLEnv, Value: cfg.url},
		&ax.EnvVar{Name: result.IDEnv, Value: resultID},
		&ax.EnvVar{Name: result.DeadlineEnv, Value: deadline.UTC().Format(time.RFC3339Nano)})
	encodedConfig, err := protojson.Marshal(task.Spec.Config)
	if err != nil || len(encodedConfig) > maxConfigSize {
		return nil, errors.New("prompt and template exceed the AX Task config limit")
	}
	encodedTask, err := yaml.Marshal(task)
	if err != nil || len(encodedTask) > maxTaskSize {
		return nil, errors.New("prompt and template exceed the AX Task size limit")
	}
	return task, nil
}

func shortHash(s string) string {
	hash := sha256.Sum256([]byte(s))
	return hex.EncodeToString(hash[:16])
}

func taskName(cfg config, turn string) string {
	return "kourier-" + shortHash(cfg.identity+"\x00"+turn)
}

func fingerprint(spec *ax.TaskSpec) string {
	b, err := proto.MarshalOptions{Deterministic: true}.Marshal(spec)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
