package main

import (
	"encoding/json"
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

func TestNewestTag(t *testing.T) {
	tags := []string{"v0.1.0", "v0.1.2", "v0.1.10", "v0.1.11-rc.1", "v0.2.0-rc.1", "v0.10.0", "v1.0.0", "x0.1.12", "v0.1.12x"}
	tests := []struct {
		line, want string
	}{
		{"0.1", "v0.1.10"}, // numeric, not lexical; a prerelease never counts
		{"0.2", ""},        // only a prerelease so far
		{"0.10", "v0.10.0"},
		{"1.0", "v1.0.0"},
		{"2.0", ""},
	}
	for _, tt := range tests {
		if got := newestTag(tt.line, tags); got != tt.want {
			t.Errorf("newestTag(%q) = %q, want %q", tt.line, got, tt.want)
		}
	}
}

func TestResolveSkipsUntaggedLine(t *testing.T) {
	// The release pull request for 0.2 adds the line before its tag exists.
	got, skipped := resolve([]string{"0.2", "0.1"}, []string{"v0.1.2", "v0.2.0-rc.1"})
	if want := []release{{Line: "0.1", Tag: "v0.1.2"}}; !slices.Equal(got, want) {
		t.Errorf("releases = %v, want %v", got, want)
	}
	if want := []string{"0.2"}; !slices.Equal(skipped, want) {
		t.Errorf("skipped = %v, want %v", skipped, want)
	}
}

func TestVersionsJSON(t *testing.T) {
	b, err := versionsJSON([]release{{Line: "0.2", Tag: "v0.2.1"}, {Line: "0.1", Tag: "v0.1.2"}})
	if err != nil {
		t.Fatal(err)
	}
	var got []versionEntry
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := []versionEntry{
		{Version: "dev", Title: "main (unreleased)", Aliases: []string{}},
		{Version: "0.2", Title: "v0.2.1", Aliases: []string{"latest"}},
		{Version: "0.1", Title: "v0.1.2", Aliases: []string{}},
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
	got := wrapperConfig("https://agentkourier.dev/", "0.1")
	for _, want := range []string{"INHERIT: mkdocs.yml\n", "site_url: https://agentkourier.dev/0.1/\n", "provider: mike\n", "default: latest\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("wrapper lacks %q:\n%s", want, got)
		}
	}
}

func TestRedirects(t *testing.T) {
	got, err := redirects("0.1", []entry{{"how-to", true}, {"index.html", false}, {"sitemap.xml", false}, {"assets", true}})
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
		"/latest /0.1/ 302",
		"/index.html /latest/index.html 301",
		"/sitemap.xml /latest/sitemap.xml 301",
		"/latest/* /0.1/:splat 302",
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
	if _, err := redirects("0.1", many); err == nil || !strings.Contains(err.Error(), "dynamic") {
		t.Errorf("err = %v, want the dynamic rule limit", err)
	}
}

func TestHeaderBlock(t *testing.T) {
	in := "# A comment.\n/stylesheets/*\n  Cache-Control: max-age=300\n\n/\n  Cache-Control: max-age=300\n/assets/javascripts/*\n\tCache-Control: immutable\n"
	got, n, err := headerBlock("0.1", []byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := "/0.1/stylesheets/*\n  Cache-Control: max-age=300\n/0.1/\n  Cache-Control: max-age=300\n/0.1/assets/javascripts/*\n  Cache-Control: immutable\n"
	if got != want || n != 3 {
		t.Errorf("got %d rules:\n%s\nwant 3:\n%s", n, got, want)
	}
	for _, bad := range []string{"  Cache-Control: x\n", "stylesheets/*\n"} {
		if _, _, err := headerBlock("0.1", []byte(bad)); err == nil {
			t.Errorf("headerBlock(%q) = nil error", bad)
		}
	}
}

func TestLegacyEntries(t *testing.T) {
	dir := t.TempDir()
	for _, d := range []string{"how-to", "assets"} {
		if err := os.Mkdir(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"index.html", "404.html", "favicon.ico", "_headers", "sitemap.xml"} {
		if err := os.WriteFile(filepath.Join(dir, f), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := legacyEntries(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []entry{{"assets", true}, {"how-to", true}, {"index.html", false}, {"sitemap.xml", false}}
	if !slices.Equal(got, want) {
		t.Errorf("got %v, want %v", got, want)
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
