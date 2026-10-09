package redact

import (
	"regexp"
	"slices"
	"strings"
	"unicode"
)

// The reading model behind the known limits (leaklimits_test.go): how the rules of main read a value, written down from the
// documented behaviour (a value is a run up to white space or a quoted string; a tag, a continuation mark and an authorization scheme
// take the word after them; the token layer reads the first word after an OAuth name) so that a known limit can say exactly where
// main shows a marker. It is a model of reading, not a copy of the code: the oracle holds it to main from both sides (a marker that
// main shows and no limit explains fails, and so does a limit that applies where main hides the marker).

// sameLine reports whether the layout keeps the next field on the line of the one before it.
func sameLine(l leakLayout) bool {
	return slices.Contains([]string{"space", "comma", "flow", "cell"}, l.id)
}

// namesRead reports whether the field is read by the rules that read names, in a style where a name and an operator come before the
// value (a flag has its own rule, and a user flag has none).
func namesRead(a *leakAtom) bool {
	switch a.style {
	case "plain", "json", "hash", "escaped", "env", "header":
		return true
	case "flag":
		return a.op == "=" // "--password=" is a name and an equals sign to them; "--password " is a flag
	}
	return false
}

// colonKeyed reports whether the field begins with a secret key that a reader of YAML sees as a key: a quoted name, or a name and
// a colon (a Kubernetes env entry's "name" is no secret name).
func colonKeyed(a *leakAtom) bool {
	if a.style == "entry" {
		return false
	}
	f := strings.Fields(a.text)
	return a.style == "json" || a.style == "hash" || a.style == "escaped" || a.style == "header" || a.style == "cookie" ||
		len(f) > 0 && strings.HasSuffix(f[0], ":")
}

// prevIndex returns the index of the field before marker m's, skipping a label that is the word "token" (an authorization scheme
// to the rules that read a value, which takes the run after it), or -1.
func prevIndex(c leakCase, m leakMarker) int {
	for i := m.atom - 1; i >= 0; i-- {
		if a := c.atoms[i]; a.style != "label" || a.text != "token" {
			return i
		}
	}
	return -1
}

// earlierCovers reports whether a field before the marker's takes the rest of its line for a block scalar, so the marker is part of
// what that field's rules redact: the bar of a table cell after a secret name that has no value or only a tag, and a block header
// with text after it, which takes the rest of its line in any layout that keeps the next field on it.
func earlierCovers(s leakSlot) bool { return coversAt(s, s.m.atom, s.m.pos) }

// coversAt is earlierCovers for the field idx of the case and a point pos of it.
func coversAt(s leakSlot, idx, pos int) bool {
	c := s.c
	for i, p := range c.atoms[:idx] {
		if p.class == "decoy" || !namesRead(p) {
			continue
		}
		// The token layer takes the first word after an OAuth name for its value, a tag or a bar among them, before the name's own
		// rules read it as a block header, unless the "=" has the value away from it (an empty value: "name= | ..."). In "password: | v"
		// the bar is a block header and takes the line, v with it; in "access_token: | v" the token layer takes the bar for the value
		// and v is shown.
		tokenTakes := p.class == "oauth" && (p.op != "=" || p.form != "empty") && p.form != "amp"
		header := p.form == "blockinline" && sameLine(c.layout) || c.layout.id == "cell" && (p.form == "empty" || tagLike(p.form))
		if tokenTakes || !header {
			continue
		}
		// A block scalar is the rest of its line and the lines under it that are indented more than the line of its header.
		if !strings.Contains(c.text[c.ends[i]:pos], "\n") || textIndent(c.text, pos) > textIndent(c.text, c.ends[i]-1) {
			return true
		}
	}
	return false
}

// textIndent is how many spaces and tabs begin the line of text that holds index i.
func textIndent(text string, i int) int {
	j := strings.LastIndexByte(text[:i], '\n') + 1
	n := 0
	for j+n < len(text) && (text[j+n] == ' ' || text[j+n] == '\t') {
		n++
	}
	return n
}

