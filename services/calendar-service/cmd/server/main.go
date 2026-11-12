package main

import (
	"context"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	calendar "lawpilot/calendar-service"
)

func required(name string) string {
	value := os.Getenv(name)
	if value == "" {
		log.Fatalf("%s is required", name)
	}
	return value
}
func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := pgxpool.New(ctx, required("CALENDAR_DATABASE_URL"))
	if err != nil {
		log.Fatal("invalid database configuration")
	}
	defer pool.Close()
	if err = pool.Ping(ctx); err != nil {
		log.Fatal("database unavailable")
	}
	if _, err = pool.Exec(ctx, "SELECT id FROM calendars LIMIT 0"); err != nil {
		log.Fatal("calendar migration required")
	}
	tls, err := calendar.Credentials(required("GRPC_CA_FILE"), required("GRPC_SERVER_CERT_FILE"), required("GRPC_SERVER_KEY_FILE"))
	if err != nil {
		log.Fatal("mTLS configuration invalid")
	}
	auth, err := calendar.Authenticator(ctx, required("KEYCLOAK_ISSUER"), required("KEYCLOAK_JWKS_URL"), required("KEYCLOAK_AUDIENCE"), os.Getenv("APP_ENV"))
	if err != nil {
		log.Fatal("identity configuration invalid")
	}
	server := grpc.NewServer(grpc.Creds(tls), grpc.UnaryInterceptor(auth), grpc.MaxRecvMsgSize(262144), grpc.MaxSendMsgSize(1048576))
	pb.RegisterCalendarServiceServer(server, &calendar.Service{Pool: pool})
	checker := health.NewServer()
	checker.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, checker)
	address := os.Getenv("CALENDAR_GRPC_ADDRESS")
	if address == "" {
		address = ":50051"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("cannot listen")
	}
	go func() {
		<-ctx.Done()
		checker.Shutdown()
		done := make(chan struct{})
		go func() { server.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			server.Stop()
		}
	}()
	log.Println("calendar gRPC service started (mTLS required)")
	if err = server.Serve(listener); err != nil {
		log.Fatal("calendar gRPC server stopped")
	}
}
