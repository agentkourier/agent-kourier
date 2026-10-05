package fakeslack

import "testing"

// TestThreadExistsLockedAgreesWithThreadLocked: a post to a thread asks whether it exists without copying it, and the
// answer is the one that the copy gives, whatever the channel holds: a parent, a parent that has replies, a reply
// whose parent is another channel's or none, a message that is itself a reply, and an empty channel.
func TestThreadExistsLockedAgreesWithThreadLocked(t *testing.T) {
	msgs := []*Message{
		{TS: "1.000001"},                       // a top-level message with no replies
		{TS: "2.000001", ThreadTS: "2.000001"}, // a parent
		{TS: "2.000002", ThreadTS: "2.000001"}, // its reply
		{TS: "3.000001", ThreadTS: "9.000001"}, // a reply to a parent that is not here
		{TS: "4.000001"},                       // another top-level message
		{TS: "4.000002", ThreadTS: "4.000001"}, // a reply to it, whose parent has not been given its own ThreadTS
		{TS: "5.000001", ThreadTS: "5.000001"}, // a parent that is also...
		{TS: "5.000001", ThreadTS: "7.000000"}, // ...the ts of a reply of another thread
	}
	for _, channel := range []string{"C1", "C-empty"} {
		s := NewHandler()
		s.channels["C1"] = msgs
		for _, ts := range []string{"", "1.000001", "2.000001", "2.000002", "3.000001", "4.000001", "4.000002", "5.000001", "6.000001", "7.000000", "9.000001"} {
			s.mu.Lock()
			got, want := s.threadExistsLocked(channel, ts), s.threadLocked(channel, ts) != nil
			s.mu.Unlock()
			if got != want {
				t.Errorf("channel %s, ts %q: threadExistsLocked = %v, threadLocked finds it = %v", channel, ts, got, want)
			}
		}
		s.Close()
	}
}
