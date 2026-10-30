// Package api implements the JSON REST API under /api/v1.
//
// The OpenAPI document in openapi.yaml is the single source of truth for
// paths, parameters and response shapes; handler tests validate against it.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/lanleecher/internal/app"
	"github.com/lan-dot-party/lanleecher/internal/auth"
	"github.com/lan-dot-party/lanleecher/internal/store"
)

// API serves /api.
type API struct {
	*app.App
	limiter *auth.RateLimiter
}

// New creates the API handler.
func New(a *app.App) *API {
	return &API{App: a, limiter: auth.NewRateLimiter(a.Cfg.API.RateLimit, a.Cfg.API.RateBurst)}
}

// MaxLimit caps ranking sizes.
const MaxLimit = 500

// Routes returns the router for /api.
func (a *API) Routes() http.Handler { return a.routes(a.Cfg.API.MetricsEnabled) }

// routesForSpec builds the full router for the spec test.
func (a *API) routesForSpec() chi.Router { return a.routes(true) }

func (a *API) routes(metrics bool) chi.Router {
	r := chi.NewRouter()
	r.Get("/", a.docs)
	r.Get("/openapi.json", a.openAPIJSON)
	r.Get("/openapi.yaml", a.openAPIYAML)
	r.Route("/v1", func(r chi.Router) {
		r.With(a.require(auth.ScopeReadRankings)).Get("/rankings/{direction}", a.rankings)
		r.With(a.require(auth.ScopeReadRankings)).Get("/events", a.listEvents)
		r.With(a.require(auth.ScopeReadRankings)).Get("/events/{slug}", a.getEvent)
		r.With(a.require(auth.ScopeWriteEvents)).Post("/events", a.startEvent)
		r.With(a.require(auth.ScopeWriteEvents)).Post("/events/{slug}/stop", a.stopEvent)
		r.With(a.require(auth.ScopeReadRankings)).Get("/schedules", a.listSchedules)
		r.With(a.require(auth.ScopeWriteEvents)).Post("/schedules", a.createSchedule)
		r.With(a.require(auth.ScopeWriteEvents)).Delete("/schedules/{id}", a.deleteSchedule)
		r.With(a.require(auth.ScopeReadClients)).Get("/clients/search", a.searchClients)
		r.With(a.require(auth.ScopeReadClients)).Get("/clients/mac/{mac}", a.clientByMAC)
		r.With(a.require(auth.ScopeReadClients)).Get("/clients/mac/{mac}/timeseries", a.clientTimeseries)
		r.With(a.require(auth.ScopeReadClients)).Get("/clients/ip/{ip}", a.clientsByIP)
		r.With(a.require(auth.ScopeReadClients)).Get("/clients/hostname/{name}", a.clientsByHostname)
		r.With(a.require(auth.ScopeReadClients)).Get("/ports", a.ports)
		r.With(a.boardAuth).Get("/board", a.board)
		r.With(a.boardAuth).Get("/board/stream", a.boardStream)
		if metrics {
			r.With(a.require(auth.ScopeReadRankings)).Get("/metrics", a.metrics)
		}
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			problem(w, http.StatusNotFound, "not_found", "Not Found", "no such endpoint")
		})
	})
	return r
}

type ctxKey int

const keyToken ctxKey = 0

// principal is who calls the API: a token or a logged-in web user.
type principal struct {
	token *store.APIToken
	user  *store.User
}

func (p principal) has(scope string) bool {
	if p.token != nil {
		return p.token.HasScope(scope)
	}
	if p.user == nil {
		return false
	}
	if scope == auth.ScopeWriteEvents {
		return p.user.Can(store.RoleOperator)
	}
	return p.user.Can(store.RoleViewer)
}

func principalOf(ctx context.Context) principal {
	p, _ := ctx.Value(keyToken).(principal)
	return p
}

