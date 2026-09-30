package stubagent_test

import (
	"fmt"
	"slices"
	"testing"

	"github.com/agentkourier/agent-kourier/test/stubagent"
)

// listed returns the tasks of a ListTasks answer.
func listed(t *testing.T, got obj) []any {
	t.Helper()
	tasks, ok := dig(got, "result", "tasks").([]any)
	if !ok {
		t.Fatalf("no tasks list in %v", got)
	}
	return tasks
}

// messageIDs returns the messageIds in the history of one listed task.
func messageIDs(tk any) []string {
	var ids []string
	history, _ := dig(tk, "history").([]any)
	for _, h := range history {
		id, _ := dig(h, "messageId").(string)
		ids = append(ids, id)
	}
	return ids
}

// TestSkeletonComparesEveryElement guards the fixture comparisons of the ListTasks tests: a list
// differs from the recorded one in its length, and in any element, not only the first.
func TestSkeletonComparesEveryElement(t *testing.T) {
	one, same := []any{obj{"a": "x"}, obj{"b": 1.0}}, []any{obj{"a": "y"}, obj{"b": 2.0}}
	if skeleton(one) != skeleton(same) || skeleton(one) == skeleton(one[:1]) || skeleton(one) == skeleton([]any{one[0], obj{"c": 1.0}}) {
		t.Errorf("skeleton(%v) must equal one of the same fields, and differ in length and in a later element", one)
	}
}

// TestListTasksFindsBothTasksOfADuplicateRun reproduces oidc-replay 04 and 07: m1 completes and
// is replayed with the contextId (a second task, spec 8.2), then m2 runs in the context. The
// list shows all three in the order they began, each with its messageId, m2's WORKING.
func TestListTasksFindsBothTasksOfADuplicateRun(t *testing.T) {
	s, agent, c := newAgent(t)
	agent.Next(stubagent.Text("first run"))
	agent.Next(stubagent.Text("second run"))
	slowTurn(agent)
	_, sent := c.call("SendMessage", msg("m1", "go slowly", ""))
	ctxID := task(t, sent)["contextId"].(string)
	c.call("SendMessage", msg("m1", "go slowly", ctxID))

	_, afterTwo := c.call("ListTasks", obj{"contextId": ctxID})

	two := listed(t, afterTwo)
	if len(two) != 2 || !slices.Equal(messageIDs(two[0]), []string{"m1"}) || !slices.Equal(messageIDs(two[1]), []string{"m1"}) {
		t.Fatalf("after the replay: %v, want two tasks holding m1", two)
	}
	expectLike(t, "ListTasks", afterTwo, fixture(t, "oidc-replay/04-m1-listtasks.response.json")[0])

	st := c.open("SendStreamingMessage", msg("m2", "go slowly again", ctxID))
	defer func() { _ = st.resp.Body.Close() }()
	_, running := c.call("ListTasks", obj{"contextId": ctxID})
	defer s.Release("go")

	three := listed(t, running)
	if len(three) != 3 || !slices.Equal(messageIDs(three[2]), []string{"m2"}) || dig(three[2], "status", "state") != working ||
		dig(three[0], "id") != dig(two[0], "id") || dig(three[1], "id") != dig(two[1], "id") {
		t.Fatalf("while m2 runs: %v, want the two m1 tasks, then m2's WORKING", three)
	}
	expectLike(t, "ListTasks while running", running, fixture(t, "oidc-replay/07-m2-listtasks-while-running.response.json")[0])
}

