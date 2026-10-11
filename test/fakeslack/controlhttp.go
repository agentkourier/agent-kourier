package fakeslack

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path"
	"slices"
	"strings"
	"time"
)

// maxAckWait bounds the timeout of GET /_control/acks/{id}; a wait never hangs.
const maxAckWait = 30 * time.Second

// isControlPath reports whether a request path belongs to the control surface
// rather than to a Slack method.
func isControlPath(p string) bool { return p == "/_control" || strings.HasPrefix(p, "/_control/") }

// The JSON bodies of the control endpoints. Each endpoint takes exactly the
// fields of its Go method, and refuses any others.
type (
	mentionReq struct {
		Channel string `json:"channel"`
		User    string `json:"user"`
		Text    string `json:"text"`
	}
	replyReq struct {
		Channel  string `json:"channel"`
		ThreadTS string `json:"thread_ts"`
		User     string `json:"user"`
		Text     string `json:"text"`
	}
	dmReq struct {
		User string `json:"user"`
		Text string `json:"text"`
	}
	clickReq struct {
		User      string `json:"user"`
		MessageTS string `json:"message_ts"`
		ActionID  string `json:"action_id"`
		Value     string `json:"value"`
	}
	submitReq struct {
		User      string      `json:"user"`
		MessageTS string      `json:"message_ts"`
		ActionID  string      `json:"action_id"`
		Values    StateValues `json:"values"`
	}
	membersReq struct {
		Channel string   `json:"channel"`
		Users   []string `json:"users"`
	}
	usergroupReq struct {
		Usergroup string   `json:"usergroup"`
		Users     []string `json:"users"`
	}
)

// MessageView is a Message as the control surface shows it: the Slack shape
// plus the streaming state and chunks that the Slack wire format leaves out.
// Read Streaming and Chunks on the view itself: they shadow Message's own
// fields of the same names, which JSON skips and so stay zero when decoded.
type MessageView struct {
	Message
	Streaming bool              `json:"streaming"`
	Chunks    []json.RawMessage `json:"chunks,omitempty"`
}

func views(msgs []Message) []MessageView {
	out := make([]MessageView, len(msgs))
	for i, m := range msgs {
		out[i] = MessageView{m, m.Streaming, m.Chunks}
	}
	return out
}

// controlRoutes is the control surface's router. Anything it does not route is
// a JSON 404, or a 405 if the path exists for another method.
type controlRoutes struct{ mux *http.ServeMux }

func (c controlRoutes) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if path.Clean(r.URL.Path) == r.URL.Path {
		if _, pattern := c.mux.Handler(r); pattern != "" {
			c.mux.ServeHTTP(w, r) // sets the path values that Handler does not
			return
		}
		for _, method := range []string{http.MethodGet, http.MethodPost} {
			other := r.Clone(r.Context())
			other.Method = method
			if _, pattern := c.mux.Handler(other); pattern != "" {
				w.Header().Set("Allow", method)
				writeControl(w, http.StatusMethodNotAllowed, map[string]string{"error": "use " + method})
				return
			}
		}
	}
	writeControl(w, http.StatusNotFound, map[string]string{"error": "no such control endpoint"})
}

