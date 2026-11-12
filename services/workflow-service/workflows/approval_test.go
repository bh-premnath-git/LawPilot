package workflows

import (
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/testsuite"
)

func input() ApprovalInput {
	return ApprovalInput{TenantID: "tenant-a", DraftID: "draft-a", DraftVersion: 1, DraftSHA256: strings.Repeat("a", 64), RequiredPermission: "draft.approve", ApprovalTimeout: time.Hour}
}
func TestApprovalExpires(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	env.ExecuteWorkflow(LawyerApprovalWorkflow, input())
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := env.GetWorkflowResult(&result); err != nil || result != "expired" {
		t.Fatalf("unexpected outcome %q: %v", result, err)
	}
}
func TestWrongScopeAndStaleSignalsCannotApproveOrReject(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	in := input()
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow("lawyer-approval", Decision{TenantID: "other", DraftID: in.DraftID, DraftVersion: in.DraftVersion, DraftSHA256: in.DraftSHA256, ApproverID: "user", Approved: false})
		env.SignalWorkflow("lawyer-approval", Decision{TenantID: in.TenantID, DraftID: in.DraftID, DraftVersion: 2, DraftSHA256: in.DraftSHA256, ApproverID: "user", Approved: true})
	}, time.Minute)
	env.ExecuteWorkflow(LawyerApprovalWorkflow, in)
	var result string
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	if err := env.GetWorkflowResult(&result); err != nil || result != "expired" {
		t.Fatalf("unexpected outcome %q: %v", result, err)
	}
}
func TestValidDecisionStillRequiresAuthoritativeAuthorization(t *testing.T) {
	var suite testsuite.WorkflowTestSuite
	env := suite.NewTestWorkflowEnvironment()
	in := input()
	env.RegisterDelayedCallback(func() {
		env.SignalWorkflow("lawyer-approval", Decision{TenantID: in.TenantID, DraftID: in.DraftID, DraftVersion: in.DraftVersion, DraftSHA256: in.DraftSHA256, ApproverID: "user", Approved: true})
	}, time.Minute)
	env.ExecuteWorkflow(LawyerApprovalWorkflow, in)
	if err := env.GetWorkflowError(); err != nil {
		t.Fatal(err)
	}
	var result string
	if err := env.GetWorkflowResult(&result); err != nil || result != "pending-authoritative-authorization" {
		t.Fatalf("unexpected outcome %q: %v", result, err)
	}
}
