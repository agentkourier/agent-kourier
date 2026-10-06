package redacttest

import (
	"strings"
	"testing"
)

// templateOf is a case's name without what was cut into it or what spaced it.
func templateOf(name string) string {
	for _, sep := range []string{" split by ", " across a "} {
		name, _, _ = strings.Cut(name, sep)
	}
	return name
}

// The sample that callers run has every template and form of the corpus, and a good deal fewer cases: a template
// that it lacks is a secret that no caller is tested against.
func TestSampleHasEveryTemplateOfTheCorpus(t *testing.T) {
	full, sample := SplitSecrets(), SplitSecretsSample()
	have := map[string]bool{}
	for _, c := range sample {
		have[templateOf(c.Name)] = true
	}
	for _, c := range full {
		if !have[templateOf(c.Name)] {
			t.Errorf("the sample lacks %q", templateOf(c.Name))
		}
	}
	if len(sample)*2 > len(full) {
		t.Errorf("the sample has %d cases of the %d: it is no sample", len(sample), len(full))
	}
}

// Every class of the deletion set that the first sample list names is in the sample's invisibles, once each.
func TestSampleCutsOneInvisibleOfEachClass(t *testing.T) {
	seen := map[string]bool{}
	for _, c := range SplitSecretsSample() {
		if strings.HasPrefix(c.Name, "a Slack bot token prefix split by ") {
			seen[strings.TrimPrefix(c.Name, "a Slack bot token prefix split by ")] = true
		}
	}
	if len(seen) != len(sampleIndexes) {
		t.Errorf("the sample cuts %d invisibles into a template, want %d: %v", len(seen), len(sampleIndexes), seen)
	}
	for _, class := range []string{"Cc ", "Cf ", "Co ", "Cs ", "invalid UTF-8", "Hangul filler", "variation selector", "Mn ", "Cn "} {
		found := false
		for name := range seen {
			found = found || strings.HasPrefix(name, class)
		}
		if !found {
			t.Errorf("the sample has no invisible of the class %q", class)
		}
	}
}
