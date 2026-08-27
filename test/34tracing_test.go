package test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/resgateio/resgate/server/rpc"
	"github.com/resgateio/resgate/server/tracing"
)

const (
	testTraceID     = "0af7651916cd43dd8448eb211c80319c"
	testSpanID      = "b7ad6b7169203331"
	testTraceParent = "00-" + testTraceID + "-" + testSpanID + "-01"
)

// enableTracing initializes the global tracer for the duration of a test,
// restoring the disabled no-op tracer afterwards. The endpoint points
// nowhere; spans are started and propagated, but never exported.
func enableTracing(t *testing.T) {
	shutdown, err := tracing.Init(tracing.Config{
		Enabled:     true,
		Endpoint:    "http://127.0.0.1:1/v1/traces",
		ServiceName: "resgate-test",
		SampleRatio: 1,
	})
	if err != nil {
		t.Fatalf("error initializing tracing: %s", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		shutdown(ctx)
		if _, err := tracing.Init(tracing.Config{Enabled: false}); err != nil {
			t.Fatalf("error disabling tracing: %s", err)
		}
	})
}

// parseTraceParent asserts h is a valid W3C traceparent header and returns
// its trace ID and span ID.
func parseTraceParent(t *testing.T, h string) (traceID string, spanID string) {
	parts := strings.Split(h, "-")
	if len(parts) != 4 || len(parts[1]) != 32 || len(parts[2]) != 16 {
		t.Fatalf("expected a valid traceparent header, but got %q", h)
	}
	return parts[1], parts[2]
}

// Test that a client subscribe with tracing results in an access request
// with a trace context that is a child of the client's trace context, and a
// get request with a new root trace context, as the get response is shared
// by all subscribers.
func TestTracingOnSubscribe(t *testing.T) {
	enableTracing(t)
	model := resourceData("test.model")

	runTest(t, func(s *Session) {
		c := s.Connect()
		creq := c.RequestWithTracing("subscribe.test.model", nil, &rpc.Tracing{TraceParent: testTraceParent})
		mreqs := s.GetParallelRequests(t, 2)

		// The access request is made per client, so it is a child of the
		// client's trace context: same trace ID, new span ID.
		accessReq := mreqs.GetRequest(t, "access.test.model")
		accessTraceID, accessSpanID := parseTraceParent(t, accessReq.Headers["traceparent"])
		if accessTraceID != testTraceID {
			t.Fatalf("expected access request trace ID to be %q, but got %q", testTraceID, accessTraceID)
		}
		if accessSpanID == testSpanID {
			t.Fatalf("expected access request span ID to differ from the client's span ID %q", testSpanID)
		}

		// The get response is shared by all subscribers, so the get request
		// starts a new trace, linked to the client's trace context. The link
		// is not observable on the wire; a new trace ID is.
		getReq := mreqs.GetRequest(t, "get.test.model")
		getTraceID, _ := parseTraceParent(t, getReq.Headers["traceparent"])
		if getTraceID == testTraceID {
			t.Fatalf("expected get request to start a new trace, but got the client's trace ID %q", getTraceID)
		}

		accessReq.RespondSuccess(json.RawMessage(`{"get":true}`))
		getReq.RespondSuccess(json.RawMessage(`{"model":` + model + `}`))
		creq.GetResponse(t)
	})
}

// Test that a client get request with tracing propagates trace context the
// same way as subscribe.
func TestTracingOnGet(t *testing.T) {
	enableTracing(t)
	model := resourceData("test.model")

	runTest(t, func(s *Session) {
		c := s.Connect()
		creq := c.RequestWithTracing("get.test.model", nil, &rpc.Tracing{TraceParent: testTraceParent})
		mreqs := s.GetParallelRequests(t, 2)

		accessReq := mreqs.GetRequest(t, "access.test.model")
		accessTraceID, _ := parseTraceParent(t, accessReq.Headers["traceparent"])
		if accessTraceID != testTraceID {
			t.Fatalf("expected access request trace ID to be %q, but got %q", testTraceID, accessTraceID)
		}

		getReq := mreqs.GetRequest(t, "get.test.model")
		getTraceID, _ := parseTraceParent(t, getReq.Headers["traceparent"])
		if getTraceID == testTraceID {
			t.Fatalf("expected get request to start a new trace, but got the client's trace ID %q", getTraceID)
		}

		accessReq.RespondSuccess(json.RawMessage(`{"get":true}`))
		getReq.RespondSuccess(json.RawMessage(`{"model":` + model + `}`))
		creq.GetResponse(t)
	})
}

