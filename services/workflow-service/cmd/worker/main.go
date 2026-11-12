package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"time"

	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
	"lawpilot/workflow-service/executions"
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
	if len(secret) < 32 || url == "" {
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
	w.RegisterWorkflow(workflows.AIExecutionWorkflow)
	w.RegisterWorkflow(workflows.LawyerApprovalWorkflow)
	w.RegisterActivityWithOptions(actions.Run, activity.RegisterOptions{Name: "ExecuteAI"})
	w.RegisterActivityWithOptions(actions.Fail, activity.RegisterOptions{Name: "FailExecution"})
	log.Println("execution worker started")
	if err = w.Run(worker.InterruptCh()); err != nil {
		log.Fatal("execution worker stopped with error")
	}
}
