package stubagent

import (
	"encoding/json"
	"net/http"
)

// SendGateState identifies the sender held immediately before a send and counts received sends.
type SendGateState struct {
	MessageID string         `json:"message_id"`
	Sender    string         `json:"sender"`
	Sends     map[string]int `json:"sends"`
}

// ServeSendControl serves the private control routes used by the failover test.
// The first caller after arming waits; later callers can complete the same queued send.
func (s *Server) ServeSendControl(w http.ResponseWriter, r *http.Request) bool {
	switch {
	case r.URL.Path == "/_control/send-gate/arm" && r.Method == http.MethodPost:
		s.mu.Lock()
		if s.sendGate != nil {
			select {
			case <-s.sendGate:
			default:
				close(s.sendGate)
			}
		}
		s.sendGate = make(chan struct{})
		s.sendGateArmed = true
		s.sendGateID = ""
		s.sendGateSender = ""
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/_control/send-gate/release" && r.Method == http.MethodPost:
		s.mu.Lock()
		if s.sendGate != nil {
			select {
			case <-s.sendGate:
			default:
				close(s.sendGate)
			}
		}
		s.sendGateArmed = false
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/_control/send-gate" && r.Method == http.MethodGet:
		s.mu.Lock()
		state := SendGateState{MessageID: s.sendGateID, Sender: s.sendGateSender, Sends: make(map[string]int, len(s.sendCounts))}
		for k, v := range s.sendCounts {
			state.Sends[k] = v
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(state)
	case r.URL.Path == "/_control/before-send" && r.Method == http.MethodPost:
		var in struct {
			MessageID string `json:"message_id"`
			Sender    string `json:"sender"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&in); err != nil || in.MessageID == "" || in.Sender == "" {
			http.Error(w, "message and sender required", http.StatusBadRequest)
			return true
		}
		s.mu.Lock()
		var held chan struct{}
		if s.sendGateArmed {
			s.sendGateArmed = false
			s.sendGateID = in.MessageID
			s.sendGateSender = in.Sender
			held = s.sendGate
		}
		s.mu.Unlock()
		if held != nil {
			select {
			case <-held:
			case <-r.Context().Done():
				return true
			case <-s.ctx.Done():
				return true
			}
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		return false
	}
	return true
}
