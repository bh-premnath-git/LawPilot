package main

import (
	"context"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"google.golang.org/grpc/keepalive"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	engine "lawpilot/context-engine"
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
	database := workload.Secret("CONTEXT_DATABASE_URL")
	if database == "" || workload.DatabaseURL(database) != nil {
		log.Fatal("database TLS/configuration invalid")
	}
	pool, err := pgxpool.New(ctx, database)
	if err != nil {
		log.Fatal("invalid database configuration")
	}
	defer pool.Close()
	if err = workload.CheckDatabaseRole(ctx, pool); err != nil {
		log.Fatal("runtime database role is unsafe")
	}
	if err = pool.Ping(ctx); err != nil {
		log.Fatal("database unavailable")
	}
	if _, err = pool.Exec(ctx, "SELECT snapshot_sha256 FROM context_packages LIMIT 0"); err != nil {
		log.Fatal("context migration required")
	}
	var evidenceReady bool
	if err = pool.QueryRow(ctx, "SELECT to_regprocedure('verify_ai_execution_result(uuid,uuid,jsonb)') IS NOT NULL").Scan(&evidenceReady); err != nil || !evidenceReady {
		log.Fatal("evidence migration required")
	}
	tls, err := engine.Credentials(required("GRPC_CA_FILE"), required("GRPC_SERVER_CERT_FILE"), required("GRPC_SERVER_KEY_FILE"))
	if err != nil {
		log.Fatal("mTLS configuration invalid")
	}
	auth, err := engine.Authenticator(ctx, required("KEYCLOAK_ISSUER"), required("KEYCLOAK_JWKS_URL"), required("KEYCLOAK_AUDIENCE"), os.Getenv("APP_ENV"))
	if err != nil {
		log.Fatal("identity configuration invalid")
	}
	server := grpc.NewServer(grpc.Creds(tls), grpc.ChainUnaryInterceptor(workload.Guard(32, 5*time.Minute), auth), grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: 5 * time.Minute, MaxConnectionAgeGrace: 30 * time.Second}), grpc.MaxRecvMsgSize(1048576), grpc.MaxSendMsgSize(1048576))
	service, err := engine.New(pool)
	if err != nil {
		log.Fatal("context/embedding configuration invalid")
	}
	pb.RegisterContextServiceServer(server, service)
	checker := health.NewServer()
	checker.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)
	healthpb.RegisterHealthServer(server, checker)
	address := os.Getenv("CONTEXT_GRPC_ADDRESS")
	if address == "" {
		address = ":50054"
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
	if err = workload.StartOperations(ctx, os.Getenv("OPS_ADDRESS"), pool.Ping); err != nil {
		log.Fatal("operations listener invalid")
	}
	log.Println("context gRPC service started (mTLS required)")
	if err = server.Serve(listener); err != nil {
		log.Fatal("context gRPC server stopped")
	}
}
