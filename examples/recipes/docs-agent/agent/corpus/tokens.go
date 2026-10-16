package corpus

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

// stopwords are dropped from queries and from the index. The list is short on purpose: it holds words that carry
// no meaning in a question about documentation.
var stopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true, "be": true, "by": true, "can": true,
	"do": true, "does": true, "for": true, "from": true, "how": true, "i": true, "in": true, "is": true, "it": true,
	"of": true, "on": true, "or": true, "that": true, "the": true, "this": true, "to": true, "was": true, "what": true,
	"when": true, "where": true, "which": true, "with": true, "you": true, "your": true,
}

// scan calls emit for every searchable token of s, in order, with the byte range of the word it came from. A word
// is a run of letters and digits, with the combining marks that follow a letter or digit: a mark belongs to the word
// it modifies, so "cafe" and an accented "café" in either composed or decomposed form are one word each, and a
// Devanagari word is not split at its vowel signs. A word is normalised to Unicode NFC before anything else is done
// to it, so the two forms of "café" give the same token. It gives its lower-case, stemmed form; a word written in
// camel case ("botMessage") also gives each of its parts ("bot", "message"), with the range of the whole word.
// Stopwords and one-letter tokens are dropped. Documents and queries are scanned alike, so the same text gives the
// same tokens.
func scan(s string, emit func(tok string, start, end int)) {
	for i := 0; i < len(s); {
		r, n := utf8.DecodeRuneInString(s[i:])
		if !isWordStart(r) {
			i += n
			continue
		}
		start := i
		for i < len(s) {
			r, n = utf8.DecodeRuneInString(s[i:])
			if !isWordStart(r) && !unicode.IsMark(r) {
				break
			}
			i += n
		}
		word := norm.NFC.String(s[start:i])
		parts := camelParts(word)
		for _, w := range append([]string{word}, parts...) {
			if tok := normalizeToken(w); tok != "" {
				emit(tok, start, i)
			}
		}
	}
}

// isWordStart reports whether r can begin a word, and, with the marks, continue one.
func isWordStart(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// camelParts splits a word at a lower-case letter followed by an upper-case one ("botMessage") and at the end of
// an acronym ("HTTPServer"). It returns nil for a word with a single part.
func camelParts(word string) []string {
	runes := []rune(word)
	var parts []string
	from := 0
	for i := 1; i < len(runes); i++ {
		lowerToUpper := unicode.IsLower(runes[i-1]) && unicode.IsUpper(runes[i])
		acronymEnd := i+1 < len(runes) && unicode.IsUpper(runes[i-1]) && unicode.IsUpper(runes[i]) && unicode.IsLower(runes[i+1])
		if lowerToUpper || acronymEnd {
			parts = append(parts, string(runes[from:i]))
			from = i
		}
	}
	if from == 0 {
		return nil
	}
	return append(parts, string(runes[from:]))
}

// normalizeToken lower-cases and stems w, and returns "" for a word that is not searchable.
func normalizeToken(w string) string {
	w = strings.ToLower(w)
	if utf8.RuneCountInString(w) < 2 || stopwords[w] {
		return ""
	}
	return stem(w)
}

// stem folds the common English endings so that "triggers", "triggered" and "triggering" meet. It is crude on
// purpose: a stem need not be a word, only the same for the same family, on both sides of the search.
func stem(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		w = w[:len(w)-3] + "y"
	case len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") && !strings.HasSuffix(w, "us") && !strings.HasSuffix(w, "is"):
		w = w[:len(w)-1]
	}
	switch {
	case len(w) > 6 && strings.HasSuffix(w, "ing"):
		w = w[:len(w)-3]
	case len(w) > 5 && strings.HasSuffix(w, "ed"):
		w = w[:len(w)-2]
	}
	if len(w) > 4 && strings.HasSuffix(w, "e") {
		w = w[:len(w)-1]
	}
	return w
}
