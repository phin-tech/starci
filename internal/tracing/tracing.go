// Package tracing is starci's own account of where a run's time went, as
// OpenTelemetry spans: the run, each step, how long each step waited for a
// machine, and the parts of running it.
//
// It asks nothing of the engine but to carry a Carrier, two small values,
// from whoever asks for a step to whichever machine runs it. An engine that
// has tracing of its own is left to place a step's span in
// its own trace; one that has none gets a whole trace from here.
//
// Where spans are sent is the standard OTEL_EXPORTER_OTLP_* variables'
// business. With none set, and no engine wanting them, they go nowhere and
// cost nothing.
package tracing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"os"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"git.phin.tech/sam/starci/internal/plan"
)

var (
	mu       sync.Mutex
	provider *sdktrace.TracerProvider
)

// Provider is the tracer provider every span here comes from. An engine with
// somewhere of its own to send spans registers a processor on it.
func Provider() *sdktrace.TracerProvider {
	mu.Lock()
	defer mu.Unlock()
	if provider == nil {
		provider = newProvider()
	}
	return provider
}

func newProvider(opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	opts = append([]sdktrace.TracerProviderOption{
		sdktrace.WithIDGenerator(chosenIDs{}),
		sdktrace.WithResource(resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceNameKey.String("starci"))),
	}, opts...)
	return sdktrace.NewTracerProvider(opts...)
}

// Use replaces the provider. It is for tests, which want to see the spans.
func Use(opts ...sdktrace.TracerProviderOption) *sdktrace.TracerProvider {
	mu.Lock()
	defer mu.Unlock()
	provider = newProvider(opts...)
	return provider
}

// Export has spans sent to the endpoint the OTEL_EXPORTER_OTLP_* variables
// name, if they name one: OTEL_EXPORTER_OTLP_ENDPOINT or
// OTEL_EXPORTER_OTLP_TRACES_ENDPOINT, over HTTP unless
// OTEL_EXPORTER_OTLP_PROTOCOL is grpc, with OTEL_EXPORTER_OTLP_HEADERS for
// credentials. It reports whether there is such an endpoint. The returned
// function sends what is still waiting; call it before the process ends.
func Export(ctx context.Context) (exporting bool, flush func(), err error) {
	flush = func() {
		ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_ = Provider().ForceFlush(ctx)
	}
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" && os.Getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT") == "" {
		return false, flush, nil
	}
	protocol := os.Getenv("OTEL_EXPORTER_OTLP_TRACES_PROTOCOL")
	if protocol == "" {
		protocol = os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")
	}
	var exporter sdktrace.SpanExporter
	if strings.HasPrefix(protocol, "grpc") {
		exporter, err = otlptracegrpc.New(ctx)
	} else {
		exporter, err = otlptracehttp.New(ctx)
	}
	if err != nil {
		return false, flush, err
	}
	Provider().RegisterSpanProcessor(sdktrace.NewBatchSpanProcessor(exporter))
	return true, flush, nil
}

func tracer() trace.Tracer { return Provider().Tracer("git.phin.tech/sam/starci") }

// A run's trace has to be the same trace however many times the run is
// picked up: an advanced script is evaluated again from the top when the
// machine carrying it out is lost. So the trace's id, and the id of the span
// that stands for the whole run, are not drawn at random but made from the
// run's own id. Every machine that touches the run arrives at the same two.

func runIDs(runID string) trace.SpanContext {
	sum := sha256.Sum256([]byte("starci run " + runID))
	var t trace.TraceID
	var s trace.SpanID
	copy(t[:], sum[:16])
	copy(s[:], sum[16:24])
	return trace.NewSpanContext(trace.SpanContextConfig{TraceID: t, SpanID: s, TraceFlags: trace.FlagsSampled})
}

type chosenKey struct{}

// chosenIDs hands out random ids, except to a span that has been told
// through its context which ids it is to have.
type chosenIDs struct{}

func (chosenIDs) NewIDs(ctx context.Context) (trace.TraceID, trace.SpanID) {
	if sc, ok := ctx.Value(chosenKey{}).(trace.SpanContext); ok {
		return sc.TraceID(), sc.SpanID()
	}
	var t trace.TraceID
	_, _ = rand.Read(t[:])
	return t, chosenIDs{}.NewSpanID(ctx, t)
}

