package web

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/lanleecher/internal/app"
	"github.com/lan-dot-party/lanleecher/internal/macaddr"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

// RankingView is the dot of the "ranking_rows" template.
type RankingView struct {
	P       *Page
	Entries []store.RankEntry
	Max     int64
	Wide    bool
}

func newRankingView(p *Page, rows []store.RankEntry, wide bool) RankingView {
	v := RankingView{P: p, Entries: rows, Wide: wide}
	for _, e := range rows {
		v.Max = max(v.Max, e.BytesDown, e.BytesUp)
	}
	return v
}

type dashboardData struct {
	Event    *store.Event
	Summary  store.EventSummary
	DownBps  float64
	UpBps    float64
	TopDown  RankingView
	TopUp    RankingView
	Chart    template.HTML
	Switches []*store.Switch
	// DHCPProblems are DHCP sources that need an admin (admins only).
	DHCPProblems []*store.DHCPSource
	// NextSchedule is the next planned event while none is running.
	NextSchedule *store.EventSchedule
}

func (s *Server) dashboardData(r *http.Request, p *Page) (*dashboardData, error) {
	ctx := r.Context()
	d := &dashboardData{}
	d.DownBps, d.UpBps = s.Sched.Throughput()
	var err error
	if d.Switches, err = s.DB.ListSwitches(ctx); err != nil {
		return nil, err
	}
	if p.Can(store.RoleAdmin) {
		d.DHCPProblems = s.dhcpProblems(r)
	}
	ev, err := s.DB.ActiveEvent(ctx)
	if errors.Is(err, store.ErrNotFound) {
		schedules, err := s.DB.ListEventSchedules(ctx, false)
		if err != nil {
			return nil, err
		}
		now := store.Now()
		for _, sc := range schedules {
			if sc.State(now) == store.SchedulePlanned {
				d.NextSchedule = sc
				break
			}
		}
		return d, nil
	}
	if err != nil {
		return nil, err
	}
	d.Event = ev
	if d.Summary, err = s.DB.EventSummary(ctx, ev.ID); err != nil {
		return nil, err
	}
	down, err := s.DB.Rankings(ctx, store.RankingQuery{EventID: ev.ID, Order: "down", Limit: 10})
	if err != nil {
		return nil, err
	}
	up, err := s.DB.Rankings(ctx, store.RankingQuery{EventID: ev.ID, Order: "up", Limit: 10})
	if err != nil {
		return nil, err
	}
	d.TopDown, d.TopUp = newRankingView(p, down, false), newRankingView(p, up, false)
	buckets, err := s.DB.EventTimeseries(ctx, ev.ID)
	if err != nil {
		return nil, err
	}
	d.Chart = lineChartDetail(buckets, "/partials/traffic-window?event="+url.QueryEscape(ev.Slug), "chart-detail")
	return d, nil
}

func (s *Server) dashboard(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, translate(language(r), "nav.dashboard"), "dashboard", nil)
	d, err := s.dashboardData(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Data = d
	s.render(w, r, "dashboard", p)
}

func (s *Server) dashboardLive(w http.ResponseWriter, r *http.Request) {
	p := s.page(r, "", "dashboard", nil)
	d, err := s.dashboardData(r, p)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Data = d
	s.partial(w, r, "dashboard", "dashboard_live", p)
}

type rankingsData struct {
	HiddenOnly    bool
	Dirs          []string
	Dir           string
	EventParam    string
	EventSlug     string
	Events        []*store.Event
	Switches      []*store.Switch
	SwitchID      int64
	Limits        []int
	Limit         int
	From, To      time.Time
	IncludeHidden bool
	NoEvent       bool
	View          RankingView
}

func parseLocalTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.ParseInLocation("2006-01-02T15:04", s, time.Local)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func (s *Server) rankingsPage(w http.ResponseWriter, r *http.Request, hiddenOnly bool) {
	ctx := r.Context()
	q := r.URL.Query()
	d := &rankingsData{HiddenOnly: hiddenOnly, Dirs: []string{"down", "up", "total"}, Dir: q.Get("dir"),
		EventParam: q.Get("event"), Limits: []int{10, 20, 50}, IncludeHidden: q.Get("include_hidden") == "1",
		From: parseLocalTime(q.Get("from")), To: parseLocalTime(q.Get("to"))}
	if d.Dir != "up" && d.Dir != "total" {
		d.Dir = "down"
	}
	if d.Limit, _ = strconv.Atoi(q.Get("limit")); d.Limit != 20 && d.Limit != 50 {
		d.Limit = 10
	}
	if hiddenOnly {
		d.Limit = 200
	}
	d.SwitchID, _ = strconv.ParseInt(q.Get("switch"), 10, 64)
	var err error
	if d.Events, err = s.DB.ListEvents(ctx, "", time.Time{}, time.Time{}); err != nil {
		s.fail(w, r, err)
		return
	}
	if d.Switches, err = s.DB.ListSwitches(ctx); err != nil {
		s.fail(w, r, err)
		return
	}
	rq := store.RankingQuery{Order: d.Dir, Limit: d.Limit, SwitchID: d.SwitchID, IncludeHidden: d.IncludeHidden,
		HiddenOnly: hiddenOnly, From: d.From, To: d.To}
	switch d.EventParam {
	case "all":
	case "", "active":
		d.EventParam = "active"
		ev, err := s.DB.ActiveEvent(ctx)
		if errors.Is(err, store.ErrNotFound) {
			// Fall back to the most recent event.
			if len(d.Events) == 0 {
				d.NoEvent = true
				break
			}
			ev = d.Events[0]
			d.EventParam = ev.Slug
		} else if err != nil {
			s.fail(w, r, err)
			return
		}
		rq.EventID, d.EventSlug = ev.ID, ev.Slug
	default:
		ev, err := s.DB.EventBySlug(ctx, d.EventParam)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		rq.EventID, d.EventSlug = ev.ID, ev.Slug
	}
	title := translate(language(r), "nav.rankings")
	nav := "rankings"
	if hiddenOnly {
		title = translate(language(r), "hidden.title")
	}
	p := s.page(r, title, nav, d)
	if !d.NoEvent {
		rows, err := s.DB.Rankings(ctx, rq)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		d.View = newRankingView(p, rows, true)
	}
	s.render(w, r, "rankings", p)
}

func (s *Server) rankings(w http.ResponseWriter, r *http.Request)   { s.rankingsPage(w, r, false) }
func (s *Server) hiddenList(w http.ResponseWriter, r *http.Request) { s.rankingsPage(w, r, true) }

func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	data := struct {
		Q    string
		Hits []store.SearchHit
	}{Q: q}
	if q != "" {
		// An exact MAC jumps straight to the device.
		if mac := macaddr.Normalize(q); mac != "" {
			if id, err := s.DB.DeviceOfMAC(r.Context(), mac); err == nil {
				redirect(w, r, "/devices/"+strconv.FormatInt(id, 10))
				return
			}
		}
		hits, err := s.DB.Search(r.Context(), q, 100)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Hits = hits
	}
	s.render(w, r, "search", s.page(r, translate(language(r), "nav.search"), "", data))
}

