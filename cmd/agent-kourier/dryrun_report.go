package main

import (
	"cmp"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	"github.com/agentkourier/agent-kourier/internal/chat"
)

// The widths of the text report, in characters.
const (
	titleWidth = 80 // a title is cut to this
	keyWidth   = 40 // a longer key is shortened
	keyKept    = 30 // to this many characters, an ellipsis and a short hash
	flapCycles = 3  // a key that fired and resolved this many times is flapping
	hashDigits = 6  // of the hash that tells two shortened keys apart
	timeLayout = time.RFC3339
	noValue    = "-" // what a column shows for a value the message has none of
)

// dryRunReport is what `agent-kourier dry-run` prints: every message of the history, oldest first, and the summary.
type dryRunReport struct {
	Binding  string        `json:"binding"`
	Channel  string        `json:"channel"`
	Messages []dryRunRow   `json:"messages"`
	Summary  dryRunSummary `json:"summary"`
}

// dryRunRow is one message and what the Binding's triggers would make of it. The text of a message is in the
// operator's own channel and read locally, so the title is printed; the bot token never is.
type dryRunRow struct {
	// TS is the message's ts, and Time the moment it stands for.
	TS   string `json:"ts"`
	Time string `json:"time,omitempty"`
	// Sender is the bot, the person or Agent Kourier that posted it.
	Sender dryRunSender `json:"sender"`
	// Outcome is a chat.ReplayOutcome.
	Outcome string `json:"outcome"`
	// Trigger is the index in the Binding's triggers of the one that matched; set only for a match.
	Trigger *int `json:"trigger,omitempty"`
	// Fields are what the matching trigger extracted: the state, the alertname and the key, for the Alertmanager trigger.
	Fields map[string]string `json:"fields,omitempty"`
	// Title is the message's title, whole: the attachment's title, else its fallback, else the text.
	Title string `json:"title,omitempty"`
	// Why says where each trigger of the bot turned an unmatched or excluded message away.
	Why string `json:"why,omitempty"`
}

// dryRunSender says who posted a message: Kind is bot, person or Agent Kourier, and ID the bot's or the person's ID.
type dryRunSender struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// dryRunSummary counts the report.
type dryRunSummary struct {
	Messages int `json:"messages"`
	// Oldest and Newest are the times of the first and last message.
	Oldest string `json:"oldest,omitempty"`
	Newest string `json:"newest,omitempty"`
	// Outcomes counts the messages by outcome, and Triggers the matches by the index of the trigger.
	Outcomes map[string]int `json:"outcomes"`
	Triggers map[string]int `json:"triggers"`
	// Keys are the distinct keys of the matches with the number of messages of each, most first.
	Keys []dryRunKey `json:"keys"`
	// Flapping lists the keys that fired and closed at least flapCycles times, most cycles first.
	Flapping []dryRunFlap `json:"flapping,omitempty"`
}

// dryRunKey is a thread key and the number of messages that carried it.
type dryRunKey struct {
	Key   string `json:"key"`
	Count int    `json:"count"`
}

// dryRunFlap is a key that opened and closed again and again. A cycle is a closing state after an opening one.
type dryRunFlap struct {
	Key    string `json:"key"`
	Cycles int    `json:"cycles"`
	// States counts the messages of the key by their state.
	States map[string]int `json:"states"`
}

