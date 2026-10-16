package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// ruleIDs returns the sorted rule IDs of the findings.
func ruleIDs(fs []finding) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.rule)
	}
	slices.Sort(out)
	return out
}

func TestScanText(t *testing.T) {
	tests := []struct {
		name string
		text string
		want []string // the rule of each finding, sorted
	}{
		// Internal planning IDs.
		{"a work package ID", "Added in WP-27g.\n", []string{"planning-id"}},
		{"a change ID", "See CT-3 for the rest.\n", []string{"planning-id"}},
		{"a drop-alert-path ID", "Removed by DA-2.\n", []string{"planning-id"}},
		{"a docs-pass ID", "Listed under DP-4.\n", []string{"planning-id"}},
		{"a gate ID with a phase", "Waits for H2-1.\n", []string{"planning-id"}},
		{"a human gate ID", "Waits for H-01.\n", []string{"planning-id"}},
		{"a tool-approvals plan ID", "Lands with TA-9 and TA-12.\n", []string{"planning-id", "planning-id"}},
		{"a tool-approvals plan ID with a letter", "The fence is TA-F.\n", []string{"planning-id"}},
		{"a paused-answers plan ID", "Added by PA-1.\n", []string{"planning-id"}},
		{"a docs check ID", "Added by DC-1.\n", []string{"planning-id"}},
		{"a decision ID in parentheses", "The exit decision (D33) reads it.\n", []string{"decision-id"}},
		{"a decision ID after spec", "As spec D37 says.\n", []string{"decision-id"}},
		{"a decision ID after the word decision", "Decision D12 settled it.\n", []string{"decision-id"}},
		{"a decision ID in brackets", "[D40] fields.\n", []string{"decision-id"}},
		{"a decision ID after a spec section in a list", "Is it so (spec 11, D37)?\n", []string{"decision-id"}},
		{"a decision ID after a dotted spec section", "Is it so (spec 8.2, D38)?\n", []string{"decision-id"}},
		{"a decision ID in running prose", "The record D33 is computed from it.\n", []string{"decision-id"}},
		{"a decision ID closing a wrapped parenthesis", "(spec 11, D37): see below.\n", []string{"decision-id"}},
		{"a decision ID at a line start", "D37): see below.\n", []string{"decision-id"}},
		{"two decision IDs in a list are two findings", "Settled by decisions D12 and D14.\n", []string{"decision-id", "decision-id"}},
		{"a bare D number is a decision ID, and the allowlist excuses a real one", "A D65 white point.\n", []string{"decision-id"}},
		{"a risk ID in parentheses", "A known gap (R19).\n", []string{"risk-id"}},
		{"a risk ID after the word risk", "This is risk R19.\n", []string{"risk-id"}},
		{"a risk ID after the word Risk", "Risk R3 is accepted.\n", []string{"risk-id"}},
		{"two risk IDs in a list are two findings", "Open risks R1 and R2 remain.\n", []string{"risk-id", "risk-id"}},
		{"a risk ID in running prose", "The gap R19 is known.\n", []string{"risk-id"}},
		{"a known false positive: Cloudflare R2", "Stored on Cloudflare (R2).\n", []string{"risk-id"}},
		{"a known false positive: a reference label", "[R2]: https://example.com\n", []string{"risk-id"}},
		{"a tasks plan path", "See tasks/plans/docs-site.md.\n", []string{"tasks-path"}},
		{"a tasks file", "Recorded in tasks/todo.md.\n", []string{"tasks-path"}},
		{"the tasks directory in code font", "Plans live under `tasks/`.\n", []string{"tasks-path"}},

		// Things that look like IDs and are not.
		{"an RFC", "Addresses follow RFC 5322 and RFC 7230.\n", nil},
		{"a D-Bus style token", "Talks to D-Bus and the D-Bus session bus.\n", nil},
		{"a letter and digits that continue a word", "R2D2, D50x, XD12, and 0xD00.\n", nil},
		{"a three-digit D number", "The D123 body.\n", nil},
		{"a parenthesised word that starts with D", "(Deployment) and (Default).\n", nil},
		{"hash names", "SHA-256 and HTTP-2 and ECDSA-P256.\n", nil},
		{"DNS labels", "A DNS-1123 label and a DNS-1035 label.\n", nil},
		{"UTF encodings", "UTF-8 text, UTF-16 units and UTF-32.\n", nil},
		{"a two-letter prefix with a lower-case tail", "Use TA-foo or ab-1.\n", nil},
		{"an A2A task method is not a tasks path", "Calls tasks/get, tasks/cancel and `tasks/resubscribe`.\n", nil},
		{"a ticket-like word inside a longer word", "DATA-1 and BADA-2 and SWP-3.\n", nil},

		// Removed names.
		{"webhookPath", "status.webhookPath holds it.\n", []string{"webhook-path"}},
		{"the hook URL path", "POST to /hooks/alertmanager/web.\n", []string{"webhook-path"}},
		{"spec.alerts", "Set spec.alerts to match.\n", []string{"spec-alerts"}},
		{"the removed preset field", "Set preset: alertmanager on the trigger.\n", []string{"removed-preset"}},
		{"the removed preset by name", "The alertmanager preset fills it in.\n", []string{"removed-preset"}},
		{"the removed preset, capitalised", "An Alertmanager preset is not needed.\n", []string{"removed-preset"}},
		{"the removed preset field, double-quoted", "Set `preset: \"alertmanager\"` on the trigger.\n", []string{"removed-preset"}},
		{"the removed preset field, single-quoted", "Set preset: 'alertmanager' on the trigger.\n", []string{"removed-preset"}},
		{"the removed preset by name, in backticks", "The `alertmanager` preset fills it in.\n", []string{"removed-preset"}},
		{"the removed preset by name, quoted", "The \"alertmanager\" preset fills it in.\n", []string{"removed-preset"}},
		{"the removed preset field, with a backticked key", "Set `preset`: alertmanager on the trigger.\n", []string{"removed-preset"}},
		{"the removed preset, name after the noun", "Set the preset `alertmanager` on the trigger.\n", []string{"removed-preset"}},
		{"the removed preset, name after the noun, plain", "The preset Alertmanager filled it in.\n", []string{"removed-preset"}},
		{"the renamed sender field", "Set `from.botId` to the bot's ID.\n", []string{"removed-trigger-field"}},
		{"the renamed sender field in flow YAML", "from: {botId: B0123456789}\n", []string{"removed-trigger-field"}},
		{"the renamed body field", "match attachmentText on the retry line.\n", []string{"removed-trigger-field"}},
		{"the renamed link field", "match: {titleLink: 'receiver=a$'}\n", []string{"removed-trigger-field"}},
		{"the new names are fine", "from: {id: B0123456789}, match link, extract body.\n", nil},
		{"Slack's own title_link is not a renamed field", "The attachment's title_link and text.\n", nil},
		{"the removed preset, name after the noun, quoted", "The preset \"alertmanager\" filled it in.\n", []string{"removed-preset"}},
		{"a trigger preset and the field name alone are not the removed preset", "A trigger preset: `preset:` is not a field.\n", nil},
		{"Alertmanager and a preset in different places are not the removed preset", "Alertmanager posts it. A preset of colours.\n", nil},
		{"approverGroups", "Name `approverGroups`.\n", []string{"unbuilt-field"}},
		{"toolApprovals", "Set toolApprovals: true.\n", []string{"unbuilt-field"}},
		{"directMessages", "A ChatConnection with directMessages.\n", []string{"unbuilt-field"}},
		{"the roadmap file", "See ROADMAP.md.\n", []string{"removed-page"}},
		{"a link to the roadmap page", "[Roadmap](roadmap.md)\n", []string{"removed-page"}},
		{"a link to the comparison page", "[Comparison](../explanation/comparison.md#x)\n", []string{"removed-page"}},
		{"the phrase wakes it if needed", "The broker wakes it if needed.\n", []string{"wrong-claim"}},
		{"alertname is not spec.alerts", "Matches labels.alertname and spec.alertTimeout.\n", nil},
		{"toolApprovals is not toolApprovalsEnabled", "A hypothetical toolApprovalsEnabled.\n", nil},
		{"a roadmap word is not the file", "Read the roadmap in the issue tracker.\n", nil},

		// Owner and workspace identifiers.
		{"the owner's channel ID", "channel C0EXAMPLE01\n", []string{"workspace-id"}},
		{"the owner's bot ID", "bot B0EXAMPLE01\n", []string{"workspace-id"}},
		{"the owner's organisation", "example-org.\n", []string{"workspace-name"}},
		{"the owner's cluster repository", "git@github.com:x/cluster-gitops.git\n", []string{"workspace-name"}},
		{"the owner's alert setup", "route alerts\n", []string{"workspace-name"}},
		{"the owner's app of apps", "cluster-apps\n", []string{"workspace-name"}},
		{"a home directory", "cd /Users/someone/src\n", []string{"home-path"}},
		{"a users URL path is not a home directory", "GET /api/users/42 and https://example.com/users/x\n", nil},
		{"a fake channel ID", "channel C0123456789\n", nil},

		// A code fence is not exempt: a leak in an example is a leak.
		{"a rule applies inside a code fence", "```yaml\nwebhookPath: /x\n```\n", []string{"webhook-path"}},
		{"a rule applies inside inline code", "Use `spec.alerts`.\n", []string{"spec-alerts"}},

		// Several matches.
		{"two rules on one line", "WP-1 and webhookPath.\n", []string{"planning-id", "webhook-path"}},
		{"one rule twice on one line is two findings", "WP-1 and WP-2.\n", []string{"planning-id", "planning-id"}},
		{"the same match twice is two findings", "WP-1 and WP-1.\n", []string{"planning-id", "planning-id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ruleIDs(scanText(rules, "docs/x.md", tt.text))
			if !slices.Equal(got, tt.want) {
				t.Errorf("scanText(%q) rules %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestScanTextSVGIgnoresGeneratedIDs(t *testing.T) {
	tests := []struct {
		name, path, text string
		want             []string
	}{
		{"a mermaid node id", "docs/a.svg", `<g class="node" id="flowchart-DB-15" transform="x"><text>Store</text></g>`, nil},
		{"a marker reference", "docs/a.svg", `<path marker-end="url(#my-svg_pointEnd-WP-1)" id="L-DB-AR-0"/>`, nil},
		{"visible text is read", "docs/a.svg", `<g id="flowchart-DB-15"><text>see WP-12</text></g>`, []string{"planning-id"}},
		{"another attribute is read", "docs/a.svg", `<svg sodipodi:docname="/Users/someone/x.svg">`, []string{"home-path"}},
		{"an id attribute outside an SVG is read", "docs/a.md", `<a id="DB-15"></a>`, []string{"planning-id"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ruleIDs(scanText(rules, tt.path, tt.text))
			if !slices.Equal(got, tt.want) {
				t.Errorf("scanText(%q) rules %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestScanTextReportsFileLineRuleAndMatch(t *testing.T) {
	tests := []struct {
		text string
		want []string
	}{
		{"fine\nfine\nSee (D33) here.\n", []string{"docs/a.md:3:", "decision-id", `"D33"`}},
		{"Added in WP-27g and later.\n", []string{"docs/a.md:1:", "planning-id", `"WP-27g"`}}, // the whole ID, suffix included
	}
	for _, tt := range tests {
		got := scanText(rules, "docs/a.md", tt.text)
		if len(got) != 1 {
			t.Fatalf("scanText(%q) = %v, want one finding", tt.text, got)
		}
		s := got[0].String()
		for _, want := range tt.want {
			if !strings.Contains(s, want) {
				t.Errorf("finding %q lacks %q", s, want)
			}
		}
	}
}

// TestRulesAreComplete keeps the table honest: the allowlist and the errors name a rule by its ID.
func TestRulesAreComplete(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range rules {
		if r.id == "" || r.why == "" || r.re == nil {
			t.Errorf("rule %+v is incomplete", r)
		}
		if seen[r.id] {
			t.Errorf("rule ID %q is used twice", r.id)
		}
		seen[r.id] = true
	}
}

func TestLinks(t *testing.T) {
	fsys := fstest.MapFS{
		"README.md":                      {Data: []byte("# Agent Kourier\n")},
		"spec.md":                        {Data: []byte("# Spec\n")},
		"docs/index.md":                  {Data: []byte("# Home\n")},
		"docs/how-to/ha.md":              {Data: []byte("# Run two replicas (high availability)\n\n## Check C# support\n\n## Same\n\n## Same\n\n## Custom {#my-id}\n\n## A - B\n\n## _Italic_ heading\n\n## chat_triggers and _x_\n\n## `_code_` stays\n\n## __Bold__ text\n\n<a id=\"html-anchor\"></a>\n\n```\n# not a heading\n```\n")},
		"charts/agent-kourier/README.md": {Data: []byte("# Chart\n")},
		"charts/agent-kourier/ci/x.yaml": {Data: []byte("a: b\n")},
	}
	tests := []struct {
		name   string
		file   string
		text   string
		want   []int // lines of the findings
		wantIn string
	}{
		{"a link to a file that exists", "README.md", "[spec](spec.md)\n", nil, ""},
		{"a link to a directory that exists", "README.md", "[docs](docs/)\n", nil, ""},
		{"a link from the chart README", "charts/agent-kourier/README.md", "[ci](ci/x.yaml) and [top](../../README.md)\n", nil, ""},
		{"a link to a missing file", "README.md", "ok\n[gone](docs/gone.md)\n", []int{2}, "docs/gone.md does not exist"},
		{"a link to a missing file in the chart README", "charts/agent-kourier/README.md", "[x](../../docs/roadmap-x.md)\n", []int{1}, "docs/roadmap-x.md does not exist"},
		{"a link that leaves the repository", "README.md", "[x](../elsewhere.md)\n", []int{1}, "outside the repository"},
		{"an external link is not fetched", "README.md", "[x](https://example.com/nope) [m](mailto:a@b.c)\n", nil, ""},
		{"a root-relative link", "README.md", "[x](/docs/index.md) [y](/docs/none.md)\n", []int{1}, "docs/none.md does not exist"},
		{"an anchor that exists in the same file", "README.md", "# Intro to it\n[x](#intro-to-it)\n", nil, ""},
		{"an anchor missing in the same file", "README.md", "# Intro\n[x](#outro)\n", []int{2}, `no heading or anchor "outro"`},
		{"an anchor into a docs page", "README.md", "[x](docs/how-to/ha.md#run-two-replicas-high-availability)\n", nil, ""},
		{"a missing anchor into a docs page", "README.md", "[x](docs/how-to/ha.md#run-three-replicas)\n", []int{1}, `no heading or anchor "run-three-replicas"`},
		{"a heading with a hash inside it", "README.md", "[x](docs/how-to/ha.md#check-c-support)\n", nil, ""},
		// GitHub renders _x_ as emphasis, and keeps the underscores of an identifier or a code span.
		{"underscore emphasis is not in the slug", "README.md", "[x](docs/how-to/ha.md#italic-heading)\n", nil, ""},
		{"an identifier keeps its underscore, emphasis around a word goes", "README.md", "[x](docs/how-to/ha.md#chat_triggers-and-x)\n", nil, ""},
		{"a code span keeps its underscores", "README.md", "[x](docs/how-to/ha.md#_code_-stays)\n", nil, ""},
		{"double underscore emphasis", "README.md", "[x](docs/how-to/ha.md#bold-text)\n", nil, ""},
		{"the slug with the emphasis marks is not an anchor", "README.md", "[x](docs/how-to/ha.md#_italic_-heading)\n", []int{1}, `no heading or anchor "_italic_-heading"`},
		{"a duplicate heading's second anchor", "README.md", "[x](docs/how-to/ha.md#same-1)\n", nil, ""},
		// The READMEs are rendered by GitHub, which does not read an attr_list id and slugs the whole heading text.
		{"an attr_list id is not an anchor on GitHub", "README.md", "[x](docs/how-to/ha.md#my-id)\n", []int{1}, `no heading or anchor "my-id"`},
		{"the GitHub slug of a heading with an attr_list id", "README.md", "[x](docs/how-to/ha.md#custom-my-id)\n", nil, ""},
		{"GitHub keeps each hyphen of a spaced dash", "README.md", "[x](docs/how-to/ha.md#a---b)\n", nil, ""},
		{"python-markdown collapses them, GitHub does not", "README.md", "[x](docs/how-to/ha.md#a-b)\n", []int{1}, `no heading or anchor "a-b"`},
		{"python-markdown repeat suffix is not GitHub's", "README.md", "[x](docs/how-to/ha.md#same_1)\n", []int{1}, `no heading or anchor "same_1"`},
		{"an html anchor", "README.md", "[x](docs/how-to/ha.md#html-anchor)\n", nil, ""},
		{"a hash line in a code fence is not a heading", "README.md", "[x](docs/how-to/ha.md#not-a-heading)\n", []int{1}, `no heading or anchor "not-a-heading"`},
		{"an anchor on a file that is not Markdown", "README.md", "[x](charts/agent-kourier/ci/x.yaml#L1)\n", nil, ""},
		{"a link inside a code fence is not checked", "README.md", "```md\n[x](docs/none.md)\n```\n", nil, ""},
		{"a closing fence with trailing spaces still closes", "README.md", "```\n[x](docs/none.md)\n```  \n[gone](docs/dead.md)\n", []int{4}, "docs/dead.md does not exist"},
		{"a closing tilde fence with a trailing tab still closes", "README.md", "~~~\n~~~\t\n[gone](docs/dead.md)\n", []int{3}, "docs/dead.md does not exist"},
		{"a longer closing fence closes", "README.md", "```\n[x](docs/none.md)\n`````\n[gone](docs/dead.md)\n", []int{4}, "docs/dead.md does not exist"},
		{"a shorter fence line does not close", "README.md", "````\n```\n[x](docs/none.md)\n````\n[gone](docs/dead.md)\n", []int{5}, "docs/dead.md does not exist"},
		{"a link inside a tilde fence is not checked", "README.md", "~~~\n[x](docs/none.md)\n~~~\n", nil, ""},
		{"a link inside inline code is not checked", "README.md", "Write `[x](docs/none.md)` like this.\n", nil, ""},
		{"a link with a title", "README.md", "[x](spec.md \"The spec\")\n", nil, ""},
		{"an image", "README.md", "![logo](docs/assets/none.png)\n", []int{1}, "docs/assets/none.png does not exist"},
		{"a reference definition", "README.md", "[spec]: spec.md\n[bad]: docs/none.md\n", []int{2}, "docs/none.md does not exist"},
		{"an html href", "README.md", "<a href=\"docs/none.md\">x</a>\n", []int{1}, "docs/none.md does not exist"},
		{"a query string", "README.md", "[x](spec.md?plain=1)\n", nil, ""},
		{"an escaped space", "README.md", "[x](docs/none%20here.md)\n", []int{1}, "docs/none here.md does not exist"},
		{"two bad links on two lines", "README.md", "[a](a.md)\n[b](b.md)\n", []int{1, 2}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &linkChecker{fsys: fsys}
			var got []int
			var msgs string
			for _, f := range c.check(tt.file, tt.text) {
				if f.rule != "dead-link" && f.rule != "dead-anchor" {
					t.Errorf("finding %v has rule %q, want dead-link or dead-anchor", f, f.rule)
				}
				got = append(got, f.line)
				msgs += f.String() + "\n"
			}
			if !slices.Equal(got, tt.want) {
				t.Fatalf("check(%q) at lines %v, want %v\n%s", tt.text, got, tt.want, msgs)
			}
			if tt.wantIn != "" && !strings.Contains(msgs, tt.wantIn) {
				t.Errorf("findings %q lack %q", msgs, tt.wantIn)
			}
		})
	}
}

func TestExcludedDocs(t *testing.T) {
	const mkdocs = "site_name: x\n\ndocs_dir: docs\n# The brand kit lives in docs/.\nexclude_docs: |\n  /brand/\n  /spikes/\n\ntheme:\n  name: material\n"
	got, err := excludedDocs(mkdocs)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"brand", "spikes"}; !slices.Equal(got, want) {
		t.Errorf("excludedDocs = %v, want %v", got, want)
	}
	for name, bad := range map[string]string{
		"no block":      "site_name: x\n",
		"a glob":        "exclude_docs: |\n  *.draft.md\n",
		"a nested path": "exclude_docs: |\n  /a/b/\n",
	} {
		if _, err := excludedDocs(bad); err == nil {
			t.Errorf("%s: no error", name)
		}
	}
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, text := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

const testMkdocs = "docs_dir: docs\nexclude_docs: |\n  /brand/\n  /spikes/\n"

// apiTypes is an API package with no fields: every unbuilt field of the content table is still unbuilt.
const apiTypes = "package v1alpha1\n"

func TestPublicFiles(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"README.md":                        "x\n",
		"charts/agent-kourier/README.md":   "x\n",
		"charts/agent-kourier/values.yaml": "x: 1\n",
		"docs/index.md":                    "x\n",
		"docs/how-to/a.md":                 "x\n",
		"docs/brand/notes.md":              "WP-1\n",
		"docs/brand/logo.svg":              "WP-1\n",
		"docs/spikes/old.md":               "WP-1\n",
		"docs/spikes/old.txt":              "WP-1\n",
		"docs/how-to/brand/kept.md":        "x\n", // only a top-level brand directory is excluded
		"internal/x.md":                    "WP-1\n",
		// Every published text file is read, not only Markdown.
		"docs/_headers":                           "/*\n",
		"docs/site.webmanifest":                   "{}\n",
		"docs/assets/diagrams/map.svg":            "<svg/>\n",
		"docs/assets/film/captions.vtt":           "WEBVTT\n",
		"docs/stylesheets/brand.css":              "a{}\n",
		"docs/notes.txt":                          "x\n",
		"docs/data.json":                          "{}\n",
		"docs/page.html":                          "<p/>\n",
		"hack/docs/overrides/main.html":           "<p/>\n",
		"hack/docs/overrides/partials/logo.html":  "<p/>\n",
		"hack/docs/overrides/partials/ignored.go": "package x\n",
		// The recipes' READMEs and YAML files (kourier/ resources, evals) are public, at any depth; other files are not yet.
		"examples/recipes/README.md":                "x\n",
		"examples/recipes/sre/README.md":            "x\n",
		"examples/recipes/sre/agent/README.md":      "x\n",
		"examples/recipes/sre/kourier/binding.yaml": "WP-1\n",
		"examples/recipes/sre/kourier/other.yml":    "WP-1\n",
		"examples/recipes/sre/evals/triggers.yaml":  "WP-1\n",
		"examples/recipes/sre/notes.md":             "WP-1\n",
		"examples/recipes/sre/agent/main.go":        "package x // WP-1\n",
		"examples/ax/README.md":                     "WP-1\n",
		// Binary files are skipped.
		"docs/assets/logo.png": "\x89PNG",
		"docs/favicon.ico":     "\x00",
		"docs/assets/film.mp4": "\x00",
	})
	got, err := publicFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{
		"README.md", "charts/agent-kourier/README.md", "charts/agent-kourier/values.yaml",
		"docs/_headers", "docs/assets/diagrams/map.svg", "docs/assets/film/captions.vtt", "docs/data.json",
		"docs/how-to/a.md", "docs/how-to/brand/kept.md", "docs/index.md", "docs/notes.txt", "docs/page.html",
		"docs/site.webmanifest", "docs/stylesheets/brand.css",
		"examples/recipes/README.md", "examples/recipes/sre/README.md", "examples/recipes/sre/agent/README.md",
		"examples/recipes/sre/evals/triggers.yaml", "examples/recipes/sre/kourier/binding.yaml", "examples/recipes/sre/kourier/other.yml",
		"hack/docs/overrides/main.html", "hack/docs/overrides/partials/logo.html",
	}
	if !slices.Equal(got, want) {
		t.Errorf("publicFiles =\n%v\nwant\n%v", got, want)
	}
}

func TestPublicFilesFailsWhenAPublicFileIsMissing(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{"mkdocs.yml": testMkdocs, "README.md": "x\n", "docs/index.md": "x\n"})
	if _, err := publicFiles(root); err == nil {
		t.Error("no error for a missing chart README")
	}
}

func TestActiveRulesRetireAFieldTheAPIHas(t *testing.T) {
	flagged := func(rs []rule, text string) bool {
		return len(scanText(rs, "docs/x.md", text)) > 0
	}
	none := activeRules(map[string]bool{})
	for _, name := range []string{"approverGroups", "toolApprovals", "directMessages"} {
		if !flagged(none, "names "+name+" here") {
			t.Errorf("%s is not flagged while the API lacks it", name)
		}
	}
	one := activeRules(map[string]bool{"toolApprovals": true})
	if flagged(one, "Set `toolApprovals: true`.") {
		t.Error("toolApprovals is flagged although the API has it")
	}
	if !flagged(one, "Set `approverGroups`.") || !flagged(one, "A `directMessages` block.") {
		t.Error("a field the API still lacks stopped being flagged")
	}
	all := activeRules(map[string]bool{"toolApprovals": true, "approverGroups": true, "directMessages": true})
	for _, ru := range all {
		if ru.id == "unbuilt-field" {
			t.Error("the unbuilt-field rule stays when the API has every one of its fields")
		}
	}
	if flagged(all, "toolApprovals approverGroups directMessages") {
		t.Error("a field of the API is flagged")
	}
	// The other rules are untouched.
	if !flagged(all, "WP-1") {
		t.Error("retiring a field dropped another rule")
	}
}

func TestAPIFieldNames(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"api/v1alpha1/types.go": "package v1alpha1\n\n// I names the field with `json:\"directMessages\"` in a comment, which is not a tag.\n// Another: `json:\"approverGroups\"`.\ntype I struct {\n\tTools bool `json:\"toolApprovals,omitempty\"`\n\tName string `json:\"name\"`\n\tSkip string `json:\"-\"`\n}\n",
		"api/v1alpha1/more.go":  "package v1alpha1\n\ntype J struct {\n\tG []string `json:\"approverGroups\"`\n}\n",
		// A test file's tags, a subdirectory and a non-Go file are not the API.
		"api/v1alpha1/types_test.go": "package v1alpha1\n\ntype K struct {\n\tD int `json:\"directMessages\"`\n}\n",
		"api/v1alpha1/testdata/x.go": "package x\n\ntype K struct {\n\tD int `json:\"directMessages\"`\n}\n",
		"api/v1alpha1/notes.txt":     "`json:\"directMessages\"`\n",
	})
	got, err := apiFieldNames(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"toolApprovals", "name", "approverGroups"} {
		if !got[want] {
			t.Errorf("apiFieldNames lacks %q: %v", want, got)
		}
	}
	for _, not := range []string{"directMessages", "-", ""} {
		if got[not] {
			t.Errorf("apiFieldNames holds %q: %v", not, got)
		}
	}
	if _, err := apiFieldNames(t.TempDir()); err == nil {
		t.Error("no error for a tree without the API package")
	}
	// A comment that looks like a tag does not retire a field.
	only := t.TempDir()
	writeTree(t, only, map[string]string{
		"api/v1alpha1/types.go": "package v1alpha1\n\n// Set `json:\"toolApprovals\"` here.\ntype I struct {\n\tName string `json:\"name\"`\n}\n",
	})
	got, err = apiFieldNames(only)
	if err != nil {
		t.Fatal(err)
	}
	if got["toolApprovals"] || !got["name"] {
		t.Errorf("apiFieldNames = %v, want name only", got)
	}
	// A Go file that does not parse is an error, not a silently empty API.
	broken := t.TempDir()
	writeTree(t, broken, map[string]string{"api/v1alpha1/types.go": "package v1alpha1\n\ntype {{{\n"})
	if _, err := apiFieldNames(broken); err == nil {
		t.Error("no error for an API file that does not parse")
	}
}

func TestAllowlist(t *testing.T) {
	findings := []finding{
		{path: "docs/a.md", line: 3, rule: "planning-id", text: "Removed in WP-9 (see upgrade)."},
		{path: "docs/a.md", line: 8, rule: "planning-id", text: "Also WP-10."},
		{path: "docs/b.md", line: 1, rule: "planning-id", text: "WP-9 again."},
	}
	const list = "# a comment\n\n" +
		"docs/a.md | planning-id | WP-9 | the upgrade note names the old ID so a reader can find it | and more\n"
	entries, bad := parseAllowlist("allowlist.txt", list)
	if len(bad) != 0 {
		t.Fatalf("parse errors: %v", bad)
	}
	left := applyAllowlist(findings, entries, "allowlist.txt")
	if len(left) != 2 || left[0].line != 8 || left[1].path != "docs/b.md" {
		t.Errorf("left = %v, want the line 8 finding and the one in docs/b.md", left)
	}
}

func TestAllowlistRejectsBadEntries(t *testing.T) {
	tests := []struct {
		name, list, want string
	}{
		{"no reason", "docs/a.md | planning-id | WP-9\n", "needs"},
		{"a blank reason", "docs/a.md | planning-id | WP-9 |   \n", "reason"},
		{"an unknown rule", "docs/a.md | no-such-rule | WP-9 | because\n", "unknown rule"},
		{"a blank needle", "docs/a.md | planning-id |  | because\n", "needs"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, bad := parseAllowlist("allowlist.txt", tt.list)
			if len(bad) != 1 || !strings.Contains(bad[0].String(), tt.want) || !strings.HasPrefix(bad[0].String(), "allowlist.txt:1:") {
				t.Errorf("parseAllowlist = %v, want one finding at allowlist.txt:1 containing %q", bad, tt.want)
			}
		})
	}
}

// TestAllowlistExcusesTheMatchItNamesNotTheLine is the review probe: an entry for the Cloudflare product R2 must not
// hide a real risk ID on the same line.
func TestAllowlistExcusesTheMatchItNamesNotTheLine(t *testing.T) {
	root := t.TempDir()
	files := map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            apiTypes,
		"README.md":                        "Cloudflare (R2) stores it, see also R1\n",
		"charts/agent-kourier/README.md":   "Fine.\n",
		"charts/agent-kourier/values.yaml": "a: 1\n",
		"docs/index.md":                    "# Home\n",
		allowlistPath:                      "README.md | risk-id | Cloudflare (R2) | the object store is Cloudflare R2, not a risk\n",
	}
	writeTree(t, root, files)
	var out bytes.Buffer
	if code := run(root, &out); code != 1 {
		t.Fatalf("run = %d, want 1 for the real R1; output:\n%s", code, out.String())
	}
	if !strings.Contains(out.String(), `README.md:1: risk-id: "R1"`) {
		t.Errorf("output lacks the R1 finding:\n%s", out.String())
	}
	if strings.Contains(out.String(), `"R2"`) {
		t.Errorf("the allowlisted R2 is reported:\n%s", out.String())
	}

	// Naming both excuses both.
	files[allowlistPath] += "README.md | risk-id | see also R1 | a cross reference the page means\n"
	writeTree(t, root, files)
	out.Reset()
	if code := run(root, &out); code != 0 {
		t.Errorf("run = %d, want 0 with both excused; output:\n%s", code, out.String())
	}
}

func TestAllowlistExcusesALinkFindingByItsTarget(t *testing.T) {
	findings := []finding{
		{path: "README.md", line: 1, rule: "dead-link", text: "[a](docs/old.md) and [b](docs/gone.md)", start: 4, end: 15},
		{path: "README.md", line: 1, rule: "dead-link", text: "[a](docs/old.md) and [b](docs/gone.md)", start: 25, end: 37},
	}
	entries, bad := parseAllowlist("allowlist.txt", "README.md | dead-link | [a](docs/old.md) | a link kept for the old URL\n")
	if len(bad) != 0 {
		t.Fatal(bad)
	}
	left := applyAllowlist(findings, entries, "allowlist.txt")
	if len(left) != 1 || left[0].start != 25 {
		t.Errorf("left = %v, want only the second link", left)
	}
}

func TestAllowlistEntryThatMatchesNothingIsAFinding(t *testing.T) {
	entries, bad := parseAllowlist("allowlist.txt", "docs/a.md | planning-id | WP-9 | a reason\n")
	if len(bad) != 0 {
		t.Fatal(bad)
	}
	left := applyAllowlist(nil, entries, "allowlist.txt")
	if len(left) != 1 || left[0].rule != "stale-allowlist" || left[0].line != 1 {
		t.Errorf("left = %v, want one stale-allowlist finding at line 1", left)
	}
}

func TestRunExitsNonZeroAndListsEveryFinding(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            apiTypes,
		"README.md":                        "See [gone](docs/gone.md) and WP-3.\n",
		"charts/agent-kourier/README.md":   "fine\n",
		"charts/agent-kourier/values.yaml": "# webhookPath here\n",
		"docs/index.md":                    "Reads (D33).\n",
		"docs/brand/notes.md":              "WP-1 C0EXAMPLE01\n",
	})
	var out bytes.Buffer
	if code := run(root, &out); code != 1 {
		t.Fatalf("run = %d, want 1; output:\n%s", code, out.String())
	}
	for _, want := range []string{
		"README.md:1: planning-id", "README.md:1: dead-link",
		"charts/agent-kourier/values.yaml:1: webhook-path",
		"docs/index.md:1: decision-id",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "docs/brand") {
		t.Errorf("output names an excluded directory:\n%s", out.String())
	}
}

