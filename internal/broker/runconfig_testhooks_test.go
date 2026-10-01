//go:build testhooks

package broker

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

// logs returns a logger and what it wrote.
func logs() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, nil)), &b
}

// In a testhooks build, and only there, a config may say it runs with no interactions, which is how
// the fast tier deploys courier before the interactions exist (35g before 35h9). It says so loudly.
func TestTestHooksBuildMayRunWithoutInteractions(t *testing.T) {
	c := valid(t)
	c.Interactions = nil
	if err := c.validate(); !errors.Is(err, ErrWiring) {
		t.Fatalf("without the option, validate = %v, want ErrWiring", err)
	}
	log, out := logs()
	c.Logger = log
	c.NoInteractions = true
	for range 2 { // validating, however often, says nothing: Run warns once, when it starts
		if err := c.validate(); err != nil {
			t.Fatalf("with the option, validate = %v, want nil", err)
		}
	}
	if out.Len() != 0 {
		t.Errorf("validate logged %q", out.String())
	}
	c.logTestOptions()
	for _, want := range []string{"testhooks build", "no interactions"} {
		if strings.Count(out.String(), want) < 1 {
			t.Errorf("the warning %q lacks %q", out.String(), want)
		}
	}
	if strings.Contains(out.String(), "renderer") {
		t.Errorf("the warning %q mentions a renderer, which Run builds", out.String())
	}
}

func TestTestHooksBuildStillNeedsAWholeConfig(t *testing.T) {
	c := valid(t)
	c.NoInteractions, c.DBPath = true, ""
	if err := c.validate(); !errors.Is(err, ErrWiring) {
		t.Errorf("validate = %v, want ErrWiring: the option excuses only the interactions", err)
	}
}
