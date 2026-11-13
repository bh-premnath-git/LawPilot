package jobs

import (
	"context"
	ctxpb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/context/v1"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/integrations/v1"
	"time"
)

type Ref struct {
	TenantID string `json:"tenantId"`
	ID       string `json:"id"`
	Kind     string `json:"kind"`
}
type Activities struct {
	Integrations pb.IntegrationServiceClient
	Context      ctxpb.ContextServiceClient
}

func (a *Activities) Run(ctx context.Context, r Ref) (string, error) {
	reply, err := a.Integrations.RunJob(ctx, &pb.Ref{TenantId: r.TenantID, Id: r.ID})
	if err != nil {
		return "", err
	}
	return reply.State, nil
}
func (a *Activities) Fail(ctx context.Context, r Ref) (string, error) {
	reply, err := a.Integrations.FailJob(ctx, &pb.Ref{TenantId: r.TenantID, Id: r.ID})
	if err != nil {
		return "", err
	}
	return reply.State, nil
}

type Deadline struct {
	State string
	At    time.Time
}

func (a *Activities) ReviewDeadline(ctx context.Context, r Ref) (Deadline, error) {
	reply, err := a.Context.GetReviewDeadline(ctx, &ctxpb.ExecutionRef{TenantId: r.TenantID, ExecutionId: r.ID})
	if err != nil {
		return Deadline{}, err
	}
	return Deadline{reply.State, reply.Deadline.AsTime()}, nil
}
func (a *Activities) Expire(ctx context.Context, r Ref) (string, error) {
	reply, err := a.Context.ExpireReview(ctx, &ctxpb.ExecutionRef{TenantId: r.TenantID, ExecutionId: r.ID})
	if err != nil {
		return "", err
	}
	return reply.State, nil
}
