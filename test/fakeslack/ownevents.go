package fakeslack

// WithoutOwnPostEvents stops the fake sending Socket Mode events for the bot's
// own posts and updates. By default every bot post is delivered as a message
// event, and every update as a message_changed event, to a connected client, as
// Slack does; courier has to recognise and ignore them. Set at construction,
// read-only after.
func WithoutOwnPostEvents() Option { return func(s *Server) { s.noOwnEvents = true } }

// ownEvent is a bot post (prev nil) or change to one, waiting to be sent.
type ownEvent struct {
	channel   string
	prev      *Message // the message before a change, without metadata
	cur       Message  // the message now, without metadata
	changedTS string   // the ts of a message_changed event
}

// queueOwnLocked records that the bot posted cur in channel, or, if prev is not
// nil, changed it from prev. The events go out from flushOwn, once the request
// has released s.mu. Callers hold s.mu.
func (s *Server) queueOwnLocked(channel string, prev, cur *Message) {
	if s.noOwnEvents {
		return
	}
	e := ownEvent{channel: channel, cur: cur.clone()}
	e.cur.Metadata = nil
	if prev != nil {
		p := prev.clone()
		p.Metadata = nil
		e.prev, e.changedTS = &p, s.nextTSLocked()
	}
	s.ownPending = append(s.ownPending, e)
}

// flushOwn sends the events queued by the request that has just been handled,
// in order. With no client connected they are dropped, as Slack drops events
// for an app that is not listening.
//
// UNCONFIRMED(WP-04): a message event for each bot post with no subtype, and
// with bot_id and app_id; that blocks are included and metadata is not; a
// message_changed event, hidden, with its own ts, for each chat.update; for
// streaming, a message event at chat.startStream, no event at chat.appendStream
// and a message_changed at chat.stopStream; and that a reply gives no
// message_changed for its thread parent.
func (s *Server) flushOwn() {
	s.mu.Lock()
	events := s.ownPending
	s.ownPending = nil
	s.mu.Unlock()
	for _, e := range events {
		_, _ = s.eventCallback(s.ownEventBody(e)) // ErrNoConnection is the dropped case
	}
}

// ownEventBody builds the Events API event for e.
func (s *Server) ownEventBody(e ownEvent) map[string]any {
	chType := s.channelType(e.channel)
	if e.prev != nil {
		return map[string]any{
			"type": "message", "subtype": "message_changed", "hidden": true, "channel": e.channel, "channel_type": chType,
			"ts": e.changedTS, "event_ts": e.changedTS, "message": e.cur, "previous_message": *e.prev,
		}
	}
	ev := map[string]any{
		"type": "message", "user": e.cur.User, "bot_id": e.cur.BotID, "app_id": e.cur.AppID, "text": e.cur.Text,
		"ts": e.cur.TS, "event_ts": e.cur.TS, "channel": e.channel, "channel_type": chType,
	}
	if e.cur.ThreadTS != "" {
		ev["thread_ts"] = e.cur.ThreadTS
	}
	if e.cur.Blocks != nil {
		ev["blocks"] = e.cur.Blocks
	}
	return ev
}
