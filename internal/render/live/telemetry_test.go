package live_test

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/agentkourier/agent-kourier/api/v1alpha1"
	"github.com/agentkourier/agent-kourier/internal/agent"
	"github.com/agentkourier/agent-kourier/internal/render/live"
	"github.com/agentkourier/agent-kourier/internal/store"
	"github.com/agentkourier/agent-kourier/internal/telemetry"
)

func TestFinalDeliveryMetricsDistinguishFailureFromCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, outcome string
		fail          bool
	}{{"delivered", "delivered", false}, {"permanent failure", "permanent_failure", true}} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			exporter := tracetest.NewInMemoryExporter()
			provider := sdktrace.NewTracerProvider(sdktrace.WithSyncer(exporter))
			defer func() { _ = provider.Shutdown(context.Background()) }()
			reg := prometheus.NewRegistry()
			obs, err := telemetry.New(reg, provider)
			if err != nil {
				t.Fatal(err)
			}
			cfg := e.cfg
			if tc.fail {
				cfg.Sender = &failingSender{err: store.ErrInvalid}
			}
			renderer := newRenderer(t, cfg)
			ctx, cancel := context.WithCancel(telemetry.WithObserver(t.Context(), obs))
			defer cancel()
			run := renderer.Start(ctx, live.Turn{Source: "turn-final", Channel: "C1", Thread: e.root, Mode: v1alpha1.OutputFinal})
			run.Handle(text("a", "private response"))
			run.Handle(agent.FinalEvent{})
			// The backend's context ends immediately; final delivery still has its own bounded lifetime.
			cancel()
			select {
			case <-run.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("final delivery did not settle")
			}
			families, err := reg.Gather()
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, f := range families {
				if f.GetName() == "agentkourier_render_final_total" {
					found = true
					if len(f.Metric) != 1 || f.Metric[0].Label[0].GetValue() != tc.outcome || f.Metric[0].Counter.GetValue() != 1 {
						t.Fatalf("final metric=%v", f)
					}
				}
			}
			if !found {
				t.Fatal("agent completed but no final delivery metric")
			}
		})
	}
}
