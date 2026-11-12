package sitesrc

import (
	"path/filepath"
	"slices"
	"testing"
)

func TestExcludedDocs(t *testing.T) {
	const mkdocs = "site_name: x\n\ndocs_dir: docs\n# The brand kit lives in docs/.\nexclude_docs: |\n  /brand/\n  /spikes/\n\ntheme:\n  name: material\n"
	got, err := ExcludedDocs(mkdocs)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"brand", "spikes"}; !slices.Equal(got, want) {
		t.Errorf("ExcludedDocs = %v, want %v", got, want)
	}
	for name, bad := range map[string]string{
		"no block":      "site_name: x\n",
		"a glob":        "exclude_docs: |\n  *.draft.md\n",
		"a nested path": "exclude_docs: |\n  /a/b/\n",
	} {
		if _, err := ExcludedDocs(bad); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

// mkdocs anchors a /name/ pattern of exclude_docs at docs/, so only a top-level directory of that name is excluded.
func TestExcluded(t *testing.T) {
	excluded := []string{"brand", "spikes"}
	tests := []struct {
		rel  string
		want bool
	}{
		{"brand", true},
		{"spikes", true},
		{".", false},
		{"how-to", false},
		{"assets/brand", false},
		{filepath.Join("assets", "brand"), false},
		{"brand/film", false}, // inside an excluded directory, which the walk never enters
		{"..", false},
		{"../brand", false},
		{"brandkit", false},
	}
	for _, tt := range tests {
		if got := Excluded(excluded, tt.rel); got != tt.want {
			t.Errorf("Excluded(%v, %q) = %v, want %v", excluded, tt.rel, got, tt.want)
		}
	}
	if Excluded(nil, "brand") {
		t.Error("Excluded(nil, \"brand\") = true, want false")
	}
}

func TestIsText(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"index.md", true},
		{"main.html", true},
		{"mkdocs.yml", true},
		{"values.yaml", true},
		{"extra.css", true},
		{"site.js", true},
		{"versions.json", true},
		{"architecture-map.svg", true},
		{"captions.vtt", true},
		{"site.webmanifest", true},
		{"robots.txt", true},
		{"_headers", true},
		{filepath.Join("docs", "_headers"), true},
		{"logo.png", false},
		{"favicon.ico", false},
		{"film.mp4", false},
		{"main.go", false},
		{".dockerignore", false},
		{"headers", false},
		{"_headers.bak", false},
	}
	for _, tt := range tests {
		if got := IsText(tt.name); got != tt.want {
			t.Errorf("IsText(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}
