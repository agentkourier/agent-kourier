package v1alpha1

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode"
)

// A CEL rule is a fast-fail copy of a rule Validate enforces: it may refuse only what Validate refuses, because a CEL
// rule that refuses a working object fails `kubectl apply` for it, and one added later can block updates to objects
// already stored. An apiserver evaluates the rules, so `make crd-corpus` (test/crdcorpus) is what holds them to the
// invariant: every entry of its corpus, and its generated variations, go to a real one. These tests are the part that
// needs no cluster. They read the committed CRDs and hold each literal that a rule copies (a list of names, a limit, a
// range, a regex) to the Go constant it copies, and hold every rule to the properties that keep a copy safe to ship.

// celRule is one entry of a schema node's x-kubernetes-validations.
type celRule struct {
	Rule, Message, MessageExpression, FieldPath string
}

// rules returns the node's own CEL rules.
func (s schema) rules() []celRule {
	list, _ := s["x-kubernetes-validations"].([]any)
	out := make([]celRule, 0, len(list))
	for _, v := range list {
		m, _ := v.(map[string]any)
		str := func(k string) string { x, _ := m[k].(string); return x }
		out = append(out, celRule{Rule: str("rule"), Message: str("message"), MessageExpression: str("messageExpression"), FieldPath: str("fieldPath")})
	}
	return out
}

// walkRules calls visit for every CEL rule under the node, with the path of the node it is on ("" for the root).
func walkRules(node any, path string, visit func(path string, r celRule)) {
	switch n := node.(type) {
	case schema:
		walkRules(map[string]any(n), path, visit)
	case map[string]any:
		for _, r := range schema(n).rules() {
			visit(path, r)
		}
		for k, v := range n {
			if k == "x-kubernetes-validations" {
				continue
			}
			walkRules(v, path+"/"+k, visit)
		}
	case []any:
		for i, v := range n {
			walkRules(v, path+"/"+strconv.Itoa(i), visit)
		}
	}
}

// ruleWith returns the one rule on the node whose text contains all of the fragments.
func ruleWith(t *testing.T, node schema, fragments ...string) string {
	t.Helper()
	var found []string
	for _, r := range node.rules() {
		if !slices.ContainsFunc(fragments, func(f string) bool { return !strings.Contains(r.Rule, f) }) {
			found = append(found, r.Rule)
		}
	}
	if len(found) != 1 {
		t.Fatalf("want one rule containing %q, found %d: %q", fragments, len(found), found)
	}
	return found[0]
}

var (
	quoted    = regexp.MustCompile(`'([^']*)'`)
	durations = regexp.MustCompile(`duration\('([^']+)'\)`)
)

// listed returns the single-quoted words of the rule's list `[ ... ]`.
func listed(t *testing.T, rule string) []string {
	t.Helper()
	start, end := strings.Index(rule, "["), strings.Index(rule, "]")
	if start < 0 || end < start {
		t.Fatalf("no list in rule %q", rule)
	}
	var out []string
	for _, m := range quoted.FindAllStringSubmatch(rule[start:end], -1) {
		out = append(out, m[1])
	}
	return out
}

