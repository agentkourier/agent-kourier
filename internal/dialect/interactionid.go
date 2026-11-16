package dialect

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// answerSuffix ends the ID of the message that sends an interaction's answer (AnswerID), so that the interaction is read off
// the message.
const answerSuffix = "/answer"

// InteractionID is the ID a pause becomes: a hash of its kind, task, request ID and tool IDs, so the same pause always has
// the same ID and agent text never reaches one. Interactions are stored under it, so it never changes. It is here, with the
// Interaction, so that the session manager, which parks a task on a pause, and the interaction module, which records the
// pause, name it alike.
func InteractionID(in Interaction) string {
	tools := make([]string, len(in.Tools))
	for i, t := range in.Tools {
		tools[i] = t.ID
	}
	slices.Sort(tools)
	var b strings.Builder
	for _, f := range append([]string{string(in.Kind), string(in.TaskID), in.RequestID}, tools...) {
		fmt.Fprintf(&b, "%d:%s;", len(f), f) // length-prefixed, so no two field lists hash alike
	}
	sum := sha256.Sum256([]byte(b.String()))
	return "ix-" + hex.EncodeToString(sum[:])[:32]
}

// AnswerID is the ID of the message that sends the answer to the interaction id, whoever decided it.
func AnswerID(id string) string { return id + answerSuffix }

// AnsweredInteraction is the interaction whose answer the message msgID sends (AnswerID), or "" for a message that sends none.
func AnsweredInteraction(msgID string) string {
	if id, ok := strings.CutSuffix(msgID, answerSuffix); ok {
		return id
	}
	return ""
}
