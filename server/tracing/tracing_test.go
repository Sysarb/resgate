package tracing

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
)

// startTestSpan starts a span on a recording tracer provider, returning the
// span, its context, and the recorder holding ended spans.
func startTestSpan(t *testing.T) (context.Context, trace.Span, *tracetest.SpanRecorder) {
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	t.Cleanup(func() { _ = tp.Shutdown(context.Background()) })
	ctx, span := tp.Tracer("test").Start(context.Background(), "test.span")
	return ctx, span, sr
}

// endedSpan ends the span and returns its recorded read-only form.
func endedSpan(t *testing.T, span trace.Span, sr *tracetest.SpanRecorder) sdktrace.ReadOnlySpan {
	span.End()
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected 1 ended span, but got %d", len(spans))
	}
	return spans[0]
}

// Test that RecordSpanError records an exception event and marks the span
// status as failed.
func TestRecordSpanErrorSetsErrorStatus(t *testing.T) {
	_, span, sr := startTestSpan(t)

	RecordSpanError(span, errors.New("system.timeout"))

	s := endedSpan(t, span, sr)
	if s.Status().Code != codes.Error {
		t.Fatalf("expected span status code %v, but got %v", codes.Error, s.Status().Code)
	}
	if s.Status().Description != "system.timeout" {
		t.Fatalf("expected span status description %q, but got %q", "system.timeout", s.Status().Description)
	}
	events := s.Events()
	if len(events) != 1 || events[0].Name != semconv.ExceptionEventName {
		t.Fatalf("expected a single %q event, but got %v", semconv.ExceptionEventName, events)
	}
}

// Test that RecordSpanError leaves the span untouched on a nil error or a
// nil span.
func TestRecordSpanErrorWithoutError(t *testing.T) {
	_, span, sr := startTestSpan(t)

	RecordSpanError(span, nil)
	RecordSpanError(nil, errors.New("system.timeout"))

	s := endedSpan(t, span, sr)
	if s.Status().Code != codes.Unset {
		t.Fatalf("expected span status code %v, but got %v", codes.Unset, s.Status().Code)
	}
	if len(s.Events()) != 0 {
		t.Fatalf("expected no span events, but got %v", s.Events())
	}
}

// Test that RecordError records an exception event and marks the status of
// the span in the context as failed.
func TestRecordErrorSetsErrorStatus(t *testing.T) {
	ctx, span, sr := startTestSpan(t)

	RecordError(ctx, errors.New("system.notFound"))

	s := endedSpan(t, span, sr)
	if s.Status().Code != codes.Error {
		t.Fatalf("expected span status code %v, but got %v", codes.Error, s.Status().Code)
	}
	events := s.Events()
	if len(events) != 1 || events[0].Name != semconv.ExceptionEventName {
		t.Fatalf("expected a single %q event, but got %v", semconv.ExceptionEventName, events)
	}
}
