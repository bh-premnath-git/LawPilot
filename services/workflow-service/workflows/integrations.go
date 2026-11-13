package workflows

import (
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"lawpilot/workflow-service/jobs"
	"time"
)

func IntegrationJobWorkflow(ctx workflow.Context, r jobs.Ref) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 5 * time.Minute, ScheduleToCloseTimeout: 30 * time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: 2 * time.Second, MaximumInterval: 30 * time.Second, MaximumAttempts: 5}})
	for i := 0; i < 100; i++ {
		var state string
		err := workflow.ExecuteActivity(ctx, "RunIntegrationJob", r).Get(ctx, &state)
		if err != nil {
			cleanup, _ := workflow.NewDisconnectedContext(ctx)
			cleanup = workflow.WithActivityOptions(cleanup, workflow.ActivityOptions{StartToCloseTimeout: 15 * time.Second, ScheduleToCloseTimeout: 10 * time.Minute})
			var failed string
			if err := workflow.ExecuteActivity(cleanup, "FailIntegrationJob", r).Get(cleanup, &failed); err != nil {
				return "", err
			}
			return failed, nil
		}
		if state != "queued" && state != "running" {
			return state, nil
		}
	}
	return "", workflow.NewContinueAsNewError(ctx, IntegrationJobWorkflow, r)
}
func LegalReviewLifecycle(ctx workflow.Context, r jobs.Ref) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 15 * time.Second, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute}})
	var deadline jobs.Deadline
	if err := workflow.ExecuteActivity(ctx, "LegalReviewDeadline", r).Get(ctx, &deadline); err != nil {
		return "", err
	}
	if deadline.State != "awaiting_review" {
		return deadline.State, nil
	}
	if delay := deadline.At.Sub(workflow.Now(ctx)); delay > 0 {
		if err := workflow.Sleep(ctx, delay); err != nil {
			return "", err
		}
	}
	var state string
	err := workflow.ExecuteActivity(ctx, "ExpireLegalReview", r).Get(ctx, &state)
	return state, err
}
