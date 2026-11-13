package workload

import (
	"context"
	"fmt"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/health"
	pb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/stats"
	"net"
	"strings"
	"testing"
	"time"
)

func TestGRPCTraceContinuityWithoutLegalPayloadAttributes(t *testing.T) {
	previous := otel.GetTracerProvider()
	propagator := otel.GetTextMapPropagator()
	defer otel.SetTracerProvider(previous)
	defer otel.SetTextMapPropagator(propagator)
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	defer provider.Shutdown(context.Background())
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	server := grpc.NewServer(grpc.StatsHandler(ServerTelemetry()))
	pb.RegisterHealthServer(server, health.NewServer())
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	defer server.Stop()
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithStatsHandler(ClientTelemetry()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	ctx, parent := provider.Tracer("synthetic").Start(context.Background(), "parent")
	traceID := parent.SpanContext().TraceID()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-sensitive-fixture", "synthetic-confidential")
	_, err = pb.NewHealthClient(connection).Check(ctx, &pb.HealthCheckRequest{})
	parent.End()
	if err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Second)
	for len(recorder.Ended()) < 3 && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	spans := recorder.Ended()
	if len(spans) < 3 {
		t.Fatal("RPC spans were not recorded")
	}
	for _, span := range spans {
		if span.SpanContext().TraceID() != traceID {
			t.Fatal("RPC trace was split")
		}
		if strings.Contains(fmt.Sprint(span.Attributes()), "synthetic-confidential") {
			t.Fatal("sensitive metadata exported")
		}
	}
}
func TestTelemetryEndpointRequiresProductionHTTPS(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "http://collector:4318")
	if _, err := StartTelemetry("test"); err == nil {
		t.Fatal("plaintext production exporter accepted")
	}
}

func TestUnknownRPCNamesCannotBecomeTraceLabels(t *testing.T) {
	if knownRPC(&stats.RPCTagInfo{FullMethodName: "/private-sensitive-data/not-a-contract"}) {
		t.Fatal("unknown RPC exported")
	}
	if !knownRPC(&stats.RPCTagInfo{FullMethodName: "/grpc.health.v1.Health/Check"}) {
		t.Fatal("registered RPC suppressed")
	}
}
