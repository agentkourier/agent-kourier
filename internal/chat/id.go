package chat

import "fmt"

// MessageID is the ID by which a message is known across restarts and redeliveries, such as the A2A
// messageId of a turn: "<platform>:<connection>:<channel>:<id>". It comes from where the message is and
// never from what it says, so the same message is always the same ID. For Slack it is the string the
// store already holds ("slack:ns/name:C1:1.2"), so nothing needs migrating.
func MessageID(r Ref) string {
	return fmt.Sprintf("%s:%s:%s:%s", r.Platform, r.Conn, r.Channel, r.ID)
}
