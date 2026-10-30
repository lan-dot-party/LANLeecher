package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Errors returned when creating or starting a schedule.
var (
	ErrScheduleInvalid = errors.New("schedule: name missing, end not after start, or end in the past")
	ErrScheduleOverlap = errors.New("schedule overlaps another planned event")
	ErrScheduleState   = errors.New("schedule was already started or its time is over")
)

// Schedule states, derived from times and the linked event.
const (
	SchedulePlanned = "planned" // not started yet
	ScheduleRunning = "running" // linked event is active
	ScheduleDone    = "done"    // linked event has ended
	ScheduleMissed  = "missed"  // window passed without a start (server was down)
)

// EventSchedule is a planned event. LANLeecher starts it at StartsAt and stops
// it at EndsAt (exclusive), unless someone did that by hand before.
type EventSchedule struct {
	ID        int64
	Name      string
	Notes     string
	StartsAt  time.Time
	EndsAt    time.Time
	CreatedAt time.Time
	// Event is the started event, nil until the schedule started.
	Event *Event
}

// State returns the schedule state at now.
func (s *EventSchedule) State(now time.Time) string {
	switch {
	case s.Event != nil && s.Event.Active():
		return ScheduleRunning
	case s.Event != nil:
		return ScheduleDone
	case !now.Before(s.EndsAt):
		return ScheduleMissed
	default:
		return SchedulePlanned
	}
}

// ScheduleAction is something ApplyEventSchedules did.
type ScheduleAction struct {
	Start    bool // true: started, false: stopped
	Schedule *EventSchedule
	Event    *Event
}

const scheduleCols = `s.id, s.name, s.notes, s.starts_at, s.ends_at, s.created_at,
	e.id, e.name, e.slug, e.started_at, e.ended_at, e.notes`

func scanSchedule(row interface{ Scan(...any) error }) (*EventSchedule, error) {
	var s EventSchedule
	var starts, ends, created int64
	var eid, estarted, eended sql.NullInt64
	var ename, eslug, enotes sql.NullString
	err := row.Scan(&s.ID, &s.Name, &s.Notes, &starts, &ends, &created,
		&eid, &ename, &eslug, &estarted, &eended, &enotes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.StartsAt, s.EndsAt, s.CreatedAt = fromUnix(starts), fromUnix(ends), fromUnix(created)
	if eid.Valid {
		s.Event = &Event{ID: eid.Int64, Name: ename.String, Slug: eslug.String, StartedAt: fromUnix(estarted.Int64),
			EndedAt: fromNullUnix(eended), Notes: enotes.String}
	}
	return &s, nil
}

func listSchedules(ctx context.Context, q querier, where string, args ...any) ([]*EventSchedule, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+scheduleCols+` FROM event_schedules s
		LEFT JOIN events e ON e.id = s.event_id WHERE `+where+` ORDER BY s.starts_at, s.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*EventSchedule
	for rows.Next() {
		s, err := scanSchedule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListEventSchedules returns schedules by start time. Without all, schedules
// whose event has already ended are left out (the event list shows them).
func (db *DB) ListEventSchedules(ctx context.Context, all bool) ([]*EventSchedule, error) {
	if all {
		return listSchedules(ctx, db.sql, `1 = 1`)
	}
	return listSchedules(ctx, db.sql, `s.event_id IS NULL OR e.ended_at IS NULL`)
}

// EventScheduleByID returns one schedule.
func (db *DB) EventScheduleByID(ctx context.Context, id int64) (*EventSchedule, error) {
	return scanSchedule(db.sql.QueryRowContext(ctx, `SELECT `+scheduleCols+` FROM event_schedules s
		LEFT JOIN events e ON e.id = s.event_id WHERE s.id = ?`, id))
}

// CreateEventSchedule plans an event. The window [StartsAt, EndsAt) must end
// in the future and must not overlap another schedule that is not over yet.
func (db *DB) CreateEventSchedule(ctx context.Context, s *EventSchedule) error {
	s.Name = strings.TrimSpace(s.Name)
	now := Now()
	if s.Name == "" || !s.EndsAt.After(s.StartsAt) || !s.EndsAt.After(now) {
		return ErrScheduleInvalid
	}
	return db.InTx(ctx, func(tx *Tx) error {
		var n int
		err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM event_schedules s LEFT JOIN events e ON e.id = s.event_id
			WHERE s.starts_at < ? AND s.ends_at > ? AND s.ends_at > ? AND (s.event_id IS NULL OR e.ended_at IS NULL)`,
			unix(s.EndsAt), unix(s.StartsAt), unix(now)).Scan(&n)
		if err != nil {
			return err
		}
		if n > 0 {
			return ErrScheduleOverlap
		}
		s.CreatedAt = now
		res, err := tx.tx.ExecContext(ctx, `INSERT INTO event_schedules (name, notes, starts_at, ends_at, created_at)
			VALUES (?, ?, ?, ?, ?)`, s.Name, s.Notes, unix(s.StartsAt), unix(s.EndsAt), unix(now))
		if err != nil {
			return err
		}
		s.ID, err = res.LastInsertId()
		return err
	})
}

