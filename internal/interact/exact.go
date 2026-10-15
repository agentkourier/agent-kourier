package interact

import (
	"encoding/json"
	"fmt"
	"math"
	"unicode"
	"unicode/utf8"

	"github.com/agentkourier/agent-kourier/internal/audit"
	"github.com/agentkourier/agent-kourier/internal/dialect"
	"github.com/agentkourier/agent-kourier/internal/redact"
	"github.com/agentkourier/agent-kourier/internal/render"
)

// The refusals of a tool approval whose prompt would show something other than what the tool will run. Their texts
// are fixed: they hold nothing the agent wrote, and they reach the audit log, which keeps the first 120 bytes of a
// reason, so each is short and says its own reason first.
var (
	errToolCut     = fmt.Errorf("%w: a tool's name or arguments are longer than can be shown whole", dialect.ErrUnusablePause)
	errToolControl = fmt.Errorf("%w: a tool's name or arguments hold a control character or invalid UTF-8", dialect.ErrUnusablePause)
	errToolKey     = fmt.Errorf("%w: a tool's argument key would be changed by cleaning", dialect.ErrUnusablePause)
	errToolName    = fmt.Errorf("%w: a tool's name would be changed by cleaning", dialect.ErrUnusablePause)
	errToolNameLog = fmt.Errorf("%w: a tool's name cannot be written whole in the audit log", dialect.ErrUnusablePause)
	errToolNameLen = fmt.Errorf("%w: a tool's name is longer than the audit log writes whole", dialect.ErrUnusablePause)
	errToolNumber  = fmt.Errorf("%w: a tool's arguments hold a number that cannot be shown exactly", dialect.ErrUnusablePause)
	errToolKind    = fmt.Errorf("%w: a tool's arguments hold a value that cannot be shown exactly", dialect.ErrUnusablePause)
)

// exactTools says whether the prompt of an approval can show every tool call exactly as it will run. A person must
// never be asked to approve something other than what will run (spec 8.4), so a pause that fails this is refused as
// unusable, before anything is recorded, and is never shown cut or changed.
//
// What the prompt shows of a tool call is its name and arguments as the agent sent them, except that secrets are
// replaced by the redaction's marker, the characters that redact.Normalize deletes (zero-width and bidirectional
// controls, variation selectors and the rest of the set it names) are not shown in a name or a value, every line end is
// shown as LF, and a number is shown as the nearest float64, which for an integer below 2^53 is itself. Everything else
// that cannot be shown is refused. It reads the pause as parsed, before the redaction, and refuses, at any depth of a
// map or a list:
//
//   - a tool name, an argument key or an argument string that the redaction would cut (errToolCut). The answer is
//     the redaction's own (redact.FreeTextWhole); it is not inferred from the length or the end of the result,
//     because the redaction lengthens text before it bounds it, and a rule can take the mark of a cut.
//   - a control character (Unicode Cc) other than tab, LF and CR, or text that is not valid UTF-8, in a name, a key or
//     a string (errToolControl). It has no honest use, the cleaning deletes it silently, and it can mean something else
//     to the tool.
//   - an argument key that the redaction changes in any way (errToolKey): its invisible characters, its white space
//     (cut at the ends, runs and line breaks written as one space), a secret in it. A key is an identifier and not
//     prose. This also refuses two keys that come to the same text, which the redaction would keep one of, in the
//     order of a Go map.
//   - a tool name that cleaning changes in any way (errToolName): the redaction of a stored name (storable) rewrites a
//     credential-shaped word whole, so that "BearerTokenValidator" would be shown, stored and audited as "[redacted]" and
//     two such tools would read alike, and it deletes invisible format characters and writes every line end as LF. A name is
//     an identifier and not prose; one that cleaning leaves as it is may be in any script.
//   - a tool name that the audit row of its decision would not write as it is (errToolNameLog, errToolNameLen): the row
//     is the only record of what was approved once the stored request is cleared, so every tool an approver can decide is
//     named whole in it (audit.ToolNameFit). It cuts a name past 302 bytes quoted, so the longest allowed name is 300 ASCII
//     characters, or 150 of two bytes, 100 of three (CJK), 75 emoji, 150 backslashes or quotes, or 50 unassigned characters;
//     and it rewrites a name that ends as a credential does ("Bearer:", "id_token=") or that cleaning as an identifier
//     changes (white space other than single spaces inside it).
//   - a number that a float64 cannot be trusted to hold as written (errToolNumber): an integer-valued one of magnitude
//     2^53 or more (arguments arrive as float64, so 2^53+1 reaches the prompt as 2^53, while the agent's tool acts
//     on its own copy), and NaN and the infinities. A decimal is the nearest float64 to what was sent, and 1.2e-7 is
//     the same value as 0.00000012; an integer below 2^53 is written in full, without an exponent. Digits kept as
//     sent, through the dialect and the stored request, would lift the limit, and are not done.
//   - a value of any kind other than the ones named here (errToolKind): the redaction does not look into it.
//
// The hint and the sub-agent's name are not checked: they say who is asking and why, not what runs, so the prompt may
// show them cut, as the stored request keeps them. An ending shows what the stored request holds, so it is never more.
func exactTools(tools []dialect.Tool) error {
	for _, t := range tools {
		if err := exactName(t.Name); err != nil {
			return err
		}
		if err := exactValue(t.Args); err != nil {
			return err
		}
	}
	return nil
}