func (s *Server) macRedirect(w http.ResponseWriter, r *http.Request) {
	mac := macaddr.Normalize(chi.URLParam(r, "mac"))
	id, err := s.DB.DeviceOfMAC(r.Context(), mac)
	if mac == "" || err != nil {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	redirect(w, r, "/devices/"+strconv.FormatInt(id, 10))
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	return id, err == nil && id > 0
}

type deviceData struct {
	Device      *store.Device
	Label       store.Label
	Events      []store.DeviceEventTotal
	TotalDown   int64
	TotalUp     int64
	IPs         []store.HistoryRow
	Hostnames   []store.HistoryRow
	Locations   []store.LocationRow
	Suggestions []store.MergeSuggestion
	Chart       template.HTML
	ChartEvent  *store.Event
}

func (s *Server) device(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := pathID(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	dev, err := s.DB.GetDevice(ctx, id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d := &deviceData{Device: dev}
	labels, err := s.DB.DeviceLabels(ctx, []int64{id})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d.Label = labels[id]
	if d.Label.Name == "" {
		d.Label.Name = "#" + strconv.FormatInt(id, 10)
	}
	if d.Events, err = s.DB.DeviceEvents(ctx, id, 0); err != nil {
		s.fail(w, r, err)
		return
	}
	for _, e := range d.Events {
		d.TotalDown += e.BytesDown
		d.TotalUp += e.BytesUp
	}
	d.IPs, _ = s.DB.DeviceIPHistory(ctx, id)
	d.Hostnames, _ = s.DB.DeviceHostnameHistory(ctx, id)
	d.Locations, _ = s.DB.DeviceLocations(ctx, id)
	d.Suggestions, _ = s.DB.ListMergeSuggestions(ctx, id)

	if slug := r.URL.Query().Get("event"); slug != "" {
		d.ChartEvent, _ = s.DB.EventBySlug(ctx, slug)
	} else if len(d.Events) > 0 {
		d.ChartEvent = d.Events[0].Event
	}
	if d.ChartEvent != nil {
		buckets, err := s.DB.DeviceTimeseries(ctx, id, d.ChartEvent.ID)
		if err == nil {
			d.Chart = lineChart(buckets)
		}
	}
	s.render(w, r, "device", s.page(r, d.Label.Name, "", d))
}

func (s *Server) deviceUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	name := strings.TrimSpace(r.PostFormValue("display_name"))
	notes := strings.TrimSpace(r.PostFormValue("notes"))
	hidden := r.PostFormValue("hidden") == "1"
	if len(name) > 80 || len(notes) > 500 {
		redirect(w, r, "/devices/"+strconv.FormatInt(id, 10)+"?err=too_long")
		return
	}
	if err := s.DB.UpdateDevice(r.Context(), id, name, hidden, notes); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.update", "device "+strconv.FormatInt(id, 10), "name="+name+" hidden="+strconv.FormatBool(hidden))
	redirect(w, r, "/devices/"+strconv.FormatInt(id, 10)+"?ok=saved")
}

func (s *Server) deviceSplit(w http.ResponseWriter, r *http.Request) {
	id, _ := pathID(r)
	mac := macaddr.Normalize(r.PostFormValue("mac"))
	if owner, err := s.DB.DeviceOfMAC(r.Context(), mac); mac == "" || err != nil || owner != id {
		s.errorPage(w, r, http.StatusBadRequest, "error.bad_request")
		return
	}
	newID, err := s.DB.SplitMAC(r.Context(), mac)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "device.split", mac, "from device "+strconv.FormatInt(id, 10))
	redirect(w, r, "/devices/"+strconv.FormatInt(newID, 10)+"?ok=saved")
}

type mergeItem struct {
	S            store.MergeSuggestion
	NameA, NameB string
}

func (s *Server) merges(w http.ResponseWriter, r *http.Request) {
	list, err := s.DB.ListMergeSuggestions(r.Context(), 0)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var ids []int64
	for _, m := range list {
		ids = append(ids, m.DeviceA, m.DeviceB)
	}
	labels, err := s.DB.DeviceLabels(r.Context(), ids)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	items := make([]mergeItem, 0, len(list))
	for _, m := range list {
		items = append(items, mergeItem{S: m, NameA: labels[m.DeviceA].Name, NameB: labels[m.DeviceB].Name})
	}
	s.render(w, r, "merges", s.page(r, translate(language(r), "merges.title"), "merges", struct{ Items []mergeItem }{items}))
}

func (s *Server) mergeDecide(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	accept := r.PostFormValue("decision") == "accept"
	if err := s.DB.DecideMergeSuggestion(r.Context(), id, accept, app.User(r.Context()).ID); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "merge."+map[bool]string{true: "accept", false: "reject"}[accept], "suggestion "+strconv.FormatInt(id, 10), "")
	back := safeNext(r.PostFormValue("back"))
	if back == "/" {
		back = "/merges"
	}
	redirect(w, r, back+"?ok=saved")
}