// limit returns the number that the rule compares a size to, after "<=".
func limit(t *testing.T, rule string) int {
	t.Helper()
	m := regexp.MustCompile(`<= (\d+)`).FindStringSubmatch(rule)
	if m == nil {
		t.Fatalf("no limit in rule %q", rule)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// durationLiterals returns the durations the rule names, but 0s, sorted.
func durationLiterals(t *testing.T, rule string) []time.Duration {
	t.Helper()
	var out []time.Duration
	for _, m := range durations.FindAllStringSubmatch(rule, -1) {
		d, err := time.ParseDuration(m[1])
		if err != nil {
			t.Fatalf("rule %q: %v", rule, err)
		}
		if d != 0 {
			out = append(out, d)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// TestCELRulesAreSafeToShip holds every rule to what keeps a copy safe: it sits in the spec and never reads the status
// (Validate ignores it; a rule in the spec runs on a write to the status subresource too, as every rule of the schema
// does), is not above the spec, has a message
// that a person can act on and is not computed (a messageExpression could print a value beyond the object), and is not
// a transition rule, which a stored object that is already invalid would fail on every update (an ordinary rule is
// ratcheted: an update that leaves the field as it was is let through).
func TestCELRulesAreSafeToShip(t *testing.T) {
	var total int
	for _, plural := range []string{"agentbackends", "chatconnections", "bindings"} {
		walkRules(crdSchema(t, plural), "", func(path string, r celRule) {
			total++
			where := plural + path + ": " + r.Rule
			if !strings.HasPrefix(path, "/properties/spec/") && path != "/properties/spec" {
				t.Errorf("%s: a rule must be on the spec or below it", where)
			}
			if strings.Contains(r.Rule, "status") || strings.Contains(r.Rule, "oldSelf") || strings.Contains(r.Rule, "metadata") {
				t.Errorf("%s: a rule must not read the status, the metadata or the old object", where)
			}
			// gofmt rewrites two apostrophes in a doc comment, which is where a marker on a type sits, into a typographic
			// quote, and a rule with one does not compile: an apiserver refuses the CRD, or in a string literal, the rule.
			if i := strings.IndexFunc(r.Rule, func(c rune) bool { return c > unicode.MaxASCII }); i >= 0 {
				t.Errorf("%s: the rule has a character that is not ASCII at byte %d: write an empty string as \"\" inside backticks", where, i)
			}
			if r.Message == "" {
				t.Errorf("%s: a rule needs a message", where)
			}
			if r.MessageExpression != "" {
				t.Errorf("%s: a messageExpression could print a value beyond what the object holds", where)
			}
			if r.FieldPath != "" && !strings.HasPrefix(r.FieldPath, ".") {
				t.Errorf("%s: fieldPath %q is not a path from the node", where, r.FieldPath)
			}
		})
	}
	if total < 30 {
		t.Errorf("only %d CEL rules found in the committed CRDs: the walk no longer reaches them", total)
	}
}

// TestCELLiteralsEqualTheBrokersConstants pins what a rule copies to the Go constant it copies, where the two are
// written differently: a list of names, a limit, a range.
func TestCELLiteralsEqualTheBrokersConstants(t *testing.T) {
	bindings, backends := crdSchema(t, "bindings"), crdSchema(t, "agentbackends")
	trigger := "spec.chat.triggers[]"

	t.Run("reserved header names", func(t *testing.T) {
		// IsReservedHeader compares the canonical form, which changes only the case of ASCII letters, so the rule lists
		// the lower-case names and compares lowerAscii. Each listed name must be one the broker reserves (a name it does
		// not would refuse an object the broker accepts), and each name the broker reserves must be listed.
		names := listed(t, ruleWith(t, backends.at(t, "spec.headers[].name"), "lowerAscii"))
		reserved := []string{"authorization", "proxy-authorization", "cookie", "x-user-id", "x-agent-name", "x-share-token", "x-kagent-insecure-runtime-identity"}
		for _, n := range names {
			if !IsReservedHeader(n) || n != strings.ToLower(n) {
				t.Errorf("the rule lists %q, which IsReservedHeader does not reserve, or is not lower case", n)
			}
		}
		for _, n := range reserved {
			if !slices.Contains(names, n) {
				t.Errorf("IsReservedHeader reserves %q and the rule does not list it", n)
			}
		}
		if len(names) != len(reserved) {
			t.Errorf("the rule lists %d names, the broker reserves %d: %v", len(names), len(reserved), names)
		}
	})

	t.Run("broadcast names", func(t *testing.T) {
		names := listed(t, ruleWith(t, bindings.at(t, "spec.chat.mentions[].name"), "in ["))
		slices.Sort(names)
		var want []string
		for n := range broadcastNames {
			want = append(want, n)
		}
		slices.Sort(want)
		if !slices.Equal(names, want) {
			t.Errorf("the rule lists %v, broadcastNames has %v", names, want)
		}
	})

	t.Run("message fields", func(t *testing.T) {
		var want []string
		for _, f := range messageFields() {
			want = append(want, string(f))
		}
		for _, field := range []string{"match", "exclude"} {
			names := listed(t, ruleWith(t, bindings.at(t, trigger+"."+field), "in ["))
			if !slices.Equal(names, want) {
				t.Errorf("%s: the rule lists %v, messageFields has %v", field, names, want)
			}
		}
	})

	t.Run("byte and character limits", func(t *testing.T) {
		// The header name and the extract regex are counted in bytes, as Validate counts them; a match or exclude value is
		// counted in characters, which is never more than its bytes, so the rule is not the stricter.
		if got := limit(t, ruleWith(t, backends.at(t, "spec.headers[].name"), "bytes(")); got != maxHeaderNameLen {
			t.Errorf("header name: rule %d, maxHeaderNameLen %d", got, maxHeaderNameLen)
		}
		if got := limit(t, ruleWith(t, bindings.at(t, trigger+".extract.{}.regex"), "bytes(")); got != maxTriggerRegexLen {
			t.Errorf("extract regex: rule %d, maxTriggerRegexLen %d", got, maxTriggerRegexLen)
		}
		for _, field := range []string{"match", "exclude"} {
			rule := ruleWith(t, bindings.at(t, trigger+"."+field), "size(self[k]) <= ")
			if strings.Contains(rule, "bytes(") {
				t.Errorf("%s: a rule over bytes of an unbounded value is over the apiserver's cost budget", field)
			}
			if got := limit(t, rule); got != maxTriggerRegexLen {
				t.Errorf("%s: rule %d, maxTriggerRegexLen %d", field, got, maxTriggerRegexLen)
			}
		}
	})

	t.Run("extract name", func(t *testing.T) {
		rule := ruleWith(t, bindings.at(t, trigger+".extract"), "matches(")
		m := regexp.MustCompile(`matches\('([^']*)'\)`).FindStringSubmatch(rule)
		if m == nil || m[1] != extractName.String() {
			t.Errorf("rule %q, extractName %q", rule, extractName)
		}
	})

	t.Run("names the rules compare to", func(t *testing.T) {
		if string(TriggerMention) != "mention" || string(TriggerBotMessage) != "botMessage" || string(ThreadByKey) != "key" || extractKey != "key" {
			t.Fatal("a name that the rules write out as a literal has changed: update the rules")
		}
		node := bindings.at(t, trigger)
		for _, f := range []string{"'mention'", "'botMessage'", "thread.by == 'key'", "'key' in self.extract"} {
			if !slices.ContainsFunc(node.rules(), func(r celRule) bool { return strings.Contains(r.Rule, f) }) {
				t.Errorf("no rule of the trigger has %s", f)
			}
		}
	})

	t.Run("duration ranges", func(t *testing.T) {
		thread := bindings.at(t, trigger+".thread")
		for _, tc := range []struct {
			what string
			rule string
			want []time.Duration
		}{
			{"cooldown", ruleWith(t, thread, "duration('168h')"), []time.Duration{minThreadCooldown, maxThreadCooldown}},
			{"maxIdle", ruleWith(t, thread, "duration('720h')"), []time.Duration{MaxThreadMaxIdle}},
			{"rateLimit.per", ruleWith(t, bindings.at(t, trigger+".limits.rateLimit.per"), "duration('24h')"), []time.Duration{MinRateWindow, MaxLimitWindow}},
		} {
			if got := durationLiterals(t, tc.rule); !slices.Equal(got, tc.want) {
				t.Errorf("%s: the rule names %v, the broker's bounds are %v", tc.what, got, tc.want)
			}
		}
		// maxIdle's floor is twice the cooldown, written as a sum.
		if rule := ruleWith(t, thread, "duration('720h')"); !strings.Contains(rule, "duration(self.cooldown) + duration(self.cooldown)") {
			t.Errorf("the maxIdle rule does not have a floor of twice the cooldown: %s", rule)
		}
	})
}