// exactName checks a tool name: it is exact text, it is what storable keeps of it (so that the prompt, the stored request and
// the audit row name the tool as the agent did), and the audit row of a decision writes it whole.
func exactName(name string) error {
	if err := exactText(name); err != nil {
		return err
	}
	if redact.FreeText(name) != name {
		return errToolName
	}
	switch audit.ToolNameFit(name) {
	case audit.NameTooLong:
		return errToolNameLen
	case audit.NameRewritten:
		return errToolNameLog
	}
	return nil
}

// exactText checks a tool name or a string of its arguments: no control character, and nothing cut.
func exactText(s string) error {
	if !plainText(s) {
		return errToolControl
	}
	if _, whole := redact.FreeTextWhole(s); !whole {
		return errToolCut
	}
	return nil
}

// plainText reports whether s is valid UTF-8 without a control character other than tab, LF and CR.
func plainText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.Is(unicode.Cc, r) && r != '\t' && r != '\n' && r != '\r' {
			return false
		}
	}
	return true
}

// exactValue checks one value of a tool's arguments and, in a map or list, everything under it.
func exactValue(v any) error {
	switch t := v.(type) {
	case nil, bool, json.Number, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return nil
	case string:
		return exactText(t)
	case float64:
		return exactNumber(t)
	case float32:
		return exactNumber(float64(t))
	case []any:
		for _, x := range t {
			if err := exactValue(x); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		return exactMap(t)
	}
	return errToolKind
}

// exactMap checks the keys of a map and what is under them. The keys are put through the redaction itself, so that
// this cannot drift from what it does: a map of the same keys with nothing under them must come out with every key
// as it went in.
func exactMap(m map[string]any) error {
	same := make(map[string]any, len(m))
	for k, x := range m {
		if !plainText(k) {
			return errToolControl
		}
		if len(k) > redact.MaxBytes {
			return errToolCut
		}
		same[k] = nil
		if err := exactValue(x); err != nil {
			return err
		}
	}
	cleaned := render.RedactArgs(same)
	for k := range m {
		if _, ok := cleaned[k]; !ok {
			return errToolKey
		}
	}
	return nil
}

// exactNumber is nil for a number that the prompt shows as it was sent: not NaN or infinite, and either not a whole
// number or one below 2^53, where float64 holds every integer.
func exactNumber(f float64) error {
	if math.IsNaN(f) || math.IsInf(f, 0) || (f == math.Trunc(f) && math.Abs(f) >= 1<<53) {
		return errToolNumber
	}
	return nil
}