// TestListTasksIsOwnerScoped: another identity's context is -31403 (oidc-identity/09), and a
// list with no filter holds only the caller's own tasks (oidc-identity/16, 17).
func TestListTasksIsOwnerScoped(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Reply(func(string) []stubagent.Step { return nil })
	a := c.as(unsigned(t, obj{"email": "a@example.test"}))
	b := c.as(unsigned(t, obj{"email": "b@example.test"}))
	_, aFirst := a.call("SendMessage", msg("a1", "one", ""))
	a.call("SendMessage", msg("a2", "two", ""))
	b.call("SendMessage", msg("b1", "mine", ""))
	aCtx := task(t, aFirst)["contextId"].(string)

	status, denied := b.call("ListTasks", obj{"contextId": aCtx})
	_, forA := a.call("ListTasks", obj{})
	_, forB := b.call("ListTasks", obj{})

	info, _ := dig(denied, "error", "data").([]any)
	if status != 200 || dig(denied, "error", "code") != float64(-31403) || len(info) != 1 || dig(info[0], "reason") != "UNAUTHORIZED" {
		t.Errorf("B listing A's context = HTTP %d %v, want -31403, UNAUTHORIZED", status, denied)
	}
	expectLike(t, "refusal", denied, fixture(t, "oidc-identity/09-b-listtasks-a-context.response.json")[0])
	var mine [][]string
	for _, tk := range listed(t, forA) {
		mine = append(mine, messageIDs(tk))
	}
	if want := [][]string{{"a1"}, {"a2"}}; !slices.EqualFunc(mine, want, slices.Equal) {
		t.Errorf("A's unfiltered list = %v, want %v", mine, want)
	}
	if bs := listed(t, forB); len(bs) != 1 || !slices.Equal(messageIDs(bs[0]), []string{"b1"}) || dig(forB, "result", "totalSize") != float64(1) {
		t.Errorf("B's unfiltered list = %v, want only b1", forB)
	}
	expectLike(t, "B's list", forB, fixture(t, "oidc-identity/17-b-listtasks-unfiltered.response.json")[0])
}

// TestListTasksShowsAPauseAsItWasMade reproduces oidc-identity/16: a list from a client that
// names no HITL extension carries the request of a pause made for one that did, and only the
// text of one made for one that did not, and no artifacts.
func TestListTasksShowsAPauseAsItWasMade(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(dbAsk(nil))
	agent.Next(dbAsk(nil))
	agent.Reply(func(string) []stubagent.Step { return []stubagent.Step{stubagent.Text("The answer is 4.")} })
	owner := c.as(unsigned(t, obj{"email": "a@example.test"}))
	withHITL(c.as(unsigned(t, obj{"email": "a@example.test"}))).send(msg("p1", "ask", ""))
	owner.send(msg("p2", "ask", ""))
	owner.call("SendMessage", msg("m3", "What is 2+2?", ""))
	owner.call("SendMessage", msg("m4", "What is 2+2?", ""))

	_, got := owner.call("ListTasks", obj{})

	tasks := listed(t, got)
	if len(tasks) != 4 {
		t.Fatalf("A's list holds %d tasks, want 4: %v", len(tasks), got)
	}
	// The fields decide it: the request only in the first pause, no artifacts anywhere.
	recorded := dig(fixture(t, "oidc-identity/16-a-listtasks-unfiltered.response.json")[0], "result", "tasks").([]any)
	for i, tk := range tasks {
		expectLike(t, fmt.Sprintf("task %d", i), obj{"result": tk}, obj{"result": recorded[[]int{1, 2, 3, 3}[i]]})
	}
}

// TestListTasksPagesEveryTaskOnce: a page holds 50 tasks (oidc-replay 10 shows the default),
// and following nextPageToken visits each task once, in the order they began.
func TestListTasksPagesEveryTaskOnce(t *testing.T) {
	for _, tc := range []struct {
		n     int
		size  int // the request's pageSize, 0 for the default
		pages []int
	}{
		{n: 50, pages: []int{50}},
		{n: 51, pages: []int{50, 1}},
		{n: 120, pages: []int{50, 50, 20}},
		{n: 7, size: 3, pages: []int{3, 3, 1}},
	} {
		t.Run(fmt.Sprintf("%d tasks, pageSize %d", tc.n, tc.size), func(t *testing.T) {
			_, agent, c := newAgent(t)
			agent.Reply(func(string) []stubagent.Step { return nil })
			var want []string
			ctxID := ""
			for i := range tc.n {
				_, sent := c.call("SendMessage", msg(fmt.Sprintf("m%d", i), "hi", ctxID))
				ctxID = task(t, sent)["contextId"].(string)
				want = append(want, task(t, sent)["id"].(string))
			}

			var got []string
			var sizes []int
			token := ""
			for range tc.n + 1 { // a lister that never ends fails, it does not hang
				params := obj{"contextId": ctxID, "pageToken": token}
				if tc.size > 0 {
					params["pageSize"] = tc.size
				}
				_, page := c.call("ListTasks", params)
				tasks := listed(t, page)
				sizes = append(sizes, len(tasks))
				for _, tk := range tasks {
					got = append(got, dig(tk, "id").(string))
				}
				wantSize := float64(50)
				if tc.size > 0 {
					wantSize = float64(tc.size)
				}
				if dig(page, "result", "totalSize") != float64(tc.n) || dig(page, "result", "pageSize") != wantSize {
					t.Errorf("paging fields = %v, want totalSize %d, pageSize %v", page["result"], tc.n, wantSize)
				}
				if token, _ = dig(page, "result", "nextPageToken").(string); token == "" {
					break
				}
			}

			if !slices.Equal(got, want) || !slices.Equal(sizes, tc.pages) {
				t.Errorf("pages of %v holding %d tasks, want pages of %v holding the %d tasks once each, in order", sizes, len(got), tc.pages, len(want))
			}
		})
	}
}

