package calendar

import (
	"context"
	"errors"
	"strings"
	"time"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"github.com/jackc/pgx/v5"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func (s *Service) ListReminders(ctx context.Context, r *pb.ListRemindersRequest) (*pb.ListRemindersResponse, error) {
	tenant, err := scope(r.Scope)
	if err != nil {
		return nil, err
	}
	reply := new(pb.ListRemindersResponse)
	who := actor(ctx)
	err = s.tx(ctx, tenant, r.CalendarId, false, func(tx pgx.Tx) error {
		_, client, err := authorize(ctx, tx, tenant, r.CalendarId, who, "read")
		if err != nil {
			return err
		}
		entry, _, err := loadEntry(ctx, tx, tenant, r.CalendarId, r.EntryId)
		if err != nil {
			return err
		}
		if !visible(ctx, tx, tenant, who, entry, client) {
			return status.Error(codes.NotFound, "NOT_FOUND")
		}
		rows, err := tx.Query(ctx, "SELECT id::text,entry_id::text,entry_revision,due_at,status FROM calendar_reminders WHERE tenant_id=$1 AND calendar_id=$2 AND entry_id=$3 AND requestor_id=$4 ORDER BY updated_at DESC,id LIMIT 100", tenant, r.CalendarId, r.EntryId, who)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			value := new(pb.Reminder)
			var due time.Time
			if err = rows.Scan(&value.Id, &value.EntryId, &value.EntryRevision, &due, &value.Status); err != nil {
				return err
			}
			value.DueAt = timestamppb.New(due)
			reply.Reminders = append(reply.Reminders, value)
		}
		return rows.Err()
	})
	return reply, err
}
func (s *Service) reminderWork(ctx context.Context, r *pb.ReminderWorkRequest, deliver bool) (*pb.ReminderWorkResponse, error) {
	if !identifier(r.ReminderId) {
		return nil, bad("INVALID_REMINDER_ID")
	}
	r.TenantId = strings.ToLower(r.TenantId)
	reply := new(pb.ReminderWorkResponse)
	err := s.tx(ctx, r.TenantId, "", false, func(tx pgx.Tx) error {
		var calendar, entry, who, state string
		var revision int64
		var due time.Time
		err := tx.QueryRow(ctx, "SELECT calendar_id::text FROM calendar_reminders WHERE tenant_id=$1 AND id=$2", r.TenantId, r.ReminderId).Scan(&calendar)
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.NotFound, "NOT_FOUND")
		}
		if err != nil {
			return err
		}
		if _, err = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(hashtextextended($1,0))", r.TenantId+":"+calendar); err != nil {
			return err
		}
		if err = tx.QueryRow(ctx, "SELECT entry_id::text,requestor_id,entry_revision,due_at,status FROM calendar_reminders WHERE tenant_id=$1 AND id=$2 FOR UPDATE", r.TenantId, r.ReminderId).Scan(&entry, &who, &revision, &due, &state); err != nil {
			return err
		}
		reply.Status = state
		reply.DueAt = timestamppb.New(due)
		if state != "scheduled" {
			return nil
		}
		e, when, err := loadEntry(ctx, tx, r.TenantId, calendar, entry)
		if err != nil {
			return err
		}
		_, client, permission := authorize(ctx, tx, r.TenantId, calendar, who, "read")
		reason := ""
		if permission != nil || !visible(ctx, tx, r.TenantId, who, e, client) {
			reason = "ACCESS_REVOKED"
		} else if e.Status != "active" || e.Revision != revision {
			reason = "ENTRY_CHANGED"
		} else if !when.Start.Add(time.Minute).After(s.now()) {
			reason = "EVENT_PASSED"
		}
		if reason != "" {
			if _, err = tx.Exec(ctx, "UPDATE calendar_reminders SET status='cancelled',reason=$3,updated_at=now() WHERE tenant_id=$1 AND id=$2", r.TenantId, r.ReminderId, reason); err != nil {
				return err
			}
			reply.Status = "cancelled"
			return event(ctx, tx, r.TenantId, who, r.ReminderId, "calendar.reminder.cancelled.v1", revision, reason)
		}
		if !deliver {
			return nil
		}
		if s.now().Before(due) {
			return status.Error(codes.FailedPrecondition, "REMINDER_NOT_DUE")
		}
		if _, err = tx.Exec(ctx, "UPDATE calendar_reminders SET status='due',updated_at=now() WHERE tenant_id=$1 AND id=$2", r.TenantId, r.ReminderId); err != nil {
			return err
		}
		reply.Status = "due"
		return event(ctx, tx, r.TenantId, who, r.ReminderId, "calendar.reminder.due.v1", revision, "in_app_due")
	})
	return reply, err
}
func (s *Service) LoadReminderWork(ctx context.Context, r *pb.ReminderWorkRequest) (*pb.ReminderWorkResponse, error) {
	return s.reminderWork(ctx, r, false)
}
func (s *Service) DeliverReminder(ctx context.Context, r *pb.ReminderWorkRequest) (*pb.ReminderWorkResponse, error) {
	return s.reminderWork(ctx, r, true)
}
