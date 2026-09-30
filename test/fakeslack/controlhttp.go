package fakeslack

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"
)

// maxAckWait bounds the timeout of GET /_control/acks/{id}; a wait never hangs.
const maxAckWait = 30 * time.Second

// isControlPath reports whether a request path belongs to the control surface
// rather than to a Slack method.
func isControlPath(p string) bool { return p == "/_control" || strings.HasPrefix(p, "/_control/") }

// controlReq is the JSON body of the control endpoints that act as a user or
// set up the workspace; each endpoint reads only the fields its Go method takes.
type controlReq struct {
	Channel   string      `json:"channel"`
	ThreadTS  string      `json:"thread_ts"`
	User      string      `json:"user"`
	Text      string      `json:"text"`
	MessageTS string      `json:"message_ts"`
	ActionID  string      `json:"action_id"`
	Value     string      `json:"value"`
	Values    StateValues `json:"values"`
	Users     []string    `json:"users"`
}

// newControlMux builds the routes of the control surface: JSON over HTTP,
// wrapping the Go control API one for one, with no Slack authentication. It is
// for tests inside a cluster and must not be exposed anywhere else.
func (s *Server) newControlMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/_control/", func(w http.ResponseWriter, _ *http.Request) {
		writeControl(w, http.StatusNotFound, map[string]string{"error": "no such control endpoint"})
	})
	envelopeID := func(id string, err error) (any, error) { return map[string]string{"envelope_id": id}, err }
	mux.HandleFunc("POST /_control/mention", post(func(q controlReq) (any, error) { return s.Mention(q.Channel, q.User, q.Text) }))
	mux.HandleFunc("POST /_control/reply", post(func(q controlReq) (any, error) {
		return s.Reply(q.Channel, q.ThreadTS, q.User, q.Text)
	}))
	mux.HandleFunc("POST /_control/dm", post(func(q controlReq) (any, error) { return s.DM(q.User, q.Text) }))
	mux.HandleFunc("POST /_control/click", post(func(q controlReq) (any, error) {
		return envelopeID(s.Click(q.User, q.MessageTS, q.ActionID, q.Value))
	}))
	mux.HandleFunc("POST /_control/submit", post(func(q controlReq) (any, error) {
		return envelopeID(s.Submit(q.User, q.MessageTS, q.ActionID, q.Values))
	}))
	mux.HandleFunc("POST /_control/users", post(func(u User) (any, error) { s.AddUser(u); return struct{}{}, nil }))
	mux.HandleFunc("POST /_control/members", post(func(q controlReq) (any, error) {
		s.SetMembers(q.Channel, q.Users...)
		return struct{}{}, nil
	}))
	mux.HandleFunc("GET /_control/connections", func(w http.ResponseWriter, _ *http.Request) {
		writeControl(w, http.StatusOK, map[string]int{"connections": s.Connections()})
	})
	mux.HandleFunc("GET /_control/messages", func(w http.ResponseWriter, r *http.Request) {
		writeControl(w, http.StatusOK, s.Messages(r.URL.Query().Get("channel")))
	})
	mux.HandleFunc("GET /_control/thread", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		thread := s.Thread(q.Get("channel"), q.Get("ts"))
		if thread == nil {
			writeControlError(w, ErrNotFound)
			return
		}
		writeControl(w, http.StatusOK, thread)
	})
	mux.HandleFunc("GET /_control/acks/{id}", s.controlAck)
	return mux
}

// controlAck answers whether an envelope has been acked, waiting up to the
// request's timeout parameter (a Go duration, at most maxAckWait, default none)
// for the ack. A timeout is an answer, not an error: acked is false.
func (s *Server) controlAck(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	wait := time.Duration(0)
	if v := r.URL.Query().Get("timeout"); v != "" {
		var err error
		if wait, err = time.ParseDuration(v); err != nil || wait < 0 || wait > maxAckWait {
			writeControl(w, http.StatusBadRequest, map[string]string{"error": "timeout must be a duration from 0s to 30s"})
			return
		}
	}
	if s.envelope(id) == nil {
		writeControlError(w, ErrNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	_ = s.WaitAck(ctx, id) // only ever the timeout; the envelope exists
	payload, acked := s.Ack(id)
	writeControl(w, http.StatusOK, struct {
		Acked   bool            `json:"acked"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}{acked, payload})
}

// post wraps a control call as a handler: it decodes the JSON body, strictly so
// that a misspelled field is an error, and writes the result or the error.
func post[T any](call func(T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in T
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeControl(w, http.StatusBadRequest, map[string]string{"error": "bad request body: " + err.Error()})
			return
		}
		out, err := call(in)
		if err != nil {
			writeControlError(w, err)
			return
		}
		writeControl(w, http.StatusOK, out)
	}
}

// writeControlError maps a control API error to a status: no connected client
// is a conflict with the fake's state (409), something that does not exist is
// 404, and anything else the caller asked for wrongly is 400.
func writeControlError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrNoConnection):
		status = http.StatusConflict
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	}
	writeControl(w, status, map[string]string{"error": err.Error()})
}

func writeControl(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
