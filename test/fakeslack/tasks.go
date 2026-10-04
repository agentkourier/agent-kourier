package fakeslack

import "encoding/json"

// Task is a task_update chunk of a streamed message as the stream stands: the fields of the last chunk that named
// its ID.
type Task struct {
	ID, Title, Status, Details, Output string
}

// Tasks returns the tasks of a streamed message, in the order of each one's first task_update chunk, each as its last
// chunk left it. It is for test assertions.
//
// UNCONFIRMED(WP-04): that a task_update chunk with an ID the stream has seen updates that task and does not make
// another, and that it replaces the earlier chunk and does not merge with it. The second is the stricter reading,
// which finds an adapter that leaves a field out of a later chunk.
func (m Message) Tasks() []Task {
	var out []Task
	for _, raw := range m.Chunks {
		var c struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Title   string `json:"title"`
			Status  string `json:"status"`
			Details string `json:"details"`
			Output  string `json:"output"`
		}
		if json.Unmarshal(raw, &c) != nil || c.Type != "task_update" {
			continue
		}
		t := Task{ID: c.ID, Title: c.Title, Status: c.Status, Details: c.Details, Output: c.Output}
		replaced := false
		for i := range out {
			if out[i].ID == c.ID {
				out[i], replaced = t, true
				break
			}
		}
		if !replaced {
			out = append(out, t)
		}
	}
	return out
}
