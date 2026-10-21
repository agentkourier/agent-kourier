package fakeslack

import (
	"encoding/json"
	"strings"
)

// Task is a card of a message: a task_update of a streamed message as the stream stands (every chunk that named its
// ID, merged), or a task_card block.
type Task struct {
	ID, Title, Status, Details, Output string
}

// Tasks returns the cards of a message: the tasks of its stream, in the order of each one's first task_update chunk, each
// as its chunks merged leave it, and then its task_card blocks, in order, with the text of their details and output.
// A chat.update with blocks replaces the stream's chunks (updateMessage), so a message that was updated has only the
// cards of its blocks. It is for test assertions.
//
// The merge is Slack's, as observed on 2026-10-18 in two workspaces (read back through the API and in the web
// client): a chunk with an ID the stream has seen updates that card and does not make another; its title and status
// replace the card's; its details and output are appended to the card's, so a chunk that repeats them shows them
// twice; and a chunk that leaves details or output out, or sends them empty, keeps what the card had. A chunk always
// carries a title and a status (slack-go sets both), so a chunk without them was not observed; here an empty one
// keeps the card's too.
//
// Once the stream has stopped, a card that is not complete is an error, as Slack shows it after chat.stopStream
// (probed on 2026-10-21 through the API: an in_progress or pending card read back as error; not in Slack's
// documentation).
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
		i := -1
		for j := range out {
			if out[j].ID == c.ID {
				i = j
				break
			}
		}
		if i < 0 {
			out = append(out, Task{ID: c.ID})
			i = len(out) - 1
		}
		t := &out[i]
		if c.Title != "" {
			t.Title = c.Title
		}
		if c.Status != "" {
			t.Status = c.Status
		}
		t.Details += c.Details
		t.Output += c.Output
	}
	if !m.Streaming {
		for i := range out {
			if out[i].Status != "complete" {
				out[i].Status = "error"
			}
		}
	}
	return append(out, m.taskCards()...)
}

// taskCards are the task_card blocks of m, each with the text of its details and output.
func (m Message) taskCards() []Task {
	var blocks []struct {
		Type    string    `json:"type"`
		TaskID  string    `json:"task_id"`
		Title   string    `json:"title"`
		Status  string    `json:"status"`
		Details *richText `json:"details"`
		Output  *richText `json:"output"`
	}
	if len(m.Blocks) == 0 || json.Unmarshal(m.Blocks, &blocks) != nil {
		return nil
	}
	var out []Task
	for _, b := range blocks {
		if b.Type == "task_card" {
			out = append(out, Task{ID: b.TaskID, Title: b.Title, Status: b.Status, Details: b.Details.text(), Output: b.Output.text()})
		}
	}
	return out
}

// richText is a rich_text block, as far as its text goes.
type richText struct {
	Elements []struct {
		Elements []struct {
			Text string `json:"text"`
		} `json:"elements"`
	} `json:"elements"`
}

// text is the text of every element of r, joined, or "" for no block.
func (r *richText) text() string {
	if r == nil {
		return ""
	}
	var b strings.Builder
	for _, e := range r.Elements {
		for _, t := range e.Elements {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}
