package workflows

import (
	"context"
	"go.temporal.io/sdk/activity"
	"go.temporal.io/sdk/testsuite"
	"lawpilot/workflow-service/jobs"
	"testing"
	"time"
)

func TestIntegrationPagesAreDurableAndTerminate(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	calls := 0
	env.RegisterActivityWithOptions(func(context.Context, jobs.Ref) (string, error) {
		calls++
		if calls == 3 {
			return "completed", nil
		}
		return "running", nil
	}, activity.RegisterOptions{Name: "RunIntegrationJob"})
	env.ExecuteWorkflow(IntegrationJobWorkflow, jobs.Ref{TenantID: "tenant", ID: "job"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result string
	env.GetWorkflowResult(&result)
	if result != "completed" || calls != 3 {
		t.Fatal(result, calls)
	}
}
func TestLegalReviewUsesDurableDeadline(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	start := time.Now()
	env.SetStartTime(start)
	env.RegisterActivityWithOptions(func(context.Context, jobs.Ref) (jobs.Deadline, error) {
		return jobs.Deadline{State: "awaiting_review", At: start.Add(24 * time.Hour)}, nil
	}, activity.RegisterOptions{Name: "LegalReviewDeadline"})
	expired := false
	env.RegisterActivityWithOptions(func(context.Context, jobs.Ref) (string, error) { expired = true; return "rejected", nil }, activity.RegisterOptions{Name: "ExpireLegalReview"})
	env.ExecuteWorkflow(LegalReviewLifecycle, jobs.Ref{TenantID: "tenant", ID: "exec"})
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if !expired || env.Now().Before(start.Add(24*time.Hour)) {
		t.Fatal("review expired before durable deadline")
	}
}