// cutDrops reports whether a Cut entry that was cut drops the marker with the last run of its output: no white space follows the
// first window of the marker that the entry reads (not counting the white space that ends the text of an entry that joins lines),
// so the run the cut went through is the marker's.
func cutDrops(s leakSlot) bool {
	if !s.cut {
		return false
	}
	read := s.read
	if s.e.line {
		read = strings.TrimRightFunc(read, unicode.IsSpace) // the lines are joined, and the ends of the text trimmed, before the last run goes
	}
	i := windowIndex(s.m.text, read)
	return i < 0 || !strings.ContainsFunc(read[i+markerWindow:], unicode.IsSpace)
}

// swallow is the run that the value of the field before a marker's takes, when the value is only a mark: where it ends, the rules
// that read the marker of the next field whatever it took, and how an entry that joins lines reads what is glued to it.
type swallow struct {
	kind       string // tag, continuation, scheme, empty or token (the token layer takes the first word after an OAuth name)
	end        int
	rescue     func(s leakSlot, end int) bool
	absorb     func(text string, end, pos int) bool
	tokenFirst bool // the token layer made the replacement, before the rules that read names: they take what is glued to it
}

// swallowOf returns the run that the field before the marker's takes when it ends inside the marker's field, and whether there is one.
func swallowOf(s leakSlot) (swallow, bool) { return swallowAt(s, s.m.atom, s.m.pos) }

// swallowAt is swallowOf for the field idx of the case, with the marker at pos. A field whose own name was taken by the run of the
// field before it has no value to take a run with.
func swallowAt(s leakSlot, idx, pos int) (swallow, bool) {
	c := s.c
	pi := prevIndex(c, leakMarker{atom: idx})
	if pi < 0 {
		return swallow{}, false
	}
	p, a, from := c.atoms[pi], c.atoms[idx], c.ends[pi]
	// A name that the run of the field before it took is no name. The passes run in order, the token layer, the rules that read names,
	// then the rule of flags: a run of a later pass takes nothing from an earlier one, and the token layer reads the name of an OAuth
	// field with no value before the rules that read names take anything. In "password: !x, api_key: v" the tag takes "api_key:", so
	// that name takes no run and v is shown; in "password: !x, access_token: v" the token layer read "access_token: v" first, and v is
	// hidden.
	if before, eaten := swallowAt(s, pi, c.starts[pi]); eaten && before.kind != "flag" && (before.kind == "token" || p.class != "oauth" || p.form != "empty" || p.op == "=" || !namesRead(p)) {
		return swallow{}, false
	}
	var sw swallow
	end := -1
	switch p.form {
	case "empty":
		switch {
		case !s.e.ident && p.style == "flag" && p.op != "=": // a flag's value is not another flag
			// The rules that read names ran before the rule of flags, and read the field's marker unless the field is a flag too.
			sw = swallow{kind: "flag", rescue: func(s leakSlot, end int) bool {
				a := s.c.atoms[s.m.atom]
				return a.style != "flag" && a.style != "userflag" || !atSignEnds(a) && namesRescue(s, end)
			}, absorb: namesAbsorb}
			end = flagReach(c.text, from, s.e.line, atSignEnds(a) && strings.HasPrefix(a.text, "-uadmin:"))
		case p.class == "oauth" && p.op != "=" && namesRead(p): // the token layer takes the word after "name: "
			sw = swallow{kind: "token", rescue: tokenRescue, absorb: tokenAbsorb, tokenFirst: true}
			end = oauthEmptyReach(c.text, from)
			if opensQuote(c.text, from) {
				sw.absorb = tokenAbsorbQuoted
			}
		case !s.e.ident && namesRead(p) && (p.class == "secret" || p.class == "oauth" && p.op == "="):
			if colonKeyed(a) || c.layout.id == "newline" && a.style == "entry" && a.op == "comma" {
				return swallow{}, false
			}
			sw = swallow{kind: "empty", rescue: namesRescue, absorb: namesAbsorb}
			end = emptyReach(c.text, from)
			if c.layout.id == "newline" && pairCovers(c, idx, pos) {
				end = pos + 1 // the pair on the line below is taken whole, its value with it
			}
		}
	case "baretag", "baretag2", "bang", "gluecomma", "gluesemi", "gluebrace", "amp":
		// The token layer takes a tag as the value of an OAuth name, but not an anchor, which the value does not begin with.
		if !s.e.ident && (p.class == "secret" || p.class == "oauth" && p.form == "amp") && namesRead(p) {
			sw = swallow{kind: "tag", rescue: namesRescue, absorb: namesAbsorb}
			end = tagReach(c.text, from)
		}
	case "baretick", "barecaret":
		if !s.e.ident && p.class == "secret" {
			sw = swallow{kind: "continuation", rescue: namesRescue, absorb: namesAbsorb}
			end = continuationReach(c.text, from)
		}
	case "barebackslash": // a backslash is skipped by the reader of nested keys, which finds the next field's name
		if !s.e.ident && (p.class == "secret" || p.class == "oauth") && !colonKeyed(a) {
			sw = swallow{kind: "continuation", rescue: namesRescue, absorb: namesAbsorb}
			end = continuationReach(c.text, from)
		}
	case "barebasic", "baretoken":
		if !s.e.ident && p.class == "secret" {
			sw = swallow{kind: "scheme", rescue: namesRescue, absorb: namesAbsorb}
			if p.form == "barebasic" { // the token layer reads Basic before the rules that read names see it
				end = basicReach(c.text, from)
				sw.tokenFirst = end >= 0
			}
			if end < 0 && !colonKeyed(a) {
				end = schemeReach(c.text, from)
			}
		}
	}
	if end <= c.starts[idx] {
		return swallow{}, false
	}
	if p.class == "oauth" && sw.kind != "token" && sw.kind != "" {
		sw.absorb = oauthPrevAbsorb // the entries that join the lines run the token layer on the replacement too
	}
	sw.end = end
	return sw, true
}

