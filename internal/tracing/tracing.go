// Package tracing sets up OpenTelemetry tracing (M9.5, ADR-024): an OTLP
// exporter when an endpoint is configured (off by default), W3C trace
// context propagation, and gRPC instrumentation for every server, tier and
// client, so one trace follows a call client → daemon → cache → server.
package tracing

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

// EndpointEnv names the OTLP endpoint for binaries without a flag (xy).
const EndpointEnv = "XYLEM_OTLP_ENDPOINT"

// Setup installs the global tracer provider for service, exporting over
// OTLP/gRPC to endpoint ("host:port" or a URL; http:// means no TLS). An
// empty endpoint leaves tracing off. Call the returned function to flush
// and stop on exit.
func Setup(ctx context.Context, service, endpoint string) (shutdown func(context.Context) error, err error) {
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}
	opt := otlptracegrpc.WithEndpoint(endpoint)
	if strings.Contains(endpoint, "://") {
		opt = otlptracegrpc.WithEndpointURL(endpoint)
	}
	exp, err := otlptracegrpc.New(ctx, opt)
	if err != nil {
		return nil, fmt.Errorf("otlp exporter %s: %w", endpoint, err)
	}
	return Install(service, sdktrace.WithBatcher(exp)), nil
}

// Install sets the global tracer provider, with the given span processing
// (an exporter, or a recorder in tests), and the W3C propagators. It returns
// the provider's shutdown.
func Install(service string, opts ...sdktrace.TracerProviderOption) func(context.Context) error {
	res := resource.NewSchemaless(semconv.ServiceName(service))
	tp := sdktrace.NewTracerProvider(append([]sdktrace.TracerProviderOption{sdktrace.WithResource(res)}, opts...)...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(propagation.TraceContext{}, propagation.Baggage{}))
	return tp.Shutdown
}

// ServerOption instruments a gRPC server (a span per incoming call, joined
// to the caller's trace). It does nothing while tracing is off.
func ServerOption() grpc.ServerOption {
	return grpc.StatsHandler(otelgrpc.NewServerHandler())
}

// DialOption instruments a gRPC client (a span per outgoing call, with the
// trace context sent along).
func DialOption() grpc.DialOption {
	return grpc.WithStatsHandler(otelgrpc.NewClientHandler())
}

// Tracer returns the tracer for Xylem's own spans.
func Tracer() trace.Tracer { return otel.Tracer("github.com/delefrati/xylem") }