// authenticate resolves a bearer token or a web session. Sessions must send
// the CSRF token on unsafe methods.
func (a *API) authenticate(w http.ResponseWriter, r *http.Request) (principal, bool) {
	if h := r.Header.Get("Authorization"); h != "" {
		raw, ok := strings.CutPrefix(h, "Bearer ")
		if !ok || !strings.HasPrefix(raw, auth.APITokenPrefix) {
			problem(w, http.StatusUnauthorized, "invalid_token", "Unauthorized", "expected Authorization: Bearer llk_...")
			return principal{}, false
		}
		tok, err := a.DB.APITokenByHash(r.Context(), auth.HashToken(strings.TrimSpace(raw)))
		if err != nil || tok.Revoked || (tok.ExpiresAt != nil && time.Now().After(*tok.ExpiresAt)) {
			problem(w, http.StatusUnauthorized, "invalid_token", "Unauthorized", "token unknown, revoked or expired")
			return principal{}, false
		}
		if !a.ipAllowed(tok, a.ClientIP(r)) {
			problem(w, http.StatusForbidden, "ip_not_allowed", "Forbidden", "client IP not in the token's allowlist")
			return principal{}, false
		}
		if !a.limiter.Allow("token:" + strconv.FormatInt(tok.ID, 10)) {
			w.Header().Set("Retry-After", "1")
			problem(w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests", "rate limit exceeded for this token")
			return principal{}, false
		}
		if err := a.DB.TouchAPIToken(context.WithoutCancel(r.Context()), tok.ID); err != nil {
			a.Log.Warn("record token use", "err", err)
		}
		return principal{token: tok}, true
	}
	if s, u := a.LoadSession(r); u != nil {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRFToken)) != 1 {
				problem(w, http.StatusForbidden, "csrf", "Forbidden", "missing or invalid X-CSRF-Token")
				return principal{}, false
			}
		}
		if !a.limiter.Allow("user:" + strconv.FormatInt(u.ID, 10)) {
			problem(w, http.StatusTooManyRequests, "rate_limited", "Too Many Requests", "rate limit exceeded")
			return principal{}, false
		}
		return principal{user: u}, true
	}
	w.Header().Set("WWW-Authenticate", `Bearer realm="lanleecher"`)
	problem(w, http.StatusUnauthorized, "unauthorized", "Unauthorized", "send Authorization: Bearer llk_...")
	return principal{}, false
}

func (a *API) ipAllowed(t *store.APIToken, ip string) bool {
	if len(t.AllowedIPs) == 0 {
		return true
	}
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, allowed := range t.AllowedIPs {
		if p, err := netip.ParsePrefix(allowed); err == nil && p.Contains(addr) {
			return true
		}
		if a, err := netip.ParseAddr(allowed); err == nil && a == addr {
			return true
		}
	}
	return false
}

func (a *API) require(scope string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			p, ok := a.authenticate(w, r)
			if !ok {
				return
			}
			if !p.has(scope) {
				problem(w, http.StatusForbidden, "insufficient_scope", "Forbidden", "this endpoint needs scope "+scope)
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), keyToken, p)))
		})
	}
}

// boardAuth allows read:board, or anonymous access when board.public is on.
func (a *API) boardAuth(next http.Handler) http.Handler {
	scoped := a.require(auth.ScopeReadBoard)(next)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Cfg.Board.Enabled && a.Cfg.Board.Public && r.Header.Get("Authorization") == "" {
			next.ServeHTTP(w, r)
			return
		}
		scoped.ServeHTTP(w, r)
	})
}

// Problem is an RFC 9457 problem detail.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
	// Code is a stable machine-readable error code.
	Code string `json:"code"`
}

func problem(w http.ResponseWriter, status int, code, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Type: "urn:lanleecher:problem:" + code, Title: title, Status: status,
		Detail: detail, Code: code})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v) // the client may have gone away
}

func (a *API) internal(w http.ResponseWriter, r *http.Request, err error) {
	a.Log.Error("api error", "path", r.URL.Path, "err", err)
	problem(w, http.StatusInternalServerError, "internal", "Internal Server Error", "see server log")
}

// resolveEvent maps ?event= to an event: a slug, "active" (default) or "all" (nil).
func (a *API) resolveEvent(ctx context.Context, w http.ResponseWriter, param string) (*store.Event, bool) {
	switch param {
	case "all":
		return nil, true
	case "", "active":
		ev, err := a.DB.ActiveEvent(ctx)
		if errors.Is(err, store.ErrNotFound) {
			problem(w, http.StatusNotFound, "no_active_event", "Not Found", "no event is running; pass ?event=<slug> or ?event=all")
			return nil, false
		}
		if err != nil {
			problem(w, http.StatusInternalServerError, "internal", "Internal Server Error", "")
			return nil, false
		}
		return ev, true
	}
	ev, err := a.DB.EventBySlug(ctx, param)
	if err != nil {
		problem(w, http.StatusNotFound, "event_not_found", "Not Found", "unknown event "+param)
		return nil, false
	}
	return ev, true
}

func parseLimit(w http.ResponseWriter, s string, def int) (int, bool) {
	if s == "" {
		return def, true
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > MaxLimit {
		problem(w, http.StatusBadRequest, "invalid_parameter", "Bad Request", "limit must be between 1 and 500")
		return 0, false
	}
	return n, true
}

func parseBool(s string) bool { return s == "true" || s == "1" || s == "yes" }

func parseTime(w http.ResponseWriter, name, s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, true
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		problem(w, http.StatusBadRequest, "invalid_parameter", "Bad Request", name+" must be RFC 3339, e.g. 2026-11-14T20:00:00Z")
		return time.Time{}, false
	}
	return t, true
}