type scheduleRow struct {
	*store.EventSchedule
	State string
}

type eventRow struct {
	Event   *store.Event
	Summary store.EventSummary
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	from, _ := time.ParseInLocation("2006-01-02", q.Get("from"), time.Local)
	to, _ := time.ParseInLocation("2006-01-02", q.Get("to"), time.Local)
	if !to.IsZero() {
		to = to.Add(24 * time.Hour)
	}
	events, err := s.DB.ListEvents(ctx, q.Get("q"), from, to)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	data := struct {
		Rows        []eventRow
		Active      *store.Event
		Schedules   []scheduleRow
		Q, From, To string
	}{Q: q.Get("q"), From: q.Get("from"), To: q.Get("to")}
	schedules, err := s.DB.ListEventSchedules(ctx, false)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	now := store.Now()
	for _, sc := range schedules {
		data.Schedules = append(data.Schedules, scheduleRow{sc, sc.State(now)})
	}
	for _, e := range events {
		sum, err := s.DB.EventSummary(ctx, e.ID)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Rows = append(data.Rows, eventRow{e, sum})
	}
	if ev, err := s.DB.ActiveEvent(ctx); err == nil {
		data.Active = ev
	}
	s.render(w, r, "events", s.page(r, translate(language(r), "nav.events"), "events", data))
}

func (s *Server) eventStart(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PostFormValue("name"))
	notes := strings.TrimSpace(r.PostFormValue("notes"))
	if name == "" || len(name) > 100 || len(notes) > 500 {
		redirect(w, r, "/events?err=bad_request")
		return
	}
	ev, err := s.DB.StartEvent(r.Context(), name, notes)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "event.start", ev.Slug, ev.Name)
	redirect(w, r, "/?ok=event_started")
}

func (s *Server) eventStop(w http.ResponseWriter, r *http.Request) {
	ev, err := s.DB.StopEvent(r.Context())
	if errors.Is(err, store.ErrNotFound) {
		redirect(w, r, "/events")
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "event.stop", ev.Slug, ev.Name)
	redirect(w, r, "/events/"+ev.Slug+"?ok=event_stopped")
}

func (s *Server) eventUpdate(w http.ResponseWriter, r *http.Request) {
	ev, err := s.DB.EventBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	notes := strings.TrimSpace(r.PostFormValue("notes"))
	if name == "" || len(name) > 100 || len(notes) > 500 {
		redirect(w, r, "/events/"+ev.Slug+"?err=bad_request")
		return
	}
	if err := s.DB.UpdateEvent(r.Context(), ev.ID, name, notes); err != nil {
		s.fail(w, r, err)
		return
	}
	s.Audit(r, "event.update", ev.Slug, name)
	redirect(w, r, "/events/"+ev.Slug+"?ok=saved")
}

type eventColumn struct {
	Event   *store.Event
	Summary store.EventSummary
	TopDown RankingView
	TopUp   RankingView
	Chart   template.HTML
	Ports   []store.PortTrafficRow
}

