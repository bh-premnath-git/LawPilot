package executions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/rpc/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
)

func (a *Activities) invokeRPC(ctx context.Context, e Execution) ([]byte, error) {
	var input map[string]any
	if json.Unmarshal(e.Input, &input) != nil {
		return nil, permanent("INVALID_REQUEST")
	}
	data, err := structpb.NewStruct(input)
	if err != nil {
		return nil, permanent("INVALID_REQUEST")
	}
	request := &pb.ExecuteCapabilityRequest{ExecutionId: e.ExecutionID, TenantId: e.TenantID, PrincipalId: e.PrincipalID, CapabilityId: e.CapabilityID, Input: data, TraceId: e.ExecutionID}
	if e.MatterID != nil {
		request.MatterId = *e.MatterID
	}
	reply, err := a.RPC.ExecuteCapability(ctx, request)
	if err != nil {
		switch status.Code(err) {
		case codes.Unavailable, codes.DeadlineExceeded, codes.ResourceExhausted:
			return nil, errors.New("SUPERVISOR_UNAVAILABLE")
		default:
			return nil, permanent("SUPERVISOR_REJECTED")
		}
	}
	if reply.Output == nil {
		return nil, permanent("INVALID_RESULT")
	}
	references := []map[string]any{}
	for _, ref := range reply.Evidence {
		value := map[string]any{"documentId": ref.DocumentId, "documentVersionId": ref.DocumentVersionId, "passageId": ref.PassageId}
		if ref.PageNumber != nil {
			value["pageNumber"] = *ref.PageNumber
		}
		references = append(references, value)
	}
	var body bytes.Buffer
	encoder := json.NewEncoder(&body)
	encoder.SetEscapeHTML(false)
	err = encoder.Encode(map[string]any{"executionId": reply.ExecutionId, "agentId": reply.AgentId, "agentVersion": reply.AgentVersion, "status": reply.Status, "output": reply.Output.AsMap(), "evidence": references, "proposedActions": []any{}, "requiresAttorneyReview": reply.RequiresAttorneyReview})
	if err != nil || body.Len() > 1048576 {
		return nil, permanent("INVALID_RESULT")
	}
	return body.Bytes(), nil
}
