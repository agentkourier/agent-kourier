package postgres

import "testing"

// TestTextPostgresCannotStoreIsInvalid: Postgres text holds no NUL and only valid UTF-8, which SQLite
// does not insist on. A value it refuses is store.ErrInvalid, as any value that is not legal, and
// changes nothing.
func TestTextPostgresCannotStoreIsInvalid(t *testing.T) {
	s, _ := openFresh(t)
	k := sessionKey("1.1")
	for name, text := range map[string]string{"NUL": "a\x00b", "invalid UTF-8": "a\xffb"} {
		r := reply("r-"+name, k)
		r.Text = text
		_, err := s.EnqueueReply(t.Context(), r)
		if !isInvalid(err) {
			t.Errorf("EnqueueReply with %s in the text = %v; want store.ErrInvalid", name, err)
		}
		if has, _ := s.HasReply(t.Context(), r.ID); has {
			t.Errorf("a reply with %s in its text was queued", name)
		}
	}
	// The pool is still good: the failed statement did not leave a connection in a failed transaction.
	if ok, err := s.EnqueueReply(t.Context(), reply("fine", k)); err != nil || !ok {
		t.Errorf("EnqueueReply after the refusals = %v, %v", ok, err)
	}
}