// DeleteEventSchedule removes a schedule. An event it already started keeps
// running but is no longer stopped automatically.
func (db *DB) DeleteEventSchedule(ctx context.Context, id int64) error {
	res, err := db.sql.ExecContext(ctx, `DELETE FROM event_schedules WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// StartEventScheduleNow starts a planned event early. It is still stopped
// automatically at its end time.
func (db *DB) StartEventScheduleNow(ctx context.Context, id int64) (*EventSchedule, error) {
	var s *EventSchedule
	err := db.InTx(ctx, func(tx *Tx) error {
		var err error
		s, err = scanSchedule(tx.tx.QueryRowContext(ctx, `SELECT `+scheduleCols+` FROM event_schedules s
			LEFT JOIN events e ON e.id = s.event_id WHERE s.id = ?`, id))
		if err != nil {
			return err
		}
		now := Now()
		if s.State(now) != SchedulePlanned {
			return ErrScheduleState
		}
		s.Event, err = tx.startSchedule(ctx, s, now)
		return err
	})
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (tx *Tx) startSchedule(ctx context.Context, s *EventSchedule, now time.Time) (*Event, error) {
	ev, err := tx.startEvent(ctx, s.Name, s.Notes, now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.tx.ExecContext(ctx, `UPDATE event_schedules SET event_id = ? WHERE id = ?`, ev.ID, s.ID); err != nil {
		return nil, err
	}
	return ev, nil
}

// ApplyEventSchedules stops events whose schedule is over and starts the
// schedule whose window contains now. It is idempotent; the poll loop calls
// it every few seconds. An event stopped or replaced by hand is left alone.
func (db *DB) ApplyEventSchedules(ctx context.Context, now time.Time) ([]ScheduleAction, error) {
	now = now.UTC().Truncate(time.Second)
	var actions []ScheduleAction
	err := db.InTx(ctx, func(tx *Tx) error {
		over, err := listSchedules(ctx, tx.tx, `e.id IS NOT NULL AND e.ended_at IS NULL AND s.ends_at <= ?`, unix(now))
		if err != nil {
			return err
		}
		for _, s := range over {
			if _, err := tx.tx.ExecContext(ctx, `UPDATE events SET ended_at = ? WHERE id = ?`, unix(now), s.Event.ID); err != nil {
				return err
			}
			t := now
			s.Event.EndedAt = &t
			actions = append(actions, ScheduleAction{Start: false, Schedule: s, Event: s.Event})
		}
		due, err := listSchedules(ctx, tx.tx, `s.event_id IS NULL AND s.starts_at <= ? AND s.ends_at > ?`, unix(now), unix(now))
		if err != nil {
			return err
		}
		if len(due) == 0 {
			return nil
		}
		// Schedules do not overlap; should they anyway, the latest start wins.
		s := due[len(due)-1]
		if s.Event, err = tx.startSchedule(ctx, s, now); err != nil {
			return err
		}
		actions = append(actions, ScheduleAction{Start: true, Schedule: s, Event: s.Event})
		return nil
	})
	if err != nil {
		return nil, err
	}
	return actions, nil
}

// NextScheduleChange returns the next start or end time after now of a
// schedule that is not over, or the zero time.
func (db *DB) NextScheduleChange(ctx context.Context, now time.Time) (time.Time, error) {
	var next sql.NullInt64
	err := db.sql.QueryRowContext(ctx, `SELECT MIN(t) FROM (
			SELECT starts_at AS t FROM event_schedules WHERE event_id IS NULL AND starts_at > ?
			UNION ALL SELECT ends_at FROM event_schedules WHERE ends_at > ?)`, unix(now), unix(now)).Scan(&next)
	if err != nil || !next.Valid {
		return time.Time{}, err
	}
	return fromUnix(next.Int64), nil
}

// ParseScheduleWindow turns dates (2006-01-02) and optional times (15:04) in
// loc into [start, end) in UTC; the end date is inclusive. Without a time the event
// starts at 00:00 on the first day and ends at 24:00 on the last day.
func ParseScheduleWindow(startDate, startTime, endDate, endTime string, loc *time.Location) (start, end time.Time, ok bool) {
	day := func(s string) (time.Time, bool) {
		t, err := time.ParseInLocation("2006-01-02", strings.TrimSpace(s), loc)
		return t, err == nil
	}
	clock := func(d time.Time, s string) (time.Time, bool) {
		c, err := time.Parse("15:04", strings.TrimSpace(s))
		if err != nil {
			return time.Time{}, false
		}
		return time.Date(d.Year(), d.Month(), d.Day(), c.Hour(), c.Minute(), 0, 0, loc), true
	}
	sd, ok1 := day(startDate)
	ed, ok2 := day(endDate)
	if !ok1 || !ok2 {
		return time.Time{}, time.Time{}, false
	}
	start, end = sd, ed.AddDate(0, 0, 1)
	if strings.TrimSpace(startTime) != "" {
		if start, ok = clock(sd, startTime); !ok {
			return time.Time{}, time.Time{}, false
		}
	}
	if strings.TrimSpace(endTime) != "" {
		if end, ok = clock(ed, endTime); !ok {
			return time.Time{}, time.Time{}, false
		}
	}
	return start.UTC(), end.UTC(), end.After(start)
}
