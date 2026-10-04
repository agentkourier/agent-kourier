package chat

import (
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"sync"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/config"
)

// TriggerMatch is a bot message that a botMessage trigger of a Binding accepted (spec 8.2): which trigger, what
// it extracted, and the message as it came. It is what the intake hands on. CT-3 gives each key its thread and
// CT-4 starts the turn from it; for now the Intake only reports it (IntakeConfig.OnTrigger).
type TriggerMatch struct {
	// Binding is the namespace/name of the Binding whose trigger matched.
	Binding string
	// Index is the trigger's place in the Binding's chat.triggers, which is its place after defaulting: an
	// empty list is the one mention trigger.
	Index int
	// Fields are the values the trigger's extract entries took out of the message, by entry name. It is
	// the shape of v1alpha1.PromptView.Fields. Every value is untrusted text from the message (R2).
	Fields map[string]string
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
	match, exclude []fieldRegex
	extract        []extractRegex
}

// compileTrigger compiles the regexes of a botMessage trigger, in the order of their field names, so that
// what is tried first does not depend on the order of a map.
func compileTrigger(index int, t *v1alpha1.Trigger) (botTrigger, error) {
	c := botTrigger{index: index, botID: t.From.BotID}
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
// says so, on the raw text of its field; a field the message lacks is empty. An extract that matches takes
// its group's text, empty or not.
func (t *botTrigger) evaluate(m Inbound) (map[string]string, bool) {
	if m.Bot == nil || m.Bot.BotID != t.botID {
		return nil, false
	}
	for _, f := range t.match {
		if !f.re.MatchString(m.Field(f.field)) {
			return nil, false
		}
	}
	for _, f := range t.exclude {
		if f.re.MatchString(m.Field(f.field)) {
			return nil, false
		}
	}
	fields := make(map[string]string, len(t.extract))
	for _, e := range t.extract {
		sub := e.re.FindStringSubmatch(m.Field(e.field))
		if sub == nil {
			return nil, false
		}
		fields[e.name] = sub[e.group]
	}
	return fields, true
}

// triggerTable is the botMessage triggers of one Binding, compiled, in the order of its list.
type triggerTable []botTrigger

// match returns the first trigger that matches m, in order: one that does not match (another bot, a filter, an
// exclude, an extract that misses) lets the next try.
func (tt triggerTable) match(m Inbound) (index int, fields map[string]string, ok bool) {
	for i := range tt {
		if fields, ok := tt[i].evaluate(m); ok {
			return tt[i].index, fields, true
		}
	}
	return 0, nil, false
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