// leaks reports whether the run leaves the marker: it ends before the marker, nothing else reads the marker, and an entry that joins
// the lines does not take what is glued to the replacement.
func (sw swallow) leaks(s leakSlot) bool {
	c, m := s.c, s.m
	if sw.end > m.pos || sw.rescue(s, sw.end) {
		return false
	}
	if sw.tokenFirst && !s.e.ident && namesAbsorb(c.text, sw.end, m.pos) {
		return false // the name before it reads what is glued to the replacement as its value
	}
	return !s.e.line || !sw.absorb(c.text, sw.end, m.pos)
}

// namesRescue is readsOwn for a run that the rules that read names took: the token layer ran before, and read what it read.
func namesRescue(s leakSlot, end int) bool {
	a := s.c.atoms[s.m.atom]
	if a.style == "entry" && (a.op == "newline" || a.op == "gap") {
		return entryIntact(s.c, s.m.atom, end)
	}
	return readsOwn(a)
}

// tokenRescue says whether a rule other than the token layer's OAuth rule reads the marker after the OAuth rule took the run: a
// Bearer credential, a Basic credential whose scheme word is after the run, a token with a shape of its own, a Kubernetes env entry whose name
// is not in the run.
func tokenRescue(s leakSlot, end int) bool {
	c, m := s.c, s.m
	a := c.atoms[m.atom]
	switch {
	case a.shape, a.style == "entry" && a.op != "comma", userinfoReads(a):
		return true
	case a.scheme == "Bearer": // the Bearer rule is the first of the token layer
		return true
	case a.scheme != "":
		return strings.LastIndex(c.text[:m.pos], a.scheme) >= end
	}
	return false
}

// tokenSwallowed reports whether the field before the marker's has an OAuth name with no value, whose value the token layer took
// from the marker's field: the OAuth name of the marker's field is gone, and its rule does not read it.
func tokenSwallowed(s leakSlot) bool {
	sw, ok := swallowOf(s)
	return ok && sw.kind == "token"
}

