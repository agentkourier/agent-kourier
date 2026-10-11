package telemetry

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// maxAttributeValueLength bounds every span string attribute. Task IDs, context IDs and states
// come from the agent, which Agent Kourier does not control, so no value may grow without limit.
const maxAttributeValueLength = 256

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
	c := Config{ServiceName: "agent-kourier", SampleRatio: 1}
	if s := getenv("OTEL_SERVICE_NAME"); s != "" {
		c.ServiceName = s
	}
	c.Environment = getenv("AGENTKOURIER_ENVIRONMENT")
	switch e := getenv("OTEL_TRACES_EXPORTER"); e {
	case "", "otlp":
	case "none":
		return c, nil
	default:
		return c, errors.New("OTEL_TRACES_EXPORTER must be otlp or none")
	}
	// The exporter reads and parses the general and the trace-specific variable of each pair
	// itself, whichever one wins, and what its parser logs on bad input can include the value.
	// So both of each pair are checked here, by the same rules, before the exporter is built: a
	// bad value is refused even where the other variable would have overridden it. (Checking both
	// keeps the documented override rule; refusing to start when both are set would not.)
	specific, base := getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT"), getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	var urls []*url.URL
	for _, e := range []string{base, specific} {
		u, err := checkEndpoint(e)
		if err != nil {
			return c, err
		}
		urls = append(urls, u)
	}
	// What the exporter will use, and so what decides its transport: the trace-specific URL if set.
	used := urls[0]
	if urls[1] != nil {
		used = urls[1]
	}
	c.Endpoint = specific
	if c.Endpoint == "" && base != "" {
		c.Endpoint = strings.TrimRight(base, "/") + "/v1/traces"
	}
	protocol := getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	if protocol != "" && protocol != "http/protobuf" {
		return c, errors.New("OTEL trace export protocol must be http/protobuf")
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
	general, err := parseHeaders(getenv("OTEL_EXPORTER_OTLP_HEADERS"))
	if err != nil {
		return c, err
	}
	c.Headers, err = parseHeaders(getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS"))
	if err != nil {
		return c, err
	}
	if getenv("OTEL_EXPORTER_OTLP_TRACES_HEADERS") == "" {
		c.Headers = general
	}
	// The parsed scheme, which url.Parse lower-cases, as the exporter reads it: HTTP:// is cleartext too.
	if len(c.Headers) > 0 && used != nil && used.Scheme == "http" {
		if getenv("AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS") != "true" {
			return c, errors.New("OTEL export headers would travel in cleartext to an http endpoint: use https, or set AGENTKOURIER_OTLP_ALLOW_INSECURE_HEADERS=true")
		}
	}
	return c, nil
}

// checkEndpoint applies the endpoint rules to one variable's value and returns it parsed; empty
// is unset, and nil. The error never carries any of raw.
func checkEndpoint(raw string) (*url.URL, error) {
	if raw == "" {
		return nil, nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("OTEL export endpoint must be an http(s) URL without credentials, query or fragment")
	}
	return u, nil
}

// parseHeaders reads comma-separated key=value pairs and percent-decodes the values, by the
// rules the exporter applies to the same variables; empty is no headers. A key must be an HTTP
// token, which the exporter also requires (it logs and drops any other pair). The error never
// carries any of raw.
func parseHeaders(raw string) (map[string]string, error) {
	headers := map[string]string{}
	if raw == "" {
		return headers, nil
	}
	for _, p := range strings.Split(raw, ",") {
		k, v, ok := strings.Cut(p, "=")
		k = strings.TrimSpace(k)
		if !ok || !headerToken(k) {
			return nil, errors.New("OTEL export headers must be comma-separated key=value pairs with a valid header name")
		}
		value, err := url.PathUnescape(v)
		if err != nil {
			return nil, errors.New("OTEL export header has invalid escaping")
		}
		headers[k] = strings.TrimSpace(value)
	}
	return headers, nil
}

// headerToken reports whether k is a nonempty RFC 9110 token.
func headerToken(k string) bool {
	if k == "" {
		return false
	}
	for _, c := range k {
		if c > unicode.MaxASCII {
			return false
		}
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && !strings.ContainsRune("!#$%&'*+-.^_`|~", c) {
			return false
		}
	}
	return true
}

// Provider builds an optional OTLP/HTTP exporter and an instance-local tracer provider.
// The caller must call shutdown after all broker work has drained, using a bounded context.
// Export failures are reported to log, by class and at most once a minute for each.
//
// The exporter reads more OTEL_EXPORTER_OTLP_* variables from the process environment than
// LoadConfig checks (certificates, compression, timeout; see docs/reference/logs-and-traces.md). Call
// InstallDiagnostics first, so that what it says about any of them goes to log.
func (c Config) Provider(ctx context.Context, version string, log *slog.Logger) (trace.TracerProvider, func(context.Context) error, error) {
	if c.Endpoint == "" {
		return noop.NewTracerProvider(), func(context.Context) error { return nil }, nil
	}
	if _, err := checkEndpoint(c.Endpoint); err != nil {
		return nil, nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(c.Endpoint), otlptracehttp.WithHeaders(c.Headers))
	if err != nil {
		return nil, nil, fmt.Errorf("create OTLP HTTP exporter: %w", err)
	}
	attrs := []attribute.KeyValue{attribute.String("service.name", c.ServiceName), attribute.String("service.version", version), attribute.String("service.instance.id", rand.Text())}
	if c.Environment != "" {
		attrs = append(attrs, attribute.String("deployment.environment.name", c.Environment))
	}
	// The raw limits are the SDK's defaults but for the value length: a zero field would mean zero.
	limits := sdktrace.SpanLimits{
		AttributeValueLengthLimit:   maxAttributeValueLength,
		AttributeValueDepthLimit:    sdktrace.DefaultAttributeValueDepthLimit,
		AttributeCountLimit:         sdktrace.DefaultAttributeCountLimit,
		EventCountLimit:             sdktrace.DefaultEventCountLimit,
		LinkCountLimit:              sdktrace.DefaultLinkCountLimit,
		AttributePerEventCountLimit: sdktrace.DefaultAttributePerEventCountLimit,
		AttributePerLinkCountLimit:  sdktrace.DefaultAttributePerLinkCountLimit,
	}
	p := sdktrace.NewTracerProvider(sdktrace.WithResource(resource.NewSchemaless(attrs...)), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(c.SampleRatio))),
		sdktrace.WithRawSpanLimits(limits), sdktrace.WithBatcher(newSafeExporter(exporter, log, time.Now)), sdktrace.WithoutPanicRecording())
	return p, p.Shutdown, nil
}
