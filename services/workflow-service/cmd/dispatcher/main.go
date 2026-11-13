package main

import (
	"context"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"go.temporal.io/sdk/client"
	"lawpilot/workflow-service/executions"
	"lawpilot/workflow-service/rpcclient"
)

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func main() {
	if workload.Secret("DISPATCHER_DATABASE_URL") == "" || workload.Secret("WORKFLOW_DATABASE_URL") == "" {
		log.Fatal("dispatcher and workflow DB URLs required")
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	pool, err := executions.Open(ctx, workload.Secret("DISPATCHER_DATABASE_URL"))
	if err != nil {
		log.Fatal("dispatcher DB configuration invalid")
	}
	defer pool.Close()
	if err = workload.CheckDatabaseRole(ctx, pool); err != nil {
		log.Fatal("runtime database role unsafe")
	}
	if err = workload.StartOperations(ctx, os.Getenv("OPS_ADDRESS"), pool.Ping); err != nil {
		log.Fatal("operations listener invalid")
	}
	statePool, err := executions.Open(ctx, workload.Secret("WORKFLOW_DATABASE_URL"))
	if err != nil {
		log.Fatal("workflow DB configuration invalid")
	}
	defer statePool.Close()
	if err = workload.CheckDatabaseRole(ctx, statePool); err != nil {
		log.Fatal("workflow database role unsafe")
	}
	options, err := rpcclient.TemporalOptions(env("TEMPORAL_ADDRESS", "127.0.0.1:7233"), env("TEMPORAL_NAMESPACE", "default"))
	if err != nil {
		log.Fatal("Temporal security configuration invalid")
	}
	c, err := client.Dial(options)
	if err != nil {
		log.Fatal("Temporal unavailable")
	}
	defer c.Close()
	host, _ := os.Hostname()
	d := &executions.Dispatcher{Pool: pool, Store: &executions.Store{Pool: statePool}, Starter: executions.TemporalStarter{Client: c, TaskQueue: env("TEMPORAL_TASK_QUEUE", "lawpilot-executions-v1")}, WorkerID: env("DISPATCHER_ID", host)}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	reconcile := time.NewTicker(time.Minute)
	defer reconcile.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if os.Getenv("ENABLE_INTEGRATION_JOBS") == "true" {
				if err = d.TickIntegrations(ctx); err != nil {
					log.Println("integration command dispatch failed")
				}
			}
			if os.Getenv("ENABLE_CALENDAR_REMINDERS") == "true" {
				if err = d.TickReminders(ctx); err != nil {
					log.Println("calendar reminder dispatch cycle failed")
				}
			}
			if err = d.Tick(ctx); err != nil {
				log.Println("dispatch cycle failed; leases will be retried")
			}
		case <-reconcile.C:
			cycle, done := context.WithTimeout(ctx, 20*time.Second)
			err = d.Reconcile(cycle)
			done()
			if err != nil {
				log.Println("reconciliation cycle failed")
			}
		}
	}
}