// buildReport classifies the history by replay and totals it. closing maps the index of a trigger to the state that
// closes its thread.
func buildReport(binding, channel string, history []chat.HistoryMessage, replay *chat.Replay, closing map[int]string) dryRunReport {
	r := dryRunReport{Binding: binding, Channel: channel, Messages: make([]dryRunRow, 0, len(history))}
	type tally struct {
		cycles int
		open   bool
		states map[string]int
	}
	keys := map[string]int{}
	tallies := map[string]*tally{}
	s := dryRunSummary{Messages: len(history), Outcomes: map[string]int{}, Triggers: map[string]int{}}
	for _, h := range history {
		v := replay.Classify(h)
		row := dryRunRow{TS: h.Message.At.ID, Time: timeOf(h.Message.At.ID), Sender: senderOf(h), Outcome: string(v.Outcome), Title: titleOf(h.Message), Why: v.Why}
		s.Outcomes[row.Outcome]++
		if v.Outcome == chat.ReplayMatched {
			row.Trigger, row.Fields = &v.Trigger, v.Fields
			s.Triggers[strconv.Itoa(v.Trigger)]++
			if key, ok := v.Fields["key"]; ok {
				keys[key]++
				t := tallies[key]
				if t == nil {
					t = &tally{states: map[string]int{}}
					tallies[key] = t
				}
				state := v.Fields["state"]
				t.states[state]++
				switch {
				case state == "":
				case state != closing[v.Trigger]:
					t.open = true
				case t.open:
					t.cycles++
					t.open = false
				}
			}
		}
		r.Messages = append(r.Messages, row)
	}
	if n := len(r.Messages); n > 0 {
		s.Oldest, s.Newest = r.Messages[0].Time, r.Messages[n-1].Time
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		s.Keys = append(s.Keys, dryRunKey{k, keys[k]})
		if t := tallies[k]; t.cycles >= flapCycles {
			s.Flapping = append(s.Flapping, dryRunFlap{Key: k, Cycles: t.cycles, States: t.states})
		}
	}
	slices.SortStableFunc(s.Keys, func(a, b dryRunKey) int { return cmp.Compare(b.Count, a.Count) })
	slices.SortStableFunc(s.Flapping, func(a, b dryRunFlap) int { return cmp.Compare(b.Cycles, a.Cycles) })
	r.Summary = s
	return r
}

// senderOf says who posted the message of h: Agent Kourier, a bot by its ID, or a person by their ID. A message the adapter
// drops, which was not decoded, is by no one known.
func senderOf(h chat.HistoryMessage) dryRunSender {
	m := h.Message
	switch {
	case h.Own:
		return dryRunSender{Kind: "agent-kourier"}
	case h.Ignored:
		return dryRunSender{Kind: "unknown"}
	case m.Bot != nil:
		return dryRunSender{Kind: "bot", ID: m.Bot.BotID}
	}
	return dryRunSender{Kind: "person", ID: m.User}
}

// titleOf is the message's title: the attachment's title, else its fallback or text, else the message's own text.
func titleOf(m chat.Inbound) string {
	if m.Bot != nil {
		return cmp.Or(m.Bot.Title, m.Bot.Fallback, m.Bot.AttachmentText, m.Text)
	}
	return m.Text
}

// timeOf is the time of a Slack ts ("seconds.microseconds"), in UTC, or "" if it is none.
func timeOf(ts string) string {
	sec, frac, _ := strings.Cut(ts, ".")
	s, err := strconv.ParseInt(sec, 10, 64)
	if err != nil {
		return ""
	}
	micros, _ := strconv.ParseInt((frac + "000000")[:6], 10, 64)
	return time.Unix(s, micros*1000).UTC().Format(timeLayout)
}

// writeJSON prints the report as one JSON document, with every title and key whole.
func (r dryRunReport) writeJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// writeText prints the report for a person: a line per message, and then the summary. The text of a message is
// untrusted, so it is cleaned of control characters and cut: it can neither move the cursor nor run over a line.
func (r dryRunReport) writeText(w io.Writer) error {
	_, _ = fmt.Fprintf(w, "dry run of %s on channel %s, %d messages, oldest first\n\n", r.Binding, r.Channel, r.Summary.Messages)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "TS\tSENDER\tOUTCOME\tTRIGGER\tSTATE\tALERTNAME\tKEY\tTITLE")
	for _, m := range r.Messages {
		trigger := noValue
		if m.Trigger != nil {
			trigger = strconv.Itoa(*m.Trigger)
		}
		why := ""
		if m.Why != "" {
			why = "\t" + cleanText(m.Why)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s%s\n", cleanText(m.TS), senderText(m.Sender), m.Outcome, trigger,
			orDash(cleanText(m.Fields["state"])), orDash(cleanText(m.Fields["alertname"])), orDash(shortKey(m.Fields["key"])), cut(cleanText(m.Title), titleWidth), why)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	return r.Summary.writeText(w)
}

