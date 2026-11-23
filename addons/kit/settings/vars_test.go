package settings

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The Vars of a fake add-on: a required ID, a duration, a choice and a variable that depends on it.
var (
	idVar      = Required("ADDON_ID")
	timeoutVar = Duration("ADDON_TIMEOUT", "10m")
	modeVar    = Func("ADDON_MODE", "fast", func(v string, _ Values) (string, error) {
		if v != "fast" && v != "slow" {
			return "", fmt.Errorf("ADDON_MODE %q is not fast or slow", v)
		}
		return v, nil
	})
	slownessVar = Func("ADDON_SLOWNESS", "", func(v string, earlier Values) (string, error) {
		if modeVar.From(earlier) == "slow" && v == "" {
			return "", fmt.Errorf("ADDON_SLOWNESS is required with ADDON_MODE=slow")
		}
		return v, nil
	})
)

// addonSpec is the token spec with the fake add-on's Vars.
func addonSpec() Spec {
	s := tokenSpec
	s.Vars = []Var{idVar, timeoutVar, modeVar, slownessVar}
	return s
}

func TestVars(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      map[string]string
		id, mode string
		timeout  time.Duration
		slowness string
	}{
		{"defaults", map[string]string{"ADDON_ID": "a1"}, "a1", "fast", 10 * time.Minute, ""},
		{"every one", map[string]string{"ADDON_ID": "a1", "ADDON_TIMEOUT": "0", "ADDON_MODE": "slow", "ADDON_SLOWNESS": "9"}, "a1", "slow", 0, "9"},
		{"an empty value is unset", map[string]string{"ADDON_ID": "a1", "ADDON_TIMEOUT": "", "ADDON_MODE": ""}, "a1", "fast", 10 * time.Minute, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, values, err := Parse(env(tc.env), addonSpec())
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got := idVar.From(values); got != tc.id {
				t.Errorf("ADDON_ID = %q, want %q", got, tc.id)
			}
			if got := timeoutVar.From(values); got != tc.timeout {
				t.Errorf("ADDON_TIMEOUT = %s, want %s", got, tc.timeout)
			}
			if got := modeVar.From(values); got != tc.mode {
				t.Errorf("ADDON_MODE = %q, want %q", got, tc.mode)
			}
			if got := slownessVar.From(values); got != tc.slowness {
				t.Errorf("ADDON_SLOWNESS = %q, want %q", got, tc.slowness)
			}
		})
	}
}

// Every problem is reported, the add-on's own first, in the order of Spec.Vars; a Var after one that failed reads it as
// its zero value.
func TestVarsRefused(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"required", map[string]string{}, "ADDON_ID is required"},
		{"duration negative", map[string]string{"ADDON_ID": "a1", "ADDON_TIMEOUT": "-5s"}, `ADDON_TIMEOUT "-5s" is not a duration of zero or more`},
		{"duration not one", map[string]string{"ADDON_ID": "a1", "ADDON_TIMEOUT": "5"}, `ADDON_TIMEOUT "5" is not a Go duration such as 90s or 720h`},
		{"depends on an earlier one", map[string]string{"ADDON_ID": "a1", "ADDON_MODE": "slow"}, "ADDON_SLOWNESS is required with ADDON_MODE=slow"},
		{"an earlier one failed", map[string]string{"ADDON_ID": "a1", "ADDON_MODE": "warp"}, `ADDON_MODE "warp" is not fast or slow`},
		{"own first, then shared", map[string]string{"ADDON_TIMEOUT": "x", "LISTEN_ADDR": "8090", "LOG_LEVEL": "loud"},
			"ADDON_ID is required\n" + `ADDON_TIMEOUT "x" is not a Go duration such as 90s or 720h` + "\n" +
				`LISTEN_ADDR "8090" is not host:port` + "\n" + `LOG_LEVEL "loud" is not debug, info, warn or error`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, values, err := Parse(env(tc.env), addonSpec())
			if err == nil {
				t.Fatal("Parse accepted it")
			}
			if err.Error() != tc.want {
				t.Errorf("error =\n%s\nwant\n%s", err, tc.want)
			}
			if idVar.From(values) != "" {
				t.Error("a refused Parse returned values")
			}
		})
	}
}

// A Var may not name a shared variable or another Var's.
func TestVarsDeclaredOnce(t *testing.T) {
	for name, vars := range map[string][]Var{
		"shared":  {Required("LISTEN_ADDR")},
		"oidc":    {Required("OIDC_SUBJECT")},
		"lease":   {Duration("LEASE_DURATION", "15s")},
		"twice":   {idVar, Required("ADDON_ID")},
		"no func": {Of[string]{key: "ADDON_ID"}},
		"no name": {idVar, Required("")},
		"zero":    {Of[string]{}},
	} {
		t.Run(name, func(t *testing.T) {
			s := tokenSpec
			s.Vars = vars
			_, _, err := Parse(env(map[string]string{"ADDON_ID": "a1"}), s)
			if err == nil || !strings.HasPrefix(err.Error(), "settings: ") {
				t.Fatalf("Parse = %v, want a settings: error", err)
			}
		})
	}
}

// From is the zero value for a Var that Parse did not read, and Declared tells it from one that Parse read.
func TestFromUndeclared(t *testing.T) {
	s := tokenSpec
	s.Vars = []Var{idVar, modeVar}
	_, values, err := Parse(env(map[string]string{"ADDON_ID": "a1"}), s)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := timeoutVar.From(values); got != 0 {
		t.Fatalf("From = %s, want 0", got)
	}
	if timeoutVar.Declared(values) {
		t.Error("Declared = true for a Var not in Spec.Vars")
	}
	if !idVar.Declared(values) || !modeVar.Declared(values) {
		t.Error("Declared = false for a Var in Spec.Vars")
	}
	if got := idVar.From(Values{}); got != "" || idVar.Declared(Values{}) {
		t.Fatalf("zero Values: From = %q, Declared = %v", got, idVar.Declared(Values{}))
	}
}

// The refusal of a Var with no name says which one it is.
func TestVarWithNoName(t *testing.T) {
	s := tokenSpec
	s.Vars = []Var{idVar, Duration("", "1s")}
	_, _, err := Parse(env(map[string]string{"ADDON_ID": "a1"}), s)
	if want := "settings: the add-on's variable 1 (counting from 0) has no name"; err == nil || err.Error() != want {
		t.Fatalf("Parse = %v, want %s", err, want)
	}
}
