package interact

import (
	"fmt"
	"time"

	"github.com/agentkourier/agent-kourier/internal/chat"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/store"
)

// messageAt is the message of an interaction's prompt. The platform and the connection are the adapter's to know:
// it reads the channel and the ID, until the outbox stores a Ref (WP-37 S5).
func messageAt(row store.Interaction) chat.Ref {
	return chat.Ref{Channel: row.Key.Channel, ID: row.MessageRef, Thread: row.Key.ThreadTS}
}

// answeredWord is what a free-text answer is shown as once the prompt is replaced. What the person typed may
// be a secret, and the channel keeps what it is shown (the owner's decision), so only that there is an answer
// is said, in courier's own word.
const answeredWord = "(answered)"

// These builders make the messages of an interaction in the chat-neutral model (package chat): the prompt that
// asks, and the ending that takes its place. They carry what the agent wrote as literal text and say nothing of
// controls, layout or limits: each adapter lays the prompt out for its platform, makes the text inert, cuts it
// to what the platform shows, and says whether it fits (Platform.Fits).

// promptOf is the neutral prompt of an ask-user pause: the agent's hint, its questions, the deadline, and the
// sub-agent that raised it, if one did. id is what a press carries back.
func promptOf(id string, in dialect.Interaction, deadline time.Time) (chat.Prompt, error) {
	if in.Kind != dialect.AskUser {
		return chat.Prompt{}, fmt.Errorf("interact: no prompt is made for a %q pause", in.Kind)
	}
	return questionsOf(id, in, deadline), nil
}

// questionsOf is what a prompt and its ending say of the question: the hint, the questions and the sub-agent.
func questionsOf(id string, in dialect.Interaction, deadline time.Time) chat.Prompt {
	p := chat.Prompt{ID: id, Hint: in.Hint, Deadline: deadline}
	for _, q := range in.Questions {
		p.Questions = append(p.Questions, chat.Question{Text: q.Text, Choices: q.Choices, Multiple: q.Multiple})
	}
	if in.Nested != nil {
		p.Subagent = in.Nested.Subagent
	}
	return p
}

// endingOf is the neutral ending of a prompt: how it ended, who decided (by, as courier verified it, or ""),
// and when. For an answered one, answers is what was answered to each question, in order; the ending shows
// the chosen choices and, for a free-text question, only that it was answered. The prompt of an ending is
// the question as asked, which has no deadline of its own to show.
func endingOf(outcome chat.Outcome, by string, at time.Time, in dialect.Interaction, answers [][]string) chat.Ending {
	e := chat.Ending{Outcome: outcome, By: by, At: at, Prompt: questionsOf("", in, time.Time{})}
	if outcome != chat.Answered {
		return e
	}
	e.Answers = make([][]string, len(in.Questions))
	for i, q := range in.Questions {
		var given []string
		if i < len(answers) {
			given = answers[i]
		}
		switch {
		case len(given) == 0: // no answer: the adapter says so
		case len(q.Choices) == 0:
			e.Answers[i] = []string{answeredWord}
		default:
			e.Answers[i] = given
		}
	}
	return e
}
