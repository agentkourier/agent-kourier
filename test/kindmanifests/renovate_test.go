package kindmanifests_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// renovateImageManager is one of renovate.json's regex managers that look images up in the docker datasource: the files it
// reads and the patterns it extracts a pin with.
type renovateImageManager struct {
	files   []*regexp.Regexp
	matches []*regexp.Regexp
}

// renovateImageManagers reads renovate.json's regex managers for images. Renovate writes its patterns as JavaScript regular
// expressions between slashes; the ones there need nothing Go's regexp lacks, and one that does fails the test.
func renovateImageManagers(t *testing.T) []renovateImageManager {
	t.Helper()
	raw, err := os.ReadFile("../../renovate.json")
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		EnabledManagers []string `json:"enabledManagers"`
		CustomManagers  []struct {
			CustomType          string   `json:"customType"`
			ManagerFilePatterns []string `json:"managerFilePatterns"`
			MatchStrings        []string `json:"matchStrings"`
			DatasourceTemplate  string   `json:"datasourceTemplate"`
		} `json:"customManagers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("renovate.json: %v", err)
	}
	if !slices.Contains(cfg.EnabledManagers, "custom.regex") {
		t.Fatalf("renovate.json's enabledManagers %v leave out custom.regex", cfg.EnabledManagers)
	}
	var out []renovateImageManager
	for _, m := range cfg.CustomManagers {
		if m.CustomType != "regex" || m.DatasourceTemplate != "docker" {
			continue
		}
		var rm renovateImageManager
		for _, p := range m.ManagerFilePatterns {
			if len(p) < 2 || !strings.HasPrefix(p, "/") || !strings.HasSuffix(p, "/") {
				t.Fatalf("renovate.json: file pattern %q is not a /regex/ without flags", p)
			}
			rm.files = append(rm.files, regexp.MustCompile(p[1:len(p)-1]))
		}
		for _, s := range m.MatchStrings {
			rm.matches = append(rm.matches, regexp.MustCompile(s))
		}
		out = append(out, rm)
	}
	if len(out) == 0 {
		t.Fatal("renovate.json has no regex manager for the docker datasource")
	}
	return out
}

// extracts reports the image references the managers that read the file at path (relative to the repository root) extract
// from body, each as name[:tag]@digest.
func extracts(managers []renovateImageManager, path, body string) map[string]bool {
	got := map[string]bool{}
	for _, m := range managers {
		if !slices.ContainsFunc(m.files, func(re *regexp.Regexp) bool { return re.MatchString(path) }) {
			continue
		}
		for _, re := range m.matches {
			name, tag, digest := re.SubexpIndex("depName"), re.SubexpIndex("currentValue"), re.SubexpIndex("currentDigest")
			for _, sm := range re.FindAllStringSubmatch(body, -1) {
				ref := sm[name]
				if tag >= 0 && sm[tag] != "" {
					ref += ":" + sm[tag]
				}
				got[ref+"@"+sm[digest]] = true
			}
		}
	}
	return got
}

// TestRenovateTracksEveryKindImagePin: every image hack/kind pins by digest, in a script or in a manifest, is one Renovate
// extracts, so a bump reaches every copy of a pin and not only those in the files it happens to read (ax/probe.yaml's curl
// pin was once left behind that way).
func TestRenovateTracksEveryKindImagePin(t *testing.T) {
	managers := renovateImageManagers(t)
	pin := regexp.MustCompile(`[a-z0-9][a-z0-9./_-]*(?::[A-Za-z0-9][A-Za-z0-9._-]*)?@sha256:[0-9a-f]{64}`)
	pinned := 0
	err := filepath.WalkDir("../../hack/kind", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		body := string(raw)
		refs := pin.FindAllString(body, -1)
		if len(refs) == 0 {
			return nil
		}
		rel := strings.TrimPrefix(filepath.ToSlash(path), "../../")
		got := extracts(managers, rel, body)
		for _, ref := range refs {
			pinned++
			if !got[ref] {
				t.Errorf("%s pins %s, which no regex manager in renovate.json extracts", rel, ref)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if pinned == 0 {
		t.Fatal("found no image pinned by digest under hack/kind")
	}
}
