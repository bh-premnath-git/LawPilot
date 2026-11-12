package executions

import (
	"context"
	"errors"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
	"lawpilot/workflow-service/reminders"
	"time"
)

type ReminderStarter interface {
	StartReminder(context.Context, string, string, reminders.Ref) error
}

func (t TemporalStarter) StartReminder(ctx context.Context, kind, id string, ref reminders.Ref) error {
	if kind == "calendar.reminder.cancelled.v1" {
		err := t.Client.CancelWorkflow(ctx, id, "")
		var absent *serviceerror.NotFound
		if errors.As(err, &absent) {
			return nil
		}
		return err
	}
	_, err := t.Client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{ID: id, TaskQueue: t.TaskQueue, WorkflowIDReusePolicy: enumspb.WORKFLOW_ID_REUSE_POLICY_REJECT_DUPLICATE}, "CalendarReminderWorkflow", ref)
	var existing *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &existing) {
		return nil
	}
	return err
}
func (d *Dispatcher) TickReminders(ctx context.Context) error {
	rows, err := d.Pool.Query(ctx, "SELECT * FROM claim_calendar_reminder_commands($1,1,30)", d.WorkerID)
	if err != nil {
		return err
	}
	type command struct{ event, tenant, reminder, kind, workflow, token string }
	commands := []command{}
	for rows.Next() {
		var c command
		if err = rows.Scan(&c.event, &c.tenant, &c.reminder, &c.kind, &c.workflow, &c.token); err != nil {
			rows.Close()
			return err
		}
		commands = append(commands, c)
	}
	rows.Close()
	if rows.Err() != nil {
		return rows.Err()
	}
	starter, ok := d.Starter.(ReminderStarter)
	if !ok {
		return errors.New("reminder starter required")
	}
	for _, c := range commands {
		call, cancel := context.WithTimeout(ctx, 10*time.Second)
		err = starter.StartReminder(call, c.kind, c.workflow, reminders.Ref{TenantID: c.tenant, ReminderID: c.reminder})
		cancel()
		var code any
		if err != nil {
			code = "TEMPORAL_UNAVAILABLE"
		}
		var settled bool
		if err = d.Pool.QueryRow(ctx, "SELECT settle_execution_command($1,$2,$3)", c.event, c.token, code).Scan(&settled); err != nil {
			return err
		}
	}
	return nil
}