// Test that resgate traces the shared get fetch even when the client
// provides no trace context, while the access request - made per client
// request rather than deduplicated by the cache - is only traced when there
// is a client trace context to attach its span to.
func TestTracingOnSubscribeWithoutClientTraceContext(t *testing.T) {
	enableTracing(t)
	model := resourceData("test.model")

	runTest(t, func(s *Session) {
		c := s.Connect()
		creq := c.Request("subscribe.test.model", nil)
		mreqs := s.GetParallelRequests(t, 2)

		accessReq := mreqs.GetRequest(t, "access.test.model")
		if accessReq.Headers != nil {
			t.Fatalf("expected no headers on access request without client trace context, but got %v", accessReq.Headers)
		}

		getReq := mreqs.GetRequest(t, "get.test.model")
		parseTraceParent(t, getReq.Headers["traceparent"])

		accessReq.RespondSuccess(json.RawMessage(`{"get":true}`))
		getReq.RespondSuccess(json.RawMessage(`{"model":` + model + `}`))
		creq.GetResponse(t)
	})
}

// Test that a system.reset event triggers get requests without trace
// context. A reset re-fetches every matching cached resource, so tracing
// each re-get would flood the tracing backend; the reset is instead traced
// as a single resgate.reset span per event, which is not observable through
// the NATS requests.
func TestTracingOnSystemResetGetRequest(t *testing.T) {
	enableTracing(t)
	model := resourceData("test.model")

	runTest(t, func(s *Session) {
		c := s.Connect()
		creq := c.RequestWithTracing("subscribe.test.model", nil, &rpc.Tracing{TraceParent: testTraceParent})
		mreqs := s.GetParallelRequests(t, 2)
		mreqs.GetRequest(t, "access.test.model").RespondSuccess(json.RawMessage(`{"get":true}`))
		getReq := mreqs.GetRequest(t, "get.test.model")
		parseTraceParent(t, getReq.Headers["traceparent"])
		getReq.RespondSuccess(json.RawMessage(`{"model":` + model + `}`))
		creq.GetResponse(t)

		// Send system reset
		s.SystemEvent("reset", json.RawMessage(`{"resources":["test.>"]}`))

		// Validate the reset get request carries no trace context
		resetReq := s.GetRequest(t).AssertSubject(t, "get.test.model")
		if resetReq.Headers != nil {
			t.Fatalf("expected no headers on reset get request, but got %v", resetReq.Headers)
		}
		resetReq.RespondSuccess(json.RawMessage(`{"model":` + model + `}`))

		// Validate no events are sent to client
		c.AssertNoEvent(t, "test.model")
	})
}

// Test that no trace headers are sent on get and access requests when
// tracing is disabled, even if the client provides a trace context.
func TestTracingDisabledSendsNoHeaders(t *testing.T) {
	model := resourceData("test.model")

	runTest(t, func(s *Session) {
		c := s.Connect()
		creq := c.RequestWithTracing("subscribe.test.model", nil, &rpc.Tracing{TraceParent: testTraceParent})
		mreqs := s.GetParallelRequests(t, 2)

		accessReq := mreqs.GetRequest(t, "access.test.model")
		if accessReq.Headers != nil {
			t.Fatalf("expected no headers on access request, but got %v", accessReq.Headers)
		}

		getReq := mreqs.GetRequest(t, "get.test.model")
		if getReq.Headers != nil {
			t.Fatalf("expected no headers on get request, but got %v", getReq.Headers)
		}

		accessReq.RespondSuccess(json.RawMessage(`{"get":true}`))
		getReq.RespondSuccess(json.RawMessage(`{"model":` + model + `}`))
		creq.GetResponse(t)
	})
}
