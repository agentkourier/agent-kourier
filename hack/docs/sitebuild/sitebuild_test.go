package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestParseVersions(t *testing.T) {
	tests := []struct {
		name    string
		data    string
		want    []string
		wantErr string
	}{
		{"one line", "releases:\n  - \"0.1\"\n", []string{"0.1"}, ""},
		{"newest first", "releases: [\"1.10\", \"1.9\", \"0.1\"]\n", []string{"1.10", "1.9", "0.1"}, ""},
		{"no lines", "releases: []\n", nil, "no release lines"},
		{"a patch version", "releases: [\"0.1.2\"]\n", nil, "not MAJOR.MINOR"},
		{"a v prefix", "releases: [\"v0.1\"]\n", nil, "not MAJOR.MINOR"},
		{"a leading zero", "releases: [\"0.01\"]\n", nil, "not MAJOR.MINOR"},
		{"dev is not a release line", "releases: [\"dev\"]\n", nil, "not MAJOR.MINOR"},
		{"a repeat", "releases: [\"0.1\", \"0.1\"]\n", nil, "listed twice"},
		{"oldest first", "releases: [\"0.1\", \"0.2\"]\n", nil, "newest first"},
		{"minor compared as a number", "releases: [\"1.9\", \"1.10\"]\n", nil, "newest first"},
		{"an unknown key", "releases: [\"0.1\"]\nlatest: \"0.1\"\n", nil, "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseVersions([]byte(tt.data))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLineTags(t *testing.T) {
	tags := []string{"v0.1.0", "v0.1.2", "v0.1.10", "v0.1.11-rc.1", "v0.2.0-rc.1", "v0.10.0", "v1.0.0", "x0.1.12", "v0.1.12x"}
	tests := []struct {
		line string
		want []string
	}{
		{"0.1", []string{"v0.1.10", "v0.1.2", "v0.1.0"}}, // numeric, not lexical; a prerelease never counts
		{"0.2", nil}, // only a prerelease so far
		{"0.10", []string{"v0.10.0"}},
		{"1.0", []string{"v1.0.0"}},
		{"2.0", nil},
	}
	for _, tt := range tests {
		if got := lineTags(tt.line, tags); !slices.Equal(got, tt.want) {
			t.Errorf("lineTags(%q) = %v, want %v", tt.line, got, tt.want)
		}
	}
}

func TestResolve(t *testing.T) {
	// The release pull request for 0.3 adds the line before its tag exists.
	got, skipped := resolve([]string{"0.3", "0.2", "0.1"}, []string{"v0.1.0", "v0.2.0", "v0.1.1", "v0.2.1", "v0.3.0-rc.1"})
	want := []release{{"0.2", "v0.2.1"}, {"0.2", "v0.2.0"}, {"0.1", "v0.1.1"}, {"0.1", "v0.1.0"}}
	if !slices.Equal(got, want) {
		t.Errorf("releases = %v, want %v", got, want)
	}
	if want := []string{"0.3"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
}

func TestUntrackedTag(t *testing.T) {
	tests := []struct {
		name  string
		lines []string
		tags  []string
		want  string
	}{
		{"every line listed", []string{"0.2", "0.1"}, []string{"v0.1.2", "v0.2.0"}, ""},
		{"a new minor not listed", []string{"0.1"}, []string{"v0.1.2", "v0.2.0"}, "v0.2.0"},
		{"a new major not listed", []string{"0.10"}, []string{"v0.10.1", "v1.0.0"}, "v1.0.0"},
		{"a prerelease of the next minor", []string{"0.1"}, []string{"v0.1.2", "v0.2.0-rc.1"}, ""},
		{"an older line dropped from the file", []string{"0.2"}, []string{"v0.1.2", "v0.2.0"}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := untrackedTag(tt.lines, tt.tags); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestCheckOut(t *testing.T) {
	root := filepath.FromSlash("/repo")
	venv := filepath.Join(root, "bin", "docs-venv")
	for _, tt := range []struct {
		out string
		ok  bool
	}{
		{filepath.Join(root, "bin", "versioned-site"), true},
		{filepath.FromSlash("/tmp/site"), true},
		{filepath.Join(root, "bin", "docs-venv-site"), true}, // a sibling, not the virtualenv
		{root, false},                        // -out . or -out ""
		{filepath.FromSlash("/"), false},     // holds the repository
		{filepath.Join(root, "bin"), false},  // holds the virtualenv
		{filepath.Join(root, "docs"), false}, // sources: inside the repository, outside bin/
		{filepath.Join(root, ".git"), false},
		{filepath.Join(venv, "site"), false}, // inside the virtualenv
		{venv, false},
	} {
		if err := checkOut(root, tt.out, venv); (err == nil) != tt.ok {
			t.Errorf("checkOut(%q) = %v, want ok %v", tt.out, err, tt.ok)
		}
	}
}

func TestVersionsJSON(t *testing.T) {
	b, err := versionsJSON([]release{{Line: "0.2", Tag: "v0.2.1"}, {Line: "0.2", Tag: "v0.2.0"}, {Line: "0.1", Tag: "v0.1.2"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []versionEntry
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := []versionEntry{
		{Version: "dev", Title: "main (unreleased)", Aliases: []string{}},
		{Version: "0.2.1", Title: "v0.2.1", Aliases: []string{"latest"}},
		{Version: "0.2.0", Title: "v0.2.0", Aliases: []string{}},
		{Version: "0.1.2", Title: "v0.1.2", Aliases: []string{}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d entries, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].Version != want[i].Version || got[i].Title != want[i].Title || !slices.Equal(got[i].Aliases, want[i].Aliases) {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	// Material reads aliases as an array; null would break its selector.
	if strings.Contains(string(b), "null") {
		t.Errorf("versions.json has a null: %s", b)
	}
}

func TestWrapperConfig(t *testing.T) {
	for _, tt := range []struct{ siteURL, root string }{
		{"https://agentkourier.dev/", "/"},
		{"https://example.org/docs/", "/docs/"},
	} {
		got, err := wrapperConfig(tt.siteURL, "0.1")
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"INHERIT: mkdocs.yml\n", "site_url: " + tt.siteURL + "0.1/\n", "site_root: " + tt.root + "\n", "provider: mike\n", "default: latest\n"} {
			if !strings.Contains(got, want) {
				t.Errorf("wrapper lacks %q:\n%s", want, got)
			}
		}
	}
}

func TestRedirects(t *testing.T) {
	releases := []release{{"0.2", "v0.2.1"}, {"0.2", "v0.2.0"}, {"0.1", "v0.1.3"}, {"0.1", "v0.1.2"}}
	got, err := redirects(releases, []entry{{"how-to", true}, {"index.html", false}, {"assets", true}})
	if err != nil {
		t.Fatal(err)
	}
	var rules []string
	for _, l := range strings.Split(strings.TrimSpace(got), "\n") {
		if !strings.HasPrefix(l, "#") {
			rules = append(rules, l)
		}
	}
	want := []string{
		"/ /latest/ 302",
		"/latest /0.2.1/ 302",
		"/0.2 /0.2.1/ 302", // each line to its newest tag
		"/0.1 /0.1.3/ 302",
		"/index.html /latest/index.html 301",
		"/latest/* /0.2.1/:splat 302",
		"/0.2/* /0.2.1/:splat 302",
		"/0.1/* /0.1.3/:splat 302",
		"/how-to/* /latest/how-to/:splat 301",
		"/assets/* /latest/assets/:splat 301",
	}
	if !slices.Equal(rules, want) {
		t.Errorf("rules:\n%s\nwant:\n%s", strings.Join(rules, "\n"), strings.Join(want, "\n"))
	}
}

func TestRedirectsLimit(t *testing.T) {
	var many []entry
	for i := range 100 {
		many = append(many, entry{name: "d" + string(rune('a'+i%26)) + string(rune('a'+i/26)), dir: true})
	}
	if _, err := redirects([]release{{"0.1", "v0.1.0"}}, many); err == nil || !strings.Contains(err.Error(), "dynamic") {
		t.Errorf("err = %v, want the dynamic rule limit", err)
	}
}

func TestHeaderBlock(t *testing.T) {
	in := "# A comment.\n/stylesheets/*\n  Cache-Control: max-age=300\n\n/\n  Cache-Control: max-age=300\n/assets/javascripts/*\n\tCache-Control: immutable\n"
	got, n, err := headerBlock(versionPlaceholder, []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := "/:version/stylesheets/*\n  Cache-Control: max-age=300\n/:version/\n  Cache-Control: max-age=300\n/:version/assets/javascripts/*\n  Cache-Control: immutable\n"
	if got != want || n != 3 {
		t.Errorf("got %d rules:\n%s\nwant 3:\n%s", n, got, want)
	}
	for _, bad := range []string{"  Cache-Control: x\n", "stylesheets/*\n"} {
		if _, _, err := headerBlock(versionPlaceholder, []byte(bad)); err == nil {
			t.Errorf("headerBlock(%q) = nil error", bad)
		}
	}
}

// The rules for the unversioned site's paths come from the fixed list, whatever the latest build holds.
func TestRedirectsKeepLegacyPaths(t *testing.T) {
	got, err := redirects([]release{{"0.2", "v0.2.0"}}, legacyPaths)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"/demo/* /latest/demo/:splat 301\n", "/index.html /latest/index.html 301\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("_redirects lacks %q", want)
		}
	}
	for _, e := range legacyPaths {
		if slices.Contains(rootFiles, e.name) {
			t.Errorf("%s is both copied to the root and redirected", e.name)
		}
	}
}

func TestSiteURL(t *testing.T) {
	dir := t.TempDir()
	data := "site_name: X\nsite_url: https://agentkourier.dev\nmarkdown_extensions:\n  - emoji:\n      g: !!python/name:x.y\n"
	if err := os.WriteFile(filepath.Join(dir, "mkdocs.yml"), []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := siteURL(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://agentkourier.dev/" {
		t.Errorf("got %q", got)
	}
}

// The repository's own file must parse.
func TestRepositoryVersionsFile(t *testing.T) {
	data, err := os.ReadFile("../versions.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseVersions(data); err != nil {
		t.Fatal(err)
	}
}

// assemble writes this tree's _headers once, for every version, whatever an older tag's own say, and removes each
// version's copy, which Pages would serve as a page.
func TestAssembleHeadersFollowThisTree(t *testing.T) {
	out := t.TempDir()
	write := func(path, data string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(out, path)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, path), []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("dev/_headers", "/*.html\n  Cache-Control: max-age=60\n")
	write("0.1.1/_headers", "/*.html\n  Cache-Control: max-age=300\n") // an older policy
	write("0.1.1/404.html", "not found")
	write("0.1.1/favicon.ico", "icon")
	write("0.1.0/index.html", "no _headers in this tag")
	b := &builder{out: out}
	if err := b.assemble([]release{{"0.1", "v0.1.1"}, {"0.1", "v0.1.0"}}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(out, "_headers"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "/:version/*.html\n  Cache-Control: max-age=60\n"; !strings.HasSuffix(string(got), want) || strings.Contains(string(got), "max-age=300\n") {
		t.Errorf("_headers =\n%s\nwant it to end with this tree's rule only:\n%s", got, want)
	}
	for _, v := range []string{"dev", "0.1.1", "0.1.0"} {
		if _, err := os.Stat(filepath.Join(out, v, "_headers")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s/_headers is still there (err %v)", v, err)
		}
	}
}
