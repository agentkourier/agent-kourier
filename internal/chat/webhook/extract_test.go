package webhook

import (
	"encoding/json"
	"errors"
	"maps"
	"strconv"
	"strings"
	"testing"
)

func ptrs(t *testing.T, raws ...string) []pointer {
	t.Helper()
	var out []pointer
	for _, r := range raws {
		p, err := parsePointer(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

// texts is the text of each value, by pointer.
func texts(vals map[string]value) map[string]string {
	out := map[string]string{}
	for k, v := range vals {
		out[k] = v.text
	}
	return out
}

// The extractor resolves each pointer to its value, and refuses what is outside its bounds with the error that names why.
func TestExtract(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		ptrs []string
		want map[string]string
		err  error
	}{
		{"a string decodes", `{"a":"xA\n"}`, []string{"/a"}, map[string]string{"/a": "xA\n"}, nil},
		{"a number is its literal", `{"a":42,"b":42.0,"c":1.5e3,"d":12345678901234567890}`, []string{"/a", "/b", "/c", "/d"},
			map[string]string{"/a": "42", "/b": "42.0", "/c": "1.5e3", "/d": "12345678901234567890"}, nil},
		{"booleans are words", `{"a":true,"b":false}`, []string{"/a", "/b"}, map[string]string{"/a": "true", "/b": "false"}, nil},
		{"null, an object and an array do not resolve", `{"a":null,"b":{},"c":[1]}`, []string{"/a", "/b", "/c"}, map[string]string{}, nil},
		{"nested", `{"a":{"b":{"c":"d"}}}`, []string{"/a/b/c"}, map[string]string{"/a/b/c": "d"}, nil},
		{"escapes ~0 and ~1", `{"a/b":"1","m~n":"2"}`, []string{"/a~1b", "/m~0n"}, map[string]string{"/a~1b": "1", "/m~0n": "2"}, nil},
		{"array index", `{"a":["x","y"]}`, []string{"/a/0", "/a/1", "/a/2", "/a/01", "/a/-"}, map[string]string{"/a/0": "x", "/a/1": "y"}, nil},
		{"an absent key does not resolve", `{"a":1}`, []string{"/b"}, map[string]string{}, nil},
		{"a duplicate on a wanted path is refused", `{"issue":"A","issue":"B"}`, []string{"/issue"}, nil, errDuplicateKey},
		{"a duplicate at an ancestor is refused", `{"a":{"b":1},"a":{"c":2}}`, []string{"/a/b"}, nil, errDuplicateKey},
		{"a duplicate after the first hit is refused", `{"a":1,"z":0,"a":2}`, []string{"/a"}, nil, errDuplicateKey},
		{"a duplicate in an unused first-of alternative is refused", `{"x":1,"y":1,"y":2}`, []string{"/x", "/y"}, nil, errDuplicateKey},
		{"a duplicate elsewhere is allowed", `{"a":1,"o":{"k":1,"k":2},"z":3,"z":4}`, []string{"/a"}, map[string]string{"/a": "1"}, nil},
		{"a root that is not an object", `["a"]`, []string{"/0"}, nil, errSchema},
		{"invalid JSON", `{"a":`, []string{"/a"}, nil, errSchema},
		{"trailing data", `{"a":1} {"a":2}`, []string{"/a"}, nil, errSchema},
		{"invalid UTF-8", "{\"a\":\"\xff\"}", []string{"/a"}, nil, errSchema},
		{"a lone high surrogate", `{"a":"\ud800"}`, []string{"/a"}, nil, errSchema},
		{"a lone low surrogate", `{"a":"\udc00x"}`, []string{"/a"}, nil, errSchema},
		{"a pair of surrogates is taken", `{"a":"😀"}`, []string{"/a"}, map[string]string{"/a": "😀"}, nil},
		{"an escaped backslash before u is no escape", `{"a":"\\ud800"}`, []string{"/a"}, map[string]string{"/a": `\ud800`}, nil},
		{"too deep", `{"a":` + strings.Repeat("[", maxDepth) + strings.Repeat("]", maxDepth) + `}`, []string{"/a"}, nil, errDepth},
		{"as deep as allowed", `{"a":` + strings.Repeat("[", maxDepth-1) + strings.Repeat("]", maxDepth-1) + `}`, []string{"/b"}, map[string]string{}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := extract([]byte(tc.body), ptrs(t, tc.ptrs...))
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("extract = %v, %v; want %v", got, err, tc.err)
				}
				return
			}
			if err != nil || !maps.Equal(texts(got), tc.want) {
				t.Fatalf("extract = %v, %v; want %v", texts(got), err, tc.want)
			}
		})
	}
}

// A number is marked as one, which the body timestamp requires; a string of digits is not.
func TestExtractMarksNumbers(t *testing.T) {
	got, err := extract([]byte(`{"n":17,"s":"17","b":true}`), ptrs(t, "/n", "/s", "/b"))
	if err != nil {
		t.Fatal(err)
	}
	if !got["/n"].number || got["/s"].number || got["/b"].number {
		t.Errorf("extract = %+v; want /n alone a number", got)
	}
}

func TestPointersThatAreRefused(t *testing.T) {
	for _, s := range []string{"", "a", "/a~", "/a~2", "/" + strings.Repeat("a", maxPointerLen)} {
		if _, err := parsePointer(s); err == nil {
			t.Errorf("parsePointer(%q) = nil; want an error", s)
		}
	}
	if _, err := parsePointer("/" + strings.Repeat("a", maxPointerLen-1)); err != nil {
		t.Errorf("a pointer of %d bytes: %v", maxPointerLen, err)
	}
}

// FuzzExtract: no panic, an error is always one of the three refusals, and on what it takes it agrees with encoding/json and a
// reference pointer walk.
func FuzzExtract(f *testing.F) {
	for _, s := range []string{`{"a":1}`, `{"a":{"b":[1,"x",null]}}`, `{"a":"😀"}`, `{"a":1,"a":2}`, `[]`, `{"a":{"b":[[[[1]]]]}}`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, body string) {
		ps := ptrs(t, "/a", "/a/b", "/a/b/0", "/a/b/1")
		got, err := extract([]byte(body), ps)
		if err != nil {
			if !errors.Is(err, errSchema) && !errors.Is(err, errDepth) && !errors.Is(err, errDuplicateKey) {
				t.Fatalf("extract(%q) = %v, not one of its refusals", body, err)
			}
			return
		}
		dec := json.NewDecoder(strings.NewReader(body))
		dec.UseNumber()
		var v any
		if dec.Decode(&v) != nil {
			t.Fatalf("extract took %q, which encoding/json refuses", body)
		}
		for _, p := range ps {
			want, ok := walk(v, p.tokens)
			// encoding/json keeps the last of a duplicate; the extractor refuses one on a wanted path, and takes one elsewhere.
			if g, gok := got[p.raw]; ok != gok || g.text != want {
				t.Fatalf("%s of %q = %q, %v; the reference walk = %q, %v", p.raw, body, g.text, gok, want, ok)
			}
		}
	})
}

func walk(v any, tokens []string) (string, bool) {
	for _, tok := range tokens {
		switch x := v.(type) {
		case map[string]any:
			v = x[tok]
		case []any:
			i := -1
			for n := range x {
				if strconv.Itoa(n) == tok {
					i = n
				}
			}
			if i < 0 {
				return "", false
			}
			v = x[i]
		default:
			return "", false
		}
	}
	switch x := v.(type) {
	case string:
		return x, true
	case json.Number:
		return string(x), true
	case bool:
		return strconv.FormatBool(x), true
	}
	return "", false
}
