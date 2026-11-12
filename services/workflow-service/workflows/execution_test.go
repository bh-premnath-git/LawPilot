package workflows

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/testsuite"
	"lawpilot/workflow-service/executions"
)

func TestExecutionRetriesTransientActivity(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	attempts := 0
	env.RegisterActivityWithOptions(func(context.Context, executions.Ref) (string, error) {
		attempts++
		if attempts < 3 {
			return "", errors.New("SUPERVISOR_UNAVAILABLE")
		}
		return "completed", nil
	}, activity.RegisterOptions{Name: "ExecuteAI"})
	env.ExecuteWorkflow(AIExecutionWorkflow, executions.Ref{TenantID: "tenant", ExecutionID: "execution"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var state string
	if err := env.GetWorkflowResult(&state); err != nil || state != "completed" || attempts != 3 {
		t.Fatalf("state=%s attempts=%d err=%v", state, attempts, err)
	}
}
func TestExecutionPersistsNonRetryableFailure(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.RegisterActivityWithOptions(func(context.Context, executions.Ref) (string, error) {
		return "", temporal.NewNonRetryableApplicationError("denied", "PERMISSION_DENIED", nil)
	}, activity.RegisterOptions{Name: "ExecuteAI"})
	env.RegisterActivityWithOptions(func(context.Context, executions.Failure) (string, error) { return "failed", nil }, activity.RegisterOptions{Name: "FailExecution"})
	env.OnActivity("FailExecution", mock.Anything, mock.MatchedBy(func(in executions.Failure) bool { return in.Code == "PERMISSION_DENIED" })).Return("failed", nil).Once()
	env.ExecuteWorkflow(AIExecutionWorkflow, executions.Ref{TenantID: "tenant", ExecutionID: "execution"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	env.AssertExpectations(t)
}
