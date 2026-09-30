package fakeslack

import (
	"encoding/base64"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Pagination limits: https://docs.slack.dev/apis/web-api/pagination/. The
// default of 100 and the cap of 999 are from the method docs.
const (
	defaultLimit = 100
	maxLimit     = 999
)

// history implements conversations.history, newest message first, with thread
// replies folded into their parents
// (https://docs.slack.dev/reference/methods/conversations.history/).
//
// Error codes from the docs: channel_not_found, invalid_cursor,
// invalid_ts_latest, invalid_ts_oldest.
//
// UNCONFIRMED(WP-04): invalid_limit. With only oldest set, results are still
// newest first and a cursor walks back toward oldest. Real Slack's direction is
// unconfirmed, so spec 8.6's reconciliation must page to the end of the window
// and not rely on it. Returning metadata only with
// include_all_metadata=1 is a deliberately strict choice: the message metadata
// page (https://docs.slack.dev/messaging/message-metadata/) says apps see only
// the event types they registered for otherwise, and the fake has no
// registration, so it hides metadata unless asked. Courier must always ask.
func (s *Server) history(form url.Values) (response, string) {
	channel := form.Get("channel")
	if channel == "" {
		return nil, "channel_not_found"
	}
	keep, code := tsFilter(form)
	if code != "" {
		return nil, code
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var msgs []Message
	for _, m := range slices.Backward(s.channels[channel]) {
		if (m.ThreadTS != "" && m.ThreadTS != m.TS) || !keep(m.TS) {
			continue // a reply, or outside the window
		}
		v := m.clone()
		for _, r := range s.channels[channel] {
			if r.ThreadTS == m.TS && r.TS != m.TS {
				v.ReplyCount++
				v.LatestReply = r.TS
			}
		}
		msgs = append(msgs, v)
	}
	return messagePage(msgs, form)
}

// replies implements conversations.replies: the parent first, then its replies
// oldest first. A reply's ts returns only that reply
// (https://docs.slack.dev/reference/methods/conversations.replies/).
//
// Error codes from the docs: channel_not_found, thread_not_found,
// invalid_cursor.
//
// UNCONFIRMED(WP-04): the invalid_ts_oldest and invalid_ts_latest codes here
// (they are documented for history); the parent is returned on the first page
// only, though real Slack may repeat it on every page; the default limit of 100
// and cap of 999 are history's, reused here; oldest and latest apply to the
// replies but not to the parent.
func (s *Server) replies(form url.Values) (response, string) {
	channel, ts := form.Get("channel"), form.Get("ts")
	if channel == "" {
		return nil, "channel_not_found"
	}
	keep, code := tsFilter(form)
	if code != "" {
		return nil, code
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	thread := s.threadLocked(channel, ts)
	for _, m := range s.channels[channel] {
		if m.TS == ts && thread == nil {
			thread = []Message{m.clone()} // a reply's ts
		}
	}
	if thread == nil {
		return nil, "thread_not_found"
	}
	msgs := []Message{thread[0]}
	for _, m := range thread[1:] {
		if keep(m.TS) {
			msgs = append(msgs, m)
		}
	}
	return messagePage(msgs, form)
}

// tsFilter builds the oldest, latest, and inclusive filter shared by history
// and replies. Slack's defaults are oldest 0 and latest now, both exclusive.
func tsFilter(form url.Values) (keep func(ts string) bool, code string) {
	oldest, latest := int64(0), int64(1<<62)
	var ok bool
	if v := form.Get("oldest"); v != "" {
		if oldest, ok = parseTS(v); !ok {
			return nil, "invalid_ts_oldest"
		}
	}
	if v := form.Get("latest"); v != "" {
		if latest, ok = parseTS(v); !ok {
			return nil, "invalid_ts_latest"
		}
	}
	inclusive := form.Get("inclusive") == "1" || form.Get("inclusive") == "true"
	return func(ts string) bool {
		t, _ := parseTS(ts)
		if inclusive {
			return t >= oldest && t <= latest
		}
		return t > oldest && t < latest
	}, ""
}

// parseTS converts "seconds.fraction" to microseconds.
func parseTS(ts string) (micros int64, ok bool) {
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || s < 0 || len(frac) > 6 {
		return 0, false
	}
	f := int64(0)
	if frac != "" {
		if f, err = strconv.ParseInt((frac + "000000")[:6], 10, 64); err != nil {
			return 0, false
		}
	}
	return s*1_000_000 + f, true
}

// messagePage paginates msgs, already in response order, and builds the
// response. Metadata is dropped unless include_all_metadata is set.
func messagePage(msgs []Message, form url.Values) (response, string) {
	out, next, code := paginate(msgs, form, func(m Message) string { return m.TS })
	if code != "" {
		return nil, code
	}
	if form.Get("include_all_metadata") != "1" {
		for i := range out {
			out[i].Metadata = nil
		}
	}
	resp := response{"messages": out, "has_more": next != "", "pin_count": 0}
	if next != "" {
		resp["response_metadata"] = map[string]string{"next_cursor": next}
	}
	return resp, ""
}

// paginate applies limit and cursor to items, already in response order. A
// cursor is the base64 of the key of the last item returned, and the next page
// starts after that item; next is empty on the last page. The result is a copy.
func paginate[T any](items []T, form url.Values, key func(T) string) (page []T, next, code string) {
	limit := defaultLimit
	if v := form.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return nil, "", "invalid_limit"
		}
		limit = min(n, maxLimit)
	}
	start := 0
	if c := form.Get("cursor"); c != "" {
		raw, err := base64.RawURLEncoding.DecodeString(c)
		start = slices.IndexFunc(items, func(it T) bool { return key(it) == string(raw) }) + 1
		if err != nil || start == 0 {
			return nil, "", "invalid_cursor"
		}
	}
	end := min(start+limit, len(items))
	page = slices.Clone(items[start:end])
	if end < len(items) {
		next = base64.RawURLEncoding.EncodeToString([]byte(key(page[len(page)-1])))
	}
	return page, next, ""
}
