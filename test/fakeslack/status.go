package fakeslack

import (
	"net/url"
	"slices"
)

// ThreadStatus is a call of assistant.threads.setStatus that the fake took: the thread, and the status it was given. An
// empty Status is a clear.
type ThreadStatus struct {
	Channel  string `json:"channel"`
	ThreadTS string `json:"thread_ts"`
	Status   string `json:"status"`
}

// ThreadStatuses returns the calls of assistant.threads.setStatus made for channel, oldest first. It is for test
// assertions, and is a copy. The fake keeps the calls and not a thread's current status: it does not model Slack
// taking a status off when the bot posts, so a test reads what was asked, in order.
func (s *Server) ThreadStatuses(channel string) []ThreadStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []ThreadStatus{}
	for _, st := range s.statuses {
		if st.Channel == channel {
			out = append(out, st)
		}
	}
	return slices.Clone(out)
}

// setThreadStatus implements assistant.threads.setStatus
// (https://docs.slack.dev/reference/methods/assistant.threads.setStatus/), which Agent Kourier uses to show an alert thread
// as busy while an agent works in it, and to take that off. The call is answered ok and kept.
//
// UNCONFIRMED(WP-04): invalid_arguments for a call with no channel_id, no thread_ts or no status parameter at all (slack-go
// always sends status, empty to clear); that the call needs no scope here (the fake checks none); that a thread the fake
// does not hold is taken as it is; and the refusal codes of a Slack whose app cannot set a status, which the fake never says.
func (s *Server) setThreadStatus(form url.Values) (response, string) {
	channel, thread := form.Get("channel_id"), form.Get("thread_ts")
	if channel == "" || thread == "" || !form.Has("status") {
		return nil, "invalid_arguments"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statuses = append(s.statuses, ThreadStatus{Channel: channel, ThreadTS: thread, Status: form.Get("status")})
	return response{}, ""
}
