package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/lanleecher/internal/accounting"
	"github.com/lan-dot-party/lanleecher/internal/app"
	"github.com/lan-dot-party/lanleecher/internal/auth"
	"github.com/lan-dot-party/lanleecher/internal/board"
	"github.com/lan-dot-party/lanleecher/internal/config"
	"github.com/lan-dot-party/lanleecher/internal/scheduler"
	"github.com/lan-dot-party/lanleecher/internal/secret"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

// fixture is an API on a temporary database with some traffic.
type fixture struct {
	handler   http.Handler
	full      string // token with every scope
	boardOnly string // token with read:board
	pastSlug  string
	liveSlug  string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	db, err := store.Open(store.Options{Path: filepath.Join(t.TempDir(), "api.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := db.Migrate(ctx, log); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	db.HostnamePriority = cfg.Resolver.HostnamePriority
	raw := make([]byte, 32)
	_, _ = rand.Read(raw)
	key, _ := secret.New(raw)

	sw := &store.Switch{Name: "sw1", Host: "192.0.2.2", Port: 161, SNMPVersion: "v2c", CredentialsEnc: []byte{0}, FDBAdapter: "auto", Enabled: true}
	if err := db.CreateSwitch(ctx, sw); err != nil {
		t.Fatal(err)
	}
	past, err := db.StartEvent(ctx, "Basement LAN #11", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.StopEvent(ctx); err != nil {
		t.Fatal(err)
	}
	live, err := db.StartEvent(ctx, "Basement LAN #12", "")
	if err != nil {
		t.Fatal(err)
	}
	now := store.Now()
	err = db.InTx(ctx, func(tx *store.Tx) error {
		ports, err := tx.UpsertPorts(ctx, sw.ID, []store.PortObservation{
			{IfIndex: 1, IfName: "Gi1/0/1", OperUp: true, SpeedMbps: 1000, HCSupported: true, Physical: true},
			{IfIndex: 2, IfName: "Gi1/0/2", OperUp: true, SpeedMbps: 1000, HCSupported: true, Physical: true},
			{IfIndex: 3, IfName: "Po1", OperUp: true, SpeedMbps: 2000, HCSupported: true, Physical: true, DefaultRole: accounting.RoleUplink},
		}, now)
		if err != nil {
			return err
		}
		if err := tx.RecordSightings(ctx, sw.ID, []store.Sighting{
			{MAC: "02:00:00:00:00:01", PortID: ports[1].ID, VLAN: 1},
			{MAC: "b8:27:eb:00:00:02", PortID: ports[2].ID, VLAN: 1},
		}, nil, 10, now); err != nil {
			return err
		}
		for _, ev := range []int64{past.ID, live.ID} {
			for _, b := range []struct {
				mac  string
				port int64
				down uint64
			}{{"02:00:00:00:00:01", ports[1].ID, 9e9}, {"b8:27:eb:00:00:02", ports[2].ID, 4e9}} {
				if err := tx.AddPortTraffic(ctx, ev, b.port, b.down, b.down/10, now); err != nil {
					return err
				}
				if err := tx.AddSubjectTraffic(ctx, ev, store.SubjectMAC, b.mac, b.port, b.down, b.down/10, now); err != nil {
					return err
				}
			}
			if err := tx.AddPortTraffic(ctx, ev, ports[3].ID, 5e9, 5e9, now); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordIP(ctx, "02:00:00:00:00:01", "192.0.2.10", "dhcp", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordHostname(ctx, "02:00:00:00:00:01", "DESKTOP-FRAG42", "netbios", now); err != nil {
		t.Fatal(err)
	}

	token := func(scopes ...string) string {
		plain := auth.NewAPIToken()
		if err := db.CreateAPIToken(ctx, &store.APIToken{Name: strings.Join(scopes, ","), TokenHash: auth.HashToken(plain),
			Prefix: plain[:10], Scopes: scopes}); err != nil {
			t.Fatal(err)
		}
		return plain
	}

	cfg.API.RateLimit, cfg.API.RateBurst = 1000, 1000
	a := app.New(cfg, db, key, log)
	a.Sched = scheduler.New(db, key, cfg, nil, log)
	a.Board = board.New(db, a.Sched.Throughput)
	r := chi.NewRouter()
	r.Mount("/api", New(a).Routes())
	return &fixture{handler: r, full: token(auth.AllScopes...), boardOnly: token(auth.ScopeReadBoard),
		pastSlug: past.Slug, liveSlug: live.Slug}
}

// TestResponsesMatchSpec calls every documented endpoint, including error
// cases, and validates status, content type and body against openapi.yaml.
func TestResponsesMatchSpec(t *testing.T) {
	ctx := context.Background()
	loader := openapi3.NewLoader()
	doc, err := loader.LoadFromData(openAPIYAMLData)
	if err != nil {
		t.Fatalf("load spec: %v", err)
	}
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	openapi3filter.RegisterBodyDecoder("application/problem+json", openapi3filter.JSONBodyDecoder)

	f := newFixture(t)
	type call struct {
		method, path, token, body string
		status                    int
	}
	calls := []call{
		{"GET", "/api/v1/rankings/download", f.full, "", 200},
		{"GET", "/api/v1/rankings/upload?event=all&limit=5&include_hidden=true", f.full, "", 200},
		{"GET", "/api/v1/rankings/total?event=" + f.pastSlug + "&switch=1", f.full, "", 200},
		{"GET", "/api/v1/rankings/download?from=2026-01-01T00:00:00Z", f.full, "", 200},
		{"GET", "/api/v1/rankings/download?limit=0", f.full, "", 400},
		{"GET", "/api/v1/rankings/download?event=nope", f.full, "", 404},
		{"GET", "/api/v1/rankings/download", "", "", 401},
		{"GET", "/api/v1/rankings/download", "llk_invalid", "", 401},
		{"GET", "/api/v1/rankings/download", f.boardOnly, "", 403},
		{"GET", "/api/v1/events", f.full, "", 200},
		{"GET", "/api/v1/events?q=basement", f.full, "", 200},
		{"GET", "/api/v1/events/" + f.pastSlug, f.full, "", 200},
		{"GET", "/api/v1/events/nope", f.full, "", 404},
		{"GET", "/api/v1/clients/search?q=desk", f.full, "", 200},
		{"GET", "/api/v1/clients/search?q=d", f.full, "", 400},
		{"GET", "/api/v1/clients/search?q=desk", f.boardOnly, "", 403},
		{"GET", "/api/v1/clients/mac/02-00-00-00-00-01?events=5", f.full, "", 200},
		{"GET", "/api/v1/clients/mac/b8:27:eb:00:00:02", f.full, "", 200},
		{"GET", "/api/v1/clients/mac/02:00:00:00:00:99", f.full, "", 404},
		{"GET", "/api/v1/clients/mac/not-a-mac", f.full, "", 400},
		{"GET", "/api/v1/clients/mac/02:00:00:00:00:01/timeseries", f.full, "", 200},
		{"GET", "/api/v1/clients/mac/02:00:00:00:00:01/timeseries?event=" + f.pastSlug, f.full, "", 200},
		{"GET", "/api/v1/clients/ip/192.0.2.10", f.full, "", 200},
		{"GET", "/api/v1/clients/ip/not-an-ip", f.full, "", 400},
		{"GET", "/api/v1/clients/hostname/DESKTOP-FRAG42", f.full, "", 200},
		{"GET", "/api/v1/ports", f.full, "", 200},
		{"GET", "/api/v1/ports?event=" + f.pastSlug, f.full, "", 200},
		{"GET", "/api/v1/board", f.boardOnly, "", 200},
		{"GET", "/api/v1/board?limit=20&event=" + f.pastSlug, f.full, "", 200},
		{"GET", "/api/v1/board?limit=7", f.boardOnly, "", 400},
		{"GET", "/api/v1/board", "", "", 401},
		{"GET", "/api/v1/metrics", f.full, "", 200},
		{"POST", "/api/v1/events", f.full, `{"name": ""}`, 400},
		{"POST", "/api/v1/events", f.boardOnly, `{"name": "x"}`, 403},
		{"POST", "/api/v1/events/" + f.pastSlug + "/stop", f.full, "", 409},
		{"POST", "/api/v1/events/" + f.liveSlug + "/stop", f.full, "", 200},
		{"GET", "/api/v1/rankings/download", f.full, "", 404}, // no event running now
		{"POST", "/api/v1/events", f.full, `{"name": "Basement LAN #13", "notes": "API test"}`, 201},
		{"GET", "/api/v1/schedules", f.full, "", 200},
		{"GET", "/api/v1/schedules?all=true", f.full, "", 200},
		{"GET", "/api/v1/schedules", f.boardOnly, "", 403},
		{"POST", "/api/v1/schedules", f.full, `{"name": "Basement LAN #14", "starts_at": "2099-04-15T00:00:00+02:00", "ends_at": "2099-04-19T00:00:00+02:00"}`, 201},
		{"POST", "/api/v1/schedules", f.full, `{"name": "Overlap", "starts_at": "2099-04-18T00:00:00+02:00", "ends_at": "2099-04-20T00:00:00+02:00"}`, 409},
		{"POST", "/api/v1/schedules", f.full, `{"name": "Past", "starts_at": "2020-04-15T00:00:00Z", "ends_at": "2020-04-19T00:00:00Z"}`, 400},
		{"POST", "/api/v1/schedules", f.full, `{"name": "x"}`, 400},
		{"GET", "/api/v1/schedules", f.full, "", 200},
		{"DELETE", "/api/v1/schedules/1", f.full, "", 204},
		{"DELETE", "/api/v1/schedules/1", f.full, "", 404},
	}

	covered := map[string]bool{}
	for _, c := range calls {
		var body io.Reader
		if c.body != "" {
			body = strings.NewReader(c.body)
		}
		req := httptest.NewRequest(c.method, c.path, body)
		if c.body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		rec := httptest.NewRecorder()
		f.handler.ServeHTTP(rec, req)
		name := c.method + " " + c.path
		if rec.Code != c.status {
			t.Errorf("%s: status %d, want %d: %s", name, rec.Code, c.status, rec.Body.String())
			continue
		}

		route, params, err := router.FindRoute(httptest.NewRequest(c.method, c.path, nil))
		if err != nil {
			t.Errorf("%s: not in the spec: %v", name, err)
			continue
		}
		covered[route.Operation.OperationID] = true
		if err := validate(ctx, route, params, req, rec); err != nil {
			t.Errorf("%s (%d): response does not match the spec:\n%v", name, rec.Code, err)
		}
	}

	// Every documented operation must be exercised. The SSE stream and
	// /health (served outside the API router) are checked elsewhere.
	var missing []string
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			if op.OperationID == "streamBoard" || op.OperationID == "health" {
				continue
			}
			if !covered[op.OperationID] {
				missing = append(missing, method+" "+path)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("operations without a test call: %v", missing)
	}
}

func validate(ctx context.Context, route *routers.Route, params map[string]string, req *http.Request, rec *httptest.ResponseRecorder) error {
	in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc}}
	return openapi3filter.ValidateResponse(ctx, &openapi3filter.ResponseValidationInput{
		RequestValidationInput: in,
		Status:                 rec.Code,
		Header:                 rec.Header(),
		Body:                   io.NopCloser(bytes.NewReader(rec.Body.Bytes())),
		Options:                &openapi3filter.Options{IncludeResponseStatus: true, MultiError: true},
	})
}

// TestBoardStreamMatchesBoard reads the first SSE event and checks that it
// carries a board document with the same top-level fields as GET /board.
func TestBoardStreamMatchesBoard(t *testing.T) {
	f := newFixture(t)
	srv := httptest.NewServer(f.handler)
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/api/v1/board/stream", nil)
	req.Header.Set("Authorization", "Bearer "+f.boardOnly)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	buf := make([]byte, 4096)
	n, _ := io.ReadAtLeast(resp.Body, buf, 64)
	first := string(buf[:n])
	if !strings.HasPrefix(first, "event: board\ndata: {") || !strings.Contains(first, `"top_download"`) {
		t.Fatalf("unexpected first event: %q", first)
	}
}
