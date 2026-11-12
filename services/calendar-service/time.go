package calendar

import (
	"strings"
	"time"
	_ "time/tzdata"

	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func bad(code string) error { return status.Error(codes.InvalidArgument, code) }
func zone(name string) (*time.Location, error) {
	if name != "UTC" && !strings.Contains(name, "/") {
		return nil, bad("IANA_TIME_ZONE_REQUIRED")
	}
	location, err := time.LoadLocation(name)
	if err != nil {
		return nil, bad("INVALID_TIME_ZONE")
	}
	return location, nil
}
func date(value *pb.CivilDate) (time.Time, error) {
	if value == nil || value.Year < 2000 || value.Year > 2200 {
		return time.Time{}, bad("INVALID_DATE")
	}
	parsed := time.Date(int(value.Year), time.Month(value.Month), int(value.Day), 0, 0, 0, 0, time.UTC)
	if parsed.Year() != int(value.Year) || parsed.Month() != time.Month(value.Month) || parsed.Day() != int(value.Day) {
		return time.Time{}, bad("INVALID_DATE")
	}
	return parsed, nil
}
func civil(t time.Time) *pb.CivilDate {
	return &pb.CivilDate{Year: int32(t.Year()), Month: int32(t.Month()), Day: int32(t.Day())}
}

// Recurrences preserve wall-clock time. Reject gaps/folds rather than guessing a
// court/appointment time. Timed single events have explicit UTC instants.
func wall(y int, m time.Month, d, h, min, sec, nano int, loc *time.Location) (time.Time, error) {
	t := time.Date(y, m, d, h, min, sec, nano, loc)
	same := func(v time.Time) bool {
		v = v.In(loc)
		return v.Year() == y && v.Month() == m && v.Day() == d && v.Hour() == h && v.Minute() == min && v.Second() == sec && v.Nanosecond() == nano
	}
	if !same(t) {
		return time.Time{}, bad("NONEXISTENT_LOCAL_TIME")
	}
	for offset := -180; offset <= 180; offset++ {
		if offset != 0 && same(t.Add(time.Duration(offset)*time.Minute)) {
			return time.Time{}, bad("AMBIGUOUS_RECURRENCE_TIME")
		}
	}
	return t, nil
}

type interval struct {
	Start, End         time.Time
	Zone               string
	AllDay             bool
	StartDate, EndDate *time.Time
}

func schedule(draft *pb.EntryDraft, days int) (interval, error) {
	if timed := draft.GetTimed(); timed != nil {
		loc, err := zone(timed.TimeZone)
		if err != nil {
			return interval{}, err
		}
		if timed.Start == nil || timed.End == nil || timed.Start.CheckValid() != nil || timed.End.CheckValid() != nil {
			return interval{}, bad("INVALID_INTERVAL")
		}
		a, b := timed.Start.AsTime(), timed.End.AsTime()
		if a.Nanosecond()%1000000 != 0 || b.Nanosecond()%1000000 != 0 {
			return interval{}, bad("MILLISECOND_PRECISION_REQUIRED")
		}
		if days != 0 {
			shift := func(t time.Time) (time.Time, error) {
				local := t.In(loc)
				day := time.Date(local.Year(), local.Month(), local.Day()+days, 0, 0, 0, 0, time.UTC)
				return wall(day.Year(), day.Month(), day.Day(), local.Hour(), local.Minute(), local.Second(), local.Nanosecond(), loc)
			}
			a, err = shift(a)
			if err != nil {
				return interval{}, err
			}
			b, err = shift(b)
			if err != nil {
				return interval{}, err
			}
		}
		if a.Year() < 2000 || b.Year() > 2200 || !b.After(a) || b.Sub(a) > 366*24*time.Hour {
			return interval{}, bad("INVALID_INTERVAL")
		}
		return interval{Start: a.UTC(), End: b.UTC(), Zone: timed.TimeZone}, nil
	}
	if all := draft.GetAllDay(); all != nil {
		loc, err := zone(all.TimeZone)
		if err != nil {
			return interval{}, err
		}
		a, err := date(all.StartDate)
		if err != nil {
			return interval{}, err
		}
		b, err := date(all.EndDateExclusive)
		if err != nil {
			return interval{}, err
		}
		a = a.AddDate(0, 0, days)
		b = b.AddDate(0, 0, days)
		if a.Year() < 2000 || b.Year() > 2200 || !b.After(a) || b.Sub(a) > 366*24*time.Hour {
			return interval{}, bad("INVALID_ALL_DAY_INTERVAL")
		}
		start, err := wall(a.Year(), a.Month(), a.Day(), 0, 0, 0, 0, loc)
		if err != nil {
			return interval{}, err
		}
		end, err := wall(b.Year(), b.Month(), b.Day(), 0, 0, 0, 0, loc)
		if err != nil {
			return interval{}, err
		}
		return interval{Start: start.UTC(), End: end.UTC(), Zone: all.TimeZone, AllDay: true, StartDate: &a, EndDate: &b}, nil
	}
	return interval{}, bad("SCHEDULE_REQUIRED")
}
func recurrence(rule *pb.RecurrenceRule) (count, step int, err error) {
	if rule == nil {
		return 1, 0, nil
	}
	if rule.Count < 1 || rule.Count > 366 || rule.Interval < 1 || rule.Interval > 52 {
		return 0, 0, bad("INVALID_RECURRENCE")
	}
	switch rule.Frequency {
	case pb.Frequency_FREQUENCY_DAILY:
		step = int(rule.Interval)
	case pb.Frequency_FREQUENCY_WEEKLY:
		step = 7 * int(rule.Interval)
	default:
		return 0, 0, status.Error(codes.Unimplemented, "UNSUPPORTED_RECURRENCE")
	}
	return int(rule.Count), step, nil
}
