package reminders

import (
	"context"
	"errors"
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"time"
)

type Ref struct {
	TenantID   string `json:"tenantId"`
	ReminderID string `json:"reminderId"`
}
type Schedule struct {
	Status string
	DueAt  time.Time
}
type Activities struct{ Client pb.CalendarServiceClient }

func (a *Activities) Load(ctx context.Context, ref Ref) (Schedule, error) {
	result, err := a.Client.LoadReminderWork(ctx, &pb.ReminderWorkRequest{TenantId: ref.TenantID, ReminderId: ref.ReminderID})
	if err != nil {
		return Schedule{}, errors.New("CALENDAR_RPC_UNAVAILABLE")
	}
	value := Schedule{Status: result.Status}
	if result.DueAt != nil {
		if result.DueAt.CheckValid() != nil {
			return value, errors.New("INVALID_REMINDER_SCHEDULE")
		}
		value.DueAt = result.DueAt.AsTime()
	}
	return value, nil
}
func (a *Activities) Deliver(ctx context.Context, ref Ref) (string, error) {
	result, err := a.Client.DeliverReminder(ctx, &pb.ReminderWorkRequest{TenantId: ref.TenantID, ReminderId: ref.ReminderID})
	if err != nil {
		return "", errors.New("CALENDAR_RPC_UNAVAILABLE")
	}
	return result.Status, nil
}