// swallowCovers reports whether the run that the field before the marker's takes reaches the marker: the marker is part of that
// value, and hidden.
func swallowCovers(s leakSlot) bool {
	sw, ok := swallowOf(s)
	if !ok {
		return false
	}
	c, m := s.c, s.m
	return sw.end > m.pos || sw.end >= 0 && (sw.tokenFirst && !s.e.ident && namesAbsorb(c.text, sw.end, m.pos) || s.e.line && sw.absorb(c.text, sw.end, m.pos))
}

// namesAbsorb says whether the rules that read names take the text glued to a replacement as part of its value: the run up to white
// space, unless a delimiter (a quote, a comma, a semicolon, a brace, a bracket or a backslash) ends it first.
func namesAbsorb(text string, end, pos int) bool {
	glued := text[end:pos]
	return glued == "" || strings.IndexByte("\"',;}]\\", glued[0]) < 0 && !strings.ContainsFunc(glued, unicode.IsSpace)
}

// tokenAbsorb says the same of the token layer's OAuth rule: the run up to white space, a quote, a comma, a semicolon, a brace, an
// ampersand or a backslash.
func tokenAbsorb(text string, end, pos int) bool {
	return !strings.ContainsAny(text[end:pos], " \t\n\"',;}&\\")
}

// oauthPrevAbsorb is the glue taken after an OAuth name's value on the second read: the token layer cuts the value at a delimiter, and
// the rules that read names stop at one.
func oauthPrevAbsorb(text string, end, pos int) bool {
	return !strings.ContainsAny(text[end:pos], " \t\n\"',;}\\")
}

// tokenAbsorbQuoted is tokenAbsorb for a value that the token layer read inside quotes (the field after the name began with one): the
// closing quote is taken with the replacement by the rules that read names.
func tokenAbsorbQuoted(text string, end, pos int) bool {
	switch {
	case strings.HasPrefix(text[end:], `\"`):
		end += 2
	case strings.HasPrefix(text[end:], `"`), strings.HasPrefix(text[end:], `'`):
		end++
	}
	return tokenAbsorb(text, end, pos)
}

// readsOwn reports whether a rule that does not need the field's name reads its marker: the token layer (an OAuth field whose first
// word is its value, a Bearer or Basic credential, a token with a shape of its own) and a Kubernetes env entry whose "name:" is
// not the first word and a password that an "@" follows (the rule for "user:password@host").
func readsOwn(a *leakAtom) bool {
	switch {
	case a.shape, a.scheme != "":
		return true
	case a.class == "oauth" && a.firstRun && a.form != "backslash":
		return true
	case a.style == "entry" && (a.op == "newline" || a.op == "gap"):
		return true
	case userinfoReads(a):
		return true
	}
	return false
}

// entryIntact reports whether a run that ends at end leaves the "name:" of a Kubernetes env entry that begins the field (after "- ").
func entryIntact(c leakCase, idx, end int) bool { return end <= c.starts[idx]+2 }

// tagReach is the reach of a tag: on the same line, the word after it (tagged).
func tagReach(text string, from int) int {
	i := from
	if i < len(text) && text[i] == ',' { // the join of the layout glues a comma to the tag's word
		i++
	}
	k := skipHSpaceStr(text, i)
	if k == i || k >= len(text) || text[k] == '\n' {
		return -1
	}
	return headerEnd(text, runEnd(text, k), k)
}

// headerEnd is the end of the reach of a word that starts at k and ends at end: the end of the line, if the word is the header of a
// block scalar (a bar), which takes the rest of its line.
func headerEnd(text string, end, k int) int {
	if t := text[k:end]; (t == "|" || t == ">") && end < len(text) {
		if j := strings.IndexByte(text[end:], '\n'); j >= 0 {
			return end + j
		}
		return len(text)
	}
	if t := text[k:end]; t == "|" || t == ">" {
		return len(text)
	}
	return end
}

// continuationReach is the reach of a continuation mark that ends its word: the first word after it, on the next line too.
func continuationReach(text string, from int) int {
	if from >= len(text) || text[from] == ',' {
		return -1 // the mark is glued to a comma, and is no mark
	}
	k := skipHSpaceStr(text, from)
	if k < len(text) && text[k] == '\n' {
		k = skipHSpaceStr(text, k+1)
	} else if k == from {
		return -1
	}
	if k >= len(text) || text[k] == '\n' {
		return -1
	}
	return runEnd(text, k)
}

