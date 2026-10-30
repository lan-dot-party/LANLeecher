package web

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/lanleecher/internal/store"
)

func parseScheduleWindow(startDate, startTime, endDate, endTime string) (start, end time.Time, ok bool) {
	return store.ParseScheduleWindow(startDate, startTime, endDate, endTime, time.Local)
}

// formatScheduleWindow shows whole days as a date range ("2027-04-15 – 2027-04-18")
// and anything else with times.
func formatScheduleWindow(start, end time.Time) string {
	ls, le := start.Local(), end.Local()
	midnight := func(t time.Time) bool { return t.Hour() == 0 && t.Minute() == 0 && t.Second() == 0 }
	if midnight(ls) && midnight(le) {
		last := le.AddDate(0, 0, -1)
		if last.Format(time.DateOnly) == ls.Format(time.DateOnly) {
			return ls.Format(time.DateOnly)
		}
		return ls.Format(time.DateOnly) + " – " + last.Format(time.DateOnly)
	}
	return formatTime(start) + " – " + formatTime(end)
}

func (s *Server) scheduleCreate(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	notes := strings.TrimSpace(r.PostFormValue("notes"))
	if name == "" || len(name) > 100 || len(notes) > 500 {
		redirect(w, r, "/events?err=bad_request")
		return
	}
	start, end, ok := parseScheduleWindow(r.PostFormValue("start_date"), r.PostFormValue("start_time"),
		r.PostFormValue("end_date"), r.PostFormValue("end_time"))
	if !ok {
		redirect(w, r, "/events?err=schedule_invalid")
		return
	}
	sc := &store.EventSchedule{Name: name, Notes: notes, StartsAt: start, EndsAt: end}
	err := s.DB.CreateEventSchedule(r.Context(), sc)
	switch {
	case errors.Is(err, store.ErrScheduleInvalid):
		redirect(w, r, "/events?err=schedule_invalid")
		return
	case errors.Is(err, store.ErrScheduleOverlap):
		redirect(w, r, "/events?err=schedule_overlap")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "schedule.create", strconv.FormatInt(sc.ID, 10), sc.Name+" "+formatScheduleWindow(sc.StartsAt, sc.EndsAt))
	if s.Planner != nil {
		s.Planner.Kick()
	}
	redirect(w, r, "/events?ok=scheduled")
}

func (s *Server) scheduleStartNow(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	sc, err := s.DB.StartEventScheduleNow(r.Context(), id)
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, store.ErrScheduleState):
		redirect(w, r, "/events?err=schedule_state")
		return
	case err != nil:
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "event.start", sc.Event.Slug, sc.Event.Name)
	if s.Board != nil {
		s.Board.Notify()
	}
	redirect(w, r, "/?ok=event_started")
}

func (s *Server) scheduleDelete(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	sc, err := s.DB.EventScheduleByID(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		redirect(w, r, "/events")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.DB.DeleteEventSchedule(r.Context(), id); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "schedule.delete", strconv.FormatInt(sc.ID, 10), sc.Name)
	redirect(w, r, "/events?ok=deleted")
}
