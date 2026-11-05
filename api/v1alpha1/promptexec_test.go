package v1alpha1

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"text/template"
	"time"
)

// within runs f and fails the test, without waiting for f, if it has not returned after d. A
// template that hangs is the fault these tests guard against, so a hang must be a failure and
// not a stuck test run.
func within(t *testing.T, d time.Duration, f func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f() }()
	select {
	case err := <-done:
		return err
	case <-time.After(d):
		t.Fatalf("still running after %s: the template was run, not refused", d)
		return nil
	}
}

// TestHostileTemplatesAreRefusedWithoutRunning holds the templates that, run, never return or
// allocate until the process dies. Each must be refused by the walk, in a moment, and none may
// be run.
func TestHostileTemplatesAreRefusedWithoutRunning(t *testing.T) {
	tests := []struct {
		name, text, wantErr string
	}{
		{"a range over an integer that does not end", `{{range 9000000000000000}}{{end}}x`, "range is not allowed"},
		{"a string doubled in a variable", `{{$x := "xxxx"}}{{range 40}}{{$x = print $x $x}}{{end}}x`, "a variable is not allowed"},
		{"the same with the variable made where the range is", `{{range 40}}{{$x := "xxxx"}}{{end}}x`, "range is not allowed"},
		{"the doubling without the range", `{{$x := "xxxx"}}{{$x = print $x $x}}{{$x = print $x $x}}x`, "a variable is not allowed"},
		{"a template that calls itself", `{{define "a"}}{{template "a"}}{{end}}{{template "a"}}`, "define is not allowed"},
		{"a block that calls itself", `{{block "a" .}}{{template "a" .}}{{end}}`, "define is not allowed"},
		{"a width the format allocates", `{{printf "%2000000000d" 1}}`, "function printf is not allowed"},
		{"a width taken from an argument", `{{printf "%*d" 2000000000 1}}`, "function printf is not allowed"},
		{"a long pipeline of js, each stage up to six times longer", `{{.State` + strings.Repeat(" | js", 60) + `}}`, "function js is not allowed"},
		{"a long pipeline of html", `{{.State` + strings.Repeat(" | html", 60) + `}}`, "function html is not allowed"},
		{"a long pipeline of urlquery", `{{.State` + strings.Repeat(" | urlquery", 60) + `}}`, "function urlquery is not allowed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := within(t, 5*time.Second, func() error { return checkPromptTemplate("promptTemplate", tt.text, nil) })
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("checkPromptTemplate() = %v, want an error containing %q", err, tt.wantErr)
			}
			tmpl, err := ParsePromptTemplate("promptTemplate", tt.text)
			if err != nil {
				t.Fatalf("the test's template does not parse: %v", err)
			}
			err = within(t, 5*time.Second, func() error { _, err := TemplateRefs(tmpl); return err })
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("TemplateRefs() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestPermittedTemplatesStillRun holds that the walk's allow-list leaves the templates that
// operators write: conditions, with, entries by name and by index, slice and len.
func TestPermittedTemplatesStillRun(t *testing.T) {
	extract := map[string]Extract{"alertname": {}, "state": {}}
	for _, text := range []string{
		"Investigate {{ .Fields.alertname }} in {{ .Channel }} ({{ .State }}).",
		`{{ if eq .State "FIRING" }}firing {{ index .Fields "alertname" }}{{ else if .Link }}{{ .Link }}{{ else }}none{{ end }}`,
		`{{ with .Fields.alertname }}{{ $.Fields.state }}{{ end }}`,
		`{{ if and (ne .State "") (gt (len .Key) 3) (not (eq .Source "x")) }}{{ slice .Key 0 10 }}{{ end }}`,
		"{{/* a note */}}",
	} {
		if err := checkPromptTemplate("promptTemplate", text, extract); err != nil {
			t.Errorf("checkPromptTemplate(%q) = %v, want none", text, err)
		}
	}
}

func TestExecutePromptOutputLimit(t *testing.T) {
	parse := func(t *testing.T, text string) *template.Template {
		t.Helper()
		tmpl, err := ParsePromptTemplate("promptTemplate", text)
		if err != nil {
			t.Fatal(err)
		}
		return tmpl
	}
	t.Run("output of exactly the limit is returned", func(t *testing.T) {
		out, err := ExecutePrompt(parse(t, "{{ .State }}"), PromptView{State: strings.Repeat("x", MaxPromptOutput)})
		if err != nil || len(out) != MaxPromptOutput {
			t.Fatalf("ExecutePrompt() = %d bytes, %v; want %d bytes", len(out), err, MaxPromptOutput)
		}
	})
	t.Run("output one byte over the limit fails and returns nothing", func(t *testing.T) {
		out, err := ExecutePrompt(parse(t, "{{ .State }}"), PromptView{State: strings.Repeat("x", MaxPromptOutput+1)})
		if !errors.Is(err, ErrPromptOutputTooLarge) || out != "" {
			t.Fatalf("ExecutePrompt() = %d bytes, %v; want ErrPromptOutputTooLarge and no output", len(out), err)
		}
	})
	t.Run("output built up from several writes fails at the write that crosses", func(t *testing.T) {
		half := strings.Repeat("x", MaxPromptOutput/2)
		_, err := ExecutePrompt(parse(t, "{{ .State }}{{ .Key }}-"), PromptView{State: half, Key: half})
		if !errors.Is(err, ErrPromptOutputTooLarge) {
			t.Fatalf("ExecutePrompt() = %v, want ErrPromptOutputTooLarge", err)
		}
	})
	t.Run("a template that would write a gigabyte does not allocate it", func(t *testing.T) {
		tmpl := parse(t, strings.Repeat("{{ .State }}", 1000))
		view := PromptView{State: strings.Repeat("x", 1<<20)}
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		_, err := ExecutePrompt(tmpl, view)
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrPromptOutputTooLarge) {
			t.Fatalf("ExecutePrompt() = %v, want ErrPromptOutputTooLarge", err)
		}
		if got := after.TotalAlloc - before.TotalAlloc; got > 16<<20 {
			t.Errorf("ExecutePrompt allocated %d MiB before it failed, want a few MiB at most", got>>20)
		}
	})
	t.Run("a runtime error of the template is returned as it is", func(t *testing.T) {
		_, err := ExecutePrompt(parse(t, "{{ slice .State 0 99 }}"), PromptView{State: "x"})
		if err == nil || errors.Is(err, ErrPromptOutputTooLarge) {
			t.Fatalf("ExecutePrompt() = %v, want the slice error", err)
		}
	})
}

// TestCheckPromptTemplateRunsThroughTheLimit shows the load-time run is held to the same
// limit as the renderer's: a template that reads a placeholder more times than the limit
// holds is refused, one that fills it exactly is not.
func TestCheckPromptTemplateRunsThroughTheLimit(t *testing.T) {
	fits := MaxPromptOutput / placeholderLen
	if err := checkPromptTemplate("promptTemplate", strings.Repeat("{{ .Key }}", fits), nil); err != nil {
		t.Errorf("a template that writes exactly the limit: %v", err)
	}
	err := checkPromptTemplate("promptTemplate", strings.Repeat("{{ .Key }}", fits+1), nil)
	if !errors.Is(err, ErrPromptOutputTooLarge) {
		t.Errorf("a template that writes past the limit: %v, want ErrPromptOutputTooLarge", err)
	}
}
