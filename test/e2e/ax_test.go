//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

const (
	axChannel  = "CAXLAUNCH"
	axAuth     = "Bearer ax-e2e-a2a-fake"
	axEndpoint = "http://agent-kourier-ax.agent-kourier.svc:8090"
)

func axScenario(t *testing.T) *scenario {
	t.Helper()
	s := newScenario(t)
	if s.env.Tier != "ax" {
		t.Skip("requires the AX Task launcher tier")
	}
	if err := s.slack.SetMembers(s.ctx, axChannel, "UFAKEBOT", "UAX"); err != nil {
		t.Fatal(err)
	}
	return s
}

func axRPC(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, axEndpoint+"/a2a", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", axAuth)
	req.Header.Set("A2A-Version", "1.0")
	client := http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("AX A2A %s: HTTP %d", method, resp.StatusCode)
	}
	var reply struct {
		Result json.RawMessage
		Error  *struct{ Code int }
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(&reply); err != nil {
		return err
	}
	if reply.Error != nil {
		return fmt.Errorf("AX A2A %s: RPC %d", method, reply.Error.Code)
	}
	return json.Unmarshal(reply.Result, out)
}

func axTask(ctx context.Context, marker string) (*a2a.Task, error) {
	var list a2a.ListTasksResponse
	const count = 10
	historyLength := 8 // Keep the user marker after the AX progress updates.
	req := a2a.ListTasksRequest{PageSize: count, HistoryLength: &historyLength, IncludeArtifacts: true}
	if err := axRPC(ctx, "ListTasks", &req, &list); err != nil {
		return nil, err
	}
	for _, task := range list.Tasks {
		for _, msg := range task.History {
			for _, part := range msg.Parts {
				if strings.Contains(part.Text(), marker) {
					return task, nil
				}
			}
		}
	}
	return nil, nil
}

func axWaitTask(s *scenario, marker string, state a2a.TaskState) *a2a.Task {
	s.t.Helper()
	var found *a2a.Task
	if err := Wait(s.ctx, stepTimeout, "AX A2A task to reach "+string(state), func(ctx context.Context) (bool, error) {
		var err error
		found, err = axTask(ctx, marker)
		return found != nil && found.Status.State == state, err
	}); err != nil {
		s.t.Fatal(err)
	}
	return found
}

func axReply(s *scenario, thread, marker string) []fakeslack.MessageView {
	s.t.Helper()
	return s.thread(axChannel, thread, "AX reply with "+marker, func(th []fakeslack.MessageView) bool {
		for _, msg := range BotMessages(th) {
			if finished(msg) && (strings.Contains(msg.Text, marker) || HasBlockText(msg.Blocks, marker)) {
				return true
			}
		}
		return false
	})
}

func TestAX_SlackRoundTripAndHistory(t *testing.T) {
	s := axScenario(t)
	first := "AX_RESULT_OK-" + s.stub.Gate("ax-first")
	second := "AX_RESULT_OK-" + s.stub.Gate("ax-second")
	root := s.mention(axChannel, "UAX", first)
	thread := axReply(s, root.TS, first)
	if got := BotMessages(thread); len(got) != 1 || got[0].ThreadTS != root.TS {
		t.Fatalf("expected one bot response in the source thread, got %s", describe(thread))
	}
	initial := axWaitTask(s, first, a2a.TaskStateCompleted)
	if len(initial.Artifacts) != 1 {
		t.Fatalf("expected one artifact, got %d", len(initial.Artifacts))
	}
	axCleanupLog(s, initial.ID, "absent", 0)

	// The binding uses the mention trigger for both a top-level message and a
	// follow-up inside the thread.
	s.reply(axChannel, root.TS, "UAX", botMention+second)
	next := axReply(s, root.TS, second)
	if bots := BotMessages(next); len(bots) != 2 ||
		(!strings.Contains(bots[1].Text, first) && !HasBlockText(bots[1].Blocks, first)) {
		t.Fatalf("follow-up did not contain earlier turn: %s", describe(next))
	}
	followup := axWaitTask(s, second, a2a.TaskStateCompleted)
	if followup.ContextID != initial.ContextID || followup.ID == initial.ID {
		t.Fatalf("follow-up task %q context %q, initial task %q context %q", followup.ID, followup.ContextID, initial.ID, initial.ContextID)
	}
}

