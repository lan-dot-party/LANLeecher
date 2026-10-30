package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Event is a LAN party. At most one event is active (EndedAt == nil).
type Event struct {
	ID        int64
	Name      string
	Slug      string
	StartedAt time.Time
	EndedAt   *time.Time
	Notes     string
}

// Active reports whether the event is running.
func (e *Event) Active() bool { return e.EndedAt == nil }

const eventCols = `id, name, slug, started_at, ended_at, notes`

func scanEvent(row interface{ Scan(...any) error }) (*Event, error) {
	var e Event
	var started int64
	var ended sql.NullInt64
	err := row.Scan(&e.ID, &e.Name, &e.Slug, &started, &ended, &e.Notes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	e.StartedAt = fromUnix(started)
	e.EndedAt = fromNullUnix(ended)
	return &e, nil
}

// ActiveEvent returns the running event or ErrNotFound.
func (db *DB) ActiveEvent(ctx context.Context) (*Event, error) {
	return scanEvent(db.sql.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE ended_at IS NULL
		ORDER BY started_at DESC LIMIT 1`))
}

// EventBySlug returns an event.
func (db *DB) EventBySlug(ctx context.Context, slug string) (*Event, error) {
	return scanEvent(db.sql.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE slug = ?`, slug))
}

// EventByID returns an event.
func (db *DB) EventByID(ctx context.Context, id int64) (*Event, error) {
	return scanEvent(db.sql.QueryRowContext(ctx, `SELECT `+eventCols+` FROM events WHERE id = ?`, id))
}

// ListEvents returns events, newest first, optionally filtered by name and
// start time range (zero times mean no bound).
func (db *DB) ListEvents(ctx context.Context, q string, from, to time.Time) ([]*Event, error) {
	query := `SELECT ` + eventCols + ` FROM events WHERE 1 = 1`
	var args []any
	if q = strings.TrimSpace(q); q != "" {
		query += ` AND (name LIKE ? ESCAPE '\' OR slug LIKE ? ESCAPE '\')`
		like := "%" + escapeLike(q) + "%"
		args = append(args, like, like)
	}
	if !from.IsZero() {
		query += ` AND started_at >= ?`
		args = append(args, unix(from))
	}
	if !to.IsZero() {
		query += ` AND started_at < ?`
		args = append(args, unix(to))
	}
	query += ` ORDER BY started_at DESC`
	rows, err := db.sql.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Event
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// StartEvent ends a running event and starts a new one.
func (db *DB) StartEvent(ctx context.Context, name, notes string) (*Event, error) {
	var e *Event
	err := db.InTx(ctx, func(tx *Tx) error {
		var err error
		e, err = tx.startEvent(ctx, name, notes, Now())
		return err
	})
	if err != nil {
		return nil, err
	}
	return e, nil
}

// startEvent ends a running event and inserts a new one starting at now.
func (tx *Tx) startEvent(ctx context.Context, name, notes string, now time.Time) (*Event, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("event name must not be empty")
	}
	e := &Event{Name: name, Notes: notes, StartedAt: now}
	if _, err := tx.tx.ExecContext(ctx, `UPDATE events SET ended_at = ? WHERE ended_at IS NULL`, unix(now)); err != nil {
		return nil, err
	}
	base := Slugify(name)
	slug := base
	for i := 2; ; i++ {
		var n int
		if err := tx.tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE slug = ?`, slug).Scan(&n); err != nil {
			return nil, err
		}
		if n == 0 {
			break
		}
		slug = base + "-" + itoa(int64(i))
	}
	e.Slug = slug
	res, err := tx.tx.ExecContext(ctx, `INSERT INTO events (name, slug, started_at, notes) VALUES (?, ?, ?, ?)`,
		name, slug, unix(now), notes)
	if err != nil {
		return nil, err
	}
	e.ID, err = res.LastInsertId()
	if err != nil {
		return nil, err
	}
	return e, nil
}

// StopEvent ends the running event. It returns ErrNotFound if none runs.
func (db *DB) StopEvent(ctx context.Context) (*Event, error) {
	e, err := db.ActiveEvent(ctx)
	if err != nil {
		return nil, err
	}
	now := Now()
	if _, err := db.sql.ExecContext(ctx, `UPDATE events SET ended_at = ? WHERE id = ?`, unix(now), e.ID); err != nil {
		return nil, err
	}
	e.EndedAt = &now
	return e, nil
}

// UpdateEventNotes changes name and notes of an event.
func (db *DB) UpdateEvent(ctx context.Context, id int64, name, notes string) error {
	_, err := db.sql.ExecContext(ctx, `UPDATE events SET name = ?, notes = ? WHERE id = ?`, name, notes, id)
	return err
}

// EventSummary holds the totals of an event.
type EventSummary struct {
	BytesDown int64
	BytesUp   int64
	// Clients counts distinct MACs that received credited traffic.
	Clients int
}

// EventSummary sums all non-uplink port traffic of an event.
func (db *DB) EventSummary(ctx context.Context, eventID int64) (EventSummary, error) {
	var s EventSummary
	err := db.sql.QueryRowContext(ctx, `SELECT COALESCE(SUM(t.bytes_down), 0), COALESCE(SUM(t.bytes_up), 0)
		FROM port_traffic_totals t JOIN ports p ON p.id = t.port_id
		WHERE t.event_id = ? AND p.role <> 'uplink'`, eventID).Scan(&s.BytesDown, &s.BytesUp)
	if err != nil {
		return s, err
	}
	err = db.sql.QueryRowContext(ctx, `SELECT COUNT(DISTINCT mac) FROM (
			SELECT subject_id AS mac FROM traffic_totals WHERE event_id = ? AND subject_type = 'mac'
			UNION SELECT m.mac FROM port_group_macs m JOIN port_groups g ON g.id = m.group_id WHERE g.event_id = ?
		)`, eventID, eventID).Scan(&s.Clients)
	return s, err
}

// Slugify turns an event name into a URL slug ("Basement LAN #12" -> "basement-lan-12"); German umlauts are transliterated.
func Slugify(name string) string {
	replacer := strings.NewReplacer("ä", "ae", "ö", "oe", "ü", "ue", "ß", "ss", "Ä", "ae", "Ö", "oe", "Ü", "ue")
	s := norm.NFKD.String(replacer.Replace(name))
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			dash = false
		case unicode.Is(unicode.Mn, r):
			// drop combining marks left by NFKD
		default:
			if !dash && b.Len() > 0 {
				b.WriteByte('-')
				dash = true
			}
		}
	}
	out := strings.TrimSuffix(b.String(), "-")
	if out == "" {
		out = "event"
	}
	if len(out) > 60 {
		out = strings.TrimSuffix(out[:60], "-")
	}
	return out
}

func escapeLike(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}
