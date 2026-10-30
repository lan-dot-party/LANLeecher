package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/lan-dot-party/lanleecher/internal/store"
)

func TestEventScheduleLifecycle(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	start := store.Now().Add(48 * time.Hour)
	end := start.Add(4 * 24 * time.Hour)

	s := &store.EventSchedule{Name: "Basement LAN #13", Notes: "planned", StartsAt: start, EndsAt: end}
	if err := db.CreateEventSchedule(ctx, s); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []*store.EventSchedule{
		{Name: "", StartsAt: start, EndsAt: end},
		{Name: "x", StartsAt: end, EndsAt: start},
		{Name: "x", StartsAt: start.Add(-time.Hour * 24 * 10), EndsAt: start.Add(-time.Hour * 24 * 9)},
	} {
		if err := db.CreateEventSchedule(ctx, bad); !errors.Is(err, store.ErrScheduleInvalid) {
			t.Errorf("invalid schedule %+v: err %v", bad, err)
		}
	}
	if err := db.CreateEventSchedule(ctx, &store.EventSchedule{Name: "overlap", StartsAt: end.Add(-time.Hour),
		EndsAt: end.Add(time.Hour)}); !errors.Is(err, store.ErrScheduleOverlap) {
		t.Errorf("overlap: err %v", err)
	}
	// Back to back is fine.
	next := &store.EventSchedule{Name: "Next LAN", StartsAt: end, EndsAt: end.Add(24 * time.Hour)}
	if err := db.CreateEventSchedule(ctx, next); err != nil {
		t.Fatalf("back-to-back schedule: %v", err)
	}

	apply := func(at time.Time) []store.ScheduleAction {
		t.Helper()
		acts, err := db.ApplyEventSchedules(ctx, at)
		if err != nil {
			t.Fatal(err)
		}
		return acts
	}

	if acts := apply(start.Add(-time.Second)); len(acts) != 0 {
		t.Fatalf("before start: %d actions", len(acts))
	}
	// A manual event that runs when the planned one is due gets replaced.
	manual, err := db.StartEvent(ctx, "Setup day", "")
	if err != nil {
		t.Fatal(err)
	}
	acts := apply(start)
	if len(acts) != 1 || !acts[0].Start || acts[0].Event.Name != s.Name || acts[0].Event.Notes != "planned" {
		t.Fatalf("at start: %+v", acts)
	}
	active, err := db.ActiveEvent(ctx)
	if err != nil || active.ID != acts[0].Event.ID {
		t.Fatalf("active event %+v, %v", active, err)
	}
	if ev, _ := db.EventByID(ctx, manual.ID); ev.Active() {
		t.Error("manual event still active")
	}
	if acts := apply(start.Add(time.Minute)); len(acts) != 0 {
		t.Fatalf("second apply: %+v", acts)
	}
	got, err := db.EventScheduleByID(ctx, s.ID)
	if err != nil || got.State(start.Add(time.Minute)) != store.ScheduleRunning {
		t.Fatalf("state %v, %v", got, err)
	}

	// At the end the event stops and the back-to-back one starts.
	acts = apply(end)
	if len(acts) != 2 || acts[0].Start || acts[0].Schedule.ID != s.ID || !acts[1].Start || acts[1].Schedule.ID != next.ID {
		t.Fatalf("at end: %+v", acts)
	}
	got, _ = db.EventScheduleByID(ctx, s.ID)
	if got.State(end) != store.ScheduleDone {
		t.Errorf("state after end: %s", got.State(end))
	}
	list, err := db.ListEventSchedules(ctx, false)
	if err != nil || len(list) != 1 || list[0].ID != next.ID {
		t.Errorf("open schedules: %+v, %v", list, err)
	}

	// Stopped by hand before its end: the planner leaves it alone.
	if _, err := db.StopEvent(ctx); err != nil {
		t.Fatal(err)
	}
	if acts := apply(next.EndsAt); len(acts) != 0 {
		t.Errorf("after manual stop: %+v", acts)
	}
}

func TestEventScheduleStartNowAndMissed(t *testing.T) {
	ctx := context.Background()
	db := openDB(t)
	start := store.Now().Add(time.Hour)
	s := &store.EventSchedule{Name: "Early bird", StartsAt: start, EndsAt: start.Add(2 * time.Hour)}
	if err := db.CreateEventSchedule(ctx, s); err != nil {
		t.Fatal(err)
	}
	got, err := db.StartEventScheduleNow(ctx, s.ID)
	if err != nil || got.Event == nil || !got.Event.Active() {
		t.Fatalf("start now: %+v, %v", got, err)
	}
	if _, err := db.StartEventScheduleNow(ctx, s.ID); !errors.Is(err, store.ErrScheduleState) {
		t.Errorf("second start now: %v", err)
	}
	if acts, _ := db.ApplyEventSchedules(ctx, start); len(acts) != 0 {
		t.Errorf("planned start after early start: %+v", acts)
	}
	if acts, _ := db.ApplyEventSchedules(ctx, s.EndsAt); len(acts) != 1 || acts[0].Start {
		t.Errorf("planned stop after early start: %+v", acts)
	}

	// The server was down for the whole window: nothing starts afterwards.
	m := &store.EventSchedule{Name: "Missed", StartsAt: s.EndsAt.Add(time.Hour), EndsAt: s.EndsAt.Add(2 * time.Hour)}
	if err := db.CreateEventSchedule(ctx, m); err != nil {
		t.Fatal(err)
	}
	if acts, _ := db.ApplyEventSchedules(ctx, m.EndsAt.Add(time.Minute)); len(acts) != 0 {
		t.Errorf("missed window started: %+v", acts)
	}
	got, _ = db.EventScheduleByID(ctx, m.ID)
	if st := got.State(m.EndsAt); st != store.ScheduleMissed {
		t.Errorf("state %s, want missed", st)
	}
	if next, _ := db.NextScheduleChange(ctx, start); !next.Equal(s.EndsAt) {
		t.Errorf("next change %v, want %v", next, s.EndsAt)
	}
	if err := db.DeleteEventSchedule(ctx, m.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.DeleteEventSchedule(ctx, m.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete twice: %v", err)
	}
}
