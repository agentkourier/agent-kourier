package crdcorpus

import (
	"fmt"
	"math/rand/v2"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
)

// The corpus holds the objects someone thought of. The variations are the ones a generator makes: a valid template with
// a few of its fields replaced by values from pools that sit on the edges of the broker's rules (a duration at every
// boundary of a range, a header name in odd cases, a regex of 512 bytes in fewer characters, a field written out empty
// or left out). The cluster test holds the schema to the invariant on them with the same direction as the corpus: of
// what the broker accepts, the schema refuses nothing. A CEL rule that is stricter than the rule it copies fails on a
// variation that the corpus's authors did not think of, and the same seed makes the failure repeat.

// VariationSeed and VariationCount are what both tests of the variations use, so that a failure reproduces.
const (
	VariationSeed  = 20261104
	VariationCount = 6000
)

// absent, as a value in a pool, removes the field.
type absent struct{}

// mutation replaces the field at path (dot separated, a number is an index into a list) with one value of the pool.
type mutation struct {
	path string
	pool func(*rand.Rand) any
}

// of returns a pool that picks one of the values.
func of(values ...any) func(*rand.Rand) any {
	return func(r *rand.Rand) any { return values[r.IntN(len(values))] }
}

// or returns a pool that takes a value of the first with the chance of a in b and otherwise of the second.
func or(a func(*rand.Rand) any, aIn, b int, other func(*rand.Rand) any) func(*rand.Rand) any {
	return func(r *rand.Rand) any {
		if r.IntN(b) < aIn {
			return a(r)
		}
		return other(r)
	}
}

// template is a valid object, and the fields that a variation may change.
type template struct {
	kind, doc string
	mutations []mutation
}

// Variations returns n generated entries, the same ones for the same seed. An entry may be valid or not: the test
// asks the broker.
func Variations(seed uint64, n int) ([]Entry, error) {
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15)) //nolint:gosec // a seeded generator is the point: a failure must repeat
	templates := variationTemplates()
	entries := make([]Entry, 0, n)
	for i := range n {
		t := templates[r.IntN(len(templates))]
		doc, err := decode([]byte(t.doc))
		if err != nil {
			return nil, fmt.Errorf("crdcorpus: variation template: %w", err)
		}
		var did []string
		for range 1 + r.IntN(3) {
			m := t.mutations[r.IntN(len(t.mutations))]
			v := m.pool(r)
			if _, rm := v.(absent); !rm {
				v = runtime.DeepCopyJSONValue(v) // a pool hands out the same value again, and a later mutation may go inside it
			}
			set(doc, m.path, v)
			did = append(did, fmt.Sprintf("%s=%s", m.path, show(v)))
		}
		e := newEntry(fmt.Sprintf("variation %d: %s", i, strings.Join(did, "; ")), runtime.DeepCopyJSON(doc))
		entries = append(entries, e)
	}
	return entries, nil
}

// show prints a value for an entry's name, short enough to read.
func show(v any) string {
	if _, ok := v.(absent); ok {
		return "<absent>"
	}
	s := fmt.Sprintf("%v", v)
	if len(s) > 60 {
		s = s[:60] + fmt.Sprintf("...(%d bytes)", len(s))
	}
	return strconv.Quote(s)
}

