package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/lan-dot-party/lanleecher/internal/store"
)

// EventPlanner starts and stops planned events (event_schedules) on time.
type EventPlanner struct {
	db   *store.DB
	log  *slog.Logger
	kick chan struct{}

	// OnChange is called after an event was started or stopped (board refresh).
	OnChange func()
	// MaxWait bounds the sleep between checks, so a changed system clock or a
	// schedule added by another process (CLI) is picked up.
	MaxWait time.Duration
}

// NewEventPlanner creates a planner. Call Run to start it.
func NewEventPlanner(db *store.DB, log *slog.Logger) *EventPlanner {
	return &EventPlanner{db: db, log: log.With("component", "event-planner"), kick: make(chan struct{}, 1),
		MaxWait: 30 * time.Second}
}

// Kick makes the planner check right away (after a schedule was added).
func (p *EventPlanner) Kick() {
	select {
	case p.kick <- struct{}{}:
	default:
	}
}

// Run checks the schedules until ctx ends. It wakes up at the next start or
// end time, so events start and stop to the second.
func (p *EventPlanner) Run(ctx context.Context) {
	for {
		p.Apply(ctx, store.Now())
		wait := p.MaxWait
		if next, err := p.db.NextScheduleChange(ctx, store.Now()); err != nil {
			p.log.Error("next schedule change", "err", err)
		} else if !next.IsZero() {
			if d := time.Until(next); d < wait {
				wait = max(d, 0)
			}
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-p.kick:
			t.Stop()
		case <-t.C:
		}
	}
}

// Apply starts or stops events that are due at now and writes the audit log.
func (p *EventPlanner) Apply(ctx context.Context, now time.Time) {
	actions, err := p.db.ApplyEventSchedules(ctx, now)
	if err != nil {
		p.log.Error("apply event schedules", "err", err)
		return
	}
	for _, a := range actions {
		action := "event.stop"
		if a.Start {
			action = "event.start"
		}
		p.log.Info("planned event", "action", action, "event", a.Event.Slug, "schedule", a.Schedule.ID)
		if err := p.db.Audit(ctx, store.AuditEntry{Actor: "schedule", Action: action, Target: a.Event.Slug,
			Details: a.Event.Name}); err != nil {
			p.log.Error("audit log", "err", err, "action", action)
		}
	}
	if len(actions) > 0 && p.OnChange != nil {
		p.OnChange()
	}
}