// TestListTasksRefusesWhatItCannotPage: UNCONFIRMED: the errors follow a2a-go's in-memory store,
// which kagent may not share.
func TestListTasksRefusesWhatItCannotPage(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Reply(func(string) []stubagent.Step { return nil })
	a := c.as(unsigned(t, obj{"email": "a@example.test"}))
	b := c.as(unsigned(t, obj{"email": "b@example.test"}))
	for i := range 2 {
		a.call("SendMessage", msg(fmt.Sprintf("a%d", i), "hi", ""))
	}
	_, first := a.call("ListTasks", obj{"pageSize": 1})
	token, _ := dig(first, "result", "nextPageToken").(string)
	if token == "" {
		t.Fatalf("a page of one of two tasks has no nextPageToken: %v", first)
	}

	for _, tc := range []struct {
		name   string
		client *client
		params obj
		code   float64
	}{
		{"a pageSize that is not a number", a, obj{"pageSize": "many"}, -32602},
		{"pageSize 0 is the default", a, obj{"pageSize": 0}, 0},
		{"pageSize 101", a, obj{"pageSize": 101}, -32600},
		{"pageSize -1", a, obj{"pageSize": -1}, -32600},
		{"a token that is not one", a, obj{"pageToken": "nope"}, -32700},
		{"another owner's token", b, obj{"pageToken": token}, -32700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, got := tc.client.call("ListTasks", tc.params)

			if code, _ := dig(got, "error", "code").(float64); code != tc.code {
				t.Errorf("ListTasks %v = %v, want code %v", tc.params, got, tc.code)
			}
		})
	}
}

// TestListTasksOfAnUnknownContextIsEmpty: UNCONFIRMED: no fixture lists a context that does not
// exist. The list is empty, with the tasks key an array and the paging fields present.
func TestListTasksOfAnUnknownContextIsEmpty(t *testing.T) {
	_, _, c := newAgent(t)

	_, got := c.call("ListTasks", obj{"contextId": "nope"})

	res, _ := got["result"].(obj)
	if tasks, ok := res["tasks"].([]any); !ok || len(tasks) != 0 || res["totalSize"] != float64(0) || res["pageSize"] != float64(50) || res["nextPageToken"] != "" {
		t.Errorf("ListTasks of an unknown context = %v, want an empty list and the paging fields", got)
	}
}

// TestListTasksGoesByTheRequestThatResumedThePause: UNCONFIRMED: no fixture answers a pause for a
// client that differs from the first. The second pause of a task belongs to the answer that
// resumed it, so an answer that names no extension makes a pause that lists as text.
func TestListTasksGoesByTheRequestThatResumedThePause(t *testing.T) {
	_, agent, c := newAgent(t)
	agent.Next(dbAsk(func(stubagent.Response) []stubagent.Step { return []stubagent.Step{dbAsk(nil)} }))
	owner := c.as(unsigned(t, obj{"email": "a@example.test"}))
	_, taskID, ctxID, request := paused(t, withHITL(c.as(unsigned(t, obj{"email": "a@example.test"}))))
	_, first := owner.call("ListTasks", obj{})

	owner.send(reply("m2", "MySQL", ctxID, taskID, answers(request["id"].(string), []string{"MySQL"})))
	_, second := owner.call("ListTasks", obj{})

	before, after := listed(t, first)[0], listed(t, second)[0]
	if dig(before, "status", "message", "metadata", hitlURI) == nil || dig(after, "status", "state") != inputRequired ||
		dig(after, "status", "message", "metadata", hitlURI) != nil || text(dig(after, "status", "message", "parts")) != dbQuestion {
		t.Errorf("first pause listed as %v, second as %v; want the request in the first, the question as text in the second", before, after)
	}
}
