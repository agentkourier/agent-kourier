package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/agentkourier/agent-kourier/internal/broker"
)

// AGENTKOURIER_CONNECTION_NAMESPACES is the platform's admission list: comma-separated namespaces, each a DNS-1123 label, with
// the spaces around an item trimmed and the empty items and the repeats dropped. Unset, empty or only separators is no list, which
// serves the own connection alone (and with no own connection is refused: configsource_test.go), and any value is refused with the files
// source, which serves the one ChatConnection of its directory.
func TestEnvConfigReadsTheAdmissionList(t *testing.T) {
	const key = "AGENTKOURIER_CONNECTION_NAMESPACES"
	crds := func(value string) env {
		return env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "agent-kourier-system/slack-main", key: value}
	}
	for name, tc := range map[string]struct {
		env  env
		want []string
		bad  string // "" is accepted; otherwise a piece of the wiring error
	}{
		"unset":                               {env{"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "a/b"}, nil, ""},
		"empty":                               {crds(""), nil, ""},
		"one":                                 {crds("team-a"), []string{"team-a"}, ""},
		"several, in order":                   {crds("team-b,team-a,team-c"), []string{"team-b", "team-a", "team-c"}, ""},
		"spaces around items":                 {crds("  team-a , team-b  "), []string{"team-a", "team-b"}, ""},
		"empty items dropped":                 {crds(",team-a,,team-b,"), []string{"team-a", "team-b"}, ""},
		"only separators":                     {crds(" , ,"), nil, ""},
		"duplicates dropped":                  {crds("team-a,team-b,team-a, team-b"), []string{"team-a", "team-b"}, ""},
		"digits and hyphens":                  {crds("0-team-9"), []string{"0-team-9"}, ""},
		"a label of 63 letters":               {crds(strings.Repeat("a", 63)), []string{strings.Repeat("a", 63)}, ""},
		"a label of 64 letters":               {crds("team-a," + strings.Repeat("a", 64)), nil, "item 2"},
		"uppercase":                           {crds("team-a,Team-B"), nil, "item 2"},
		"a wildcard":                          {crds("*"), nil, "item 1"},
		"a wildcard among some":               {crds("team-a,*,team-b"), nil, "item 2"},
		"a slash (namespace/name)":            {crds("team-a/slack"), nil, "item 1"},
		"an underscore":                       {crds("team_a"), nil, "item 1"},
		"a leading hyphen":                    {crds("-team"), nil, "item 1"},
		"a trailing hyphen":                   {crds("team-"), nil, "item 1"},
		"a dot":                               {crds("team.a"), nil, "item 1"},
		"the position counts the empty items": {crds("team-a,,Bad"), nil, "item 3"},
		"files refuses a list":                {env{key: "team-a"}, nil, key + " is set with AGENTKOURIER_CONFIG_SOURCE=files"},
		"files refuses a list that holds nothing": {env{key: " , "}, nil, key + " is set with AGENTKOURIER_CONFIG_SOURCE=files"},
		"files accepts it unset":                  {env{}, nil, ""},
		"files accepts it empty":                  {env{key: ""}, nil, ""},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := loadEnv(tc.env.get)
			if tc.bad != "" {
				if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), tc.bad) {
					t.Errorf("loadEnv = %v, want an ErrWiring saying %q", err, tc.bad)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(c.connectionNamespaces, tc.want) {
				t.Errorf("connectionNamespaces = %q, want %q", c.connectionNamespaces, tc.want)
			}
		})
	}
}

// A bad item is named by its position and never by its value: the variable is one a person may paste a token into, and the other
// items are not the bad one's to show either.
func TestEnvConfigDoesNotEchoTheAdmissionList(t *testing.T) {
	const canary = "xoxb-FAKE-LEAK-CANARY"
	_, err := loadEnv(env{
		"AGENTKOURIER_CONFIG_SOURCE": "crds", "AGENTKOURIER_CHAT_CONNECTION": "a/b",
		"AGENTKOURIER_CONNECTION_NAMESPACES": "team-a, " + canary + " ,team-c",
	}.get)
	if !errors.Is(err, broker.ErrWiring) || !strings.Contains(err.Error(), "item 2") {
		t.Fatalf("loadEnv = %v; want an ErrWiring that names item 2", err)
	}
	for _, shown := range []string{canary, "team-a", "team-c"} {
		if strings.Contains(err.Error(), shown) {
			t.Errorf("the error shows %q: %v", shown, err)
		}
	}
	// The files refusal does not show the value either.
	_, err = loadEnv(env{"AGENTKOURIER_CONNECTION_NAMESPACES": canary}.get)
	if !errors.Is(err, broker.ErrWiring) || strings.Contains(err.Error(), canary) {
		t.Errorf("loadEnv with files = %v; want an ErrWiring that does not show the value", err)
	}
}
