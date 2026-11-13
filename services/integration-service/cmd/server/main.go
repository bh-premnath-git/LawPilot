package main

import (
	"context"
	ctxpb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"github.com/jackc/pgx/v5/pgxpool"
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	app "lawpilot/integration-service"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	stopTelemetry, telemetryErr := workload.StartTelemetry("integration-service")
	if telemetryErr != nil {
		log.Fatal("telemetry configuration invalid")
	}
	defer stopTelemetry()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	url := workload.Secret("INTEGRATION_DATABASE_URL")
	if url == "" || workload.DatabaseURL(url) != nil {
		log.Fatal("integration database/security configuration required")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatal("database configuration invalid")
	}
	defer pool.Close()
	if err = workload.CheckDatabaseRole(ctx, pool); err != nil {
		log.Fatal("runtime database role is unsafe")
	}
	if err = pool.Ping(ctx); err != nil {
		log.Fatal("database unavailable")
	}
	if _, err = pool.Exec(ctx, "SELECT remote_recurrence FROM provider_sync_bindings LIMIT 0"); err != nil {
		log.Fatal("integration migration required")
	}
	providers, keys, err := app.NewProviders()
	if err != nil {
		log.Fatal("provider configuration invalid")
	}
	if err = app.ValidateExtensions(providers, os.Getenv("PROVIDER_WRITES_ENABLED") == "true"); err != nil {
		log.Fatal("provider extension configuration invalid")
	}
	storage, err := app.NewStorage(ctx)
	if err != nil {
		log.Fatal("storage configuration invalid")
	}
	service := &app.Service{Pool: pool, Providers: providers, Keys: keys, Storage: storage, Writes: os.Getenv("PROVIDER_WRITES_ENABLED") == "true"}
	if service.Writes && len(providers) == 0 {
		log.Fatal("provider writes require configured providers")
	}
	if storage != nil {
		service.Scanner, err = app.NewScanner()
		if err != nil {
			log.Fatal("scanner configuration invalid")
		}
		for _, target := range []struct{ Address, Name string }{{os.Getenv("PARSER_GRPC_ADDRESS"), "document-parser"}, {os.Getenv("CONTEXT_GRPC_ADDRESS"), "context-engine"}} {
			if target.Address == "" {
				log.Fatal("file processing RPC addresses required")
			}
			creds, err := workload.ClientCredentials(target.Name, "integration-service")
			if err != nil {
				log.Fatal("client mTLS invalid")
			}
			connection, err := grpc.NewClient(target.Address, grpc.WithStatsHandler(workload.ClientTelemetry()), grpc.WithTransportCredentials(creds), grpc.WithDefaultCallOptions(grpc.MaxCallSendMsgSize(11*1024*1024), grpc.MaxCallRecvMsgSize(2*1024*1024)))
			if err != nil {
				log.Fatal("RPC configuration invalid")
			}
			defer connection.Close()
			if target.Name == "document-parser" {
				service.Parser = pb.NewDocumentParserClient(connection)
			} else {
				service.Context = ctxpb.NewContextServiceClient(connection)
			}
		}
	}
	creds, err := workload.ServerCredentials(os.Getenv("GRPC_CA_FILE"), os.Getenv("GRPC_SERVER_CERT_FILE"), os.Getenv("GRPC_SERVER_KEY_FILE"))
	if err != nil {
		log.Fatal("server TLS required")
	}
	auth, err := app.Authenticator(ctx, os.Getenv("KEYCLOAK_ISSUER"), os.Getenv("KEYCLOAK_JWKS_URL"), os.Getenv("KEYCLOAK_AUDIENCE"), os.Getenv("APP_ENV"))
	if err != nil {
		log.Fatal("identity config invalid")
	}
	server := grpc.NewServer(grpc.StatsHandler(workload.ServerTelemetry()), grpc.Creds(creds), grpc.ChainUnaryInterceptor(workload.Guard(32, 5*time.Minute), auth), grpc.KeepaliveParams(keepalive.ServerParameters{MaxConnectionAge: 5 * time.Minute, MaxConnectionAgeGrace: 30 * time.Second}), grpc.MaxRecvMsgSize(131072), grpc.MaxSendMsgSize(1048576))
	pb.RegisterIntegrationServiceServer(server, service)
	address := os.Getenv("INTEGRATION_GRPC_BIND")
	if address == "" {
		address = ":50055"
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Fatal("listen unavailable")
	}
	if err = workload.StartOperations(ctx, os.Getenv("OPS_ADDRESS"), pool.Ping, workload.QueueMetrics(pool)); err != nil {
		log.Fatal("operations listener failed")
	}
	go func() {
		<-ctx.Done()
		done := make(chan struct{})
		go func() { server.GracefulStop(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			server.Stop()
		}
	}()
	if service.Storage != nil {
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if service.CleanupExpired(ctx) != nil {
						log.Println("expired upload cleanup will retry")
					}
				}
			}
		}()
	}
	go func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if service.DrainSyncSignals(ctx) != nil {
					log.Println("provider sync signals will retry")
				}
			}
		}
	}()
	log.Println("integration service started; optional capabilities are explicitly configured")
	if server.Serve(listener) != nil {
		log.Fatal("service stopped")
	}
}
