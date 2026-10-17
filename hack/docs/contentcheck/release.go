package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// The release-version check keeps the public docs on the current release. The version a page names (in an image tag,
// in `--version` on a helm command for the published chart, in the release workflow's signing identity, and in "the
// current release is" and "Status: v") must be the chart's own version in charts/agent-kourier/Chart.yaml. The release
// gate (hack/release/check-version.sh) refuses a release tag that is not that version, so cutting a release takes a
// pull request that bumps the chart, and this check fails that pull request until no page names another version.

// chartFile is the chart whose version the docs must name, relative to the repository root.
const chartFile = "charts/agent-kourier/Chart.yaml"

// releaseVersionRuleID names the version check in a finding and in an allowlist entry.
const releaseVersionRuleID = "release-version"

// publishedChart is the reference of the published chart, which a helm command names before its --version.
const publishedChart = "oci://ghcr.io/agentkourier/charts/agent-kourier"

// semverPattern matches a semantic version with an optional prerelease suffix, captured without a leading v.
const semverPattern = `(\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?)`

// releaseRefs match the places a page names a release version on one line. Each captures the version.
var releaseRefs = []*regexp.Regexp{
	regexp.MustCompile(`ghcr\.io/agentkourier/agent-kourier:v` + semverPattern),
	regexp.MustCompile(`ghcr\.io/agentkourier/charts/agent-kourier:` + semverPattern),
	regexp.MustCompile(`agentkourier/agent-kourier/\.github/workflows/release\.yml@refs/tags/v` + semverPattern),
	regexp.MustCompile(`(?i)\bcurrent release is\s+` + "`?" + `v?` + semverPattern),
	regexp.MustCompile(`\bStatus: v` + semverPattern),
}

// helmVersion matches the --version of a helm command and captures the version; a placeholder such as <new version>
// does not match.
var helmVersion = regexp.MustCompile(`--version\s+` + "`?" + semverPattern)

// chartVersion returns the version in the chart's Chart.yaml under root, or "" when the file does not exist, as in a
// test tree without a chart.
func chartVersion(root string) (string, error) {
	b, err := os.ReadFile(filepath.Join(root, chartFile)) //nolint:gosec // the repository's own chart
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read %s: %w", chartFile, err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "version:"); ok {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if v == "" {
				break
			}
			return v, nil
		}
	}
	return "", fmt.Errorf("%s has no version", chartFile)
}

// releaseVersionFindings returns a finding for each release version on a line of text that is not want. A helm
// command's --version counts when the line names the published chart, or continues a line that does with a trailing
// backslash.
func releaseVersionFindings(path, text, want string) []finding {
	if want == "" {
		return nil
	}
	var out []finding
	add := func(i int, line string, m []int) {
		got := line[m[2]:m[3]]
		if got == want {
			return
		}
		out = append(out, finding{path: path, line: i + 1, rule: releaseVersionRuleID, text: line, start: m[0], end: m[1],
			msg: fmt.Sprintf("names version %s, but %s is %s: name the release the chart is", got, chartFile, want)})
	}
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		for _, re := range releaseRefs {
			for _, m := range re.FindAllStringSubmatchIndex(line, -1) {
				add(i, line, m)
			}
		}
		if !strings.Contains(line, publishedChart) {
			continue
		}
		for _, m := range helmVersion.FindAllStringSubmatchIndex(line, -1) {
			add(i, line, m)
		}
		if strings.HasSuffix(strings.TrimSpace(line), `\`) && i+1 < len(lines) {
			for _, m := range helmVersion.FindAllStringSubmatchIndex(lines[i+1], -1) {
				add(i+1, lines[i+1], m)
			}
		}
	}
	return out
}