// set puts v at the path of obj, making the maps on the way, or removes the field for absent. A number in the path is
// an index into a list that the template has.
func set(obj map[string]any, path string, v any) {
	segs := strings.Split(path, ".")
	var cur any = obj
	for i, seg := range segs {
		last := i == len(segs)-1
		switch c := cur.(type) {
		case map[string]any:
			if last {
				if _, rm := v.(absent); rm {
					delete(c, seg)
				} else {
					c[seg] = v
				}
				return
			}
			next, ok := c[seg]
			if !ok {
				next = map[string]any{}
				c[seg] = next
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx >= len(c) {
				return // an earlier mutation shortened the list: this one has nothing to change
			}
			if last {
				c[idx] = v
				return
			}
			cur = c[idx]
		default:
			return // an earlier mutation replaced the object on the way with a value that has no fields
		}
	}
}

// durations is a pool of the spellings of a duration: every boundary of the ranges the broker holds, signed zeros, the
// forms ParseDuration takes and the ones the schema's pattern lets through that it refuses, and a few that are
// random sequences of the grammar's pieces.
func durations() func(*rand.Rand) any {
	curated := []any{
		"1m", "59s", "60s", "61s", "168h", "168h1ns", "167h59m59s", "10080m", "10s", "9s", "9.999s", "24h", "24h1s", "23h59m59s",
		"1440m", "720h", "720h1s", "2m", "119s", "121s", "0", "0s", "-0", "+0", "-1s", "-1m", "-1h", "1h", "30m", "2160h", "1.5h",
		".5h", "5.h", "1h30m", "100ms", "1µs", "1μs", "1ns", "-0.0000000001ns", "0.0000000001ns", "1e3s", "1d", "", "banana",
		"1 h", "s", ".s", "99999999999h", "9223372036854775807ns", "2562047h47m16.854775807s", "2562047h47m16.854775808s",
		"-2562047h47m16.854775808s", "00000000000001h", "+30m", "-30m", "1h1h", "0.5h", "4h", "8h", "336h",
	}
	pieces := []string{"0", "1", "2", "5", "10", "30", ".", "-", "+", "s", "m", "h", "ms", "us", "µs", "μs", "ns", "d", "x"}
	return func(r *rand.Rand) any {
		if r.IntN(10) < 7 {
			return curated[r.IntN(len(curated))]
		}
		var b strings.Builder
		for range 1 + r.IntN(6) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		return b.String()
	}
}

// urls is a pool of URLs: the ones that sit on the edge of what url.Parse accepts, and random sequences of the pieces
// that make it do otherwise.
func urls() func(*rand.Rand) any {
	curated := []any{
		"http://kagent.kagent:8083/", "https://kagent", "HTTP://KAGENT/", "hTtPs://x", "http://kagent#f", "http://kagent#", "http://kagent?q",
		"http://kagent?q@x", "http://kagent/p@x", "http://kagent#a@b", "http://u@kagent", "http://u:p@kagent", "http://@kagent", "http://:80",
		"http://:", "http://", "http:///x", "http:/x", "http:x", "http:", "ftp://kagent", "//kagent", "kagent", "kagent:8083", "/x", "",
		"http://[::1]:8443/", "http://[::1", "http://kagent/a b", "http://ka gent", "http://kagent\n", "http://kagent\t/x", " http://kagent",
		"http://kagent ", "http://ka^gent", "http://ka{gent", "http://ka|gent", "http://ka}gent", "http://ka\\gent", "http://ka`gent",
		"http://ka\"gent", "http://ka<gent>", "http://ka'gent", "http://ka%41gent", "http://ka%zzgent", "http://ka%gent", "http://bücher.de/",
		"http://kagent:80:80", "http://kagent:port", "https://a.b.c.d.example.com:8443/a/b/c?d=e#f", "http://kagent/\x7f", "http://\x7fkagent",
	}
	pieces := []string{"http", "HTTPS", "ftp", "://", ":", "/", "@", "u", "h", ".", "80", "[::1]", "[", "]", " ", "#", "?", "%", "%41", "\\", "\t", "\n", "\r", "\x7f", "é", "{", "|", "^", "`", "#", "#"}
	return func(r *rand.Rand) any {
		if r.IntN(10) < 6 {
			return curated[r.IntN(len(curated))]
		}
		// Most start with a scheme and an authority, or nearly all of them would be refused for having none.
		var b strings.Builder
		if r.IntN(10) < 8 {
			b.WriteString([]string{"http://", "https://", "HTTP://", "hTtPs://"}[r.IntN(4)])
			b.WriteString([]string{"kagent", "h", "kagent.kagent:8083", "[::1]", ":80", "a.b"}[r.IntN(6)])
		}
		for range 1 + r.IntN(5) {
			b.WriteString(pieces[r.IntN(len(pieces))])
		}
		return b.String()
	}
}

// headerNames is a pool of header names: each reserved one in random case, with a space, a suffix or a prefix, and names
// of multibyte characters that are near 256 bytes.
func headerNames() func(*rand.Rand) any {
	reserved := []string{"Authorization", "Proxy-Authorization", "Cookie", "X-User-Id", "X-Agent-Name", "X-Share-Token", "X-Kagent-Insecure-Runtime-Identity"}
	other := []any{"X-Name", "Authorization ", " Authorization", "Authorization-X", "X-Authorization", "Cookies", "x-user-id2", "Аuthorization", "Authorizatıon", "X_User_Id", "é", ""}
	return func(r *rand.Rand) any {
		switch r.IntN(6) {
		case 0:
			return other[r.IntN(len(other))]
		case 1: // a name of multibyte characters, around the 256 byte limit
			return strings.Repeat("a", r.IntN(3)*2) + strings.Repeat("é", 124+r.IntN(8))
		case 2: // a name of ASCII around the limit
			return strings.Repeat("X", 253+r.IntN(6))
		default:
			var b strings.Builder
			for _, c := range reserved[r.IntN(len(reserved))] {
				if r.IntN(2) == 0 {
					b.WriteString(strings.ToUpper(string(c)))
				} else {
					b.WriteString(strings.ToLower(string(c)))
				}
			}
			if r.IntN(4) == 0 {
				return b.String() + []string{" ", "x", "-2", "\t"}[r.IntN(4)]
			}
			return b.String()
		}
	}
}

// regexes is a pool of regular expressions for a match or exclude value: empty, at and over 512 bytes and characters,
// invalid, and ordinary.
func regexes() func(*rand.Rand) any {
	return of("^FIRING", "x", "", "(", strings.Repeat("a", 511), strings.Repeat("a", 512), strings.Repeat("a", 513), strings.Repeat("é", 255),
		strings.Repeat("é", 256), strings.Repeat("é", 257), "(?P<x>y)", "[", "\\", strings.Repeat("é", 300), ".*")
}

// extractRegexes is a pool of regexes for an extract entry named by name: with and without the named group in each
// syntax, with the group only in a character class or behind an escape, and at the byte limit.
func extractRegexes(name string) func(*rand.Rand) any {
	return of(
		"(?P<"+name+">.+)", "(?<"+name+">.+)", "(?P<other>.+)", ".+", "(?P<"+name+">", "(?P<"+name+"x>.+)", "x(?P<"+name+">y)z", "(?:(?P<"+name+">x))|y",
		"[(?P<"+name+">]", "\\(?P<"+name+">x)", "(?i)(?P<"+name+">x)", "(?P<"+name+">"+strings.Repeat("a", 490)+")", "(?P<"+name+">a"+strings.Repeat("é", 240)+")",
		"(?P<"+name+">a"+strings.Repeat("é", 251)+")", "(?P<"+name+">a"+strings.Repeat("é", 252)+")", strings.Repeat("(?P<"+name+">x)", 60),
	)
}

func variationTemplates() []template {
	dur := durations()
	messageFields := of("text", "title", "link", "fallback", "footer", "body", "subject", "Text", "")
	extractNames := of("key", "state", "a", "aB_9", "State", "1a", "a-b", "a b", "_a", "kéy")
	return []template{
		{kind: "AgentBackend", doc: backendDoc, mutations: []mutation{
			{"spec.url", urls()},
			{"spec.url", urls()},
			{"spec.headers.0.name", headerNames()},
			{"spec.headers.0.name", headerNames()},
			{"spec.headers.0.value", of("v", "", absent{}, nil)},
			{"spec.headers.0.valueSecretRef", of(nil, nil)},
			{"spec.headers.0.valueSecretRef", of(map[string]any{"name": "s"}, map[string]any{"name": "s", "key": "k"}, absent{}, map[string]any{"name": "s", "key": ""})},
			{"spec.headers.1.name", headerNames()},
			{"spec.headers.1.value", of("v", "", absent{})},
			{"spec.headers.1.valueSecretRef", of(map[string]any{"name": "s"}, absent{})},
			{"spec.headers", of(absent{}, []any{}, []any{map[string]any{"name": "X-A", "value": "v"}})},
		}},
		{kind: "Binding", doc: bindingDoc, mutations: bindingMutations(dur, messageFields, extractNames)},
		{kind: "Binding", doc: bindingDoc, mutations: bindingMutations(dur, messageFields, extractNames)},
		{kind: "Binding", doc: mentionBindingDoc, mutations: []mutation{
			{"spec.chat.triggers.0.from", of(absent{}, map[string]any{"id": "B0123456789"}, map[string]any{})},
			{"spec.chat.triggers.0.match", of(absent{}, map[string]any{}, map[string]any{"title": "x"}, map[string]any{"title": ""})},
			{"spec.chat.triggers.0.exclude", of(absent{}, map[string]any{}, map[string]any{"title": "x"})},
			{"spec.chat.triggers.0.extract", of(absent{}, map[string]any{}, map[string]any{"key": map[string]any{"field": "title", "regex": "(?P<key>x)"}})},
			{"spec.chat.triggers.0.thread", of(absent{}, map[string]any{}, map[string]any{"by": ""}, map[string]any{"by": "key"}, map[string]any{"cooldown": "0s"}, map[string]any{"cooldown": "-0"},
				map[string]any{"cooldown": "2h"}, map[string]any{"maxIdle": "0"}, map[string]any{"maxIdle": "5h"}, map[string]any{"closeWhen": map[string]any{}}, map[string]any{"closeWhen": map[string]any{"a": "b"}})},
			{"spec.chat.triggers.0.thread.cooldown", or(dur, 3, 4, of(absent{}))},
			{"spec.chat.triggers.0.promptTemplate", of(absent{}, "", "x")},
			{"spec.chat.triggers.0.limits", of(absent{}, map[string]any{}, map[string]any{"maxRunsPerDay": int64(0)}, map[string]any{"rateLimit": map[string]any{"maxRuns": int64(1), "per": "10s", "maxConcurrent": int64(1)}})},
			{"spec.chat.triggers.0.type", of("mention", "mention", "botMessage", "")},
			{"spec.chat.mentions", of(absent{}, []any{map[string]any{"name": "here", "user": "U0123456789"}}, []any{map[string]any{"name": "channel", "user": "U0123456789"}},
				[]any{map[string]any{"name": "everyone", "user": "U0123456789"}}, []any{map[string]any{"name": "everyone1", "user": "U0123456789"}}, []any{map[string]any{"name": "ok", "user": "U0123456789"}})},
		}},
	}
}

func bindingMutations(dur, messageFields, extractNames func(*rand.Rand) any) []mutation {
	const t = "spec.chat.triggers.1."
	return []mutation{
		// Objects written out empty and values written as null, which the apiserver reads as unset (it prunes a null, or
		// defaults it) and the broker's decoder reads as unset.
		{"spec.interactions", of(map[string]any{}, nil, map[string]any{"timeout": nil, "toolApprovals": nil, "approverGroups": nil})},
		{"spec.session", of(map[string]any{}, nil, map[string]any{"threadTTL": nil})},
		{"spec.feedback", of(map[string]any{}, nil)},
		{"spec.chat.acknowledgment", of(map[string]any{}, nil, map[string]any{"enabled": nil, "emoji": nil})},
		{"spec.chat.triggers.1.limits", of(nil, map[string]any{"rateLimit": nil, "maxRunsPerDay": nil})},
		{"spec.chat.triggers.1.thread", of(nil, map[string]any{"by": nil, "cooldown": nil, "maxIdle": nil, "closeWhen": nil})},
		{"spec.chat.triggers.1.match", of(nil, nil)},
		{"spec.chat.triggers.1.extract", of(nil, nil)},
		{"spec.chat.triggers.1.from.kind", of(nil, nil)},
		{"spec.chat.triggers.0", of(map[string]any{"type": "mention", "from": nil, "match": nil, "thread": nil, "limits": nil, "promptTemplate": nil})},
		{"spec.identity.tokenSecretRef", of(nil, map[string]any{"name": "s", "key": nil})},
		{"spec.identity.userId", of("u", "", absent{}, nil)},
		{"spec.identity.tokenSecretRef", of(absent{}, map[string]any{"name": "s"}, map[string]any{"name": "s", "key": ""})},
		{"spec.identity", of(map[string]any{}, map[string]any{"userId": ""}, map[string]any{"userId": "u"})},
		{"spec.interactions.toolApprovals", of(true, false, absent{})},
		{"spec.interactions.approverGroups", of(absent{}, []any{}, []any{"S0123456789"})},
		{"spec.interactions.timeout", dur},
		{"spec.session.threadTTL", dur},
		{"spec.chat.mentions", of(absent{}, []any{map[string]any{"name": "here", "user": "U0123456789"}}, []any{map[string]any{"name": "ok", "user": "U0123456789"}},
			[]any{map[string]any{"name": "channel", "user": "U0123456789"}}, []any{map[string]any{"name": "everyone", "user": "U0123456789"}})},
		{t + "type", of("botMessage", "botMessage", "botMessage", "mention", "")},
		{t + "from", of(absent{}, map[string]any{"id": "B0123456789"}, map[string]any{"id": "B0123456789", "kind": "bot"}, map[string]any{"id": "B0123456789", "kind": ""})},
		{t + "from.id", of("B0123456789", "", "b1", "Bx", "B")},
		{t + "match", of(absent{}, map[string]any{}, map[string]any{"title": "^FIRING"})},
		{t + "match." + "text", regexes()},
		{t + "match." + "title", regexes()},
		{t + "exclude", of(absent{}, map[string]any{}, map[string]any{"text": "x"})},
		{t + "exclude." + "text", regexes()},
		{t + "match.body", regexes()},
		{t + "match", func(r *rand.Rand) any { return map[string]any{messageFields(r).(string): regexes()(r)} }},
		{t + "exclude", func(r *rand.Rand) any { return map[string]any{messageFields(r).(string): regexes()(r)} }},
		{t + "extract", of(absent{}, map[string]any{}, map[string]any{"key": map[string]any{"field": "title", "regex": "(?P<key>.+)"}})},
		{t + "extract", func(r *rand.Rand) any {
			n1, n2 := extractNames(r).(string), extractNames(r).(string)
			return map[string]any{
				n1: map[string]any{"field": messageFields(r), "regex": extractRegexes(n1)(r)},
				n2: map[string]any{"field": "text", "regex": extractRegexes(n2)(r)},
			}
		}},
		{t + "extract.key.regex", extractRegexes("key")},
		{t + "extract.state.regex", extractRegexes("state")},
		{t + "extract.state", of(absent{}, map[string]any{"field": "text", "regex": "(?P<state>x)"})},
		{t + "extract.key", of(absent{}, map[string]any{"field": "text", "regex": "(?P<key>x)"})},
		{t + "thread", of(absent{}, map[string]any{}, map[string]any{"by": "key", "cooldown": "2h"})},
		{t + "thread.by", of("key", "key", "", absent{}, "other")},
		{t + "thread.cooldown", dur},
		{t + "thread.maxIdle", dur},
		{t + "thread.closeWhen", of(absent{}, map[string]any{}, map[string]any{"state": "RESOLVED"}, map[string]any{"state": ""}, map[string]any{"other": "x"}, map[string]any{"key": "x"},
			map[string]any{"state": "x", "key": "y"})},
		{t + "promptTemplate", of(absent{}, "", "x", "{{ .Fields.state }}", "{{ .Fields.nope }}", "{{")},
		{t + "limits", of(absent{}, map[string]any{}, map[string]any{"maxRunsPerDay": int64(0)}, map[string]any{"rateLimit": map[string]any{"maxRuns": int64(1), "per": "10s", "maxConcurrent": int64(1)}})},
		{t + "limits.rateLimit.per", dur},
		{t + "limits.rateLimit", of(absent{}, map[string]any{"maxRuns": int64(1), "per": "1h", "maxConcurrent": int64(1)})},
		{t + "limits.maxRunsPerDay", of(absent{}, int64(0), int64(5), int64(-1))},
		// Whole lists, for the rule that refuses a trigger twice: the same trigger, a later one that differs only in what the
		// broker leaves out of the comparison, and two that differ in the sender or the kind.
		{"spec.chat.triggers", of(
			[]any{map[string]any{"type": "mention"}, map[string]any{"type": "mention"}},
			[]any{botTrigger("B0123456789", ""), botTrigger("B0123456789", "")},
			[]any{botTrigger("B0123456789", ""), botTrigger("B0123456789", "bot")},
			[]any{botTrigger("B0123456789", ""), botTrigger("B0123456780", "")},
			[]any{botTrigger("B0123456789", ""), withPrompt(botTrigger("B0123456789", ""), "y")},
			[]any{map[string]any{"type": "mention"}, botTrigger("B0123456789", "")},
			[]any{},
		)},
	}
}

// botTrigger is the smallest botMessage trigger Validate accepts, from the sender id and kind (empty leaves it out).
func botTrigger(id, kind string) map[string]any {
	from := map[string]any{"id": id}
	if kind != "" {
		from["kind"] = kind
	}
	return map[string]any{
		"type": "botMessage", "from": from, "promptTemplate": "x",
		"limits": map[string]any{"rateLimit": map[string]any{"maxRuns": int64(1), "per": "10s", "maxConcurrent": int64(1)}, "maxRunsPerDay": int64(0)},
	}
}

func withPrompt(t map[string]any, prompt string) map[string]any {
	t["promptTemplate"] = prompt
	return t
}

const backendDoc = `
apiVersion: agentkourier.dev/v1alpha1
kind: AgentBackend
metadata: {name: variation, namespace: sre}
spec:
  dialect: a2a
  url: "http://kagent.kagent:8083/"
  headers:
    - {name: X-A, value: v}
    - {name: X-B, valueSecretRef: {name: s}}
`

const bindingDoc = `
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: {name: variation, namespace: sre}
spec:
  agent: {backendRef: {name: kagent}, name: k8s-agent, namespace: kagent}
  identity: {userId: u}
  chat:
    connectionRef: {name: slack}
    channel: C0123456789
    triggers:
      - {type: mention}
      - type: botMessage
        from: {id: B0123456789}
        match: {title: "^FIRING"}
        exclude: {text: x}
        extract:
          state: {field: title, regex: "(?P<state>FIRING|RESOLVED)"}
          key: {field: fallback, regex: "(?P<key>.+)"}
        thread: {by: key, cooldown: 2h, maxIdle: 5h, closeWhen: {state: RESOLVED}}
        limits: {rateLimit: {maxRuns: 5, per: 10m, maxConcurrent: 2}, maxRunsPerDay: 20}
        promptTemplate: "An alert is {{ .State }}: {{ .Key }}."
  interactions: {toolApprovals: true, approverGroups: [S0123456789], timeout: 30m}
  session: {threadTTL: 90h}
`

const mentionBindingDoc = `
apiVersion: agentkourier.dev/v1alpha1
kind: Binding
metadata: {name: variation, namespace: sre}
spec:
  agent: {backendRef: {name: kagent}, name: k8s-agent, namespace: kagent}
  identity: {userId: u}
  chat:
    connectionRef: {name: slack}
    channel: C0123456789
    triggers:
      - {type: mention}
`
