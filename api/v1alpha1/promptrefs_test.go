package v1alpha1

import (
	"reflect"
	"strings"
	"testing"
)

func TestTemplateRefs(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		want    PromptRefs
		wantErr string // a substring; empty means the template is accepted
	}{
		{
			name: "no references",
			text: "Investigate the alert.",
			want: PromptRefs{},
		},
		{
			name: "every view field and two entries, repeated and out of order",
			text: "{{ .State }} {{ .Channel }} {{ .Fields.b }} {{ .Fields.a }} {{ .Fields.b }} {{ .Link }} {{ .Key }} {{ .Source }} {{ .State }}",
			want: PromptRefs{View: []string{"Channel", "Key", "Link", "Source", "State"}, Fields: []string{"a", "b"}},
		},
		{
			name: "a reference inside a branch the placeholder view does not take",
			text: `{{ if eq .State "FIRING" }}{{ .Fields.alertname }}{{ else }}{{ .Link }}{{ end }}`,
			want: PromptRefs{View: []string{"Link", "State"}, Fields: []string{"alertname"}},
		},
		{
			name: "index with a constant name, quoted and raw",
			text: "{{ index .Fields \"a\" }} {{ index .Fields `b` }}",
			want: PromptRefs{Fields: []string{"a", "b"}},
		},
		{
			name: "index through $ with a constant name",
			text: `{{ index $.Fields "a" }} {{ with .Key }}{{ index $.Fields "b" }}{{ end }}`,
			want: PromptRefs{View: []string{"Key"}, Fields: []string{"a", "b"}},
		},
		{
			name: "references from the root inside with",
			text: `{{ with .Key }}{{ $.Fields.a }}{{ end }}{{ $.State }}`,
			want: PromptRefs{View: []string{"Key", "State"}, Fields: []string{"a"}},
		},
		{
			name: "references as arguments of functions and in pipelines",
			text: `{{ slice .Key 0 10 }} {{ len .Fields.a }} {{ .State | len }} {{ index .Channel 0 }}`,
			want: PromptRefs{View: []string{"Channel", "Key", "State"}, Fields: []string{"a"}},
		},
		{
			name: "every function that is allowed, in conditions",
			text: `{{ if and (eq .State "a") (ne .Key "b") (not (lt .Link "c")) }}{{ .Source }}{{ else if or (le .Channel "d") (gt .Link "e") (ge .Key "f") }}{{ .Fields.a }}{{ end }}`,
			want: PromptRefs{View: []string{"Channel", "Key", "Link", "Source", "State"}, Fields: []string{"a"}},
		},
		{
			name: "a comment and a constant of each kind",
			text: `{{/* a note */}}{{ if eq .State "a" }}x{{ end }}{{ if eq (len .State) 3 }}x{{ end }}{{ if eq true false }}x{{ end }}`,
			want: PromptRefs{View: []string{"State"}},
		},

		{
			name:    "unknown top-level field inside a branch",
			text:    `{{ if eq .State "FIRING" }}{{ .Stat }}{{ end }}`,
			wantErr: `promptTemplate:1:`,
		},
		{
			name:    "unknown top-level field names the fields there are",
			text:    "{{ .Severity }}",
			wantErr: "no field .Severity in the prompt view (it has .Channel, .State, .Key, .Link, .Source and .Fields)",
		},
		{
			name:    "a view field is matched by case",
			text:    "{{ .state }}",
			wantErr: "no field .state",
		},
		{
			name:    "unknown top-level field through $",
			text:    "{{ $.Stat }}",
			wantErr: "no field .Stat",
		},
		{
			name:    "bare .Fields",
			text:    "{{ .Fields }}",
			wantErr: "name an entry",
		},
		{
			name:    "range over .Fields",
			text:    "{{ range $k, $v := .Fields }}{{ $v }}{{ end }}",
			wantErr: "range is not allowed",
		},
		{
			name:    "with .Fields",
			text:    "{{ with .Fields }}{{ .alertname }}{{ end }}",
			wantErr: "name an entry",
		},
		{
			name:    "bare .Fields passed to a function",
			text:    `{{ len .Fields }}`,
			wantErr: "name an entry",
		},
		{
			name:    "bare .Fields in parentheses",
			text:    `{{ (.Fields).alertname }}`,
			wantErr: "name an entry",
		},
		{
			name:    "bare .Fields through $",
			text:    "{{ $.Fields }}",
			wantErr: "name an entry",
		},
		{
			name:    "index with a name that is not constant",
			text:    "{{ index .Fields .Key }}",
			wantErr: "index .Fields takes one constant name",
		},
		{
			name:    "index with a piped name",
			text:    `{{ "a" | index .Fields }}`,
			wantErr: "index .Fields takes one constant name",
		},
		{
			name:    "index with more than a name",
			text:    `{{ index .Fields "a" 0 }}`,
			wantErr: "index .Fields takes one constant name",
		},
		{
			name:    "the whole view as dot",
			text:    "{{ . }}",
			wantErr: "the whole prompt view",
		},
		{
			name:    "dot inside with, where it would be text, is refused as well",
			text:    "{{ with .Key }}{{ . }}{{ end }}",
			wantErr: "the whole prompt view",
		},
		{
			name:    "the whole view as dot inside a branch",
			text:    `{{ if .State }}{{ len . }}{{ end }}`,
			wantErr: "the whole prompt view",
		},
		{
			name:    "the whole view as $",
			text:    "{{ $ }}",
			wantErr: "the whole prompt view",
		},
		{
			name:    "a field of a text value",
			text:    "{{ .Fields.alertname.Name }}",
			wantErr: "holds text, which has no field",
		},
		{
			name:    "a field of a text view field",
			text:    "{{ .State.Name }}",
			wantErr: "holds text, which has no field",
		},
		{
			name:    "range over a number",
			text:    "{{ range 3 }}x{{ end }}",
			wantErr: "promptTemplate:1:9: range is not allowed",
		},
		{
			name:    "range with an else",
			text:    "{{ range .State }}x{{ else }}y{{ end }}",
			wantErr: "range is not allowed",
		},
		{
			name:    "range in the branch of an if",
			text:    "{{ if .State }}{{ range 3 }}x{{ end }}{{ end }}",
			wantErr: "range is not allowed",
		},
		{
			name:    "define",
			text:    `{{ define "x" }}x{{ end }}y`,
			wantErr: "define is not allowed",
		},
		{
			name:    "define holding a bad reference is refused as a define",
			text:    `{{ define "x" }}{{ .Stat }}{{ end }}x`,
			wantErr: "define is not allowed",
		},
		{
			name:    "template",
			text:    `{{ define "x" }}x{{ end }}{{ template "x" }}`,
			wantErr: "template and block are not allowed",
		},
		{
			name:    "template of a name that is never defined",
			text:    `{{ template "x" }}`,
			wantErr: "template and block are not allowed",
		},
		{
			name:    "block",
			text:    `{{ block "x" .Key }}x{{ end }}`,
			wantErr: "template and block are not allowed",
		},
		{
			name:    "a variable declared",
			text:    `{{ $k := .Key }}{{ $k }}`,
			wantErr: "promptTemplate:1:3: a variable is not allowed",
		},
		{
			name:    "a variable assigned",
			text:    `{{ $k := "x" }}{{ $k = "y" }}`,
			wantErr: "a variable is not allowed",
		},
		{
			name:    "a variable declared by if",
			text:    `{{ if $k := .Key }}x{{ end }}`,
			wantErr: "a variable is not allowed",
		},
		{
			name:    "a variable declared by with",
			text:    `{{ with $k := .Key }}x{{ end }}`,
			wantErr: "a variable is not allowed",
		},
		{
			name:    "a variable declared in parentheses",
			text:    `{{ len ($k := .Key) }}`,
			wantErr: "a variable is not allowed",
		},
		{
			name:    "print",
			text:    `{{ print .Key .Key }}`,
			wantErr: "function print is not allowed",
		},
		{
			name:    "printf, which a width specifier makes allocate",
			text:    `{{ printf "%1000000000d" 1 }}`,
			wantErr: "function printf is not allowed",
		},
		{
			name:    "println",
			text:    `{{ println .Key }}`,
			wantErr: "function println is not allowed",
		},
		{
			name:    "print at the end of a pipeline",
			text:    `{{ .Key | print }}`,
			wantErr: "function print is not allowed",
		},
		{
			name:    "print as the condition of an if",
			text:    `{{ if print .Key }}x{{ end }}`,
			wantErr: "function print is not allowed",
		},
		{
			name:    "print inside parentheses",
			text:    `{{ len (print .Key) }}`,
			wantErr: "function print is not allowed",
		},
		{
			name:    "print as a bare argument",
			text:    `{{ eq .State print }}`,
			wantErr: "function print is not allowed",
		},
		{
			name:    "html",
			text:    `{{ html .Key }}`,
			wantErr: "function html is not allowed",
		},
		{
			name:    "js, which a pipeline of them makes grow without bound",
			text:    `{{ .Key | js | js | js }}`,
			wantErr: "function js is not allowed",
		},
		{
			name:    "urlquery",
			text:    `{{ urlquery .Key }}`,
			wantErr: "function urlquery is not allowed",
		},
		{
			name:    "call",
			text:    `{{ call .Key }}`,
			wantErr: "function call is not allowed",
		},
		{
			name:    "the allowed functions are named",
			text:    `{{ print .Key }}`,
			wantErr: "use only and, eq, ge, gt, index, le, len, lt, ne, not, or and slice",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmpl, err := ParsePromptTemplate("promptTemplate", tt.text)
			if err != nil {
				t.Fatalf("the test's template does not parse: %v", err)
			}
			got, err := TemplateRefs(tmpl)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("TemplateRefs() = %+v, %v; want an error containing %q", got, err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("TemplateRefs() error: %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("TemplateRefs() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTemplateRefsLocatesTheProblem(t *testing.T) {
	tmpl, err := ParsePromptTemplate("promptTemplate", "line one\nline {{ .Stat }}\n")
	if err != nil {
		t.Fatal(err)
	}
	_, err = TemplateRefs(tmpl)
	if err == nil || !strings.HasPrefix(err.Error(), "promptTemplate:2:") {
		t.Errorf("error = %v, want it to start with the location promptTemplate:2:", err)
	}
}

func TestTemplateRefsEmptyTemplate(t *testing.T) {
	tmpl, err := ParsePromptTemplate("promptTemplate", "")
	if err != nil {
		t.Fatal(err)
	}
	got, err := TemplateRefs(tmpl)
	if err != nil || !reflect.DeepEqual(got, PromptRefs{}) {
		t.Errorf("TemplateRefs(empty) = %+v, %v; want no references and no error", got, err)
	}
}

// TestPromptViewFieldsAreKnown ties the walk's list of view fields to the type, so a field added
// to PromptView is accepted by the walk, and the error message names it.
func TestPromptViewFieldsAreKnown(t *testing.T) {
	text := ""
	rt := reflect.TypeOf(PromptView{})
	for i := range rt.NumField() {
		name := rt.Field(i).Name
		if name == "Fields" {
			text += "{{ .Fields.x }}"
			continue
		}
		text += "{{ ." + name + " }}"
	}
	tmpl, err := ParsePromptTemplate("promptTemplate", text)
	if err != nil {
		t.Fatal(err)
	}
	got, err := TemplateRefs(tmpl)
	if err != nil {
		t.Fatalf("TemplateRefs() error: %v", err)
	}
	if len(got.View) != rt.NumField()-1 || len(got.Fields) != 1 {
		t.Errorf("TemplateRefs() = %+v, want every PromptView field but Fields, and one entry", got)
	}
}
