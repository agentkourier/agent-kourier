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
			text: `{{ printf "%s/%s" .Channel .Fields.a | html }} {{ slice .Key 0 10 }} {{ len .State }}`,
			want: PromptRefs{View: []string{"Channel", "Key", "State"}, Fields: []string{"a"}},
		},
		{
			name: "a variable that holds a checked reference",
			text: `{{ $k := .Key }}{{ $k }} {{ $x := index .Fields "a" }}{{ $x }}`,
			want: PromptRefs{View: []string{"Key"}, Fields: []string{"a"}},
		},
		{
			name: "a defined template is walked too",
			text: `{{ define "x" }}{{ .Fields.a }}{{ end }}{{ template "x" .Key }}`,
			want: PromptRefs{View: []string{"Key"}, Fields: []string{"a"}},
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
			name:    "unknown top-level field in a defined template",
			text:    `{{ define "x" }}{{ .Stat }}{{ end }}x`,
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
			wantErr: "name an entry",
		},
		{
			name:    "with .Fields",
			text:    "{{ with .Fields }}{{ .alertname }}{{ end }}",
			wantErr: "name an entry",
		},
		{
			name:    "bare .Fields passed to a function",
			text:    `{{ printf "%v" .Fields }}`,
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
			name:    "bare .Fields stored in a variable",
			text:    "{{ $f := .Fields }}{{ $f.alertname }}",
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
			text:    `{{ if .State }}{{ printf "%v" . }}{{ end }}`,
			wantErr: "the whole prompt view",
		},
		{
			name:    "the whole view as $",
			text:    "{{ $ }}",
			wantErr: "the whole prompt view",
		},
		{
			name:    "the whole view passed to a template",
			text:    `{{ define "x" }}x{{ end }}{{ template "x" . }}`,
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
