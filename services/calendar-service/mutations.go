package calendar

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func (s *Service) CreateCalendar(ctx context.Context, r *pb.CreateCalendarRequest) (*pb.CreateCalendarResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.CreateCalendarResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, "", true, func(tx pgx.Tx) error {
		if err := member(ctx, tx, tenant, who); err != nil {
			return err
		}
		ok, err := capability(ctx, tx, tenant, who, "calendar.manage")
		if err != nil {
			return err
		}
		if !ok {
			return status.Error(codes.PermissionDenied, "PERMISSION_DENIED")
		}
		if len(strings.TrimSpace(r.Name)) == 0 || len(r.Name) > 120 {
			return bad("INVALID_CALENDAR")
		}
		if _, err = zone(r.DefaultTimeZone); err != nil {
			return err
		}
		hash := fingerprint("create-calendar", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		approval := true
		if r.ApprovalRequired != nil {
			approval = *r.ApprovalRequired
		}
		id := uuid.NewString()
		if _, err = tx.Exec(ctx, "INSERT INTO calendars(id,tenant_id,name,default_time_zone,approval_required,created_by)VALUES($1,$2,$3,$4,$5,$6)", id, tenant, r.Name, r.DefaultTimeZone, approval, who); err != nil {
			return err
		}
		for _, permission := range []string{"read", "write", "approve", "manage"} {
			if _, err = tx.Exec(ctx, "INSERT INTO calendar_grants(tenant_id,calendar_id,principal_id,permission)VALUES($1,$2,$3,$4)", tenant, id, who, permission); err != nil {
				return err
			}
		}
		reply.Calendar, err = calendarRecord(ctx, tx, tenant, id)
		if err != nil {
			return err
		}
		if err = event(ctx, tx, tenant, who, id, "calendar.created.v1", 1, "native_only"); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func (s *Service) GrantCalendar(ctx context.Context, r *pb.GrantCalendarRequest) (*pb.GrantCalendarResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.GrantCalendarResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, true, func(tx pgx.Tx) error {
		calendar, _, err := authorize(ctx, tx, tenant, r.CalendarId, who, "manage")
		if err != nil {
			return err
		}
		if err = member(ctx, tx, tenant, r.PrincipalId); err != nil {
			return status.Error(codes.PermissionDenied, "TARGET_OUTSIDE_TENANT")
		}
		valid := map[string]bool{}
		for _, p := range r.Permissions {
			if p != "read" && p != "write" && p != "approve" && p != "manage" || valid[p] {
				return bad("INVALID_GRANT")
			}
			valid[p] = true
		}
		hash := fingerprint("grant-calendar", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		if r.ExpectedRevision != calendar.Revision {
			return status.Error(codes.Aborted, "STALE_REVISION")
		}
		if _, err = tx.Exec(ctx, "DELETE FROM calendar_grants WHERE tenant_id=$1 AND calendar_id=$2 AND principal_id=$3", tenant, r.CalendarId, r.PrincipalId); err != nil {
			return err
		}
		for _, p := range r.Permissions {
			if _, err = tx.Exec(ctx, "INSERT INTO calendar_grants(tenant_id,calendar_id,principal_id,permission)VALUES($1,$2,$3,$4)", tenant, r.CalendarId, r.PrincipalId, p); err != nil {
				return err
			}
		}
		if _, err = tx.Exec(ctx, "UPDATE calendars SET revision=revision+1 WHERE tenant_id=$1 AND id=$2", tenant, r.CalendarId); err != nil {
			return err
		}
		reply.Calendar, err = calendarRecord(ctx, tx, tenant, r.CalendarId)
		if err != nil {
			return err
		}
		if err = event(ctx, tx, tenant, who, r.CalendarId, "calendar.grants.updated.v1", reply.Calendar.Revision, "grants_updated"); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func commandMatters(ctx context.Context, tx pgx.Tx, tenant, calendar, who string, command *pb.CalendarCommand) error {
	if command == nil {
		return bad("COMMAND_REQUIRED")
	}
	var draft *pb.EntryDraft
	id := ""
	if c := command.GetCreate(); c != nil {
		draft = c.Draft
	} else if c := command.GetUpdate(); c != nil {
		draft = c.Draft
		id = c.EntryId
	} else if c := command.GetCancel(); c != nil {
		id = c.EntryId
	} else {
		return bad("COMMAND_REQUIRED")
	}
	if draft != nil {
		if err := matterAccess(ctx, tx, tenant, who, draft.MatterId); err != nil {
			return err
		}
	}
	if id != "" {
		e, _, err := loadEntry(ctx, tx, tenant, calendar, id)
		if err != nil {
			return err
		}
		return matterAccess(ctx, tx, tenant, who, e.MatterId)
	}
	return nil
}
func (s *Service) MutateEntry(ctx context.Context, r *pb.MutateEntryRequest) (*pb.MutateEntryResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.MutateEntryResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, true, func(tx pgx.Tx) error {
		calendar, _, err := authorize(ctx, tx, tenant, r.CalendarId, who, "write")
		if err != nil {
			return err
		}
		if err = commandMatters(ctx, tx, tenant, r.CalendarId, who, r.Command); err != nil {
			return err
		}
		hash := fingerprint("mutate-entry", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		if calendar.ApprovalRequired {
			return status.Error(codes.FailedPrecondition, "APPROVAL_REQUIRED")
		}
		reply.Result, err = s.command(ctx, tx, tenant, r.CalendarId, who, r.Command, false)
		if err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func (s *Service) ProposeEntry(ctx context.Context, r *pb.ProposeEntryRequest) (*pb.ProposeEntryResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.ProposeEntryResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, true, func(tx pgx.Tx) error {
		if _, _, err := authorize(ctx, tx, tenant, r.CalendarId, who, "write"); err != nil {
			return err
		}
		if err := commandMatters(ctx, tx, tenant, r.CalendarId, who, r.Command); err != nil {
			return err
		}
		hash := fingerprint("propose-entry", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		preview, err := s.command(ctx, tx, tenant, r.CalendarId, who, r.Command, true)
		if err != nil {
			return err
		}
		body, err := proto.MarshalOptions{Deterministic: true}.Marshal(r.Command)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(body)
		id := uuid.NewString()
		if _, err = tx.Exec(ctx, "INSERT INTO calendar_proposals(id,tenant_id,calendar_id,command,sha256,proposed_by,affected_count,affected_ids)VALUES($1,$2,$3,$4,$5,$6,$7,$8)", id, tenant, r.CalendarId, body, hex.EncodeToString(digest[:]), who, len(preview.EntryIds), previewIDs(r.Command, preview)); err != nil {
			return err
		}
		reply.Result = &pb.MutationResult{ProposalId: id, Status: "pending", AffectedCount: int32(len(preview.EntryIds)), AffectedEntryIds: previewIDs(r.Command, preview)}
		if err = event(ctx, tx, tenant, who, id, "calendar.entry.proposed.v1", 1, "pending"); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}
func proposal(ctx context.Context, tx pgx.Tx, tenant, calendar, id string) (*pb.GetProposalResponse, error) {
	if !identifier(id) {
		return nil, bad("INVALID_PROPOSAL_ID")
	}
	reply := &pb.GetProposalResponse{ProposalId: id, Command: new(pb.CalendarCommand)}
	var body []byte
	err := tx.QueryRow(ctx, "SELECT command,sha256,status,proposed_by,affected_count,affected_ids FROM calendar_proposals WHERE tenant_id=$1 AND calendar_id=$2 AND id=$3", tenant, calendar, id).Scan(&body, &reply.Sha256, &reply.Status, &reply.ProposedBy, &reply.AffectedCount, &reply.AffectedEntryIds)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "NOT_FOUND")
	}
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	if hex.EncodeToString(digest[:]) != reply.Sha256 {
		return nil, status.Error(codes.DataLoss, "PROPOSAL_INTEGRITY_FAILURE")
	}
	if err = proto.Unmarshal(body, reply.Command); err != nil {
		return nil, err
	}
	return reply, nil
}
func (s *Service) GetProposal(ctx context.Context, r *pb.GetProposalRequest) (*pb.GetProposalResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	var reply *pb.GetProposalResponse
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, false, func(tx pgx.Tx) error {
		if _, _, err := authorize(ctx, tx, tenant, r.CalendarId, who, "write"); err != nil {
			return err
		}
		var err error
		reply, err = proposal(ctx, tx, tenant, r.CalendarId, r.ProposalId)
		if err != nil {
			return err
		}
		if who != reply.ProposedBy {
			if _, _, err = authorize(ctx, tx, tenant, r.CalendarId, who, "approve"); err != nil {
				return status.Error(codes.NotFound, "NOT_FOUND")
			}
		}
		return commandMatters(ctx, tx, tenant, r.CalendarId, who, reply.Command)
	})
	return reply, err
}
func (s *Service) DecideProposal(ctx context.Context, r *pb.DecideProposalRequest) (*pb.DecideProposalResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.DecideProposalResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, true, func(tx pgx.Tx) error {
		if _, _, err := authorize(ctx, tx, tenant, r.CalendarId, who, "approve"); err != nil {
			return err
		}
		stored, err := proposal(ctx, tx, tenant, r.CalendarId, r.ProposalId)
		if err != nil {
			return err
		}
		if r.ExpectedSha256 != stored.Sha256 {
			return status.Error(codes.Aborted, "STALE_PROPOSAL")
		}
		if r.Decision != "approve" && r.Decision != "reject" {
			return bad("INVALID_DECISION")
		}
		if err = commandMatters(ctx, tx, tenant, r.CalendarId, who, stored.Command); err != nil {
			return err
		}
		hash := fingerprint("decide-proposal", r)
		hit, err := cached(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
		if err != nil || hit {
			return err
		}
		if stored.Status != "pending" {
			return status.Error(codes.AlreadyExists, "PROPOSAL_ALREADY_DECIDED")
		}
		state := "rejected"
		reply.Result = &pb.MutationResult{ProposalId: r.ProposalId, Status: state}
		if r.Decision == "approve" {
			if _, _, err = authorize(ctx, tx, tenant, r.CalendarId, stored.ProposedBy, "write"); err != nil {
				return err
			}
			reply.Result, err = s.command(ctx, tx, tenant, r.CalendarId, stored.ProposedBy, stored.Command, false)
			if err != nil {
				return err
			}
			state = "approved"
			reply.Result.ProposalId = r.ProposalId
			reply.Result.Status = state
		}
		if _, err = tx.Exec(ctx, "UPDATE calendar_proposals SET status=$3,reviewed_by=$4,reviewed_at=now() WHERE tenant_id=$1 AND id=$2", tenant, r.ProposalId, state, who); err != nil {
			return err
		}
		if err = event(ctx, tx, tenant, who, r.ProposalId, "calendar.proposal."+state+".v1", 1, state); err != nil {
			return err
		}
		return save(ctx, tx, tenant, who, r.IdempotencyKey, hash, reply)
	})
	return reply, err
}

func previewIDs(command *pb.CalendarCommand, result *pb.MutationResult) []string {
	if command.GetCreate() != nil {
		return []string{}
	}
	return result.EntryIds
}
