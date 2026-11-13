package contextengine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5/pgxpool"
	"io"
	"os"
	"regexp"
	"strings"
	"time"
)

type EventRegistry struct {
	Version      string   `json:"version"`
	Types        []string `json:"types"`
	CommandTypes []string `json:"commandTypes"`
	Consumers    []string `json:"consumers"`
}
type EventWorker struct {
	Pool     *pgxpool.Pool
	Registry map[string]bool
	ID       string
}

func LoadEvents(path string) (map[string]bool, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var registry EventRegistry
	if json.Unmarshal(raw, &registry) != nil || registry.Version != "1.0" || len(registry.Types) == 0 {
		return nil, errors.New("invalid event registry")
	}
	out := map[string]bool{}
	for _, kind := range registry.Types {
		if out[kind] || !regexp.MustCompile(`^[a-z][a-z0-9_.]+\.v1$`).MatchString(kind) {
			return nil, errors.New("invalid event type")
		}
		out[kind] = true
	}
	return out, nil
}

type eventEnvelope struct {
	SpecVersion string `json:"specversion"`
	ID          string `json:"id"`
	Source      string `json:"source"`
	Type        string `json:"type"`
	Time        string `json:"time"`
	Tenant      string `json:"tenantId"`
	Version     string `json:"schemaVersion"`
	Aggregate   string `json:"aggregateId"`
	Correlation string `json:"correlationId,omitempty"`
	Causation   string `json:"causationId,omitempty"`
	Data        struct {
		Revision    *int64 `json:"revision,omitempty"`
		ExecutionID string `json:"executionId,omitempty"`
		State       string `json:"state,omitempty"`
		Decision    string `json:"decisionCode,omitempty"`
	} `json:"data"`
}

func (w *EventWorker) Validate(tenant, kind, version, aggregate string, body []byte) string {
	if !w.Registry[kind] {
		return "UNREGISTERED_EVENT"
	}
	var e eventEnvelope
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&e) != nil || decoder.Decode(&struct{}{}) != io.EOF || len(body) > 8192 {
		return "INVALID_EVENT_SCHEMA"
	}
	if !validID(e.ID) || !validID(aggregate) || strings.ToLower(e.Tenant) != tenant || strings.ToLower(e.Aggregate) != aggregate || e.Type != kind || e.SpecVersion != "1.0" || version != "1.0" || e.Version != "1.0" {
		return "INVALID_EVENT_SCHEMA"
	}
	if _, err := time.Parse(time.RFC3339Nano, e.Time); err != nil {
		return "INVALID_EVENT_SCHEMA"
	}
	sourceOK := e.Source == "context-engine" && strings.HasPrefix(kind, "document.") || (e.Source == "calendar-service" || e.Source == "integration-service") && strings.HasPrefix(kind, "calendar.") || (e.Source == "platform-api" || e.Source == "workflow-service" || e.Source == "migration-005") && strings.HasPrefix(kind, "lawpilot.ai.") || (e.Source == "platform-api" || e.Source == "integration-service") && kind == "matter.updated.v1"
	if !sourceOK || e.RevisionInvalid() {
		return "INVALID_EVENT_SCHEMA"
	}
	for _, id := range []string{e.Correlation, e.Causation} {
		if id != "" && !validID(id) {
			return "INVALID_EVENT_SCHEMA"
		}
	}
	if e.Data.ExecutionID != "" && e.Data.ExecutionID != aggregate {
		return "INVALID_EVENT_SCHEMA"
	}
	if len(e.Data.State) > 80 || len(e.Data.Decision) > 100 {
		return "INVALID_EVENT_SCHEMA"
	}
	return ""
}
func (e eventEnvelope) RevisionInvalid() bool {
	return e.Data.Revision != nil && *e.Data.Revision < 1 || !strings.HasPrefix(e.Type, "lawpilot.ai.") && e.Data.Revision == nil
}
func (w *EventWorker) Tick(ctx context.Context) error {
	rows, err := w.Pool.Query(ctx, "SELECT event_id,tenant_id,event_type,event_version,coalesce(aggregate_id::text,''),event_body,lease_token,created_at FROM claim_domain_events($1,20,30)", w.ID)
	if err != nil {
		return err
	}
	type command struct {
		id, tenant, kind, version, aggregate, token string
		body                                        []byte
		created                                     time.Time
	}
	commands := []command{}
	for rows.Next() {
		var c command
		if err = rows.Scan(&c.id, &c.tenant, &c.kind, &c.version, &c.aggregate, &c.body, &c.token, &c.created); err != nil {
			rows.Close()
			return err
		}
		commands = append(commands, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, c := range commands {
		code := w.Validate(c.tenant, c.kind, c.version, c.aggregate, c.body)
		var rejection any
		if code != "" {
			rejection = code
		}
		var settled bool
		if err = w.Pool.QueryRow(ctx, "SELECT publish_domain_event($1,$2,$3)", c.id, c.token, rejection).Scan(&settled); err != nil {
			var ignored bool
			_ = w.Pool.QueryRow(ctx, "SELECT publish_domain_event($1,$2,'EVENT_DELIVERY_FAILED')", c.id, c.token).Scan(&ignored)
			return errors.New("publisher transaction failed")
		}
	}
	deliveries, err := w.Pool.Query(ctx, "SELECT * FROM claim_domain_deliveries(20,30)")
	if err != nil {
		return err
	}
	type delivery struct{ id, consumer, token string }
	pending := []delivery{}
	for deliveries.Next() {
		var d delivery
		if err = deliveries.Scan(&d.id, &d.consumer, &d.token); err != nil {
			deliveries.Close()
			return err
		}
		pending = append(pending, d)
	}
	deliveries.Close()
	if deliveries.Err() != nil {
		return deliveries.Err()
	}
	for _, d := range pending {
		var settled bool
		if err = w.Pool.QueryRow(ctx, "SELECT consume_domain_delivery($1,$2,$3)", d.id, d.consumer, d.token).Scan(&settled); err != nil {
			var ignored bool
			_ = w.Pool.QueryRow(ctx, "SELECT fail_domain_delivery($1,$2,$3)", d.id, d.consumer, d.token).Scan(&ignored)
			return errors.New("consumer transaction failed")
		}
	}
	return nil
}
