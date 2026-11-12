package workflows

import (
	"context"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"lawpilot/workflow-service/reminders"
	"testing"
	"time"
)

func TestReminderDurableTimerAndSuppression(t *testing.T) {
	for _, suppressed := range []bool{false, true} {
		var suite testsuite.WorkflowTestSuite
		env := suite.NewTestWorkflowEnvironment()
		due := time.Now().Add(24 * time.Hour)
		env.SetStartTime(due.Add(-time.Hour))
		deliveries := 0
		env.RegisterActivityWithOptions(func(context.Context, reminders.Ref) (reminders.Schedule, error) {
			state := "scheduled"
			if suppressed {
				state = "cancelled"
			}
			return reminders.Schedule{Status: state, DueAt: due}, nil
		}, activity.RegisterOptions{Name: "LoadCalendarReminder"})
		env.RegisterActivityWithOptions(func(context.Context, reminders.Ref) (string, error) { deliveries++; return "due", nil }, activity.RegisterOptions{Name: "DeliverCalendarReminder"})
		env.ExecuteWorkflow(CalendarReminderWorkflow, reminders.Ref{TenantID: "tenant", ReminderID: "reminder"})
		if err := env.GetWorkflowError(); err != nil {
			t.Fatal(err)
		}
		if suppressed && deliveries != 0 || !suppressed && deliveries != 1 {
			t.Fatal("incorrect reminder delivery count")
		}
	}
}