func TestAX_FailureAndTimeout(t *testing.T) {
	s := axScenario(t)
	fail := "AX_RESULT_FAIL-" + s.stub.Gate("ax-fail")
	root := s.mention(axChannel, "UAX", fail)
	thread := axReply(s, root.TS, "The agent's task failed.")
	for _, msg := range BotMessages(thread) {
		if strings.Contains(msg.Text, "ax-fixture-private-stderr") || strings.Contains(string(msg.Blocks), "ax-fixture-private-stderr") {
			t.Fatal("child stderr reached Slack")
		}
	}
	failed := axWaitTask(s, fail, a2a.TaskStateFailed)
	if len(failed.Artifacts) != 0 {
		t.Fatal("failed turn has an artifact")
	}

	var ref string
	for _, msg := range BotMessages(thread) {
		if _, value, ok := strings.Cut(msg.Text, "Troubleshooting reference: "); ok {
			ref = strings.TrimSuffix(value, ".")
		}
	}
	if ref == "" {
		t.Fatal("Slack failure has no troubleshooting reference")
	}
	if err := Wait(s.ctx, stepTimeout, "broker failure reference and task ID", func(ctx context.Context) (bool, error) {
		pods, err := s.pods.List(ctx, "agent-kourier", agentKourierPods)
		if err != nil {
			return false, err
		}
		for _, pod := range pods {
			logs, err := s.pods.Logs(ctx, "agent-kourier", pod.Name, "agent-kourier", false, 100)
			if err != nil {
				return false, err
			}
			for line := range strings.SplitSeq(logs, "\n") {
				if strings.Contains(line, ref) && strings.Contains(line, string(failed.ID)) {
					return true, nil
				}
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}
	axCleanupLog(s, failed.ID, "suspended", 7)
	axCleanupLog(s, failed.ID, "absent", 7)

	hold := "AX_RESULT_HOLD-" + s.stub.Gate("ax-deadline")
	root = s.mention(axChannel, "UAX", hold)
	axReply(s, root.TS, "The agent's request timed out.")
	timedOut := axWaitTask(s, hold, a2a.TaskStateFailed)
	if timedOut.Status.Message == nil || timedOut.Status.Message.Metadata["code"] != "DEADLINE_EXCEEDED" {
		t.Fatal("failed turn lacks the timeout code")
	}
}

func TestAX_BusyTurnCanBeCanceled(t *testing.T) {
	s := axScenario(t)
	first := "AX_RESULT_HOLD-" + s.stub.Gate("ax-cancel")
	s.mention(axChannel, "UAX", first)
	task := axWaitTask(s, first, a2a.TaskStateWorking)
	waiting := "AX_RESULT_OK-" + s.stub.Gate("ax-waiting")
	other := s.mention(axChannel, "UAX", waiting)
	if err := Wait(s.ctx, 5*time.Second, "queued Slack acknowledgment", func(ctx context.Context) (bool, error) {
		reactions, err := s.slack.Reactions(ctx, axChannel, other.TS)
		for _, r := range reactions {
			if r.Name == queuedReaction {
				return true, err
			}
		}
		return false, err
	}); err != nil {
		t.Fatal(err)
	}
	if waiting, err := axTask(s.ctx, waiting); err != nil || waiting != nil {
		t.Fatalf("task was admitted while one slot was occupied: %v, %v", waiting != nil, err)
	}
	var canceled a2a.Task
	if err := axRPC(s.ctx, "CancelTask", &a2a.CancelTaskRequest{ID: task.ID}, &canceled); err != nil || canceled.Status.State != a2a.TaskStateCanceled {
		t.Fatalf("CancelTask = %s, %v", canceled.Status.State, err)
	}
	axReply(s, other.TS, waiting)
	axWaitTask(s, waiting, a2a.TaskStateCompleted)
}

func TestAX_SilentCommandReconnectsToSameTask(t *testing.T) {
	s := axScenario(t)
	const counter = "agentkourier_stream_reconnects_total"
	before, err := s.agentKourier.Sum(s.ctx, counter, nil)
	if err != nil && !errors.Is(err, ErrNoSample) {
		t.Fatal(err)
	}
	marker := "AX_RESULT_SILENT-" + s.stub.Gate("ax-silent")
	m := s.mention(axChannel, "UAX", marker)
	active := axWaitTask(s, marker, a2a.TaskStateWorking)
	const running = "AX sandbox running."
	s.thread(axChannel, m.TS, running, func(thread []fakeslack.MessageView) bool {
		for _, msg := range BotMessages(thread) {
			for _, chunk := range msg.Chunks {
				if strings.Contains(string(chunk), running) {
					return true
				}
			}
		}
		return false
	})
	final := axReply(s, m.TS, marker)
	if bots := BotMessages(final); len(bots) != 1 {
		t.Fatalf("expected one result in original thread: %s", describe(final))
	}
	settled := axWaitTask(s, marker, a2a.TaskStateCompleted)
	if settled.ID != active.ID {
		t.Fatal("reconnect opened a different A2A task")
	}
	after, err := s.agentKourier.Sum(s.ctx, counter, nil)
	if err != nil || after <= before {
		t.Fatalf("watchdog did not reconnect: before %v, after %v, error %v", before, after, err)
	}
}

func TestAX_StandbyForwardsCallbackPaths(t *testing.T) {
	s := axScenario(t)
	const selector = "app.kubernetes.io/name=agent-kourier-ax"
	var standby string
	if err := Wait(s.ctx, stepTimeout, "a ready AX standby", func(ctx context.Context) (bool, error) {
		pods, err := s.pods.List(ctx, "agent-kourier", selector)
		if err != nil {
			return false, err
		}
		for _, pod := range pods {
			if pod.DeletionTimestamp != nil || pod.Status.PodIP == "" {
				continue
			}
			url := "http://" + net.JoinHostPort(pod.Status.PodIP, "8090")
			if n, err := NewAgentKourier(url).Counter(ctx, "agentkourier_ax_leader", nil); err == nil && n == 0 {
				standby = url
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		t.Fatal(err)
	}

	client := http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/ax/results/v1/start", "/ax/results/v1"} {
		for _, tc := range []struct {
			forwarded bool
			status    int
		}{
			{false, http.StatusUnauthorized}, {true, http.StatusServiceUnavailable},
		} {
			req, err := http.NewRequestWithContext(s.ctx, http.MethodPost, standby+path, strings.NewReader("{}"))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", axAuth)
			if tc.forwarded {
				req.Header.Set("X-Agent-Kourier-Ax-Forwarded", "1")
			}
			res, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			_ = res.Body.Close()
			if res.StatusCode != tc.status || tc.forwarded && res.Header.Get("Retry-After") == "" {
				t.Fatalf("standby %s returned %d, want %d", path, res.StatusCode, tc.status)
			}
		}
	}
}

func axCleanupLog(s *scenario, id a2a.TaskID, disposition string, exit int) {
	s.t.Helper()
	if err := Wait(s.ctx, stepTimeout, "AX cleanup "+disposition, func(ctx context.Context) (bool, error) {
		pods, err := s.pods.List(ctx, "agent-kourier", "app.kubernetes.io/name=agent-kourier-ax")
		if err != nil {
			return false, err
		}
		for _, pod := range pods {
			logs, err := s.pods.Logs(ctx, "agent-kourier", pod.Name, "ax", false, 100)
			if err != nil {
				return false, err
			}
			for line := range strings.SplitSeq(logs, "\n") {
				var event struct {
					Task    a2a.TaskID `json:"a2a_task"`
					UID     string     `json:"ax_uid"`
					Name    string     `json:"ax_task"`
					Cleanup string     `json:"cleanup"`
					Exit    *int       `json:"exit_code"`
				}
				if json.Unmarshal([]byte(line), &event) == nil && event.Task == id && event.Cleanup == disposition &&
					event.UID != "" && event.Name != "" && (exit < 0 && event.Exit == nil || event.Exit != nil && *event.Exit == exit) {
					return !strings.Contains(line, "ax-fixture-private-stderr"), nil
				}
			}
		}
		return false, nil
	}); err != nil {
		s.t.Fatal(err)
	}
}

func TestAX_ReceiptBeforeTakeoverLeavesTurnInterrupted(t *testing.T) {
	s := axScenario(t)
	base := "AX_RESULT_OK-" + s.stub.Gate("ax-before-takeover")
	original := s.mention(axChannel, "UAX", base)
	axReply(s, original.TS, base)
	completed := axWaitTask(s, base, a2a.TaskStateCompleted)

	before, err := axReceiptCount(s.ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	marker := "AX_RESULT_STALL_SETTLEMENT-" + s.stub.Gate("ax-interrupted")
	root := s.mention(axChannel, "UAX", marker)
	active := axWaitTask(s, marker, a2a.TaskStateWorking)
	if err := Wait(s.ctx, stepTimeout, "an accepted AX result receipt", func(ctx context.Context) (bool, error) {
		n, err := axReceiptCount(ctx, s)
		return n > before, err
	}); err != nil {
		t.Fatal(err)
	}
	var open a2a.Task
	if err := axRPC(s.ctx, "GetTask", &a2a.GetTaskRequest{ID: active.ID}, &open); err != nil {
		t.Fatal(err)
	}
	if open.ID != active.ID || open.Status.State != a2a.TaskStateWorking || len(open.Artifacts) != 0 {
		t.Fatalf("accepted receipt exposed task %q in state %q with %d artifacts", open.ID, open.Status.State, len(open.Artifacts))
	}

	var canceled a2a.Task
	if err := axRPC(s.ctx, "CancelTask", &a2a.CancelTaskRequest{ID: active.ID}, &canceled); err == nil || !strings.Contains(err.Error(), "-32002") {
		t.Fatalf("CancelTask did not lose to the accepted receipt: %v", err)
	}

	deadline, cancel := context.WithTimeout(s.ctx, 90*time.Second)
	defer cancel()
	if err := s.pods.RolloutRestart(deadline, kube.AgentKourierNamespace, kube.AXDeployment); err != nil {
		t.Fatal(err)
	}
	failed := axWaitTask(s, marker, a2a.TaskStateFailed)
	if failed.ID != active.ID || failed.Status.Message == nil ||
		failed.Status.Message.Parts[0].Text() != "Interrupted by an agent restart." || len(failed.Artifacts) != 0 {
		t.Fatal("takeover did not settle the original turn as interrupted")
	}
	axReply(s, root.TS, "The agent's task failed.")
	axCleanupLog(s, failed.ID, "absent", 0)
	replayed := axWaitTask(s, base, a2a.TaskStateCompleted)
	if replayed.ID != completed.ID || len(replayed.Artifacts) != 1 {
		t.Fatal("takeover changed the already settled turn")
	}
}

func axReceiptCount(ctx context.Context, s *scenario) (float64, error) {
	pods, err := s.pods.List(ctx, "agent-kourier", "app.kubernetes.io/name=agent-kourier-ax")
	if err != nil {
		return 0, err
	}
	var count float64
	for _, pod := range pods {
		if pod.Status.PodIP == "" || pod.DeletionTimestamp != nil {
			continue
		}
		url := "http://" + net.JoinHostPort(pod.Status.PodIP, "8090")
		n, err := NewAgentKourier(url).Counter(ctx, "agentkourier_ax_callback_total", map[string]string{"route": "result", "status": "204"})
		if err != nil && !errors.Is(err, ErrNoSample) {
			return 0, err
		}
		count += n
	}
	return count, nil
}