func (s *Server) eventColumn(r *http.Request, p *Page, ev *store.Event, limit int, full bool) (*eventColumn, error) {
	ctx := r.Context()
	c := &eventColumn{Event: ev}
	var err error
	if c.Summary, err = s.DB.EventSummary(ctx, ev.ID); err != nil {
		return nil, err
	}
	down, err := s.DB.Rankings(ctx, store.RankingQuery{EventID: ev.ID, Order: "down", Limit: limit})
	if err != nil {
		return nil, err
	}
	up, err := s.DB.Rankings(ctx, store.RankingQuery{EventID: ev.ID, Order: "up", Limit: limit})
	if err != nil {
		return nil, err
	}
	c.TopDown, c.TopUp = newRankingView(p, down, false), newRankingView(p, up, false)
	if full {
		buckets, err := s.DB.EventTimeseries(ctx, ev.ID)
		if err != nil {
			return nil, err
		}
		c.Chart = lineChartDetail(buckets, "/partials/traffic-window?event="+url.QueryEscape(ev.Slug), "chart-detail")
		if c.Ports, err = s.DB.PortTrafficByEvent(ctx, ev.ID); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (s *Server) event(w http.ResponseWriter, r *http.Request) {
	ev, err := s.DB.EventBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p := s.page(r, ev.Name, "events", nil)
	c, err := s.eventColumn(r, p, ev, 20, true)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	p.Data = c
	s.render(w, r, "event", p)
}

func (s *Server) compare(w http.ResponseWriter, r *http.Request) {
	events, err := s.DB.ListEvents(r.Context(), "", time.Time{}, time.Time{})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	selected := r.URL.Query()["e"]
	if len(selected) == 0 && len(events) >= 2 {
		selected = []string{events[1].Slug, events[0].Slug}
	}
	for len(selected) < 3 {
		selected = append(selected, "")
	}
	selected = selected[:3]
	p := s.page(r, translate(language(r), "events.compare"), "events", nil)
	data := struct {
		Events   []*store.Event
		Selected []string
		Columns  []*eventColumn
	}{Events: events, Selected: selected}
	for _, slug := range selected {
		if slug == "" {
			continue
		}
		ev, err := s.DB.EventBySlug(r.Context(), slug)
		if err != nil {
			continue
		}
		c, err := s.eventColumn(r, p, ev, 10, false)
		if err != nil {
			s.fail(w, r, err)
			return
		}
		data.Columns = append(data.Columns, c)
	}
	p.Data = data
	s.render(w, r, "compare", p)
}

// exportEvent writes the full ranking (hidden entries marked) with both directions.
func (s *Server) exportEvent(w http.ResponseWriter, r *http.Request) {
	ev, err := s.DB.EventBySlug(r.Context(), chi.URLParam(r, "slug"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	rows, err := s.DB.Rankings(r.Context(), store.RankingQuery{EventID: ev.ID, Order: "total", Limit: 100000, IncludeHidden: true})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	filename := "lanleecher-" + ev.Slug
	switch chi.URLParam(r, "format") {
	case "csv":
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.csv"`)
		cw := csv.NewWriter(w)
		_ = cw.Write([]string{"rank", "type", "id", "name", "hostname", "ip", "macs", "bytes_down", "bytes_up", "hidden"})
		for _, e := range rows {
			_ = cw.Write([]string{strconv.Itoa(e.Rank), e.Type, strconv.FormatInt(e.Key, 10), csvSafe(e.Name), csvSafe(e.Hostname),
				e.IP, strings.Join(e.MACs, " "), strconv.FormatInt(e.BytesDown, 10), strconv.FormatInt(e.BytesUp, 10),
				strconv.FormatBool(e.Hidden)})
		}
		cw.Flush()
		if err := cw.Error(); err != nil {
			s.Log.Debug("csv export interrupted", "err", err)
		}
	case "json":
		type entry struct {
			Rank      int      `json:"rank"`
			Type      string   `json:"type"`
			ID        int64    `json:"id"`
			Name      string   `json:"name"`
			Hostname  string   `json:"hostname"`
			IP        string   `json:"ip"`
			MACs      []string `json:"macs"`
			BytesDown int64    `json:"bytes_down"`
			BytesUp   int64    `json:"bytes_up"`
			Hidden    bool     `json:"hidden"`
		}
		out := make([]entry, 0, len(rows))
		for _, e := range rows {
			out = append(out, entry{e.Rank, e.Type, e.Key, e.Name, e.Hostname, e.IP, e.MACs, e.BytesDown, e.BytesUp, e.Hidden})
		}
		sum, _ := s.DB.EventSummary(r.Context(), ev.ID)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`.json"`)
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]any{"event": map[string]any{"name": ev.Name, "slug": ev.Slug, "started_at": ev.StartedAt,
			"ended_at": ev.EndedAt}, "totals": map[string]any{"bytes_down": sum.BytesDown, "bytes_up": sum.BytesUp,
			"clients": sum.Clients}, "entries": out})
	default:
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
	}
}

// csvSafe defuses spreadsheet formulas in names that come from the network.
func csvSafe(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