func TestRunHonoursTheAllowlist(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            apiTypes,
		"README.md":                        "Fine.\n",
		"charts/agent-kourier/README.md":   "Fine.\n",
		"charts/agent-kourier/values.yaml": "a: 1\n",
		"docs/index.md":                    "Names `webhookPath` on purpose.\n",
		allowlistPath:                      "docs/index.md | webhook-path | webhookPath | the upgrade note names the removed field\n",
	})
	var out bytes.Buffer
	if code := run(root, &out); code != 0 {
		t.Fatalf("run = %d, want 0; output:\n%s", code, out.String())
	}
}

func TestRunDoesNotFlagAFieldTheAPIHas(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            "package v1alpha1\n\ntype I struct {\n\tT bool `json:\"toolApprovals\"`\n}\n",
		"README.md":                        "Fine.\n",
		"charts/agent-kourier/README.md":   "Fine.\n",
		"charts/agent-kourier/values.yaml": "a: 1\n",
		"docs/index.md":                    "Set `toolApprovals` and `approverGroups`.\n",
	})
	var out bytes.Buffer
	if code := run(root, &out); code != 1 {
		t.Fatalf("run = %d, want 1; output:\n%s", code, out.String())
	}
	if strings.Contains(out.String(), "toolApprovals") || !strings.Contains(out.String(), `"approverGroups"`) {
		t.Errorf("output should flag approverGroups only:\n%s", out.String())
	}
}

