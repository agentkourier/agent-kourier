package chat

import (
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// TriggerMatch is a bot message that a botMessage trigger of a Binding accepted (spec 8.2): which trigger, what
// it extracted, and the message as it came. The Intake decides what it does to the thread of its key (see
// TriggerDecision), and hands it to IntakeConfig.OnStart if it starts a turn.
type TriggerMatch struct {
	// Binding is the namespace/name of the Binding whose trigger matched.
	Binding string
	// Index is the trigger's place in the Binding's chat.triggers, which is its place after defaulting: an
	// empty list is the one mention trigger.
	Index int
	// Fields are the values the trigger's extract entries took out of the message, by entry name. It is
	// the shape of v1alpha1.PromptView.Fields. Every value is untrusted text from the message (R2).
	Fields map[string]string
	// Thread is what the trigger says about threads: how a key maps to one, the cooldown, and what closes it.
	Thread v1alpha1.TriggerThread
	// Received is when the Intake took the message. The thread rules take it for now: the cooldown runs from
	// it, and it is the time a note names.
	Received time.Time
	// Message is the message, whole.
	Message Inbound
}

// fieldRegex is one match or exclude entry, or the regex of an extract, compiled.
type fieldRegex struct {
	field v1alpha1.MessageField
	re    *regexp.Regexp
}

// extractRegex is an extract entry compiled: the value is the regex's group of the entry's name.
type extractRegex struct {
	name  string
	group int
	fieldRegex
}

// botTrigger is a botMessage trigger with its regexes compiled, which load validated, so a failure to compile
// here is a trigger that never matches rather than an error to return.
type botTrigger struct {
	index          int // in the Binding's trigger list
	botID          string
	thread         v1alpha1.TriggerThread
	match, exclude []fieldRegex
	extract        []extractRegex
}

// compileTrigger compiles the regexes of a botMessage trigger, in the order of their field names, so that
// what is tried first does not depend on the order of a map.
func compileTrigger(index int, t *v1alpha1.Trigger) (botTrigger, error) {
	c := botTrigger{index: index, botID: t.From.BotID, thread: t.Thread}
	compile := func(entries map[v1alpha1.MessageField]string) ([]fieldRegex, error) {
		var out []fieldRegex
		for _, f := range slices.Sorted(maps.Keys(entries)) {
			re, err := regexp.Compile(entries[f])
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f, err)
			}
			out = append(out, fieldRegex{f, re})
		}
		return out, nil
	}
	var err error
	if c.match, err = compile(t.Match); err != nil {
		return botTrigger{}, fmt.Errorf("match %w", err)
	}
	if c.exclude, err = compile(t.Exclude); err != nil {
		return botTrigger{}, fmt.Errorf("exclude %w", err)
	}
	for _, name := range slices.Sorted(maps.Keys(t.Extract)) {
		e := t.Extract[name]
		re, err := regexp.Compile(e.Regex)
		if err != nil {
			return botTrigger{}, fmt.Errorf("extract %s: %w", name, err)
		}
		group := re.SubexpIndex(name)
		if group < 0 {
			return botTrigger{}, fmt.Errorf("extract %s: no group of that name", name)
		}
		c.extract = append(c.extract, extractRegex{name, group, fieldRegex{e.Field, re}})
	}
	return c, nil
}

// evaluate applies the trigger to m: it is for m's bot, every match regex matches, no exclude regex does, and
// every extract finds its group; it returns the extracted values. A regex is a search, not anchored unless it
// says so, on the raw text of its field; a field the message lacks is empty. An extract's value is its group's
// text, which may be empty if the group took part in the match; a group that took no part (an optional one) is
// a miss, like a regex that does not match, so that no message gets an empty key by default.
func (t *botTrigger) evaluate(m Inbound) (map[string]string, bool) {
	fields, why := t.check(m)
	return fields, why.kind == missNone
}

// missKind is the step of a trigger at which a message was turned away.
type missKind int

const (
	missNone    missKind = iota // the trigger took the message
	missBot                     // the message is not from the trigger's bot
	missMatch                   // a match regex does not match
	missExclude                 // an exclude regex matches
	missExtract                 // an extract finds no group
)

// miss is why a trigger did not take a message: the step, and the field (match, exclude) or the entry's name
// (extract) at which it failed.
type miss struct {
	kind   missKind
	detail string
}

// check is evaluate with the reason for a miss, which the dry run shows (Replay). The steps run here and
// nowhere else, so that what the dry run reports is what the intake does.
func (t *botTrigger) check(m Inbound) (map[string]string, miss) {
	if m.Bot == nil || m.Bot.BotID != t.botID {
		return nil, miss{kind: missBot}
	}
	for _, f := range t.match {
		if !f.re.MatchString(m.Field(f.field)) {
			return nil, miss{missMatch, string(f.field)}
		}
	}
	for _, f := range t.exclude {
		if f.re.MatchString(m.Field(f.field)) {
			return nil, miss{missExclude, string(f.field)}
		}
	}
	fields := make(map[string]string, len(t.extract))
	for _, e := range t.extract {
		text := m.Field(e.field)
		loc := e.re.FindStringSubmatchIndex(text)
		// No match, or a match in which the entry's group took no part (an optional group): both are a miss.
		// A group that took part and matched empty text is a match, with an empty value.
		if loc == nil || loc[2*e.group] < 0 {
			return nil, miss{missExtract, e.name}
		}
		fields[e.name] = text[loc[2*e.group]:loc[2*e.group+1]]
	}
	return fields, miss{}
}

// triggerTable is the botMessage triggers of one Binding, compiled, in the order of its list.
type triggerTable []botTrigger

// match returns the first trigger that matches m, in order, and what it extracted: one that does not match
// (another bot, a filter, an exclude, an extract that misses) lets the next try.
func (tt triggerTable) match(m Inbound) (t *botTrigger, fields map[string]string, ok bool) {
	for i := range tt {
		if fields, ok := tt[i].evaluate(m); ok {
			return &tt[i], fields, true
		}
	}
	return nil, nil, false
}

// tableCache holds the trigger tables of one routing table, compiled the first time a Binding needs its own.
// When the Snapshot changes, which a reload does, it starts again, so a Binding's table is never older than
// the Snapshot that was asked about.
type tableCache struct {
	mu     sync.Mutex
	snap   *config.Snapshot
	tables map[*v1alpha1.Binding]triggerTable
}

// table returns the compiled botMessage triggers of b, which snap holds. A trigger that does not compile,
// which load should have rejected, is left out and logged at WARN, once per routing table.
func (c *tableCache) table(snap *config.Snapshot, b *v1alpha1.Binding, log *slog.Logger) triggerTable {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.snap != snap {
		c.snap, c.tables = snap, map[*v1alpha1.Binding]triggerTable{}
	}
	if t, ok := c.tables[b]; ok {
		return t
	}
	var t triggerTable
	for i := range b.Spec.Chat.Triggers {
		tr := &b.Spec.Chat.Triggers[i]
		if tr.Type != v1alpha1.TriggerBotMessage || tr.From == nil {
			continue
		}
		bt, err := compileTrigger(i, tr)
		if err != nil {
			log.Warn("chat: a botMessage trigger does not compile and never matches", "binding", v1alpha1.NamespacedName(b.Namespace, b.Name), "trigger", i, "err", err)
			continue
		}
		t = append(t, bt)
	}
	c.tables[b] = t
	return t
}
