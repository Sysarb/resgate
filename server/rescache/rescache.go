package rescache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jirenius/timerqueue"
	"github.com/resgateio/resgate/logger"
	"github.com/resgateio/resgate/server/codec"
	"github.com/resgateio/resgate/server/metrics"
	"github.com/resgateio/resgate/server/mq"
	"github.com/resgateio/resgate/server/reserr"
	"github.com/resgateio/resgate/server/rpc"
	"github.com/resgateio/resgate/server/tracing"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

// Cache is an in memory resource cache.
type Cache struct {
	mq               mq.Client
	logger           logger.Logger
	workers          int
	resetThrottle    int
	unsubscribeDelay time.Duration
	conns            map[string]Conn
	metrics          *metrics.MetricSet

	mu         sync.Mutex
	started    bool
	eventSubs  map[string]*EventSubscription
	inCh       chan *EventSubscription
	unsubQueue *timerqueue.Queue
	resetSub   mq.Unsubscriber

	// Handlers for testing
	onUnsubscribe func(rid string)

	// Deprecated behavior logging
	depMutex  sync.Mutex
	depLogged map[string]featureType
}

// Subscriber interface represents a subscription made on a client connection
type Subscriber interface {
	CID() string
	Loaded(resourceSub *ResourceSubscription, err error)
	Event(event *ResourceEvent)
	ResourceName() string
	ResourceQuery() string
	Reaccess(t *Throttle)
}

// Conn interface represents a connection listening on events
type Conn interface {
	CID() string
	TokenReset(tids map[string]bool, subject string)
}

// ResourceEvent represents an event on a resource
type ResourceEvent struct {
	Event     string
	Payload   json.RawMessage
	Idx       int
	Value     codec.Value
	Changed   map[string]codec.Value
	OldValues map[string]codec.Value
	// Version is the targeted internal version of the resource
	Version uint
	// Update flags if the event causes a version bump. Set by eg. add/remove/change.
	Update bool
}

// NewCache creates a new Cache instance
func NewCache(mq mq.Client, workers int, resetThrottle int, unsubscribeDelay time.Duration, l logger.Logger, ms *metrics.MetricSet) *Cache {
	return &Cache{
		mq:               mq,
		logger:           l,
		workers:          workers,
		resetThrottle:    resetThrottle,
		unsubscribeDelay: unsubscribeDelay,
		conns:            make(map[string]Conn),
		depLogged:        make(map[string]featureType),
		metrics:          ms,
	}
}

// SetLogger sets the logger.
// Must be called before Start is called.
func (c *Cache) SetLogger(l logger.Logger) {
	c.logger = l
}

// SetOnUnsubscribe sets a callback that is called when a resource is removed
// from the cache and unsubscribed. Used for testing purpose.
// Must be called before Start is called.
func (c *Cache) SetOnUnsubscribe(cb func(rid string)) {
	c.onUnsubscribe = cb
}

// Start will initialize the cache, subscribing to global events
// It is assumed mq.Connect has already been called.
func (c *Cache) Start() error {
	if c.started {
		return errors.New("cache: already started")
	}
	inCh := make(chan *EventSubscription, 100)
	c.eventSubs = make(map[string]*EventSubscription)
	c.unsubQueue = timerqueue.New(c.mqUnsubscribe, c.unsubscribeDelay)
	c.inCh = inCh

	for i := 0; i < c.workers; i++ {
		go c.startWorker(inCh)
	}

	resetSub, err := c.mq.Subscribe("system", func(subj string, payload []byte, _ error) {
		ev := subj[7:]
		switch ev {
		case "reset":
			c.handleSystemReset(payload)
		case "tokenReset":
			c.handleSystemTokenReset(payload)
		}

	})
	if err != nil {
		c.Stop()
		return err
	}

	c.resetSub = resetSub
	c.started = true
	return nil
}

// Logf writes a formatted log message
func (c *Cache) Logf(format string, v ...interface{}) {
	c.logger.Log(fmt.Sprintf(format, v...))
}

// Errorf writes a formatted log message
func (c *Cache) Errorf(format string, v ...interface{}) {
	c.logger.Error(fmt.Sprintf(format, v...))
}

// Subscribe fetches a resource from the cache, and if it is
// not cached, starts subscribing to the resource and sends a get request
func (c *Cache) Subscribe(sub Subscriber, t *Throttle) {
	eventSub, err := c.getSubscription(sub.ResourceName(), true)
	if err != nil {
		sub.Loaded(nil, err)
		return
	}

	eventSub.addSubscriber(sub, t)
}