// senderText is the sender as the text report shows it.
func senderText(s dryRunSender) string {
	switch s.Kind {
	case "person":
		return "person:" + cleanText(s.ID)
	case "bot":
		return cleanText(s.ID)
	}
	return s.Kind
}

// outcomeOrder is the order the summary lists the outcomes in.
var outcomeOrder = []chat.ReplayOutcome{chat.ReplayMatched, chat.ReplayUnmatched, chat.ReplayExcluded, chat.ReplayOtherBot, chat.ReplayOwn, chat.ReplayPerson, chat.ReplayIgnored}

// writeText prints the summary: the counts per outcome and per trigger, the distinct keys, and the keys that flap.
func (s dryRunSummary) writeText(w io.Writer) error {
	span := ""
	if s.Oldest != "" {
		span = fmt.Sprintf(", %s to %s", s.Oldest, s.Newest)
	}
	_, _ = fmt.Fprintf(w, "\nsummary: %d messages%s\n\noutcomes\n", s.Messages, span)
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	for _, o := range outcomeOrder {
		if n := s.Outcomes[string(o)]; n > 0 {
			_, _ = fmt.Fprintf(tw, "  %s\t%d\n", o, n)
		}
	}
	_, _ = fmt.Fprintln(tw, "\nmatches per trigger")
	for _, t := range slices.SortedFunc(maps.Keys(s.Triggers), func(a, b string) int { return cmp.Or(cmp.Compare(len(a), len(b)), strings.Compare(a, b)) }) {
		_, _ = fmt.Fprintf(tw, "  trigger %s\t%d\n", t, s.Triggers[t])
	}
	if len(s.Triggers) == 0 {
		_, _ = fmt.Fprintln(tw, "  none")
	}
	_, _ = fmt.Fprintf(tw, "\nkeys: %d distinct (messages, key)\n", len(s.Keys))
	for _, k := range s.Keys {
		_, _ = fmt.Fprintf(tw, "  %d\t%s\n", k.Count, shortKey(k.Key))
	}
	_, _ = fmt.Fprintf(tw, "\nflapping: keys that fired and closed at least %d times\n", flapCycles)
	for _, f := range s.Flapping {
		_, _ = fmt.Fprintf(tw, "  %d cycles\t%s\t(%s)\n", f.Cycles, shortKey(f.Key), statesText(f.States))
	}
	if len(s.Flapping) == 0 {
		_, _ = fmt.Fprintln(tw, "  none")
	}
	return tw.Flush()
}

// statesText is the counts of a key's messages by state, such as "27 RESOLVED, 68 FIRING", most first.
func statesText(states map[string]int) string {
	var parts []string
	for _, state := range slices.SortedFunc(maps.Keys(states), func(a, b string) int { return cmp.Or(cmp.Compare(states[b], states[a]), strings.Compare(a, b)) }) {
		parts = append(parts, fmt.Sprintf("%d %s", states[state], cleanText(state)))
	}
	return strings.Join(parts, ", ")
}

// cleanText is s on one line, with every control and format character (a terminal escape, a bidirectional
// override) replaced by a space.
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, s)
}

// cut is s cut to at most n characters, the last of them an ellipsis if it was cut.
func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// shortKey is a key as the text report shows it: whole if it is short, else its first characters and a short hash of
// all of it, so that two long keys are told apart.
func shortKey(key string) string {
	key = cleanText(key)
	if len([]rune(key)) <= keyWidth {
		return key
	}
	sum := sha256.Sum256([]byte(key))
	return string([]rune(key)[:keyKept]) + "…#" + hex.EncodeToString(sum[:])[:hashDigits]
}

// orDash is s, or a dash if it is empty.
func orDash(s string) string { return cmp.Or(s, noValue) }
