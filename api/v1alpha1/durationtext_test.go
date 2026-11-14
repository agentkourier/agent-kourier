package v1alpha1

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// durationDoc returns the JSON of a Binding that holds text at the path of a duration field, in the form of the paths of
// durationFields: "spec.chat.triggers[].thread.cooldown" puts it in the first trigger.
func durationDoc(t *testing.T, path string, text any) []byte {
	t.Helper()
	segs := strings.Split(path, ".")
	var build func(i int) any
	build = func(i int) any {
		if i == len(segs) {
			return text
		}
		name, inList := strings.CutSuffix(segs[i], "[]")
		var inner any = map[string]any{name: build(i + 1)}
		if inList {
			inner = map[string]any{name: []any{build(i + 1)}}
		}
		return inner
	}
	j, err := json.Marshal(build(0))
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func TestDurationTextProblems(t *testing.T) {
	zeros := func(n int) string { return strings.Repeat("0h", n/2) }
	for _, f := range durationFields {
		t.Run(f.path, func(t *testing.T) {
			for _, tc := range []struct {
				name string
				text any
				want string // the one problem, or empty for none
			}{
				{"at the cap", zeros(maxDurationTextLen), ""},
				{"over the cap", zeros(maxDurationTextLen + 2),
					fmt.Sprintf("is %d characters, longer than the %d character limit", maxDurationTextLen+2, maxDurationTextLen)},
				// A character is counted once, as the schema's maxLength counts it: 64 characters of 65 bytes pass.
				{"64 characters of 65 bytes", strings.Repeat("0", maxDurationTextLen-3) + "1µs", ""},
				{"65 characters of 66 bytes", strings.Repeat("0", maxDurationTextLen-2) + "1µs",
					fmt.Sprintf("is %d characters, longer than the %d character limit", maxDurationTextLen+1, maxDurationTextLen)},
				{"not a string", 3, ""}, // the decode reports the wrong type
				{"null", nil, ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := DurationTextProblems(durationDoc(t, f.path, tc.text))
					var want []string
					if tc.want != "" {
						want = []string{strings.Replace(f.path, "[]", "[0]", 1) + ": " + tc.want}
					}
					var msgs []string
					for _, e := range got {
						msgs = append(msgs, e.Error())
					}
					if !slices.Equal(msgs, want) {
						t.Fatalf("DurationTextProblems = %q, want %q", msgs, want)
					}
				})
			}
		})
	}
}

func TestDurationTextProblemsShowNoValueAndSkipWhatItDoesNotKnow(t *testing.T) {
	j := durationDoc(t, "spec.session.threadTTL", "s3cret"+strings.Repeat("0h", maxDurationTextLen))
	got := DurationTextProblems(j)
	if len(got) != 1 || strings.Contains(got[0].Error(), "s3cret") {
		t.Errorf("DurationTextProblems = %v; want one problem that shows no value", got)
	}
	// A field of another case is an unknown field, which the decode reports or ignores; it is not a duration.
	if got := DurationTextProblems(durationDoc(t, "spec.session.ThreadTTL", strings.Repeat("0h", maxDurationTextLen))); len(got) != 0 {
		t.Errorf("a field of another case was read as threadTTL: %v", got)
	}
	// A document with a field of the wrong type, and one that is not JSON at all, give no problem of their own.
	for _, doc := range []string{`{"spec":{"chat":{"triggers":"x"}}}`, `{"spec":3}`, `not json`, ``} {
		if got := DurationTextProblems([]byte(doc)); len(got) != 0 {
			t.Errorf("DurationTextProblems(%q) = %v; want none", doc, got)
		}
	}
	// Every trigger is read, and each problem names its own index.
	two := []byte(`{"spec":{"chat":{"triggers":[{"thread":{"cooldown":"1h"}},{"thread":{"cooldown":"` + strings.Repeat("0h", maxDurationTextLen) + `"}},{"limits":{"rateLimit":{"per":"` + strings.Repeat("0h", maxDurationTextLen) + `"}}}]}}}`)
	got = DurationTextProblems(two)
	if len(got) != 2 || !strings.HasPrefix(got[0].Error(), "spec.chat.triggers[1].thread.cooldown: ") || !strings.HasPrefix(got[1].Error(), "spec.chat.triggers[2].limits.rateLimit.per: ") {
		t.Errorf("DurationTextProblems = %v; want triggers[1].thread.cooldown and triggers[2].limits.rateLimit.per", got)
	}
}

// TestDurationFieldsAreEveryDuration holds the list of duration fields, which the text cap and the schema tests read, to
// the fields of Binding that have the type metav1.Duration, so that a new one is not left out of either.
func TestDurationFieldsAreEveryDuration(t *testing.T) {
	var found []string
	var walk func(path string, typ reflect.Type)
	walk = func(path string, typ reflect.Type) {
		switch typ.Kind() {
		case reflect.Pointer:
			walk(path, typ.Elem())
		case reflect.Slice:
			walk(path+"[]", typ.Elem())
		case reflect.Map:
			walk(path+".{}", typ.Elem())
		case reflect.Struct:
			if typ == reflect.TypeFor[metav1.Duration]() {
				found = append(found, path)
				return
			}
			for f := range typ.Fields() {
				name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
				if name == "" || name == "-" || name == "metadata" || name == "status" {
					continue
				}
				walk(path+"."+name, f.Type)
			}
		}
	}
	walk("", reflect.TypeFor[Binding]())
	slices.Sort(found)
	var listed []string
	for _, f := range durationFields {
		listed = append(listed, "."+f.path)
	}
	slices.Sort(listed)
	if !slices.Equal(found, listed) {
		t.Errorf("Binding has metav1.Duration fields %v, durationFields lists %v", found, listed)
	}
}
