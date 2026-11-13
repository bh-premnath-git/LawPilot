package workload

import (
	"context"
	"errors"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"google.golang.org/grpc/stats"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"net/url"
	"os"
	"strings"
	"time"
)

// Export only standard RPC spans. Never attach legal payloads, credentials,
// document IDs, tenant labels or SQL statements to traces.
func StartTelemetry(role string) (func(), error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return func() {}, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && os.Getenv("APP_ENV") == "development")) {
		return nil, errors.New("invalid telemetry endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	exporter, err := otlptracehttp.New(ctx, otlptracehttp.WithEndpointURL(endpoint))
	if err != nil {
		return nil, errors.New("telemetry exporter configuration invalid")
	}
	provider := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(0.1))), sdktrace.WithResource(resource.NewSchemaless(attribute.String("service.name", role))))
	otel.SetTracerProvider(provider)
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = provider.Shutdown(ctx)
	}, nil
}
func ServerTelemetry() stats.Handler { return otelgrpc.NewServerHandler(otelgrpc.WithFilter(knownRPC)) }
func ClientTelemetry() stats.Handler { return otelgrpc.NewClientHandler(otelgrpc.WithFilter(knownRPC)) }

// Reject attacker-chosen unknown RPC names before tracing; even method labels
// must come from a registered protobuf contract rather than arbitrary input.
func knownRPC(info *stats.RPCTagInfo) bool {
	parts := strings.Split(strings.TrimPrefix(info.FullMethodName, "/"), "/")
	if len(parts) != 2 {
		return false
	}
	descriptor, err := protoregistry.GlobalFiles.FindDescriptorByName(protoreflect.FullName(parts[0] + "." + parts[1]))
	if err != nil {
		return false
	}
	_, ok := descriptor.(protoreflect.MethodDescriptor)
	return ok
}
