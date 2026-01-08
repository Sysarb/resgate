package tracing

import (
	"context"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

var (
	tracer     trace.Tracer = noop.NewTracerProvider().Tracer("")
	enabled    bool
	initMu     sync.Mutex
	propagator = propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	)
)

// Config holds tracing configuration
type Config struct {
	Enabled     bool
	Endpoint    string
	ServiceName string
	SampleRatio float64
}

// Init initializes the OpenTelemetry tracer provider
func Init(cfg Config) (shutdown func(context.Context) error, err error) {
	initMu.Lock()
	defer initMu.Unlock()

	if !cfg.Enabled {
		enabled = false
		tracer = noop.NewTracerProvider().Tracer("")
		return func(context.Context) error { return nil }, nil
	}

	ctx := context.Background()

	// Create OTLP HTTP exporter
	opts := []otlptracehttp.Option{
		otlptracehttp.WithEndpointURL(cfg.Endpoint),
	}
	exporter, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}

	// Create resource with service name
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, err
	}

	// Create sampler
	var sampler sdktrace.Sampler
	if cfg.SampleRatio >= 1.0 {
		sampler = sdktrace.AlwaysSample()
	} else if cfg.SampleRatio <= 0 {
		sampler = sdktrace.NeverSample()
	} else {
		sampler = sdktrace.TraceIDRatioBased(cfg.SampleRatio)
	}

	// Create tracer provider
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sampler)),
	)

	// Set global tracer provider and propagator
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagator)

	tracer = tp.Tracer("resgate")
	enabled = true

	return tp.Shutdown, nil
}

// Enabled returns whether tracing is enabled
func Enabled() bool {
	return enabled
}

// StartSpan starts a new span with the given name
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return tracer.Start(ctx, name, opts...)
}

// EndSpan ends a span if it's not nil
func EndSpan(span trace.Span) {
	if span != nil {
		span.End()
	}
}

// RecordError records an error on the current span
func RecordError(ctx context.Context, err error) {
	span := trace.SpanFromContext(ctx)
	if span != nil && err != nil {
		span.RecordError(err)
	}
}

// SetAttributes sets attributes on the current span
func SetAttributes(ctx context.Context, attrs ...attribute.KeyValue) {
	span := trace.SpanFromContext(ctx)
	if span != nil {
		span.SetAttributes(attrs...)
	}
}

// InjectHeaders injects trace context into a map for NATS headers
func InjectHeaders(ctx context.Context) map[string]string {
	if !enabled {
		return nil
	}
	carrier := make(propagation.MapCarrier)
	propagator.Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil
	}
	result := make(map[string]string, len(carrier))
	for k, v := range carrier {
		result[k] = v
	}
	return result
}

// ExtractContext extracts trace context from headers (traceparent/tracestate)
func ExtractContext(traceparent, tracestate string) context.Context {
	if traceparent == "" {
		return context.Background()
	}
	carrier := make(propagation.MapCarrier)
	carrier["traceparent"] = traceparent
	if tracestate != "" {
		carrier["tracestate"] = tracestate
	}
	return propagator.Extract(context.Background(), carrier)
}
