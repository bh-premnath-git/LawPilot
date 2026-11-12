package workflows

import (
 "errors"
 "go.temporal.io/sdk/workflow"
)

// Example of durable lawyer approval. This workflow does NOT send messages or mutate Clio.
type ApprovalInput struct { TenantID string; DraftID string; DraftVersion int64; RequiredPermission string }
type Decision struct { ApproverID string; DraftVersion int64; Approved bool }
func LawyerApprovalWorkflow(ctx workflow.Context,in ApprovalInput)(string,error){
 if in.TenantID=="" || in.DraftID=="" || in.DraftVersion<1 {return "",errors.New("invalid approval input")}
 signal:=workflow.GetSignalChannel(ctx,"lawyer-approval")
 var decision Decision
 signal.Receive(ctx,&decision)
 if !decision.Approved {return "rejected",nil}
 if decision.DraftVersion!=in.DraftVersion {return "",errors.New("stale draft approval")}
 if decision.ApproverID=="" {return "",errors.New("missing approver")}
 // Production: reauthorize approver and verify immutable draft hash in a typed Activity.
 // An unverified signal is NEVER sufficient to execute a side effect.
 return "pending-authoritative-authorization",nil
}
