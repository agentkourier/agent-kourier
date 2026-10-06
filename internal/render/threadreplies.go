package render

// The thread context of a mentioned turn (spec 8.2, D38; design: tasks/plans/thread-reply-mentions.md). In a Binding
// with chat.threadReplies: mention, the replies that people wrote in a thread without mentioning courier are not turns;
// a mentioned turn carries the ones since the previous turn, after the person's text, as
//
//	<thread-replies-0123456789ab>
//	<reply><from>Ada (U1)</from><text>...</text></reply>
//	1 earlier reply left out.
//	</thread-replies-0123456789ab>
//
// oldest first, with a sentence before it that says the block is data and not instructions and names the tag. It is
// built by the untrusted block module (untrusted.go), which writes that sentence, the tag, the escaping and the cut, and
// drops the oldest replies that do not fit, counting them in the last line. This file says only what the block holds:
// each reply is an item with an author, a label made of the name and the ID, and a text, and each name, ID and text is
// cleaned of control and format characters, has its secrets taken out (redact.FreeText, which normalizes and bounds it) and
// is escaped: no "<" of a reply survives, so nothing in it can close the block early or open a tag that passes for courier's.

// Caps on the thread context, in UTF-16 units as chat.Truncate counts them. Anything cut ends in chat.Truncated, and what is
// left out as a whole is counted in the block's last line. They are not configurable in v1.
const (
	// MaxThreadReplies is how many replies a block holds: the newest.
	MaxThreadReplies = 50
	// MaxThreadReplyUnits caps the text of one reply, as escaped inside its tags and with the mark of a cut.
	MaxThreadReplyUnits = 1000
	// MaxThreadNameUnits caps each of the name and the ID of one reply's author, as escaped inside its tags.
	MaxThreadNameUnits = 100
	// MaxThreadBlockUnits caps the whole section: the sentence before the block, the block, its tags, its replies and its
	// last line. Replies that do not fit are left out, the oldest first.
	MaxThreadBlockUnits = 6000
)

// ThreadReply is one reply of a thread that the context shows. Every field is untrusted.
type ThreadReply struct {
	// UserID is who wrote it, by the platform's ID, and Name is what the platform calls them, or "" if it could not say.
	UserID, Name string
	// Text is what they wrote, verbatim.
	Text string
}

// threadTagPrefix starts the name of the block's tag; 12 hex digits follow.
const threadTagPrefix = "thread-replies-"

// threadItem is a reply as the module takes it: the author's name and ID, which are one line, are the label, and the
// text is the content. A reply whose text is nothing once cleaned has nothing to show, whoever wrote it.
func threadItem(r ThreadReply) untrustedItem {
	return untrustedItem{Wrapper: "reply", Fields: []untrustedField{
		{Name: "from", Value: r.Name, Qualifier: r.UserID, Kind: kindFreeText, OneLine: true, Label: true, Cap: MaxThreadNameUnits},
		{Name: "text", Value: r.Text, Kind: kindFreeText, Cap: MaxThreadReplyUnits},
	}}
}

// ThreadReplies returns the section a mentioned turn carries of the replies in its thread: the sentence and the block of
// entries, oldest first, in at most MaxThreadBlockUnits. entries are the replies that people wrote, oldest first, and
// dropped is how many older ones the caller left out already. It keeps the newest MaxThreadReplies entries that fit, and says
// how many it left out altogether, dropped included, in the block's last line. It returns "" if there is no reply to show.
func ThreadReplies(entries []ThreadReply, dropped int) string {
	section, _ := ThreadRepliesCounted(entries, dropped)
	return section
}

// ThreadRepliesCounted is ThreadReplies, and also says how many replies the section leaves out: dropped, and the entries
// cut by count or by size. A caller that counts a cut (a metric) asks it.
func ThreadRepliesCounted(entries []ThreadReply, dropped int) (section string, leftOut int) {
	items := make([]untrustedItem, len(entries))
	for i, r := range entries {
		items[i] = threadItem(r)
	}
	return untrustedBlock{
		// The clause ends in the comma that closes its aside, before the verb of the sentence the module writes.
		Opening:   "Replies that people wrote in this thread since the previous turn, without mentioning courier,",
		Plural:    true,
		Order:     "oldest first",
		TagPrefix: threadTagPrefix,
		Oldest: &oldestPolicy{
			Noun: leftOutNoun{One: "earlier reply", Many: "earlier replies"}, MaxItems: MaxThreadReplies, Dropped: dropped,
		},
		MaxUnits:      MaxThreadBlockUnits,
		OmitWhenEmpty: true,
		Items:         items,
	}.render()
}
