package fakeslack

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestControlErrorStatusAndPartialMessage(t *testing.T) {
	stored := UserMessage{Channel: "C1", TS: "1.2", Envelopes: []string{"env-000001"}}
	tests := []struct {
		name    string
		err     error
		partial any
		want    int
		message bool
	}{
		{"no client", ErrNoConnection, nil, http.StatusConflict, false},
		{"not found", ErrNotFound, nil, http.StatusNotFound, false},
		{"invalid", invalid("no"), nil, http.StatusBadRequest, false},
		{"anything else", errors.New("boom"), nil, http.StatusInternalServerError, false},
		{"lost event after storing", ErrNoConnection, stored, http.StatusConflict, true},
		{"nothing stored", ErrNoConnection, UserMessage{}, http.StatusConflict, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeControlError(rec, tc.err, tc.partial)
			var body struct{ Message *UserMessage }
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("body %q: %v", rec.Body, err)
			}
			if rec.Code != tc.want || (body.Message != nil) != tc.message || (tc.message && body.Message.TS != "1.2") {
				t.Fatalf("status %d, body %s; want %d, message present %v", rec.Code, rec.Body, tc.want, tc.message)
			}
		})
	}
}
