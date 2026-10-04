package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// Config is the explicitly parsed trace-export configuration. No endpoint means disabled.
type Config struct {
	Endpoint    string
	ServiceName string
	Environment string
	SampleRatio float64
	Headers     map[string]string
}

// LoadConfig reads supported OTEL settings without exposing their contents in errors.
func LoadConfig(getenv func(string) string) (Config, error) {
	c := Config{ServiceName: "agent-courier", SampleRatio: 1}
	if s := getenv("OTEL_SERVICE_NAME"); s != "" {
		c.ServiceName = s
	}
	c.Environment = getenv("COURIER_ENVIRONMENT")
	switch e := getenv("OTEL_TRACES_EXPORTER"); e {
	case "", "otlp":
	case "none":
		return c, nil
	default:
		return c, errors.New("OTEL_TRACES_EXPORTER must be otlp or none")
	}
	c.Endpoint = getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if c.Endpoint == "" {
		if base := getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			c.Endpoint = strings.TrimRight(base, "/") + "/v1/traces"
		}
	}
	protocol := getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		return c, errors.New("OTEL trace export protocol must be http/protobuf")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return c, errors.New("OTEL export endpoint must be an http(s) URL without credentials, query or fragment")
		}
	}
	if s := getenv("OTEL_TRACES_SAMPLER"); s != "" && s != "parentbased_traceidratio" {
		return c, errors.New("OTEL_TRACES_SAMPLER must be parentbased_traceidratio")
	}
	if s := getenv("OTEL_TRACES_SAMPLER_ARG"); s != "" {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil || !(v >= 0 && v <= 1) {
			return c, errors.New("OTEL_TRACES_SAMPLER_ARG must be a number between 0 and 1")
		}
		c.SampleRatio = v
	}
	headers := getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS")
	if headers == "" {
		headers = getenv("OTEL_EXPORTER_OTLP_HEADERS")
	}
	c.Headers = map[string]string{}
	if headers != "" {
		for _, p := range strings.Split(headers, ",") {
			k, v, ok := strings.Cut(p, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return c, errors.New("OTEL export headers must be comma-separated key=value pairs")
			}
			value, err := url.PathUnescape(strings.TrimSpace(v))
			if err != nil {
				return c, errors.New("OTEL export header has invalid escaping")
			}
			c.Headers[strings.TrimSpace(k)] = value
		}
	}
	return c, nil
}

// Provider builds an optional OTLP/HTTP exporter and an instance-local tracer provider.
// The caller must call shutdown after all broker work has drained, using a bounded context.
func (c Config) Provider(ctx context.Context, version string) (trace.TracerProvider, func(context.Context) error, error) {
	if c.Endpoint == "" {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(c.Endpoint), otlptracehttp.WithHeaders(c.Headers))
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP HTTP exporter: %w", err)
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", c.ServiceName), attribute.String("service.version", version), attribute.String("service.instance.id", rand.Text())}
	if c.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", c.Environment))
	}
	p := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.NewSchemaless(attrs...)), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.SampleRatio))), sdktrace.WithBatcher(safeExporter{next: exporter}), sdktrace.WithoutPanicRecording())
	return p, p.Shutdown, nil
}
