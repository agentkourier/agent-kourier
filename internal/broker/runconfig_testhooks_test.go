//go:build testhooks

package broker

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
)

// logs returns a logger and what it wrote.
func logs() (*slog.Logger, *bytes.Buffer) {
	var b bytes.Buffer
	return slog.New(slog.NewTextHandler(&b, nil)), &b
}

// In a testhooks build, and only there, a config may say it runs with no renderer or no
// interactions, which is how the fast tier deploys courier before the renderer exists (35g before
// 35h). Each says so loudly.
func TestTestHooksBuildMayRunWithoutARendererOrInteractions(t *testing.T) {
	c := valid(t)
	c.Sink, c.Interactions = nil, nil
	if err := c.validate(); !errors.Is(err, ErrWiring) {
		t.Fatalf("with neither option, validate = %v, want ErrWiring", err)
	}

	c.NoRenderer = true
	if err := c.validate(); !errors.Is(err, ErrWiring) || !strings.Contains(err.Error(), "Interactions") {
		t.Fatalf("NoRenderer alone: validate = %v, want the missing Interactions", err)
	}
	log, out := logs()
	c.Logger = log
	c.NoInteractions = true
	for range 2 { // validating, however often, says nothing: Run warns once, when it starts
		if err := c.validate(); err != nil {
			t.Fatalf("with both options, validate = %v, want nil", err)
		}
	}
	if out.Len() != 0 {
		t.Errorf("validate logged %q", out.String())
	}
	c.logTestOptions()
	for _, want := range []string{"testhooks build", "no renderer", "no interactions"} {
		if strings.Count(out.String(), want) < 1 {
			t.Errorf("the warning %q lacks %q", out.String(), want)
		}
	}
}

func TestTestHooksBuildStillNeedsAWholeConfig(t *testing.T) {
	c := valid(t)
	c.NoRenderer, c.NoInteractions, c.DBPath = true, true, ""
	if err := c.validate(); !errors.Is(err, ErrWiring) {
		t.Errorf("validate = %v, want ErrWiring: the options excuse only the renderer and the interactions", err)
	}
}

// Run says so once, when it starts, and a Run with the option set works without a renderer.
func TestRunWarnsOnceAtStartThatItHasNoRenderer(t *testing.T) {
	c := startConfig(t)
	c.Sink, c.NoRenderer = nil, true
	out := &logBuffer{}
	c.Logger = slog.New(slog.NewTextHandler(out, nil))
	r := start(t, c)
	r.until("/readyz", http.StatusOK)

	if n := strings.Count(out.String(), "testhooks build: running with no renderer"); n != 1 {
		t.Errorf("the no-renderer warning was logged %d times, want once: %q", n, out.String())
	}
}
