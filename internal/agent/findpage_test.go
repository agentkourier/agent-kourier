package agent

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/dialect/generic"
)

// listing is a fake ListTasks. It records the pageSize and pageToken of each request. A page asked for with a pageSize of more
// than fits (or none, which is the server's own default) is an answer over the bound; one that fits is what pages says for
// its page token, and a page token in over is over the bound at every size.
type listing struct {
	mu     sync.Mutex
	sizes  []int
	tokens []string
	fits   int
	pages  map[string]string
	over   string
}

func (l *listing) serve(call rpc, w http.ResponseWriter) {
	if call.Method != "ListTasks" {
		return
	}
	var p struct {
		PageSize  int    `json:"pageSize"`
		PageToken string `json:"pageToken"`
	}
	_ = json.Unmarshal(call.Params, &p)
	l.mu.Lock()
	l.sizes, l.tokens = append(l.sizes, p.PageSize), append(l.tokens, p.PageToken)
	big := p.PageToken == l.over && l.over != "" || p.PageSize == 0 || p.PageSize > l.fits
	page := l.pages[p.PageToken]
	l.mu.Unlock()
	if big {
		_, _ = w.Write(oversizedAnswer())
		return
	}
	_, _ = w.Write([]byte(page))
}

// listPage is a page of the answer to ListTasks: the tasks, and the token of the next page if there is one.
func listPage(next string, tasks ...string) string {
	token := ""
	if next != "" {
		token = `,"nextPageToken":"` + next + `"`
	}
	return `{"jsonrpc":"2.0","id":"1","result":{"tasks":[` + strings.Join(tasks, ",") + `]` + token + `}}`
}

// foundTask is a task whose history holds message m-1.
const foundTask = `{"id":"t-found","contextId":"c-1","status":{"state":"TASK_STATE_WORKING"},"history":[{"messageId":"m-1","role":"ROLE_USER","parts":[{"text":"go"}]}]}`

// A page of the task list that is past the bound is asked for again at half the size, and again at half of that, down to a
// floor, and the pages after it are asked for at the size that worked. The server's own default is unknown, so the first
// retry is half of the 50 that the protocol names as the default.
func TestFindTaskReadsAPageThatIsTooLargeAtHalfTheSize(t *testing.T) {
	l := &listing{fits: 6, pages: map[string]string{"": listPage("p2"), "p2": listPage("", foundTask)}}
	fa := newFakeAgent(t, l.serve)
	task, holders, err := fa.agentViaWith(t, generic.New(0), quick).FindTask(t.Context(), "c-1", "m-1")
	if err != nil || task == nil || task.ID != "t-found" || !slices.Equal(holders, []a2a.TaskID{"t-found"}) {
		t.Fatalf("FindTask = %v, %v, %v; want the holder on the second page", task, holders, err)
	}
	if want := []int{0, 25, 12, 6, 6}; !slices.Equal(l.sizes, want) {
		t.Errorf("page sizes asked = %v, want %v: halved until a page fits, and kept for the next page", l.sizes, want)
	}
	if want := []string{"", "", "", "", "p2"}; !slices.Equal(l.tokens, want) {
		t.Errorf("page tokens asked = %q, want %q: a retry is for the same page", l.tokens, want)
	}
}

// A page that does not fit at the floor ends the lookup with the error, and nothing more is read.
func TestFindTaskGivesUpOnAPageThatIsTooLargeAtTheFloor(t *testing.T) {
	l := &listing{}
	fa := newFakeAgent(t, l.serve)
	task, holders, err := fa.agentViaWith(t, generic.New(0), quick).FindTask(t.Context(), "c-1", "m-1")
	if !errors.Is(err, dialect.ErrResponseTooLarge) || !errors.Is(err, ErrLookupIncomplete) || task != nil || holders != nil {
		t.Fatalf("FindTask = %v, %v, %v; want an incomplete lookup, caused by the page that is too large, and no task", task, holders, err)
	}
	if want := []int{0, 25, 12, 6, 3}; !slices.Equal(l.sizes, want) {
		t.Errorf("page sizes asked = %v, want %v: down to a floor of 3 and no further", l.sizes, want)
	}
}

// A holder found on an earlier page is the answer when a later page cannot be read at any size: the agent has the turn, which
// is what the lookup is for. The page that could not be read might hold a second run of it, which this does not see.
func TestFindTaskKeepsAHolderFromAnEarlierPageWhenALaterOneIsTooLarge(t *testing.T) {
	l := &listing{fits: 50, over: "p2", pages: map[string]string{"": listPage("p2", foundTask)}}
	fa := newFakeAgent(t, l.serve)
	task, holders, err := fa.agentViaWith(t, generic.New(0), quick).FindTask(t.Context(), "c-1", "m-1")
	if err != nil || task == nil || task.ID != "t-found" || !slices.Equal(holders, []a2a.TaskID{"t-found"}) {
		t.Fatalf("FindTask = %v, %v, %v; want the holder of the first page, and no error", task, holders, err)
	}
	if want := []int{0, 25, 25, 12, 6, 3}; !slices.Equal(l.sizes, want) {
		t.Errorf("page sizes asked = %v, want %v: the first page at the server's own size and then 25, the second at every size down to the floor before the holder was used", l.sizes, want)
	}
}
