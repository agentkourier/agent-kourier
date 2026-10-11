package main

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"

	yaml "sigs.k8s.io/yaml/goyaml.v3"
)

// valuesReference returns the Markdown page of a Helm chart's values file: a section for each top-level key, with its
// comment, and its default or a table of the keys under it. from names the file in the page's text.
func valuesReference(src []byte, from string) (string, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(src, &doc); err != nil {
		return "", fmt.Errorf("parse: %w", err)
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode {
		return "", fmt.Errorf("parse: the file is not one mapping")
	}
	var b strings.Builder
	b.WriteString("# Helm values\n\n")
	fmt.Fprintf(&b, "This page lists every value of the `agent-kourier` Helm chart, with its default and the comment `%s` gives it.\n", from)
	top := doc.Content[0]
	for i := 0; i+1 < len(top.Content); i += 2 {
		key, val := top.Content[i], top.Content[i+1]
		b.WriteString("\n## " + key.Value + "\n\n")
		if c := comment(key.HeadComment); c != "" {
			b.WriteString(prose(c) + "\n\n")
		}
		if val.Kind == yaml.MappingNode && val.Style&yaml.FlowStyle == 0 && len(val.Content) > 0 {
			b.WriteString("| Key | Default | Description |\n|---|---|---|\n")
			rows(&b, key.Value, val)
			continue
		}
		fmt.Fprintf(&b, "Default: `%s`\n", inline(val))
		if c := comment(key.LineComment + "\n" + val.LineComment); c != "" {
			b.WriteString("\n" + escapeText(oneLine(c), false) + "\n")
		}
	}
	b.WriteString(generatedNote(from))
	return b.String(), nil
}

// rows writes a table row for each key under m, and for a nested block mapping, a row for each key under that.
func rows(b *strings.Builder, prefix string, m *yaml.Node) {
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i], m.Content[i+1]
		path := prefix + "." + key.Value
		desc := escapeText(oneLine(comment(key.HeadComment+"\n"+key.LineComment+"\n"+val.LineComment)), true)
		if val.Kind == yaml.MappingNode && val.Style&yaml.FlowStyle == 0 && len(val.Content) > 0 {
			if desc != "" {
				fmt.Fprintf(b, "| `%s` |  | %s |\n", path, desc)
			}
			rows(b, path, val)
			continue
		}
		fmt.Fprintf(b, "| `%s` | `%s` | %s |\n", path, inline(val), desc)
	}
}

// inline is a value as one line of YAML.
func inline(n *yaml.Node) string {
	if n.Kind == yaml.ScalarNode {
		if n.Tag == "!!str" && n.Value == "" {
			return `""`
		}
		if n.Tag == "!!null" {
			return "null"
		}
		return n.Value
	}
	c := *n
	c.Style |= yaml.FlowStyle
	c.HeadComment, c.LineComment, c.FootComment = "", "", ""
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&c); err != nil {
		return "?"
	}
	return strings.TrimSpace(buf.String())
}

// comment is a YAML comment's text without its markers.
func comment(c string) string {
	var lines []string
	for line := range strings.SplitSeq(c, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "#")
		line = strings.TrimPrefix(line, " ")
		lines = append(lines, line)
	}
	return strings.TrimRight(strings.Join(lines, "\n"), "\n ")
}

// oneLine joins a comment's lines, without its maintainer references, for a table cell.
func oneLine(c string) string {
	return readerText(strings.Join(strings.Fields(c), " "))
}

var exampleKey = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9.-]*:$`)

// prose writes a head comment as paragraphs, and its indented lines (and a "key:" line that opens them) as YAML code.
func prose(c string) string {
	lines := strings.Split(c, "\n")
	isCode := func(i int) bool {
		if strings.HasPrefix(lines[i], "  ") {
			return true
		}
		return exampleKey.MatchString(lines[i]) && i+1 < len(lines) && strings.HasPrefix(lines[i+1], "  ")
	}
	var out []string
	var para, code []string
	flush := func() {
		if len(para) > 0 {
			out = append(out, escapeText(readerText(strings.Join(para, " ")), false))
			para = nil
		}
		if len(code) > 0 {
			out = append(out, "```yaml\n"+strings.Join(dedent(code), "\n")+"\n```")
			code = nil
		}
	}
	for i, line := range lines {
		switch {
		case line == "":
			flush()
		case isCode(i):
			if len(para) > 0 {
				flush()
			}
			code = append(code, line)
		default:
			if len(code) > 0 {
				flush()
			}
			para = append(para, line)
		}
	}
	flush()
	return strings.Join(out, "\n\n")
}

// dedent removes the indentation that every line of a code block shares.
func dedent(lines []string) []string {
	n := -1
	for _, l := range lines {
		if ind := len(l) - len(strings.TrimLeft(l, " ")); n < 0 || ind < n {
			n = ind
		}
	}
	out := make([]string, len(lines))
	for i, l := range lines {
		out[i] = l[n:]
	}
	return out
}