// Access sends an access request
func (c *Cache) Access(sub Subscriber, token interface{}, isHTTP bool, callback func(access *Access, meta *codec.Meta)) {
	rname := sub.ResourceName()
	payload := codec.CreateRequest(nil, sub, sub.ResourceQuery(), token, isHTTP)
	subj := "access." + rname
	c.sendRequest(rname, subj, payload, func(data []byte, err error) {
		if err != nil {
			callback(&Access{Error: reserr.RESError(err)}, nil)
			return
		}

		access, meta, rerr := codec.DecodeAccessResponse(data)
		if !isHTTP {
			meta = nil
		}
		callback(&Access{AccessResult: access, Error: rerr}, meta)
	})
}

// Call sends a method call request
func (c *Cache) Call(req codec.Requester, rname, query, action string, token, params interface{}, isHTTP bool, t *rpc.Tracing, callback func(result json.RawMessage, rid string, meta *codec.Meta, t *rpc.Tracing, err error)) {
	payload := codec.CreateRequest(params, req, query, token, isHTTP)
	subj := "call." + rname + "." + action

	// Extract parent context and start span
	ctx := extractTracingContext(t)
	ctx, span := tracing.StartSpan(ctx, "resgate.call."+action,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("res.resource", rname),
			attribute.String("res.action", action),
		),
	)

	// Inject trace context into headers
	headers := tracing.InjectHeaders(ctx)

	c.sendRequestWithHeaders(rname, subj, payload, headers, func(data []byte, err error) {
		// Extract trace context before ending span
		respTracing := extractResponseTracing(ctx, t)
		defer tracing.EndSpan(span)

		if err != nil {
			tracing.RecordError(ctx, err)
			callback(nil, "", nil, respTracing, err)
			return
		}

		// [DEPRECATED:deprecatedNewCallRequest]
		if action == "new" {
			result, rid, meta, err := codec.DecodeCallResponse(data)
			if err == nil && rid == "" {
				rid, err = codec.TryDecodeLegacyNewResult(result)
				if err != nil || rid != "" {
					c.deprecated(rname, deprecatedNewCallRequest)
					if err != nil {
						tracing.RecordError(ctx, err)
					}
					callback(nil, rid, meta, respTracing, err)
					return
				}
			}
			if err != nil {
				tracing.RecordError(ctx, err)
			}
			callback(result, rid, meta, respTracing, err)
			return
		}

		result, rid, meta, decodeErr := codec.DecodeCallResponse(data)
		if decodeErr != nil {
			tracing.RecordError(ctx, decodeErr)
		}
		callback(result, rid, meta, respTracing, decodeErr)
	})
}

// Auth sends an auth method call
func (c *Cache) Auth(req codec.AuthRequester, rname, query, action string, token, params interface{}, isHTTP bool, t *rpc.Tracing, callback func(result json.RawMessage, rid string, meta *codec.Meta, t *rpc.Tracing, err error)) {
	payload := codec.CreateAuthRequest(params, req, query, token, isHTTP)
	subj := "auth." + rname + "." + action

	// Extract parent context and start span
	ctx := extractTracingContext(t)
	ctx, span := tracing.StartSpan(ctx, "resgate.auth."+action,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(
			attribute.String("res.resource", rname),
			attribute.String("res.action", action),
		),
	)

	// Inject trace context into headers
	headers := tracing.InjectHeaders(ctx)

	c.sendRequestWithHeaders(rname, subj, payload, headers, func(data []byte, err error) {
		// Extract trace context before ending span
		respTracing := extractResponseTracing(ctx, t)
		defer tracing.EndSpan(span)

		if err != nil {
			tracing.RecordError(ctx, err)
			callback(nil, "", nil, respTracing, err)
			return
		}

		result, rid, meta, decodeErr := codec.DecodeCallResponse(data)
		if decodeErr != nil {
			tracing.RecordError(ctx, decodeErr)
		}
		callback(result, rid, meta, respTracing, decodeErr)
	})
}

// CustomAuth sends an auth method call to a custom subject
func (c *Cache) CustomAuth(req codec.AuthRequester, subj, query string, token, params interface{}, callback func(result json.RawMessage, rid string, meta *codec.Meta, err error)) {
	payload := codec.CreateAuthRequest(params, req, query, token, false)
	c.mq.SendRequest(subj, payload, func(_ string, data []byte, err error) {
		if err != nil {
			callback(nil, "", nil, err)
			return
		}

		callback(codec.DecodeCallResponse(data))
	})
}

func (c *Cache) sendRequest(rname, subj string, payload []byte, cb func(data []byte, err error)) {
	c.sendRequestWithHeaders(rname, subj, payload, nil, cb)
}

func (c *Cache) sendRequestWithHeaders(rname, subj string, payload []byte, headers map[string]string, cb func(data []byte, err error)) {
	eventSub, _ := c.getSubscription(rname, false)
	c.mq.SendRequestWithHeaders(subj, payload, headers, func(_ string, data []byte, err error) {
		eventSub.Enqueue(func() {
			cb(data, err)
			eventSub.removeCount(1)
		})
	})
}