func TestRunScansPublishedFilesThatAreNotMarkdown(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            apiTypes,
		"README.md":                        "Fine.\n",
		"charts/agent-kourier/README.md":   "Fine.\n",
		"charts/agent-kourier/values.yaml": "# fine\n",
		"docs/index.md":                    "# Home\n",
		"docs/assets/film/captions.vtt":    "WEBVTT\n\n00:00.000 --> 00:01.000\nThe broker wakes it if needed.\n",
		"hack/docs/overrides/main.html":    "<p>/Users/someone</p>\n",
	})
	var out bytes.Buffer
	if code := run(root, &out); code != 1 {
		t.Fatalf("run = %d, want 1; output:\n%s", code, out.String())
	}
	for _, want := range []string{"docs/assets/film/captions.vtt:4: wrong-claim", "hack/docs/overrides/main.html:1: home-path"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

func TestRunChecksTheRecipeREADMEs(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            apiTypes,
		"README.md":                        "Fine.\n",
		"charts/agent-kourier/README.md":   "Fine.\n",
		"charts/agent-kourier/values.yaml": "# fine\n",
		"docs/index.md":                    "# Home\n",
		"examples/recipes/README.md":       "# Recipes\n[docs](../../docs/index.md) [gone](../../docs/gone.md)\n",
		"examples/recipes/a/README.md":     "Tracked in WP-1.\n[up](../README.md#recipes) [bad](../README.md#nope)\n",
		"examples/recipes/a/notes.md":      "WP-1 [gone](gone.md)\n", // not a README: not a public page
	})
	var out bytes.Buffer
	if code := run(root, &out); code != 1 {
		t.Fatalf("run = %d, want 1; output:\n%s", code, out.String())
	}
	for _, want := range []string{
		"examples/recipes/README.md:2: dead-link", "examples/recipes/a/README.md:1: ", "examples/recipes/a/README.md:2: dead-anchor",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "notes.md") || strings.Contains(out.String(), "recipes/README.md:1") {
		t.Errorf("output names a file or line that is clean or not public:\n%s", out.String())
	}
	// An allowlist entry excuses a finding in a recipe README, and a stale one is still a finding.
	writeTree(t, root, map[string]string{"hack/docs/contentcheck/allowlist.txt": "examples/recipes/README.md | dead-link | ../../docs/gone.md | an example of a dead link\n" +
		"examples/recipes/a/README.md | dead-link | nothing.md | stale\n"})
	out.Reset()
	run(root, &out)
	if strings.Contains(out.String(), "recipes/README.md:2") || !strings.Contains(out.String(), "matches nothing") {
		t.Errorf("allowlist handling for a recipe README:\n%s", out.String())
	}
}

