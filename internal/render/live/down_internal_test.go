package live

import (
	"errors"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// A Ref made on the platform of a connection that is down names the connection, as the adapter's do, and every call fails with
// the error that says the connection is down.
func TestDownMakesRefsOfItsConnectionAndFailsEveryCall(t *testing.T) {
	d := down{conn: "ns/b", err: chat.ErrConnectionDown}
	if got, want := d.RefAt("C1", "1.1", "2.2"), (chat.Ref{Conn: "ns/b", Channel: "C1", Thread: "1.1", ID: "2.2"}); got != want {
		t.Errorf("RefAt = %+v, want %+v", got, want)
	}
	ctx := t.Context()
	s := d.StreamAt(d.RefAt("C1", "1.1", "2.2"))
	_, streamErr := d.Stream(ctx, chat.Target{}, "", "")
	for name, err := range map[string]error{
		"Update": d.Update(ctx, chat.Ref{}, chat.Outgoing{}), "Stream": streamErr, "MarkBusy": d.MarkBusy(ctx, chat.Target{}, true),
		"Append": s.Append(ctx, "x"), "Step": s.Step(ctx, chat.Step{}), "Stop": s.Stop(ctx, "", chat.Message{}, ""),
	} {
		if !errors.Is(err, chat.ErrConnectionDown) {
			t.Errorf("%s = %v; want an error that wraps ErrConnectionDown", name, err)
		}
	}
	if v := d.Classify(chat.OpPost, chat.ErrConnectionDown); v.Class != chat.Transient {
		t.Errorf("Classify = %+v; want transient", v)
	}
}
