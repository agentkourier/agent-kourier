//go:build testhooks

package broker

import (
	"context"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/store/sqlite"
	"github.com/agentkourier/agent-kourier/test/fakeslack"
)

// The whole path of a vote (spec 8.7), through the real broker: the final message of an investigation that an alert started
// carries the buttons; a member's press records a vote, which they can change, and counts it under its alert; a bot's press
// and a press of a person outside the channel record nothing and are told nothing; the voter is told in an ephemeral and the channel is not
// disturbed; and the vote is in the store with the Binding, the agent, the alert and the session, and in the audit log by
// identifiers and verdict.
func TestRunRecordsTheVotesOnAnInvestigationsFinalMessage(t *testing.T) {
	a := newAlerts(t)
	a.fake.AddUser(fakeslack.User{ID: "U2", Name: "grace"})
	a.fake.AddUser(fakeslack.User{ID: "U3", Name: "outsider"})
	a.fake.AddUser(fakeslack.User{ID: "UBOT", Name: "helper", IsBot: true})
	a.fake.SetMembers(alertChannel, "U1", "U2", "UBOT") // U3 is not in the channel
	a.run(t)

	alert := a.fire(t, post("FIRING", "KubePodCrashLooping"))
	reply := a.inThread(t, alert.TS, "the final answer with its buttons", func(m fakeslack.Message) bool {
		return hasText("investigated")(m) && strings.Contains(string(m.Blocks), "feedback-useful")
	})
	value := regexp.MustCompile(`fb1\|investigation\|useful\|[^"]+`).FindString(string(reply.Blocks))
	if value == "" {
		t.Fatalf("the buttons carry no investigation's value: %s", reply.Blocks)
	}
	notUseful := strings.Replace(value, "|useful|", "|not_useful|", 1)
	before := len(a.fake.Messages(alertChannel))

	click := func(user, action, v string) {
		t.Helper()
		env, err := a.fake.Click(user, reply.TS, action, v)
		if err != nil {
			t.Fatal(err)
		}
		ctx, stop := context.WithTimeout(t.Context(), 10*time.Second)
		defer stop()
		if err := a.fake.WaitAck(ctx, env); err != nil {
			t.Fatalf("the press of %s was not acknowledged: %v", user, err)
		}
	}
	const counted = `courier_investigation_feedback_total{agent="kagent/k8s-agent",alertname="KubePodCrashLooping",binding="sre/sre-alerts",verdict="%s"} 1`
	click("U1", "feedback-useful", value)
	a.r.waitMetric(strings.Replace(counted, "%s", "useful", 1))
	click("U1", "feedback-not-useful", notUseful) // U1 changes their mind
	a.r.waitMetric(strings.Replace(counted, "%s", "not_useful", 1))
	click("U2", "feedback-useful", value)
	click("UBOT", "feedback-useful", value) // ignored
	click("U3", "feedback-useful", value)   // not a member: ignored
	settle()

	told := func(user string) (n int) {
		for _, e := range a.fake.Ephemerals(alertChannel) {
			if e.Recipient == user {
				n++
			}
		}
		return n
	}
	if told("U1") != 2 || told("U2") != 1 || told("UBOT") != 0 || told("U3") != 0 {
		t.Errorf("told U1 %d, U2 %d, the bot %d, the outsider %d; want each member once for each press, and the bot and the outsider not at all", told("U1"), told("U2"), told("UBOT"), told("U3"))
	}
	if after := len(a.fake.Messages(alertChannel)); after != before {
		t.Errorf("the channel has %d messages after the votes, %d before; want no channel noise from a vote", after, before)
	}

	a.r.cancel()
	if err := a.r.wait(); err != nil {
		t.Fatal(err)
	}
	st, err := sqlite.Open(t.Context(), a.cfg.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	key := store.ConversationKey{Connection: "courier-system/slack-main", Channel: alertChannel, Thread: alert.TS}
	votes, err := st.ListVotes(t.Context(), key)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]store.FeedbackVote{}
	for _, v := range votes {
		got[v.User] = v
	}
	u1, u2 := got["U1"], got["U2"]
	if len(votes) != 2 || u1.Verdict != store.VerdictNotUseful || u2.Verdict != store.VerdictUseful {
		t.Fatalf("votes = %+v; want U1's latest (not useful) and U2's, and none for the bot or the person outside the channel", votes)
	}
	if u1.Binding != "sre/sre-alerts" || u1.Agent != "kagent/k8s-agent" || u1.Alert != "KubePodCrashLooping" || u1.Kind != store.FeedbackInvestigation || u1.Turn == "" {
		t.Errorf("U1's vote = %+v; want the Binding, agent, alert and kind of the investigation", u1)
	}
	entries, err := st.ListAudit(t.Context(), time.Time{}, 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	var audited []string
	for _, e := range entries {
		if strings.HasPrefix(e.Outcome, "feedback:") {
			audited = append(audited, e.User+" "+e.Outcome)
			for _, w := range alertWords {
				if strings.Contains(e.Subject+e.Outcome, w) {
					t.Errorf("the audit entry %+v names %q: a vote is audited by identifiers and verdict", e, w)
				}
			}
		}
	}
	want := []string{"U1 feedback: verdict=useful changed=false", "U1 feedback: verdict=not_useful changed=true", "U2 feedback: verdict=useful changed=false"}
	if strings.Join(audited, "\n") != strings.Join(want, "\n") {
		t.Errorf("audited %q; want %q", audited, want)
	}
}
