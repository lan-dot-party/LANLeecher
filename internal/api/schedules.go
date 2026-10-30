package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/lanleecher/internal/store"
)

// ScheduleJSON is a planned event in responses.
type ScheduleJSON struct {
	ID       int64      `json:"id"`
	Name     string     `json:"name"`
	Notes    string     `json:"notes"`
	StartsAt time.Time  `json:"starts_at"`
	EndsAt   time.Time  `json:"ends_at"`
	State    string     `json:"state"`
	Event    *EventJSON `json:"event"`
}

func scheduleJSON(s *store.EventSchedule, now time.Time) *ScheduleJSON {
	return &ScheduleJSON{ID: s.ID, Name: s.Name, Notes: s.Notes, StartsAt: s.StartsAt, EndsAt: s.EndsAt,
		State: s.State(now), Event: eventJSON(s.Event)}
}

func (a *API) listSchedules(w http.ResponseWriter, r *http.Request) {
	list, err := a.DB.ListEventSchedules(r.Context(), r.URL.Query().Get("all") == "true")
	if err != nil {
		a.internal(w, r, err)
		return
	}
	now := store.Now()
	out := make([]*ScheduleJSON, 0, len(list))
	for _, s := range list {
		out = append(out, scheduleJSON(s, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": out})
}

func (a *API) createSchedule(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name     string    `json:"name"`
		Notes    string    `json:"notes"`
		StartsAt time.Time `json:"starts_at"`
		EndsAt   time.Time `json:"ends_at"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&body); err != nil ||
		strings.TrimSpace(body.Name) == "" || body.StartsAt.IsZero() || body.EndsAt.IsZero() {
		problem(w, http.StatusBadRequest, "invalid_body", "Bad Request",
			`expected {"name": "...", "starts_at": "<RFC 3339>", "ends_at": "<RFC 3339>", "notes": "..."}`)
		return
	}
	s := &store.EventSchedule{Name: body.Name, Notes: body.Notes, StartsAt: body.StartsAt.UTC().Truncate(time.Second),
		EndsAt: body.EndsAt.UTC().Truncate(time.Second)}
	err := a.DB.CreateEventSchedule(r.Context(), s)
	switch {
	case errors.Is(err, store.ErrScheduleInvalid):
		problem(w, http.StatusBadRequest, "invalid_schedule", "Bad Request", "ends_at must be after starts_at and in the future")
		return
	case errors.Is(err, store.ErrScheduleOverlap):
		problem(w, http.StatusConflict, "schedule_overlap", "Conflict", "the period overlaps another planned event")
		return
	case err != nil:
		a.internal(w, r, err)
		return
	}
	a.audit(r, "schedule.create", strconv.FormatInt(s.ID, 10))
	if a.Planner != nil {
		a.Planner.Kick()
	}
	writeJSON(w, http.StatusCreated, map[string]any{"schedule": scheduleJSON(s, store.Now())})
}

func (a *API) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err == nil {
		err = a.DB.DeleteEventSchedule(r.Context(), id)
	}
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, strconv.ErrSyntax) || errors.Is(err, strconv.ErrRange) {
		problem(w, http.StatusNotFound, "schedule_not_found", "Not Found", "unknown planned event")
		return
	}
	if err != nil {
		a.internal(w, r, err)
		return
	}
	a.audit(r, "schedule.delete", chi.URLParam(r, "id"))
	w.WriteHeader(http.StatusNoContent)
}
