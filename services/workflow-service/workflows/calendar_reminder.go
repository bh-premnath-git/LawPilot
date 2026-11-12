package workflows

import (
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"lawpilot/workflow-service/reminders"
	"time"
)

func CalendarReminderWorkflow(ctx workflow.Context, ref reminders.Ref) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 15 * time.Second, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: 5 * time.Minute}})
	var schedule reminders.Schedule
	if err := workflow.ExecuteActivity(ctx, "LoadCalendarReminder", ref).Get(ctx, &schedule); err != nil {
		return "", err
	}
	if schedule.Status != "scheduled" {
		return schedule.Status, nil
	}
	delay := schedule.DueAt.Sub(workflow.Now(ctx))
	if delay > 0 {
		if err := workflow.Sleep(ctx, delay); err != nil {
			return "cancelled", err
		}
	}
	var state string
	err := workflow.ExecuteActivity(ctx, "DeliverCalendarReminder", ref).Get(ctx, &state)
	return state, err
}