// extractTracingContext extracts trace context from rpc.Tracing
func extractTracingContext(t *rpc.Tracing) context.Context {
	if t == nil {
		return context.Background()
	}
	return tracing.ExtractContext(t.TraceParent, t.TraceState)
}

// extractResponseTracing extracts W3C trace context from the current span for response.
// Only returns tracing if the original request included tracing.
func extractResponseTracing(ctx context.Context, reqTracing *rpc.Tracing) *rpc.Tracing {
	if reqTracing == nil {
		return nil
	}
	traceparent, tracestate := tracing.ExtractHeaders(ctx)
	if traceparent == "" {
		return nil
	}
	return &rpc.Tracing{
		TraceParent: traceparent,
		TraceState:  tracestate,
	}
}

// AddConn adds a connection listening to events such as system token reset
// event.
func (c *Cache) AddConn(conn Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.conns[conn.CID()] = conn
}

// RemoveConn removes a connection listening to events.
func (c *Cache) RemoveConn(conn Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.conns, conn.CID())
}

// getSubscription returns the existing eventSubscription after adding its count, or creates a new
// subscription with count of 1. If the subscribe flag is true, a mq subscription is also made.
func (c *Cache) getSubscription(name string, subscribe bool) (*EventSubscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	eventSub, ok := c.eventSubs[name]
	if !ok {
		eventSub = &EventSubscription{
			ResourceName: name,
			cache:        c,
			count:        1,
		}

		c.eventSubs[name] = eventSub

		// Metrics
		if c.metrics != nil {
			c.metrics.CacheResources.Add(1)
			c.metrics.CacheSubscriptions.Add(1)
		}

	} else {
		eventSub.addCount()

		// Metrics
		if c.metrics != nil {
			c.metrics.CacheSubscriptions.Add(1)
		}
	}

	if subscribe && eventSub.mqSub == nil {
		mqSub, err := c.mq.Subscribe("event."+name, func(subj string, payload []byte, _ error) {
			eventSub.enqueueEvent(subj, payload)
		})
		if err != nil {
			return nil, err
		}

		eventSub.mqSub = mqSub
	}

	return eventSub, nil
}

// Stop closes the worker channel, stops all the workers,  and clears
// the unsubscribe queue
func (c *Cache) Stop() {
	if !c.started {
		return
	}
	close(c.inCh)
	c.unsubQueue.Clear()
	c.resetSub = nil
	c.started = false
}

func (c *Cache) startWorker(ch chan *EventSubscription) {
	for eventSub := range ch {
		eventSub.processQueue()
	}
}

func (c *Cache) mqUnsubscribe(v interface{}) {
	eventSub := v.(*EventSubscription)
	c.mu.Lock()
	defer c.mu.Unlock()

	if !eventSub.mqUnsubscribe() {
		return
	}

	delete(c.eventSubs, eventSub.ResourceName)

	// Metrics
	if c.metrics != nil {
		c.metrics.CacheResources.Add(-1)
	}

	if c.onUnsubscribe != nil {
		c.onUnsubscribe(eventSub.ResourceName)
	}
}

func (c *Cache) handleSystemReset(payload []byte) {
	r, err := codec.DecodeSystemReset(payload)
	if err != nil {
		c.Errorf("Error decoding system reset: %s", err)
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	var t *Throttle
	if c.resetThrottle > 0 {
		t = NewThrottle(c.resetThrottle)
	}

	c.forEachMatch(r.Resources, func(e *EventSubscription) {
		e.handleResetResource(t)
	})
	c.forEachMatch(r.Access, func(e *EventSubscription) {
		e.handleResetAccess(t)
	})
}

func (c *Cache) forEachMatch(p []string, cb func(e *EventSubscription)) {
	if len(p) == 0 {
		return
	}

	patterns := make([]ResourcePattern, 0, len(p))

	for _, r := range p {
		pattern := ParseResourcePattern(r)
		if pattern.IsValid() {
			patterns = append(patterns, pattern)
		}
	}

	for resourceName, eventSub := range c.eventSubs {
		for _, p := range patterns {
			if p.Match(resourceName) {
				cb(eventSub)
			}
		}
	}
}

func (c *Cache) handleSystemTokenReset(payload []byte) {
	r, err := codec.DecodeSystemTokenReset(payload)
	if err != nil {
		c.Errorf("Error decoding system token reset: %s", err)
		return
	}

	if r.Subject == "" {
		c.Errorf("Missing subject in system token reset")
		return
	}

	// Quick exit if no token IDs are available.
	if len(r.TIDs) == 0 {
		return
	}

	m := make(map[string]bool, len(r.TIDs))
	for _, tid := range r.TIDs {
		m[tid] = true
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Iterate over all current connections and let each validate against any
	// existing token ID (tid).
	for _, sub := range c.conns {
		sub.TokenReset(m, r.Subject)
	}
}
