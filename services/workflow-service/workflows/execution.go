package workflows

import (
	"errors"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"
	"lawpilot/workflow-service/executions"
)

func AIExecutionWorkflow(ctx workflow.Context, ref executions.Ref) (string, error) {
	ctx = workflow.WithActivityOptions(ctx, workflow.ActivityOptions{StartToCloseTimeout: 40 * time.Second, ScheduleToCloseTimeout: 30 * time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, BackoffCoefficient: 2, MaximumInterval: time.Minute, MaximumAttempts: 8}})
	var status string
	err := workflow.ExecuteActivity(ctx, "ExecuteAI", ref).Get(ctx, &status)
	if err == nil {
		return status, nil
	}
	code := "ACTIVITY_RETRIES_EXHAUSTED"
	var application *temporal.ApplicationError
	if errors.As(err, &application) && application.NonRetryable() {
		code = application.Type()
	}
	cleanup, _ := workflow.NewDisconnectedContext(ctx)
	cleanup = workflow.WithActivityOptions(cleanup, workflow.ActivityOptions{StartToCloseTimeout: 15 * time.Second, ScheduleToCloseTimeout: 25 * time.Minute, RetryPolicy: &temporal.RetryPolicy{InitialInterval: time.Second, MaximumInterval: time.Minute}})
	if finalErr := workflow.ExecuteActivity(cleanup, "FailExecution", executions.Failure{Ref: ref, Code: code}).Get(cleanup, &status); finalErr != nil {
		return "", finalErr
	}
	return status, nil
}
