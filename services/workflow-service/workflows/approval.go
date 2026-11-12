package workflows

import (
	"errors"
	"regexp"
	"time"

	"go.temporal.io/sdk/workflow"
)

// This workflow records a scoped decision; it never authorizes provider writes.
type ApprovalInput struct {
	TenantID           string
	DraftID            string
	DraftVersion       int64
	DraftSHA256        string
	RequiredPermission string
	ApprovalTimeout    time.Duration
}
type Decision struct {
	TenantID     string
	DraftID      string
	ApproverID   string
	DraftVersion int64
	DraftSHA256  string
	Approved     bool
}
type ApprovalState struct {
	Status         string
	IgnoredSignals int
}

func LawyerApprovalWorkflow(ctx workflow.Context, in ApprovalInput) (string, error) {
	if in.TenantID == "" || in.DraftID == "" || in.DraftVersion < 1 || in.RequiredPermission != "draft.approve" || !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(in.DraftSHA256) {
		return "", errors.New("invalid approval input")
	}
	if in.ApprovalTimeout == 0 {
		in.ApprovalTimeout = 24 * time.Hour
	}
	if in.ApprovalTimeout < 0 || in.ApprovalTimeout > 30*24*time.Hour {
		return "", errors.New("invalid approval timeout")
	}
	state := ApprovalState{Status: "pending"}
	if err := workflow.SetQueryHandler(ctx, "approval-state", func() (ApprovalState, error) { return state, nil }); err != nil {
		return "", err
	}
	deadline := workflow.Now(ctx).Add(in.ApprovalTimeout)
	signal := workflow.GetSignalChannel(ctx, "lawyer-approval")
	for {
		var decision Decision
		received, _ := signal.ReceiveWithTimeout(ctx, deadline.Sub(workflow.Now(ctx)), &decision)
		if !received {
			state.Status = "expired"
			return state.Status, nil
		}
		// Even a rejection must be bound to this tenant and immutable draft version.
		if decision.TenantID != in.TenantID || decision.DraftID != in.DraftID || decision.DraftVersion != in.DraftVersion || decision.DraftSHA256 != in.DraftSHA256 || decision.ApproverID == "" {
			state.IgnoredSignals++
			continue
		}
		if !decision.Approved {
			state.Status = "rejected"
			return state.Status, nil
		}
		// A trusted activity must reauthorize the approver before any future side effect.
		state.Status = "pending-authoritative-authorization"
		return state.Status, nil
	}
}
