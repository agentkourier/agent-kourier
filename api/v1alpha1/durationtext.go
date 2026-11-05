package v1alpha1

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"

	kjson "sigs.k8s.io/json"
)

// maxDurationTextLen is the most characters the text of a duration may have: a real one is a few ("2160h", "1h30m"), and
// the schema's pattern lets through any run of numbers and units, so without a cap the text of one is as long as the
// object may be ("0h0h0h" and on). The cap is a maxLength in the schema and DurationTextProblems on the file and the custom
// resource route. It counts characters, as the schema does, and not bytes, so the schema is never the stricter of the two.
const maxDurationTextLen = 64

// durationText holds the text of each duration field of a Binding, as the JSON has it. Its field names are the JSON
// names, which TestDurationFieldsAreEveryDuration holds to the fields that have the type metav1.Duration.
type durationText struct {
	Spec struct {
		Interactions struct {
			Timeout json.RawMessage `json:"timeout"`
		} `json:"interactions"`
		Session struct {
			ThreadTTL json.RawMessage `json:"threadTTL"`
		} `json:"session"`
		Chat struct {
			Triggers []struct {
				Thread struct {
					Cooldown json.RawMessage `json:"cooldown"`
					MaxIdle  json.RawMessage `json:"maxIdle"`
				} `json:"thread"`
				Limits struct {
					RateLimit *struct {
						Per json.RawMessage `json:"per"`
					} `json:"rateLimit"`
				} `json:"limits"`
			} `json:"triggers"`
		} `json:"chat"`
	} `json:"spec"`
}

// DurationTextProblems returns a problem, as "path: message", for each duration of the Binding in j, a JSON document, whose
// text is longer than maxDurationTextLen characters. A Duration is decoded before Validate sees it and its text is gone
// by then, so the decoders of both routes call this on the document first. The problem shows no value.
//
// It reads only what is there to read: a field of the wrong type, a value that is not a string and a document that is not
// a Binding give no problem here, since the decode reports them. Field names are matched by case, as the decode matches
// them, so a field spelled in another case is an unknown field and not a duration.
func DurationTextProblems(j []byte) []error {
	var d durationText
	_ = kjson.UnmarshalCaseSensitivePreserveInts(j, &d) // a type error still fills in what it can; the decode reports it
	var out []error
	check := func(path string, raw json.RawMessage) {
		var text *string
		if json.Unmarshal(raw, &text) != nil || text == nil {
			return
		}
		if n := utf8.RuneCountInString(*text); n > maxDurationTextLen {
			out = append(out, fmt.Errorf("%s: is %d characters, longer than the %d character limit", path, n, maxDurationTextLen))
		}
	}
	check("spec.interactions.timeout", d.Spec.Interactions.Timeout)
	check("spec.session.threadTTL", d.Spec.Session.ThreadTTL)
	for i, t := range d.Spec.Chat.Triggers {
		at := fmt.Sprintf("spec.chat.triggers[%d]", i)
		check(at+".thread.cooldown", t.Thread.Cooldown)
		check(at+".thread.maxIdle", t.Thread.MaxIdle)
		if t.Limits.RateLimit != nil {
			check(at+".limits.rateLimit.per", t.Limits.RateLimit.Per)
		}
	}
	return out
}
