// Package otel provides OpenTelemetry span instrumentation for HTTP handlers.
// It sets up a global tracer provider and exports a middleware that wraps each
// request in a span carrying a decision_id (UUID) attribute, following
// GENAI_SEMCONV field naming to align with @wasmagent/otel-exporter.
package otel

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	stdouttrace "go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// DecisionIDHeader is the HTTP response header that carries the unique
// decision ID generated for each request.
const DecisionIDHeader = "X-Decision-Id"

// tracerName is the OpenTelemetry instrumentation scope name.
const tracerName = "github.com/WasmAgent/symkernel"

// decisionIDKey is an unexported type used as the context key for decision_id.
type decisionIDKey struct{}

// DecisionIDFromContext returns the decision_id (UUID) associated with ctx,
// or the empty string if none was set by the Middleware.
func DecisionIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(decisionIDKey{}).(string); ok {
		return v
	}
	return ""
}

// InitProvider creates a TracerProvider backed by a stdout trace exporter and
// registers it as the global OpenTelemetry provider. It returns a shutdown
// function that MUST be called when the program exits to flush pending spans.
//
// If w is nil, trace output is silently discarded (io.Discard). Pass os.Stdout
// to emit human-readable span data to the terminal.
func InitProvider(ctx context.Context, w io.Writer) (func(context.Context) error, error) {
	if w == nil {
		w = io.Discard
	}

	exp, err := stdouttrace.New(stdouttrace.WithWriter(w))
	if err != nil {
		return nil, fmt.Errorf("otel: create stdout exporter: %w", err)
	}

	tp := tracesdk.NewTracerProvider(
		tracesdk.WithBatcher(exp),
		tracesdk.WithResource(resource.Default()),
	)
	otel.SetTracerProvider(tp)

	return tp.Shutdown, nil
}

// Middleware wraps next in an OpenTelemetry span that records the HTTP method,
// URL, and a unique decision_id (UUID). The decision_id is:
//   - stored in the request context (accessible via DecisionIDFromContext)
//   - set as the X-Decision-Id response header before the handler runs
//
// The middleware follows the same func(next http.Handler) http.Handler
// signature as auth.Middleware so the two compose naturally.
func Middleware(next http.Handler) http.Handler {
	tracer := otel.Tracer(tracerName)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := uuid.New().String()

		ctx := context.WithValue(r.Context(), decisionIDKey{}, id)
		w.Header().Set(DecisionIDHeader, id)

		ctx, span := tracer.Start(ctx, r.Method+" "+r.URL.Path, trace.WithAttributes(
			attribute.String("decision_id", id),
			attribute.String("http.method", r.Method),
			attribute.String("http.url", r.URL.String()),
		))
		defer span.End()

		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r.WithContext(ctx))

		if sw.status >= 400 {
			span.SetStatus(codes.Error, http.StatusText(sw.status))
		}
	})
}

// statusWriter wraps http.ResponseWriter to capture the status code for
// span status recording.
type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.wrote {
		return
	}
	sw.status = code
	sw.wrote = true
	sw.ResponseWriter.WriteHeader(code)
}
