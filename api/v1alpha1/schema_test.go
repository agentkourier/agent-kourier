package v1alpha1

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/yaml"
)

// A schema marker is a fast-fail copy of a rule Validate enforces, so the generated schema must never refuse an object
// Validate accepts. These tests read the committed CRDs (which TestCommittedCRDsMatchGeneration holds equal to the
// markers) and pin each copy to the Go rule it copies: a regex literal that must equal the Go one, a cap that must equal
// its constant, or, where the two are written differently, the same answer on every string of a small alphabet. The
// objects themselves are checked against an apiserver by `make crd-corpus` (test/crdcorpus).

// committedCRDs is where `make crds` writes the generated CRDs, relative to this package.
const committedCRDs = "../../charts/agent-kourier/files/crds"

// schema is one node of a CRD's OpenAPI schema, as decoded from the committed YAML.
type schema map[string]any

// crdSchema reads the v1alpha1 schema of the CRD committed for file (such as "bindings").
func crdSchema(t *testing.T, plural string) schema {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(committedCRDs, "agentkourier.dev_"+plural+".yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var crd struct {
		Spec struct {
			Versions []struct {
				Name   string
				Schema struct {
					OpenAPIV3Schema schema `json:"openAPIV3Schema"`
				}
			}
		}
	}
	if err := yaml.Unmarshal(b, &crd); err != nil {
		t.Fatalf("%s: %v", plural, err)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != "v1alpha1" {
		t.Fatalf("%s: want exactly the version v1alpha1, got %+v", plural, crd.Spec.Versions)
	}
	return crd.Spec.Versions[0].Schema.OpenAPIV3Schema
}

// at follows a path of property names from s. A "[]" after a name goes into the array's items, and the segment "{}"
// into the values of a map.
func (s schema) at(t *testing.T, path string) schema {
	t.Helper()
	node := s
	for seg := range strings.SplitSeq(path, ".") {
		if seg == "{}" {
			node = node.child(t, path, "additionalProperties")
			continue
		}
		name, items := strings.CutSuffix(seg, "[]")
		node = node.child(t, path, "properties").child(t, path, name)
		if items {
			node = node.child(t, path, "items")
		}
	}
	return node
}

func (s schema) child(t *testing.T, path, key string) schema {
	t.Helper()
	c, ok := s[key].(map[string]any)
	if !ok {
		t.Fatalf("schema path %s: no %q (have %v)", path, key, slices.Sorted(mapKeys(s)))
	}
	return c
}

func mapKeys(m map[string]any) func(yield func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// str returns the string keyword, or "" when the schema has none.
func (s schema) str(key string) string { v, _ := s[key].(string); return v }

// num returns an integer keyword, or -1 when the schema has none.
func (s schema) num(key string) int {
	switch v := s[key].(type) {
	case float64:
		return int(v)
	case int64:
		return int(v)
	}
	return -1
}

// accepts reports whether the schema's string keywords accept v, as an apiserver applies them: lengths count
// characters, and the pattern is not anchored unless it says so.
func (s schema) accepts(t *testing.T, v string) bool {
	t.Helper()
	return s.matcher(t)(v)
}

// matcher is accepts with the pattern compiled once.
func (s schema) matcher(t *testing.T) func(string) bool {
	t.Helper()
	minLen, maxLen := s.num("minLength"), s.num("maxLength")
	var re *regexp.Regexp
	if p := s.str("pattern"); p != "" {
		var err error
		if re, err = regexp.Compile(p); err != nil {
			t.Fatalf("pattern %q: %v", p, err)
		}
	}
	return func(v string) bool {
		n := utf8.RuneCountInString(v)
		return (minLen < 0 || n >= minLen) && (maxLen < 0 || n <= maxLen) && (re == nil || re.MatchString(v))
	}
}

// every is the strings of a small alphabet up to length n, the empty one included.
func every(alphabet []string, n int) []string {
	all := []string{""}
	for frontier := []string{""}; n > 0; n-- {
		var next []string
		for _, prefix := range frontier {
			for _, c := range alphabet {
				next = append(next, prefix+c)
			}
		}
		all = append(all, next...)
		frontier = next
	}
	return all
}

// nameProbes are strings that sit on the edges of the DNS-1123 and Secret key grammars: the ends and middles of
// names, dots, dashes, underscores, capitals, a non-ASCII letter, and the lengths 63, 64, 253 and 254.
func nameProbes() []string {
	probes := every([]string{"a", "0", "-", ".", "_", "A", "é", "/"}, 4)
	for _, n := range []int{62, 63, 64, 252, 253, 254, 255, 600} {
		probes = append(probes, strings.Repeat("a", n), strings.Repeat("a.", n/2)+"a", "a"+strings.Repeat("-", n-2)+"a", "é"+strings.Repeat("a", n-1))
	}
	return slices.Compact(slices.Sorted(slices.Values(probes)))
}

// sameAnswer fails for every probe on which the schema is stricter than the broker, which is what breaks the invariant.
// With exact set, it fails for every probe on which the schema is looser too: a marker that copies a rule whole must
// not drift into a weaker one, which only fails to fast-fail and which nothing else would notice. A pattern that is
// meant to be a superset, as the duration's is, does not set it.
func sameAnswer(t *testing.T, what string, s schema, brokerAccepts func(string) bool, probes []string, exact bool) {
	t.Helper()
	var stricter, looser int
	accepts := s.matcher(t)
	for _, p := range probes {
		got, want := accepts(p), brokerAccepts(p)
		switch {
		case !got && want:
			stricter++
			if stricter <= 5 {
				t.Errorf("%s: the schema refuses %q, which the broker accepts", what, p)
			}
		case got && !want:
			looser++
			if exact && looser <= 5 {
				t.Errorf("%s: the schema accepts %q, which the broker refuses; the marker is meant to copy the rule whole", what, p)
			}
		}
	}
	t.Logf("%s: %d probes, schema looser than the broker on %d", what, len(probes), looser)
}

func isDNSSubdomain(v string) bool { return len(validation.IsDNS1123Subdomain(v)) == 0 }
func isDNSLabel(v string) bool     { return len(validation.IsDNS1123Label(v)) == 0 }

// TestSchemaNamesMatchTheBrokersRules covers the Secret, backend, connection and namespace names and keys, whose broker
// rules call the apimachinery validators. The marker is the validator's regex and length, written out. An optional field
// the broker accepts when empty accepts the empty string in the schema too, since a stored object can say `namespace: ""`.
func TestSchemaNamesMatchTheBrokersRules(t *testing.T) {
	probes := nameProbes()
	for _, tc := range []struct {
		plural, path string
		brokerOK     func(string) bool
	}{
		// Required names: the broker refuses "".
		{"agentbackends", "spec.headers[].valueSecretRef.name", isDNSSubdomain},
		{"bindings", "spec.identity.tokenSecretRef.name", isDNSSubdomain},
		{"chatconnections", "spec.credentialsSecretRef.name", isDNSSubdomain},
		{"bindings", "spec.agent.backendRef.name", isDNSSubdomain},
		{"bindings", "spec.chat.connectionRef.name", isDNSSubdomain},
		{"bindings", "spec.agent.namespace", isDNSLabel},
		{"agentbackends", "spec.allowedNamespaces[]", isDNSLabel},
		{"chatconnections", "spec.allowedNamespaces[]", isDNSLabel},
		// Optional values the broker accepts when they are empty.
		{"bindings", "spec.agent.backendRef.namespace", func(v string) bool { return v == "" || isDNSLabel(v) }},
		{"bindings", "spec.chat.connectionRef.namespace", func(v string) bool { return v == "" || isDNSLabel(v) }},
		{"agentbackends", "spec.headers[].valueSecretRef.key", func(v string) bool { return v == "" || len(validation.IsConfigMapKey(v)) == 0 }},
		{"bindings", "spec.identity.tokenSecretRef.key", func(v string) bool { return v == "" || len(validation.IsConfigMapKey(v)) == 0 }},
	} {
		t.Run(tc.plural+"/"+tc.path, func(t *testing.T) {
			sameAnswer(t, tc.path, crdSchema(t, tc.plural).at(t, tc.path), tc.brokerOK, probes, true)
		})
	}
}

// TestSchemaEmojiMatchesTheBrokersRule: the broker tests a reaction name only when it is not empty (`Emoji != "" && !match`),
// so the schema's pattern accepts "" besides what reactionName does, and is not equal to its literal.
func TestSchemaEmojiMatchesTheBrokersRule(t *testing.T) {
	probes := every([]string{"a", "0", "+", "-", "_", ":", "A", "é"}, 4)
	sameAnswer(t, "acknowledgment.emoji", crdSchema(t, "bindings").at(t, "spec.chat.acknowledgment.emoji"),
		func(v string) bool { return v == "" || reactionName.MatchString(v) }, probes, true)
}

// TestSchemaEnumsListTheEmptyValueExactlyWhereTheBrokerAcceptsIt holds the enums to the invariant for a field written as
// "": where the broker accepts it, the schema must too, and where it refuses it, the enum must not list it, or a
// fast-fail is lost. The broker is asked: each row sets the field to "" in a valid object and runs Default and
// Validate, so the answer is the code's and not a column kept by hand. Default fills five of these from "" (output,
// progress, threadReplies, botMentions, from.kind); Validate reads an empty thread.by as unset. Every enum of the
// schema has a row, so a new one must be put in the table.
func TestSchemaEnumsListTheEmptyValueExactlyWhereTheBrokerAcceptsIt(t *testing.T) {
	bindings, backends, conns := crdSchema(t, "bindings"), crdSchema(t, "agentbackends"), crdSchema(t, "chatconnections")
	trigger := "spec.chat.triggers[]"
	binding := func(set func(*Binding)) func(*testing.T) error {
		return func(t *testing.T) error {
			o := load[Binding](t, "binding.yaml")
			set(o)
			o.Default()
			return o.Validate()
		}
	}
	rows := []struct {
		node   schema
		name   string
		broker func(*testing.T) error // the broker's answer for the field written as ""
	}{
		{bindings.at(t, "spec.chat.output"), "output", binding(func(o *Binding) { o.Spec.Chat.Output = "" })},
		{bindings.at(t, "spec.chat.progress"), "progress", binding(func(o *Binding) { o.Spec.Chat.Progress = "" })},
		{bindings.at(t, "spec.chat.threadReplies"), "threadReplies", binding(func(o *Binding) { o.Spec.Chat.ThreadReplies = "" })},
		{bindings.at(t, "spec.chat.botMentions"), "botMentions", binding(func(o *Binding) { o.Spec.Chat.BotMentions = "" })},
		{bindings.at(t, trigger+".from.kind"), "from.kind", binding(func(o *Binding) { bot(o).From.Kind = "" })},
		// An empty by with a cooldown or closeWhen is refused for them, so the rest of the thread is cleared: the field alone is the question.
		{bindings.at(t, trigger+".thread.by"), "thread.by", binding(func(o *Binding) { bot(o).Thread = TriggerThread{By: ""} })},
		{backends.at(t, "spec.dialect"), "dialect", func(t *testing.T) error {
			o := load[AgentBackend](t, "agentbackend.yaml")
			o.Spec.Dialect = ""
			o.Default()
			return o.Validate()
		}},
		{conns.at(t, "spec.platform"), "platform", func(t *testing.T) error {
			o := load[ChatConnection](t, "chatconnection.yaml")
			o.Spec.Platform = ""
			o.Default()
			return o.Validate()
		}},
		{bindings.at(t, trigger+".type"), "trigger type", binding(func(o *Binding) { bot(o).Type = "" })},
		{bindings.at(t, trigger+".extract.{}.field"), "extract field", binding(func(o *Binding) {
			e := bot(o).Extract["state"]
			e.Field = ""
			bot(o).Extract["state"] = e
		})},
	}
	for _, tc := range rows {
		// The fixtures are valid, so the problem a refusal reports is the one the "" made.
		accepts := tc.broker(t) == nil
		enum, _ := tc.node["enum"].([]any)
		if len(enum) == 0 {
			t.Errorf("%s has no enum", tc.name)
		}
		if got := slices.Contains(enum, any("")); got != accepts {
			t.Errorf("%s: the enum lists \"\" = %t, but the broker accepts \"\" = %t", tc.name, got, accepts)
		}
	}
	var n int
	for _, sc := range []schema{bindings, backends, conns} {
		n += countEnums(sc)
	}
	if n != len(rows) {
		t.Errorf("the schemas have %d enums and the table %d rows: put the new one in it", n, len(rows))
	}
}

func countEnums(v any) int {
	var n int
	switch t := v.(type) {
	case map[string]any:
		if _, ok := t["enum"]; ok {
			n++
		}
		for _, c := range t {
			n += countEnums(c)
		}
	case schema:
		return countEnums(map[string]any(t))
	case []any:
		for _, c := range t {
			n += countEnums(c)
		}
	}
	return n
}

// TestSchemaPatternsEqualTheGoLiterals holds each pattern marker to the regex literal of the rule it copies. The
// comments on those literals say they must be equal; this is what makes it so.
func TestSchemaPatternsEqualTheGoLiterals(t *testing.T) {
	for _, tc := range []struct {
		plural, path string
		re           *regexp.Regexp
	}{
		{"bindings", "spec.chat.channel", channelID},
		{"bindings", "spec.chat.triggers[].from.id", botID},
		{"bindings", "spec.chat.mentions[].name", mentionName},
		{"bindings", "spec.chat.mentions[].user", mentionUser},
		{"bindings", "spec.interactions.approverGroups[]", approverGroupID},
	} {
		t.Run(tc.path, func(t *testing.T) {
			if got := crdSchema(t, tc.plural).at(t, tc.path).str("pattern"); got != tc.re.String() {
				t.Errorf("schema pattern = %q, Go rule = %q", got, tc.re.String())
			}
		})
	}
}

// TestSchemaCapsEqualTheBrokersCaps pins every bound in the schema to the constant Validate enforces, so that neither
// can move alone.
func TestSchemaCapsEqualTheBrokersCaps(t *testing.T) {
	bindings, backends := crdSchema(t, "bindings"), crdSchema(t, "agentbackends")
	trigger := "spec.chat.triggers[]"
	for _, tc := range []struct {
		name string
		node schema
		key  string
		want int
	}{
		{"mentions", bindings.at(t, "spec.chat.mentions"), "maxItems", maxMentions},
		{"approverGroups", bindings.at(t, "spec.interactions.approverGroups"), "maxItems", maxApproverGroups},
		{"triggers", bindings.at(t, "spec.chat.triggers"), "maxItems", maxTriggers},
		{"extract entries", bindings.at(t, trigger+".extract"), "maxProperties", maxExtract},
		// A closeWhen key must be an extract's, so this bound refuses nothing that the extract cap does not.
		{"closeWhen entries", bindings.at(t, trigger+".thread.closeWhen"), "maxProperties", maxExtract},
		// Only six message fields exist, and the broker refuses any other key.
		{"match entries", bindings.at(t, trigger+".match"), "maxProperties", len(messageFields())},
		{"exclude entries", bindings.at(t, trigger+".exclude"), "maxProperties", len(messageFields())},
		{"headers", backends.at(t, "spec.headers"), "maxItems", maxHeaders},
		{"header name", backends.at(t, "spec.headers[].name"), "maxLength", maxHeaderNameLen},
		// The broker counts bytes and the schema characters, so the schema is never the stricter of the two.
		{"extract regex", bindings.at(t, trigger+".extract.{}.regex"), "maxLength", maxTriggerRegexLen},
		{"extract regex minimum", bindings.at(t, trigger+".extract.{}.regex"), "minLength", 1},
	} {
		if got := tc.node.num(tc.key); got != tc.want {
			t.Errorf("%s: schema %s = %d, Go rule = %d", tc.name, tc.key, got, tc.want)
		}
	}
}

// TestSchemaListSemantics holds the two lists whose duplicates the broker refuses to the list types that make the
// apiserver refuse them.
func TestSchemaListSemantics(t *testing.T) {
	bindings := crdSchema(t, "bindings")
	groups := bindings.at(t, "spec.interactions.approverGroups")
	if got := groups.str("x-kubernetes-list-type"); got != "set" {
		t.Errorf("approverGroups list type = %q, want set: a group named twice is refused", got)
	}
	mentions := bindings.at(t, "spec.chat.mentions")
	if got := mentions.str("x-kubernetes-list-type"); got != "map" {
		t.Errorf("mentions list type = %q, want map: a name declared twice is refused", got)
	}
	if got, _ := mentions["x-kubernetes-list-map-keys"].([]any); !slices.Equal(got, []any{"name"}) {
		t.Errorf("mentions list map keys = %v, want [name]", got)
	}
	// A key of a map list must be required or defaulted.
	if req, _ := bindings.at(t, "spec.chat.mentions[]")["required"].([]any); !slices.Contains(req, any("name")) {
		t.Errorf("mention name is not required (%v), so it cannot be a list map key", req)
	}
}

// durationFields are the fields of metav1.Duration type, which generate as plain strings.
var durationFields = []struct{ plural, path string }{
	{"bindings", "spec.interactions.timeout"},
	{"bindings", "spec.session.threadTTL"},
	{"bindings", "spec.chat.triggers[].limits.rateLimit.per"},
	{"bindings", "spec.chat.triggers[].thread.cooldown"},
	{"bindings", "spec.chat.triggers[].thread.maxIdle"},
}

// TestSchemaDurationPatternIsASupersetOfParseDuration holds the Duration pattern to its one requirement: it accepts every
// string time.ParseDuration accepts, which is what the broker's decoder calls. It does not have to refuse the rest (an
// overflow, "s", "."), and CEL or the broker does that.
func TestSchemaDurationPatternIsASupersetOfParseDuration(t *testing.T) {
	// "µ" is U+00B5 and "μ" U+03BC, the two spellings ParseDuration takes for microseconds.
	alphabet := []string{"0", "1", ".", "-", "+", "s", "m", "h", "n", "u", "µ", "μ"}
	probes := every(alphabet, 5)
	probes = append(probes, "1h30m", "1h30m15s", "+30m", "-30m", ".5h", "5.h", "1.5h", "100ms", "100µs", "100μs", "1ns", "0", "+0", "-0",
		"1h1h", "10000000000000000000s", "banana", "30", "1d", "1 h", "1H", "", "h", ".", ".s", "1.2.3s", "1e3s")
	nodes := make([]schema, 0, len(durationFields))
	for _, f := range durationFields {
		nodes = append(nodes, crdSchema(t, f.plural).at(t, f.path))
	}
	for i, f := range durationFields {
		if nodes[i].str("pattern") != nodes[0].str("pattern") {
			t.Errorf("%s has pattern %q, but %s has %q: one pattern for every duration", f.path, nodes[i].str("pattern"), durationFields[0].path, nodes[0].str("pattern"))
		}
	}
	sameAnswerAsParse := func(v string) bool { _, err := time.ParseDuration(v); return err == nil }
	var accepted int
	for _, p := range probes {
		if sameAnswerAsParse(p) {
			accepted++
		}
	}
	if accepted < 100 {
		t.Fatalf("only %d of the %d probes are durations; the alphabet no longer reaches the grammar", accepted, len(probes))
	}
	if nodes[0].str("pattern") == "" {
		t.Fatalf("%s has no pattern, so `banana` passes the schema", durationFields[0].path)
	}
	sameAnswer(t, "duration pattern", nodes[0], sameAnswerAsParse, probes, false)
	// And it must still refuse what is plainly not a duration, or it is no fast-fail at all.
	for _, v := range []string{"banana", "30", "1d", "1 h", ""} {
		if nodes[0].accepts(t, v) {
			t.Errorf("the duration pattern accepts %q", v)
		}
	}
}