func (chosenIDs) NewSpanID(ctx context.Context, _ trace.TraceID) trace.SpanID {
	var s trace.SpanID
	_, _ = rand.Read(s[:])
	return s
}

// Carrier is what travels with a step from whoever asks for it to whichever
// machine runs it. An engine carries it and need not look inside.
type Carrier struct {
	// Parent is the span the step belongs under, as a W3C traceparent.
	Parent string `json:"traceparent,omitempty"`
	// Asked is when the step was asked for. The time from then until a
	// machine starts on it is time spent waiting, and is its own span.
	Asked time.Time `json:"asked,omitzero"`
}

// Ask makes the Carrier for a step of the run with this id, asked for now.
func Ask(runID string) Carrier {
	carrier := propagation.MapCarrier{}
	propagation.TraceContext{}.Inject(trace.ContextWithSpanContext(context.Background(), runIDs(runID)), carrier)
	return Carrier{Parent: carrier["traceparent"], Asked: time.Now()}
}

// Step begins a step's span on the machine that runs it, and first records
// how long the step waited to be started. The context it returns is the one
// to run the step with. The function it returns ends the span, with how the
// step went.
//
// If ctx is already inside a span, the engine has tracing of its own and
// has placed this step in its trace: the step goes there. Otherwise it goes
// under the run the Carrier names.
func (c Carrier) Step(ctx context.Context, name string) (context.Context, func(plan.Result)) {
	if !trace.SpanContextFromContext(ctx).IsValid() && c.Parent != "" {
		ctx = propagation.TraceContext{}.Extract(ctx, propagation.MapCarrier{"traceparent": c.Parent})
	}
	step := attribute.String("starci.step", name)
	if !c.Asked.IsZero() {
		_, waited := tracer().Start(ctx, "queued", trace.WithTimestamp(c.Asked), trace.WithAttributes(step))
		waited.End()
	}
	ctx, span := tracer().Start(ctx, "step "+name, trace.WithAttributes(step))
	return ctx, func(r plan.Result) {
		span.SetAttributes(
			attribute.String("starci.status", r.Status),
			attribute.Bool("starci.cached", r.Cached),
			attribute.String("starci.machine", r.Machine),
			attribute.Float64("starci.seconds", r.Seconds),
		)
		if r.Status != "passed" {
			span.SetStatus(codes.Error, firstLine(r.Detail))
		}
		span.End()
	}
}

// Waited records how long a run waited between being asked for and being
// started on, as a span under the run.
func Waited(ctx context.Context, runID string, asked time.Time) {
	if asked.IsZero() {
		return
	}
	if !trace.SpanContextFromContext(ctx).IsValid() {
		ctx = trace.ContextWithRemoteSpanContext(ctx, runIDs(runID))
	}
	_, span := tracer().Start(ctx, "queued", trace.WithTimestamp(asked), trace.WithAttributes(attribute.String("starci.run", runID)))
	span.End()
}

// Run is what a run's own span says about it.
type Run struct {
	// ID is the run's id, as given to Ask for its steps.
	ID string
	// Began is when the run was asked for.
	Began time.Time
	Repo  string
	SHA   string
	Mode  string
	OK    bool
	// Error is why it ended early, if it did.
	Error string
}

// End records the span that stands for a whole run: the one its steps have
// been naming as their parent all along. It is recorded once, when the run
// is over, by whichever machine sees it finish, so that a run picked up
// again after an interruption still has one.
//
// An engine with tracing of its own has a span for the run already; ctx is
// then inside it, and nothing is recorded here.
func End(ctx context.Context, r Run) {
	if trace.SpanContextFromContext(ctx).IsValid() {
		return
	}
	ctx = context.WithValue(context.Background(), chosenKey{}, runIDs(r.ID))
	_, span := tracer().Start(ctx, "run "+r.Repo, trace.WithNewRoot(), trace.WithTimestamp(r.Began), trace.WithAttributes(
		attribute.String("starci.repo", r.Repo),
		attribute.String("starci.sha", r.SHA),
		attribute.String("starci.mode", r.Mode),
		attribute.String("starci.run", r.ID),
		attribute.Bool("starci.ok", r.OK),
	))
	if !r.OK {
		span.SetStatus(codes.Error, firstLine(r.Error))
	}
	span.End()
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}
