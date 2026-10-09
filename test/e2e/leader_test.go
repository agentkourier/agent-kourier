package e2e

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/test/e2e/kube"
)

// metricsServer serves what Agent Kourier's /metrics says of leadership and the chat connection; a nil leader is a process that
// does not answer (frozen) and is a server that hangs until the test ends.
func leadershipServer(t *testing.T, leader, connected int, hang <-chan struct{}) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hang != nil {
			select {
			case <-hang:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = fmt.Fprintf(w, "# TYPE agentkourier_leader gauge\nagentkourier_leader %d\nagentkourier_chat_connected{connection=\"agent-kourier/slack-fast\"} %d\n", leader, connected)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// replicasOf makes Replicas over fixed replicas whose base URLs are the servers'.
func replicasOf(urls map[string]string, list func() []kube.Replica) *Replicas {
	return &Replicas{
		list: func(context.Context) ([]kube.Replica, error) { return list(), nil },
		base: func(r kube.Replica) string { return urls[r.Name] },
		ask:  time.Second,
	}
}

func rep(name, run string) kube.Replica { return kube.Replica{Name: name, ContainerID: run} }

func TestLeadersAreTheReplicasThatSayTheyLead(t *testing.T) {
	hang := make(chan struct{})
	t.Cleanup(func() { close(hang) })
	urls := map[string]string{
		"a": leadershipServer(t, 1, 1, nil),
		"b": leadershipServer(t, 0, 0, nil),
		"c": leadershipServer(t, 0, 0, hang), // frozen: it does not answer in time, and is no leader
	}
	reps := replicasOf(urls, func() []kube.Replica { return []kube.Replica{rep("a", "1"), rep("b", "2"), rep("c", "3")} })

	got, err := reps.Leaders(t.Context())
	if err != nil || len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("Leaders = %+v, %v; want only a", got, err)
	}
}

func TestWaitLeaderWaitsForExactlyOneThatIsNotAnExcludedProcessAndIsConnected(t *testing.T) {
	urls := map[string]string{"a": leadershipServer(t, 1, 1, nil), "b": leadershipServer(t, 1, 1, nil)}
	var listed int
	list := func() []kube.Replica {
		listed++
		if listed < 3 { // two leaders at first: the old one has not gone yet
			return []kube.Replica{rep("a", "1"), rep("b", "2")}
		}
		return []kube.Replica{rep("b", "2")}
	}
	got, err := replicasOf(urls, list).WaitLeader(t.Context(), 10*time.Second, "a/1")
	if err != nil || got.ID() != "b/2" {
		t.Fatalf("WaitLeader = %+v, %v; want b/2, once it is the only one leading", got, err)
	}
}

func TestWaitLeaderDoesNotAcceptALeaderWhoseChatIsNotConnected(t *testing.T) {
	urls := map[string]string{"a": leadershipServer(t, 1, 0, nil)}
	reps := replicasOf(urls, func() []kube.Replica { return []kube.Replica{rep("a", "1")} })
	_, err := reps.WaitLeader(t.Context(), 300*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "leader") {
		t.Fatalf("WaitLeader = %v; want a timeout that says what it waited for", err)
	}
}

func TestCounterReadsOneReplicasMetric(t *testing.T) {
	urls := map[string]string{"a": leadershipServer(t, 1, 1, nil)}
	reps := replicasOf(urls, func() []kube.Replica { return nil })
	v, err := reps.Counter(t.Context(), rep("a", "1"), "agentkourier_leader", nil)
	if err != nil || v != 1 {
		t.Errorf("Counter = %v, %v; want 1", v, err)
	}
	if _, err := reps.Counter(t.Context(), rep("zz", "1"), "agentkourier_leader", nil); err == nil {
		t.Error("Counter of a replica with no address succeeded")
	}
}

func TestReplicaURLIsThePodIPOnAgentKourierPort(t *testing.T) {
	if got := replicaURL(kube.Replica{IP: "10.1.2.3"}); got != "http://10.1.2.3:8080" {
		t.Errorf("replicaURL = %q", got)
	}
	if got := replicaURL(kube.Replica{IP: "fd00::1"}); got != "http://[fd00::1]:8080" {
		t.Errorf("replicaURL of an IPv6 address = %q; want it bracketed", got)
	}
}
