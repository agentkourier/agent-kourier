//go:build e2e

package e2e

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// TestS16_ResumedLeaderCannotSendQueuedReply parks a runner immediately before Send,
// freezes its process past the lease, and lets the new leader send the queued turn.
// It counts received A2A attempts, including duplicate IDs the stub would otherwise deduplicate.
func TestS16_ResumedLeaderCannotSendQueuedReply(t *testing.T) {
	f := newFailover(t)
	const fault = "hold-before-send"
	rollout, cancel := context.WithTimeout(f.ctx, 3*time.Minute)
	defer cancel()
	if err := f.pods.SetFault(rollout, kube.AgentKourierNamespace, kube.AgentKourierDeployment, agentKourierContainer, fault); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(f.ctx), 3*time.Minute)
		defer cancel()
		if err := f.pods.SetFault(ctx, kube.AgentKourierNamespace, kube.AgentKourierDeployment, agentKourierContainer, ""); err != nil {
			t.Error(err)
		}
	})
	old := f.leader()
	warm := f.gate("S16-send-warm")
	warmMessage := f.mention(ChannelS16, "US16WARM", "answer me gate:"+warm)
	f.thread(ChannelS16, warmMessage.TS, "warm the old leader's agent credentials", held(warm, "", true))
	if err := f.stub.Release(f.ctx, warm); err != nil {
		t.Fatal(err)
	}
	f.thread(ChannelS16, warmMessage.TS, "warm-up turn completes", answered(warm, ""))
	if now := f.leader(); now.ID() != old.ID() {
		t.Fatal("leadership changed during warm-up")
	}
	if err := f.stub.ArmSendGate(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.stub.ReleaseSendGate(context.WithoutCancel(f.ctx)) })
	gate := f.gate("S16-send")
	m := f.mention(ChannelS16, "US16SEND", "answer me gate:"+gate)
	var held stubagent.SendGateState
	if err := Wait(f.ctx, stepTimeout, "old leader parked before Send", func(ctx context.Context) (bool, error) {
		state, err := f.stub.SendGate(ctx)
		held = state
		return err == nil && state.MessageID != "", err
	}); err != nil {
		t.Fatal(err)
	}
	if held.Sender != old.Name {
		t.Fatalf("held sender %q, leader %q", held.Sender, old.Name)
	}
	if held.Sends[held.MessageID] != 0 {
		t.Fatalf("parked message already sent %d times", held.Sends[held.MessageID])
	}
	t.Cleanup(func() { _ = f.sig.Cont(context.WithoutCancel(f.ctx), old.ContainerID) })
	if err := f.sig.Stop(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	next := f.takeover(old)
	if err := f.stub.Release(f.ctx, gate); err != nil {
		t.Fatal(err)
	}
	f.thread(ChannelS16, m.TS, "new leader completes queued turn", func(th []fakeslack.MessageView) bool { return answered(gate, "")(th) })
	state, err := f.stub.SendGate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Sends[held.MessageID] != 1 {
		t.Fatalf("new leader sends=%d; want 1", state.Sends[held.MessageID])
	}
	if err := f.stub.ReleaseSendGate(f.ctx); err != nil {
		t.Fatal(err)
	}
	if err := f.sig.Cont(f.ctx, old.ContainerID); err != nil {
		t.Fatal(err)
	}
	exit, err := f.pods.WaitExit(f.ctx, kube.AgentKourierNamespace, old.Name, agentKourierContainer, old.ContainerID)
	if err != nil {
		t.Fatal(err)
	}
	if exit.ExitCode != exitLeadershipLost {
		t.Fatalf("old leader exit %d; want %d", exit.ExitCode, exitLeadershipLost)
	}
	state, err = f.stub.SendGate(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if state.Sends[held.MessageID] != 1 {
		t.Fatalf("deposed leader reached A2A: sends=%d for %s; want 1", state.Sends[held.MessageID], held.MessageID)
	}
	logs, err := f.pods.Logs(f.ctx, kube.AgentKourierNamespace, old.Name, agentKourierContainer, exit.Previous, 400)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs, "an agent call (Send) was refused") {
		t.Fatalf("old leader did not reach the send fence; exit reasons %q", ExitReasons(logs))
	}
	if now := f.leader(); now.ID() != next.ID() {
		t.Fatalf("leader %s; want %s", now.ID(), next.ID())
	}
}
