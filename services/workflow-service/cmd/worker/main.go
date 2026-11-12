package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	calendarpb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	rpcpb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/rpc/v1"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"google.golang.org/grpc"
	"lawpilot/workflow-service/executions"
	"lawpilot/workflow-service/reminders"
	"lawpilot/workflow-service/rpcclient"
	"lawpilot/workflow-service/workflows"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func main() {
	secret := os.Getenv("INTERNAL_HMAC_KEY")
	url := os.Getenv("WORKFLOW_DATABASE_URL")
	legacy := os.Getenv("ENABLE_LEGACY_INTERNAL_HTTP") == "true" && os.Getenv("APP_ENV") == "development"
	if url == "" || (legacy && len(secret) < 32) {
		log.Fatal("WORKFLOW_DATABASE_URL and INTERNAL_HMAC_KEY (32+ characters) required")
	}
	pool, err := executions.Open(context.Background(), url)
	if err != nil {
		log.Fatal("database configuration invalid")
	}
	defer pool.Close()
	registry, err := executions.LoadRegistry(env("CAPABILITY_REGISTRY_PATH", "contracts/capabilities/registry.v1.json"))
	if err != nil {
		log.Fatal("capability registry invalid")
	}
	c, err := client.Dial(client.Options{HostPort: env("TEMPORAL_ADDRESS", "127.0.0.1:7233"), Namespace: env("TEMPORAL_NAMESPACE", "default")})
	if err != nil {
		log.Fatal("Temporal unavailable")
	}
	defer c.Close()
	w := worker.New(c, env("TEMPORAL_TASK_QUEUE", "lawpilot-executions-v1"), worker.Options{})
	actions := &executions.Activities{Store: &executions.Store{Pool: pool}, Registry: registry, SupervisorURL: env("SUPERVISOR_URL", "http://127.0.0.1:3001"), Secret: secret, HTTP: &http.Client{Timeout: 25 * time.Second}}
	if target := os.Getenv("CALENDAR_GRPC_ADDRESS"); target != "" {
		credentials, err := rpcclient.Credentials("calendar-service")
		if err != nil {
			log.Fatal("calendar mTLS client configuration invalid")
		}
		connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials))
		if err != nil {
			log.Fatal("calendar gRPC configuration invalid")
		}
		defer connection.Close()
		reminder := &reminders.Activities{Client: calendarpb.NewCalendarServiceClient(connection)}
		w.RegisterWorkflow(workflows.CalendarReminderWorkflow)
		w.RegisterActivityWithOptions(reminder.Load, activity.RegisterOptions{Name: "LoadCalendarReminder"})
		w.RegisterActivityWithOptions(reminder.Deliver, activity.RegisterOptions{Name: "DeliverCalendarReminder"})
	}
	if target := os.Getenv("SUPERVISOR_GRPC_ADDRESS"); target != "" {
		credentials, err := rpcclient.Credentials("root-supervisor")
		if err != nil {
			log.Fatal("supervisor mTLS configuration invalid")
		}
		connection, err := grpc.NewClient(target, grpc.WithTransportCredentials(credentials), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(1048576)))
		if err != nil {
			log.Fatal("supervisor gRPC configuration invalid")
		}
		defer connection.Close()
		actions.RPC = rpcpb.NewSupervisorServiceClient(connection)
	} else if legacy {
		actions.AllowLegacyHTTP = true
	} else {
		log.Fatal("SUPERVISOR_GRPC_ADDRESS is required")
	}
	w.RegisterWorkflow(workflows.AIExecutionWorkflow)
	w.RegisterWorkflow(workflows.LawyerApprovalWorkflow)
	w.RegisterActivityWithOptions(actions.Run, activity.RegisterOptions{Name: "ExecuteAI"})
	w.RegisterActivityWithOptions(actions.Fail, activity.RegisterOptions{Name: "FailExecution"})
	log.Println("execution worker started")
	if err = w.Run(worker.InterruptCh()); err != nil {
		log.Fatal("execution worker stopped with error")
	}
}