// schemeReach is the reach of an authorization scheme with white space after it on its line: the word after the white space.
func schemeReach(text string, from int) int {
	k := skipHSpaceStr(text, from)
	if k == from || k >= len(text) || text[k] == '\n' {
		return -1
	}
	return plainEnd(text, k)
}

// modelPlainWord is a word with at most a leading capital, which the Basic rule keeps (a copy of the rule's, so that the model does not
// call the code it models).
var modelPlainWord = regexp.MustCompile(`^[A-Z]?[a-z]+$`)

// basicReach is the reach of the word Basic for the token layer, which takes the base64 characters after it (eight or more, with
// up to two equals signs, and no plain word) after any white space and colons, line breaks among them.
func basicReach(text string, from int) int {
	k := from
	for k < len(text) && strings.IndexByte(" \t\n:", text[k]) >= 0 {
		k++
	}
	if k == from {
		return -1
	}
	j := k
	for j < len(text) && (text[j] >= 'a' && text[j] <= 'z' || text[j] >= 'A' && text[j] <= 'Z' || text[j] >= '0' && text[j] <= '9' || text[j] == '+' || text[j] == '/') {
		j++
	}
	if j-k < 8 {
		return -1
	}
	for e := 0; e < 2 && j < len(text) && text[j] == '='; e++ {
		j++
	}
	if modelPlainWord.MatchString(text[k:j]) {
		return -1
	}
	return j
}

// flagReach is the reach of a flag with no value: on its line, the word after the white space, unless that is another flag. The rule for
// "user:password@host" runs first and has replaced a glued user flag's "-uadmin:x" before the "@" by then, so the word is no flag: dashOK.
func flagReach(text string, from int, joined, dashOK bool) int {
	k := skipHSpaceStr(text, from)
	if joined { // the entries that join the lines read a line break as a space the second time
		for k < len(text) && unicode.IsSpace(rune(text[k])) {
			k++
		}
	}
	if k == from || k >= len(text) || text[k] == '\n' || text[k] == '-' && !dashOK {
		return -1
	}
	return runEnd(text, k)
}

// emptyReach is the reach of a name with no value: the first word after the white space, line breaks among it.
func emptyReach(text string, from int) int {
	k := from
	for k < len(text) && unicode.IsSpace(rune(text[k])) {
		k++
	}
	if k >= len(text) {
		return -1
	}
	return headerEnd(text, runEnd(text, k), k)
}

// oauthEmptyReach is the reach of an OAuth name with no value for the token layer: after white space and a quote, the run up to white
// space, a quote, a comma, a semicolon, a brace, an ampersand or a backslash.
func oauthEmptyReach(text string, from int) int {
	k := from
	for k < len(text) && unicode.IsSpace(rune(text[k])) {
		k++
	}
	for k < len(text) && text[k] == '\\' {
		k++
	}
	if k < len(text) && (text[k] == '"' || text[k] == '\'') {
		k++
	}
	j := k
	for j < len(text) && !unicode.IsSpace(rune(text[j])) && strings.IndexByte("\"',;}\\&", text[j]) < 0 {
		j++
	}
	if j == k {
		return -1
	}
	return j
}

// opensQuote reports whether the word after the white space that follows from opens with a quote, which the token layer reads past.
func opensQuote(text string, from int) bool {
	k := from
	for k < len(text) && unicode.IsSpace(rune(text[k])) {
		k++
	}
	for k < len(text) && text[k] == '\\' {
		k++
	}
	return k < len(text) && (text[k] == '"' || text[k] == '\'')
}

