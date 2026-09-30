package stubagent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
)

// Measured: a list with no pageSize answers pageSize 50 and an empty nextPageToken, with every
// task on the page (oidc-replay 04, 07, 10; oidc-identity 16, 17).
const (
	defaultPageSize = 50
	maxPageSize     = 100
)

// listTasks answers ListTasks with the caller's own tasks, oldest first, a page at a time.
//
// Measured (WP-03): a list by contextId or with no filter (only the caller's tasks, oidc-identity
// 16, 17); another identity's contextId is -31403 (oidc-identity/09); each task is as GetTask
// shows it, its history holding the messageIds, less the artifacts (oidc-replay 04, 07); the
// result always has tasks, totalSize, pageSize and nextPageToken.
// UNCONFIRMED: paging past a page, which WP-03 never reached: that totalSize counts the tasks
// before paging and pageSize is the size asked for; the token, here the last task of the page;
// -32700 for a bad token and -32600 for a pageSize outside 1 to 100 (a2a-go's in-memory store;
// kagent's store is its own); the order, by creation here, as the fixtures are ordered by
// creation and by last update alike; a contextId that names no session listing nothing; and the
// params ignored: status, statusTimestampAfter, historyLength, includeArtifacts and tenant.
func (a *Agent) listTasks(o *rw, params json.RawMessage) {
	var p struct {
		ContextID string `json:"contextId"`
		PageSize  int    `json:"pageSize"`
		PageToken string `json:"pageToken"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		o.fail(invalidParams("params: %v", err))
		return
	}
	a.s.mu.Lock()
	res, err := a.page(o.owner, p.ContextID, p.PageSize, p.PageToken)
	a.s.mu.Unlock()
	if err != nil {
		o.fail(err)
		return
	}
	o.event(res)
	o.done()
}

// page builds the result of a ListTasks for owner. The caller holds s.mu.
func (a *Agent) page(owner, ctxID string, size int, token string) (obj, *rpcError) {
	if s := a.sessions[ctxID]; s != nil && s.owner != owner {
		return nil, deniedError()
	}
	switch {
	case size == 0:
		size = defaultPageSize
	case size < 0 || size > maxPageSize:
		return nil, &rpcError{codeInvalidReq, fmt.Sprintf("page size must be between 1 and %d inclusive, got %d: invalid request", maxPageSize, size), "INVALID_REQUEST"}
	}
	var mine []*task
	for _, t := range a.list {
		if t.sess.owner == owner && (ctxID == "" || t.sess.id == ctxID) {
			mine = append(mine, t)
		}
	}
	from := 0
	if token != "" { // the id of the last task of the page before, among the caller's own
		last, err := base64.RawURLEncoding.DecodeString(token)
		at := slices.IndexFunc(mine, func(t *task) bool { return t.id == string(last) })
		if err != nil || at < 0 {
			return nil, &rpcError{codeParse, "parse error: invalid page token", "PARSE_ERROR"}
		}
		from = at + 1
	}
	end, next := min(from+size, len(mine)), ""
	if end < len(mine) {
		next = base64.RawURLEncoding.EncodeToString([]byte(mine[end-1].id))
	}
	page := []obj{}
	for _, t := range mine[from:end] {
		page = append(page, t.listed())
	}
	return obj{"tasks": page, "totalSize": len(mine), "pageSize": size, "nextPageToken": next}, nil
}

// listed is the task as ListTasks shows it: as GetTask does, less the artifacts. Measured
// (oidc-identity/16): a list from a client that names no extension carries the HITL request of
// one pause and only the text of another, so a pause lists as the request that made it left it.
// Here a pause made for a client that did not name the extension shows only its text.
// UNCONFIRMED: that GetTask does the same (the stub's goes by the reading request's header, see
// rw.view), and that a task resumed by an answer takes the answer's extension.
func (t *task) listed() obj {
	snap := t.snapshot()
	delete(snap, "artifacts")
	if !t.ext {
		snap = withoutHITL(snap)
	}
	return snap
}
