package main

import (
	"context"
	"github.com/bh-premnath-git/LawPilot/packages/workload"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	engine "lawpilot/context-engine"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	url := os.Getenv("EVENT_DATABASE_URL")
	if url == "" {
		log.Fatal("EVENT_DATABASE_URL required")
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Fatal("invalid events database")
	}
	defer pool.Close()
	if err = workload.CheckDatabaseRole(ctx, pool); err != nil {
		log.Fatal("runtime role is unsafe")
	}
	if err = workload.StartOperations(ctx, os.Getenv("OPS_ADDRESS"), pool.Ping); err != nil {
		log.Fatal("operations listener invalid")
	}
	if err = pool.Ping(ctx); err != nil {
		log.Fatal("events database unavailable")
	}
	var migrated bool
	if err = pool.QueryRow(ctx, "SELECT to_regprocedure('claim_domain_events(text,integer,integer)') IS NOT NULL").Scan(&migrated); err != nil || !migrated {
		log.Fatal("event migration required")
	}

	path := os.Getenv("EVENT_REGISTRY_PATH")
	if path == "" {
		path = "contracts/events/registry.v1.json"
	}
	registry, err := engine.LoadEvents(path)
	if err != nil {
		log.Fatal("invalid event registry")
	}
	worker := &engine.EventWorker{Pool: pool, Registry: registry, ID: uuid.NewString()}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	log.Println("domain publisher/consumers started")
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			call, cancel := context.WithTimeout(ctx, 25*time.Second)
			err := worker.Tick(call)
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Println("event cycle failed; leased items will retry")
			}
		}
	}
}