// newControlRoutes builds the routes of the control surface: JSON over HTTP,
// wrapping the Go control API one for one, with no Slack authentication. It is
// for tests inside a cluster and must not be exposed anywhere else.
func (s *Server) newControlRoutes() controlRoutes {
	mux := http.NewServeMux()
	envelopeID := func(id string, err error) (any, error) { return map[string]string{"envelope_id": id}, err }
	mux.HandleFunc("POST /_control/mention", post(func(q mentionReq) (any, error) { return s.Mention(q.Channel, q.User, q.Text) }))
	mux.HandleFunc("POST /_control/reply", post(func(q replyReq) (any, error) {
		return s.Reply(q.Channel, q.ThreadTS, q.User, q.Text)
	}))
	mux.HandleFunc("POST /_control/dm", post(func(q dmReq) (any, error) { return s.DM(q.User, q.Text) }))
	mux.HandleFunc("POST /_control/webhook", post(func(q WebhookPost) (any, error) { return s.Webhook(q) }))
	mux.HandleFunc("POST /_control/click", post(func(q clickReq) (any, error) {
		return envelopeID(s.Click(q.User, q.MessageTS, q.ActionID, q.Value))
	}))
	mux.HandleFunc("POST /_control/submit", post(func(q submitReq) (any, error) {
		return envelopeID(s.Submit(q.User, q.MessageTS, q.ActionID, q.Values))
	}))
	mux.HandleFunc("POST /_control/users", post(func(u User) (any, error) {
		if u.ID == "" {
			return nil, invalid("fakeslack: a user needs an id")
		}
		s.AddUser(u)
		return struct{}{}, nil
	}))
	mux.HandleFunc("POST /_control/members", post(func(q membersReq) (any, error) {
		if q.Channel == "" {
			return nil, invalid("fakeslack: a channel's members need a channel")
		}
		s.SetMembers(q.Channel, q.Users...)
		return struct{}{}, nil
	}))
	mux.HandleFunc("POST /_control/usergroups", post(func(q usergroupReq) (any, error) {
		if q.Usergroup == "" {
			return nil, invalid("fakeslack: a user group needs an id")
		}
		s.SetUserGroup(q.Usergroup, q.Users...)
		return struct{}{}, nil
	}))
	mux.HandleFunc("GET /_control/connections", func(w http.ResponseWriter, _ *http.Request) {
		writeControl(w, http.StatusOK, map[string]int{"connections": s.Connections()})
	})
	mux.HandleFunc("GET /_control/messages", func(w http.ResponseWriter, r *http.Request) {
		writeControl(w, http.StatusOK, views(s.Messages(r.URL.Query().Get("channel"))))
	})
	mux.HandleFunc("GET /_control/thread", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		thread := s.Thread(q.Get("channel"), q.Get("ts"))
		if thread == nil {
			writeControlError(w, ErrNotFound, nil)
			return
		}
		writeControl(w, http.StatusOK, views(thread))
	})
	// The one read path that shows ephemerals, which no user but the recipient may see, so it is for
	// e2e tests only: every other read path, Slack's and the rest of this surface, is blind to them.
	mux.HandleFunc("GET /_control/ephemerals", func(w http.ResponseWriter, r *http.Request) {
		channel := r.URL.Query().Get("channel")
		if channel == "" {
			writeControlError(w, invalid("fakeslack: ephemerals need a channel"), nil)
			return
		}
		writeControl(w, http.StatusOK, s.Ephemerals(channel))
	})
	mux.HandleFunc("GET /_control/reactions", s.controlReactions)
	mux.HandleFunc("GET /_control/acks/{id}", s.controlAck)
	return controlRoutes{mux}
}

// controlReactions answers with the reactions on a message: none is an empty list, and a message
// the fake does not hold is a 404, so that a wrong ts is not mistaken for a message nobody reacted to.
func (s *Server) controlReactions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	channel, ts := q.Get("channel"), q.Get("ts")
	if channel == "" || ts == "" {
		writeControlError(w, invalid("fakeslack: reactions need a channel and a ts"), nil)
		return
	}
	if !slices.ContainsFunc(s.Messages(channel), func(m Message) bool { return m.TS == ts }) {
		writeControlError(w, ErrNotFound, nil)
		return
	}
	reactions := s.Reactions(channel, ts)
	if reactions == nil {
		reactions = []Reaction{}
	}
	writeControl(w, http.StatusOK, reactions)
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
		writeControlError(w, ErrNotFound, nil)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), wait)
	defer cancel()
	_ = s.WaitAck(ctx, id) // ends at the timeout, or when the Server closes
	payload, acked := s.Ack(id)
	writeControl(w, http.StatusOK, struct {
		Acked   bool            `json:"acked"`
		Payload json.RawMessage `json:"payload,omitempty"`
	}{acked, payload})
}

// post wraps a control call as a handler. It decodes the JSON body strictly: a
// field the endpoint does not take, or anything after the body, is a 400. It
// inherits encoding/json's rules, which are that keys match field names without
// regard to case and that a key given twice takes its last value.
func post[T any](call func(T) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var in T
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		err := dec.Decode(&in)
		if err == nil && dec.Decode(&struct{}{}) != io.EOF {
			err = errors.New("unexpected data after the JSON body")
		}
		if err != nil {
			writeControl(w, http.StatusBadRequest, map[string]string{"error": "bad request body: " + err.Error()})
			return
		}
		out, err := call(in)
		if err != nil {
			writeControlError(w, err, out)
			return
		}
		writeControl(w, http.StatusOK, out)
	}
}

// writeControlError maps a control API error to a status: no connected client
// is a conflict with the fake's state (409), something that does not exist is
// 404, a request no user could make is 400, and anything else is the fake's own
// fault (500). If the failed call had already stored a user message, the body
// carries it as "message", so that a test that retries does not post it twice.
func writeControlError(w http.ResponseWriter, err error, partial any) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, ErrNoConnection):
		status = http.StatusConflict
	case errors.Is(err, ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, ErrInvalid):
		status = http.StatusBadRequest
	}
	body := map[string]any{"error": err.Error()}
	if um, ok := partial.(UserMessage); ok && um.TS != "" {
		body["message"] = um
	}
	writeControl(w, status, body)
}

func writeControl(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