// pairCovers reports whether the field is a "name = value" pair on the line after a name with no value, whose value the rules take
// with the name above it: the marker is in the value's first word.
func pairCovers(c leakCase, idx, pos int) bool {
	a := c.atoms[idx]
	if a.style != "plain" && a.style != "env" || a.op != "=" && a.op != " = " && a.op != " => " {
		return false
	}
	at := strings.Index(c.text[c.starts[idx]:], a.op)
	if at < 0 {
		return false
	}
	v := c.starts[idx] + at + len(a.op)
	if quoted := v < len(c.text) && (c.text[v] == '"' || c.text[v] == '\''); a.class == "oauth" && !quoted {
		return false // the OAuth value is a replacement by then, and the pair's value a word of it, unless it is quoted
	}
	return runEnd(c.text, v) > pos
}

// runEnd is where the word that starts at i ends, as the rules read a value: a quoted string, or a run up to white space; an
// authorization scheme word takes the word after it.
func runEnd(text string, i int) int {
	j := plainEnd(text, i)
	switch strings.ToLower(text[i:j]) {
	case "bearer", "basic", "token", "digest":
		k := skipHSpaceStr(text, j)
		if k > j && k < len(text) && text[k] != '\n' && !secretKeyAt(text, k) { // before a secret key the scheme stands alone
			return plainEnd(text, k)
		}
	}
	return j
}

// secretNames are the names the generator gives its secret and OAuth fields, and the names of the headers and cookies.
var secretNames = func() map[string]bool {
	out := map[string]bool{"Cookie": true, "Set-Cookie": true}
	for _, tables := range []map[string][]string{keyNames, envNames, flagNames, headerNames} {
		for _, class := range []string{"secret", "oauth"} {
			for _, n := range tables[class] {
				out[n] = true
			}
		}
	}
	return out
}()

// secretKeyAt reports whether the word at i is a secret name and a colon, or a quoted one: a key to the reader of nested keys.
func secretKeyAt(text string, i int) bool {
	end := plainEnd(text, i)
	w := strings.Trim(text[i:end], `"'\`)
	if strings.HasSuffix(text[i:end], ":") && secretNames[strings.TrimSuffix(w, ":")] {
		return true
	}
	return end < len(text) && (text[end] == ':' || strings.HasPrefix(text[end:], `\":`)) && secretNames[w]
}

// plainEnd is runEnd without the scheme words.
func plainEnd(text string, i int) int {
	if i >= len(text) {
		return len(text)
	}
	switch {
	case strings.HasPrefix(text[i:], `\"`):
		if k := strings.Index(text[i+2:], `\"`); k >= 0 {
			return i + 2 + k + 2
		}
		return len(text)
	case text[i] == '"':
		for j := i + 1; j < len(text); j++ {
			switch text[j] {
			case '\\':
				j++
			case '"':
				return j + 1
			}
		}
		return len(text)
	case text[i] == '\'':
		if k := strings.IndexByte(text[i+1:], '\''); k >= 0 {
			return i + 1 + k + 1
		}
		return len(text)
	}
	j := i
	for j < len(text) && !unicode.IsSpace(rune(text[j])) {
		j++
	}
	return j
}

// skipHSpaceStr returns the first index of text at or after i that is not a space or a tab.
func skipHSpaceStr(text string, i int) int {
	for i < len(text) && (text[i] == ' ' || text[i] == '\t') {
		i++
	}
	return i
}

// tagLike reports whether the value form is only a tag or an anchor, possibly glued to a delimiter.
func tagLike(form string) bool {
	switch form {
	case "baretag", "baretag2", "bang", "gluecomma", "gluesemi", "gluebrace", "amp":
		return true
	}
	return false
}

// atSignEnds reports whether a field is a user flag whose password holds an "@" before the marker: the rule for "user:password@host"
// takes the user and what comes before the "@" and leaves the user-flag rule nothing to read, so the rule of flags is the only one
// left to take the marker, and the run of a flag with no value before the field takes it.
func atSignEnds(a *leakAtom) bool {
	return a.style == "userflag" && !a.lineOnly && strings.Contains(a.text, "@{0}")
}

// userinfoReads reports whether the rule for "user:password@host" reads the marker of a user flag, whatever the flag: the password
// is followed by an "@".
func userinfoReads(a *leakAtom) bool {
	return a.style == "userflag" && strings.Contains(a.text, "{0}@")
}
