package executions

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

type Command struct {
	EventID    string
	Ref        Ref
	Type       string
	WorkflowID string
	Token      string
	Attempt    int
}
type WorkflowStarter interface {
	Start(context.Context, Command) error
	Describe(context.Context, string) (enumspb.WorkflowExecutionStatus, error)
}
type TemporalStarter struct {
	Client    client.Client
	TaskQueue string
}

func (t TemporalStarter) Start(ctx context.Context, command Command) error {
	if command.Type == "lawpilot.ai.cancelled.v1" {
		err := t.Client.CancelWorkflow(ctx, command.WorkflowID, "")
		var absent *serviceerror.NotFound
		if errors.As(err, &absent) {
			return nil
		}
		return err
	}
	_, err := t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: command.WorkflowID, TaskQueue: t.TaskQueue, WorkflowExecutionTimeout: time.Hour, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, "AIExecutionWorkflow", command.Ref)
	var existing *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &existing) {
		return nil
	}
	return err
}
func (t TemporalStarter) Describe(ctx context.Context, id string) (enumspb.WorkflowExecutionStatus, error) {
	result, err := t.Client.DescribeWorkflowExecution(ctx, id, "")
	if err != nil {
		return 0, err
	}
	return result.WorkflowExecutionInfo.Status, nil
}

type Dispatcher struct {
	Pool     *pgxpool.Pool
	Starter  WorkflowStarter
	Store    *Store
	WorkerID string
}

func (d *Dispatcher) Tick(ctx context.Context) error {
	rows, err := d.Pool.Query(ctx, "SELECT * FROM claim_execution_commands($1,1,30)", d.WorkerID)
	if err != nil {
		return err
	}
	commands := []Command{}
	for rows.Next() {
		var c Command
		if err = rows.Scan(&c.EventID, &c.Ref.TenantID, &c.Ref.ExecutionID, &c.Type, &c.WorkflowID, &c.Token, &c.Attempt); err != nil {
			rows.Close()
			return err
		}
		commands = append(commands, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, command := range commands {
		callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = d.Starter.Start(callCtx, command)
		cancel()
		// A lost start acknowledgement is reconciled by the same workflow ID. Never
		// create a new ID or retry an uncertain provider write here.
		var code any
		if err != nil {
			code = "TEMPORAL_UNAVAILABLE"
		}
		var settled bool
		if err = d.Pool.QueryRow(ctx, "SELECT settle_execution_command($1,$2,$3)", command.EventID, command.Token, code).Scan(&settled); err != nil {
			return err
		}
	}
	return nil
}
func (d *Dispatcher) Reconcile(ctx context.Context) error {
	rows, err := d.Pool.Query(ctx, "SELECT * FROM stale_execution_commands(100)")
	if err != nil {
		return err
	}
	type stale struct {
		Ref        Ref
		WorkflowID string
	}
	refs := []stale{}
	for rows.Next() {
		var value stale
		if err = rows.Scan(&value.Ref.TenantID, &value.Ref.ExecutionID, &value.WorkflowID); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, value)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	for _, value := range refs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		status, err := d.Starter.Describe(callCtx, value.WorkflowID)
		cancel()
		if err != nil {
			continue
		}
		if status == enumspb.WORKFLOW_EXECUTION_STATUS_FAILED || status == enumspb.WORKFLOW_EXECUTION_STATUS_TIMED_OUT || status == enumspb.WORKFLOW_EXECUTION_STATUS_TERMINATED || status == enumspb.WORKFLOW_EXECUTION_STATUS_CANCELED {
			if _, err = d.Store.Finish(ctx, value.Ref, "failed", "WORKFLOW_TERMINATED", nil, false); err != nil {
				return err
			}
		}
	}
	return nil
}
