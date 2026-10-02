package chattest

import (
	"context"
	"slices"
	"testing"
)

// The tests that show an unsafe adapter is caught only need the cases that its leak touches, and the whole of Corpus,
// in every place and by every method, costs minutes under -race. They run on this slice instead: a case or two of every
// kind of construct and of every group of the Corpus. The adapters that are held to the suite (the Fake, the reference
// platform here, and the Slack adapter) are checked with all of Corpus.
var representative = []string{
	// broadcasts and links to a channel
	"channel", "here", "everyone", "user group", "date", "channel link", "plain @channel", "entity-encoded broadcast",
	// mentions and the user IDs that only look like one
	"user", "user with a name", "user text in a sentence",
	"ID then a broadcast", "ID then a mention", "ID with a trailing newline", "ID in a token",
	// links
	"autolink", "link with a label", "mailto", "javascript", "markdown link", "markdown image", "markdown reference link",
	"markdown reference link, destination on the next line", "bare URL", "bare www", "URL in a sentence",
	// HTML and entities
	"script", "bold tag", "comment", "ampersand entity", "angle brackets",
	// markdown
	"bold", "italic", "strike", "code", "fence", "heading", "quote", "dash list", "table", "backslash", "trailing backslash",
	"emphasis around a mention", "code around a link",
	// invisible characters and line breaks
	"zero-width space", "right-to-left override", "control characters", "split broadcast", "split mention", "invalid UTF-8",
	"newline", "CRLF", "newline then a mention", "newline then a heading", "tabs and spaces", "only white space",
	// text that must come out as written
	"plain", "unicode", "punctuation", "empty",
}

// slice is the representative cases, and the cases of Corpus with these names.
func slice(more ...string) []Case { return pick(append(slices.Clone(representative), more...)...) }

// pick is the cases of Corpus with these names. It panics for a name that is not in Corpus, as a test helper may, so
// that renaming a case cannot quietly empty a test.
func pick(names ...string) []Case {
	var out []Case
	for _, name := range names {
		i := slices.IndexFunc(Corpus, func(c Case) bool { return c.Name == name })
		if i < 0 {
			panic("the corpus has no case named " + name)
		}
		if !slices.ContainsFunc(out, func(c Case) bool { return c.Name == name }) {
			out = append(out, Corpus[i])
		}
	}
	return out
}

// catch is what the suite finds of a Subject on the representative slice, and on the cases of more, with the text
// alone. It is for a test that shows a leak is found, as CheckWith is for one that shows an adapter is safe.
func catch(ctx context.Context, s Subject, more ...string) []Violation {
	return CheckWith(ctx, s, Options{Short: true, Cases: slice(more...)})
}

// catchEveryVariant is catch with each case sent three times, with the marker before it, after it and around it on
// lines of their own: a bare URL is a link only when nothing is glued to it, so a test that reads such a case by name needs
// the variants.
func catchEveryVariant(ctx context.Context, s Subject, more ...string) []Violation {
	return CheckWith(ctx, s, Options{Cases: slice(more...)})
}

// kindsOf is every kind of construct that either reader finds in a case.
func kindsOf(cs []Case) map[Kind]bool {
	out := map[Kind]bool{}
	for _, c := range cs {
		for _, sh := range []Shown{ScanSlack(c.Text), ScanSlackPlain(c.Text), ScanCommonMark(c.Text)} {
			for _, a := range sh.Active {
				out[a.Kind] = true
			}
		}
	}
	return out
}

// TestTheRepresentativeSliceHoldsEveryKindOfConstruct: a self-test that runs on the slice is no weaker, in what it can
// find, than one that runs on Corpus: the slice has a case in which each kind of construct is read, and one from every
// group of the Corpus.
func TestTheRepresentativeSliceHoldsEveryKindOfConstruct(t *testing.T) {
	t.Parallel()
	whole, part := kindsOf(Corpus), kindsOf(slice())
	for k := range whole {
		if !part[k] {
			t.Errorf("no case of the representative slice is read as a %s, and one of the Corpus is", k)
		}
	}
	for _, g := range [][]Case{broadcast, mentions, forgedUsers, links, html, markdown, invisible, lines, benign} {
		if !slices.ContainsFunc(slice(), func(c Case) bool { return slices.Contains(g, c) }) {
			t.Errorf("the representative slice holds nothing of the group that starts with %q", g[0].Name)
		}
	}
	if len(slice()) > len(Corpus)*3/5 {
		t.Errorf("the slice holds %d of %d cases: it is not much cheaper", len(slice()), len(Corpus))
	}
}
