package webhook

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// maxPointerLen is the longest JSON Pointer the platform takes, in bytes.
const maxPointerLen = 256

// maxDepth is the deepest nesting of objects and arrays a body may have: a deeper one is refused (400, malformed, depth). The body's
// own size is the chat listener's bound, which refuses a larger one (413) before the platform is called.
const maxDepth = 64

// The ways the extractor refuses a body. None quotes anything of it.
var (
	// errSchema is a body that is not one JSON object of valid UTF-8 with no lone surrogate escape.
	errSchema = errors.New("webhook: the body is not one JSON object of valid UTF-8")
	// errDepth is a body nested deeper than maxDepth.
	errDepth = fmt.Errorf("webhook: the body is nested deeper than %d", maxDepth)
	// errDuplicateKey is a body with a key twice in an object on the way to a pointer's target.
	errDuplicateKey = errors.New("webhook: a key on the way to a value the connection reads is in its object twice")
)

// pointer is a parsed JSON Pointer (RFC 6901): its text and its reference tokens, unescaped.
type pointer struct {
	raw    string
	tokens []string
}

// parsePointer parses s, which must start with '/', is not the root, is at most maxPointerLen bytes, and has '~' only as ~0 or ~1.
// ~1 is '/' and ~0 is '~', unescaped in that order (RFC 6901 4).
func parsePointer(s string) (pointer, error) {
	if s == "" || s[0] != '/' || len(s) > maxPointerLen {
		return pointer{}, fmt.Errorf("webhook: %q is not a JSON Pointer of at most %d bytes that starts with '/'", s, maxPointerLen)
	}
	for i := 0; i < len(s); i++ {
		if s[i] == '~' && (i+1 >= len(s) || (s[i+1] != '0' && s[i+1] != '1')) {
			return pointer{}, fmt.Errorf("webhook: %q has a '~' that is not ~0 or ~1", s)
		}
	}
	parts := strings.Split(s[1:], "/")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.ReplaceAll(p, "~1", "/"), "~0", "~")
	}
	return pointer{raw: s, tokens: parts}, nil
}

// value is what a pointer resolved to: a string's decoded text, a number's literal as written, or true or false. number says it
// was a JSON number.
type value struct {
	text   string
	number bool
}

// node is one step of the wanted paths: the keys (or canonical array indexes) that lead on, and the pointers whose target it is.
type node struct {
	children map[string]*node
	targets  []string
}

// trie builds the wanted paths of ptrs.
func trie(ptrs []pointer) *node {
	root := &node{}
	for _, p := range ptrs {
		n := root
		for _, t := range p.tokens {
			if n.children == nil {
				n.children = map[string]*node{}
			}
			c, ok := n.children[t]
			if !ok {
				c = &node{}
				n.children[t] = c
			}
			n = c
		}
		n.targets = append(n.targets, p.raw)
	}
	return root
}

// frame is one object or array the decoder is inside.
type frame struct {
	object bool
	at     *node // the wanted path here, or nil off the wanted paths
	index  int   // the next array index
	key    string
	isKey  bool            // the next token of an object is a key
	seen   map[string]bool // the wanted keys met in this object, for the duplicate check
}

// extract reads body once and returns the value of each of ptrs that resolves, by the pointer's text. It checks first that the body is
// valid UTF-8 with no lone surrogate escape and one valid JSON value, then makes a single json.Decoder.Token pass over the whole of it
// with UseNumber, which never stops early, so a later duplicate is still found. The root must be an object, and no nesting deeper than
// maxDepth. A key met twice in an object on the way to a pointer's target, when it is the next step of that way, is refused; any other
// duplicate is allowed. A string resolves to its decoded value, a number to its literal as written, true and false to those words;
// null, an object or an array does not resolve. The error is errSchema, errDepth or errDuplicateKey.
func extract(body []byte, ptrs []pointer) (map[string]value, error) {
	if !utf8.Valid(body) || !json.Valid(body) || loneSurrogate(body) {
		return nil, errSchema
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	root := trie(ptrs)
	out := map[string]value{}
	var stack []*frame
	first := true
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, errSchema
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			stack = stack[:len(stack)-1]
			if n := len(stack); n > 0 && stack[n-1].object {
				stack[n-1].isKey = true
			}
			continue
		}
		var at *node
		switch {
		case first:
			first = false
			if d, ok := tok.(json.Delim); !ok || d != '{' {
				return nil, errSchema
			}
			at = root
		default:
			top := stack[len(stack)-1]
			if top.object && top.isKey {
				k, _ := tok.(string)
				if top.at != nil {
					if _, wanted := top.at.children[k]; wanted {
						if top.seen[k] {
							return nil, errDuplicateKey
						}
						top.seen[k] = true
					}
				}
				top.key, top.isKey = k, false
				continue
			}
			seg := top.key
			if !top.object {
				seg = strconv.Itoa(top.index)
				top.index++
			} else {
				top.isKey = true
			}
			if top.at != nil {
				at = top.at.children[seg]
			}
		}
		switch v := tok.(type) {
		case json.Delim: // '{' or '['
			if len(stack) >= maxDepth {
				return nil, errDepth
			}
			f := &frame{object: v == '{', at: at, isKey: v == '{'}
			if f.object && at != nil && len(at.children) > 0 {
				f.seen = map[string]bool{}
			}
			stack = append(stack, f)
		case string:
			resolve(out, at, value{text: v})
		case json.Number:
			resolve(out, at, value{text: string(v), number: true})
		case bool:
			resolve(out, at, value{text: strconv.FormatBool(v)})
		}
	}
	return out, nil
}

// resolve records v for every pointer whose target is at.
func resolve(out map[string]value, at *node, v value) {
	if at == nil {
		return
	}
	for _, p := range at.targets {
		out[p] = v
	}
}

// loneSurrogate says whether body, which json.Valid passed, holds a \u escape of a surrogate that is not one half of a pair. Every
// backslash of valid JSON is inside a string, so the scan needs no state of its own.
func loneSurrogate(body []byte) bool {
	pending := false // the escape before was a high surrogate, and this must be its low one
	for i := 0; i < len(body); i++ {
		if body[i] != '\\' {
			if pending {
				return true
			}
			continue
		}
		if i+1 >= len(body) {
			return pending
		}
		if body[i+1] != 'u' || i+5 >= len(body) {
			if pending {
				return true
			}
			i++
			continue
		}
		r, err := strconv.ParseUint(string(body[i+2:i+6]), 16, 32)
		if err != nil {
			return true
		}
		switch {
		case r >= 0xD800 && r <= 0xDBFF:
			if pending {
				return true
			}
			pending = true
		case r >= 0xDC00 && r <= 0xDFFF:
			if !pending {
				return true
			}
			pending = false
		default:
			if pending {
				return true
			}
		}
		i += 5
	}
	return pending
}