func TestRunScansTheRecipeYAML(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            apiTypes,
		"README.md":                        "Fine.\n",
		"charts/agent-kourier/README.md":   "Fine.\n",
		"charts/agent-kourier/values.yaml": "# fine\n",
		"docs/index.md":                    "# Home\n",
		// An eval pasted from a real conversation can carry a home path; a Binding can carry a plan ID.
		"examples/recipes/a/evals/triggers.yaml":  "cases:\n  - name: x\n    message: {text: '/Users/someone/work'}\n",
		"examples/recipes/a/kourier/binding.yaml": "# see WP-1\nkind: Binding\n",
		"examples/recipes/a/kourier/clean.yaml":   "kind: Binding\n",
		"examples/recipes/a/agent/not-scanned.go": "package x // WP-1\n",
	})
	var out bytes.Buffer
	if code := run(root, &out); code != 1 {
		t.Fatalf("run = %d, want 1; output:\n%s", code, out.String())
	}
	for _, want := range []string{"examples/recipes/a/evals/triggers.yaml:3: home-path", "examples/recipes/a/kourier/binding.yaml:1: planning-id"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
	for _, not := range []string{"clean.yaml", "not-scanned.go"} {
		if strings.Contains(out.String(), not) {
			t.Errorf("output names %s:\n%s", not, out.String())
		}
	}
}

func TestRunExitsZeroOnACleanTree(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"mkdocs.yml":                       testMkdocs,
		"api/v1alpha1/types.go":            apiTypes,
		"README.md":                        "[docs](docs/index.md#home)\n",
		"charts/agent-kourier/README.md":   "[top](../../README.md)\n",
		"charts/agent-kourier/values.yaml": "# image settings\nimage: {}\n",
		"docs/index.md":                    "# Home\n",
	})
	var out bytes.Buffer
	if code := run(root, &out); code != 0 {
		t.Errorf("run = %d, want 0; output:\n%s", code, out.String())
	}
	if out.Len() != 0 {
		t.Errorf("output on a clean tree: %q", out.String())
	}
}

func TestRunReportsAnUnreadableTree(t *testing.T) {
	var out bytes.Buffer
	if code := run(t.TempDir(), &out); code != 2 {
		t.Errorf("run on an empty directory = %d, want 2; output: %s", code, out.String())
	}
}

// TestPublicDocsAreClean runs the check over the real repository, so `go test ./...` fails on a finding.
func TestPublicDocsAreClean(t *testing.T) {
	got, err := check("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		t.Errorf("%s", f)
	}
}
