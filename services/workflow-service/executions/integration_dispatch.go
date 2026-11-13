package executions

import (
	"context"
	"errors"
	"go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"lawpilot/workflow-service/jobs"
	"time"
)

type IntegrationStarter interface {
	StartIntegration(context.Context, string, jobs.Ref) error
}

func (t TemporalStarter) StartIntegration(ctx context.Context, id string, ref jobs.Ref) error {
	name := "IntegrationJobWorkflow"
	if ref.Kind == "legal_review" {
		name = "LegalReviewLifecycle"
	}
	_, err := t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: t.TaskQueue, WorkflowIDReusePolicy: enums.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, name, ref)
	var duplicate *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &duplicate) {
		return nil
	}
	return err
}
func (d *Dispatcher) TickIntegrations(ctx context.Context) error {
	rows, err := d.Pool.Query(ctx, "SELECT * FROM claim_integration_commands($1,10,30)", d.WorkerID)
	if err != nil {
		return err
	}
	type command struct{ Event, Tenant, ID, Kind, Workflow, Token string }
	items := []command{}
	for rows.Next() {
		var c command
		if err = rows.Scan(&c.Event, &c.Tenant, &c.ID, &c.Kind, &c.Workflow, &c.Token); err != nil {
			rows.Close()
			return err
		}
		items = append(items, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	starter, ok := d.Starter.(IntegrationStarter)
	if !ok {
		return errors.New("integration starter required")
	}
	for _, c := range items {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = starter.StartIntegration(call, c.Workflow, jobs.Ref{TenantID: c.Tenant, ID: c.ID, Kind: c.Kind})
		cancel()
		var failure any
		if err != nil {
			failure = "TEMPORAL_UNAVAILABLE"
		}
		var settled bool
		if err = d.Pool.QueryRow(ctx, "SELECT settle_execution_command($1,$2,$3)", c.Event, c.Token, failure).Scan(&settled); err != nil {
			return err
		}
	}
	return nil
}
