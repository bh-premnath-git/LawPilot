package calendar

import (
	pb "github.com/bh-premnath-git/LawPilot/contracts/gen/go/lawpilot/calendar/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
	"testing"
	"time"
)

func TestAllDayExclusiveEndAcrossDST(t *testing.T) {
	for _, test := range []struct {
		month, day int
		hours      float64
	}{{3, 8, 23}, {11, 1, 25}} {
		draft := &pb.EntryDraft{Schedule: &pb.EntryDraft_AllDay{AllDay: &pb.AllDayInterval{StartDate: &pb.CivilDate{Year: 2026, Month: int32(test.month), Day: int32(test.day)}, EndDateExclusive: &pb.CivilDate{Year: 2026, Month: int32(test.month), Day: int32(test.day + 1)}, TimeZone: "America/New_York"}}}
		value, err := schedule(draft, 0)
		if err != nil || value.End.Sub(value.Start).Hours() != test.hours {
			t.Fatalf("hours=%v err=%v", value.End.Sub(value.Start).Hours(), err)
		}
	}
}
func TestRecurrencePreservesWallClockAndRejectsGapFold(t *testing.T) {
	start, _ := time.Parse(time.RFC3339, "2026-03-07T09:00:00-05:00")
	draft := &pb.EntryDraft{Schedule: &pb.EntryDraft_Timed{Timed: &pb.TimedInterval{Start: timestamppb.New(start), End: timestamppb.New(start.Add(time.Hour)), TimeZone: "America/New_York"}}}
	next, err := schedule(draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	loc, _ := time.LoadLocation("America/New_York")
	if next.Start.In(loc).Hour() != 9 || next.Start.Sub(start) != 23*time.Hour {
		t.Fatal("wall clock shifted")
	}
	for _, value := range []string{"2026-03-07T02:30:00-05:00", "2026-10-31T01:30:00-04:00"} {
		at, _ := time.Parse(time.RFC3339, value)
		draft.Schedule = &pb.EntryDraft_Timed{Timed: &pb.TimedInterval{Start: timestamppb.New(at), End: timestamppb.New(at.Add(15 * time.Minute)), TimeZone: "America/New_York"}}
		if _, err := schedule(draft, 1); err == nil {
			t.Fatal("ambiguous or nonexistent recurrence accepted")
		}
	}
}
func TestInvalidDatesZonesAndUnboundedRecurrence(t *testing.T) {
	if _, err := date(&pb.CivilDate{Year: 2026, Month: 2, Day: 30}); err == nil {
		t.Fatal("invalid date accepted")
	}
	if _, err := zone("Local"); err == nil {
		t.Fatal("implicit zone accepted")
	}
	if _, _, err := recurrence(&pb.RecurrenceRule{Frequency: pb.Frequency_FREQUENCY_DAILY, Interval: 1}); err == nil {
		t.Fatal("unbounded recurrence accepted")
	}
}
