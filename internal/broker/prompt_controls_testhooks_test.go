//go:build testhooks

package broker

import (
	"encoding/json"
	"strconv"
	"testing"
)

// questionControl is the ID of the input block of question i of a prompt's blocks and of its element's action, as
// the adapter laid the prompt out, so that a press answers where the prompt asks and assumes no IDs.
func questionControl(t *testing.T, blocks json.RawMessage, i int) (block, action string) {
	t.Helper()
	var bs []struct {
		Type    string `json:"type"`
		BlockID string `json:"block_id"`
		Element struct {
			ActionID string `json:"action_id"`
		} `json:"element"`
	}
	if err := json.Unmarshal(blocks, &bs); err != nil {
		t.Fatalf("blocks %s: %v", blocks, err)
	}
	want := "q" + strconv.Itoa(i)
	for _, b := range bs {
		if b.Type == "input" && b.BlockID == want && b.Element.ActionID != "" {
			return b.BlockID, b.Element.ActionID
		}
	}
	t.Fatalf("the prompt %s has no input for question %d", blocks, i)
	return "", ""
}
