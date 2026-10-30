// Package web serves the server-rendered UI (html/template + htmx).
//
// Everything except /login, /board (if public), /health and /static is behind
// a login. Hostnames and ifAlias come from the network and are untrusted;
// html/template escapes all output and the CSP forbids inline scripts.
package web

import (
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/lan-dot-party/lanleecher/internal/app"
	"github.com/lan-dot-party/lanleecher/internal/auth"
	"github.com/lan-dot-party/lanleecher/internal/store"
	"github.com/lan-dot-party/lanleecher/pkg/version"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static
var staticFS embed.FS

// Server is the web UI.
type Server struct {
	*app.App
	pages map[string]*template.Template
	login *auth.LoginLimiter
}

var pageFiles = []string{
	"login", "dashboard", "rankings", "search", "device", "merges", "events", "event", "compare",
	"switches", "switch_form", "ports", "users", "tokens", "audit", "settings", "account", "board", "error",
	"dhcp_list", "dhcp_form", "dhcp_source",
}

// New parses all templates.
func New(a *app.App) (*Server, error) {
	s := &Server{App: a, pages: map[string]*template.Template{}, login: auth.NewLoginLimiter()}
	for _, name := range pageFiles {
		t, err := template.New(name).Funcs(funcs).ParseFS(templateFS,
			"templates/layout.html", "templates/partials.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("parse template %s: %w", name, err)
		}
		s.pages[name] = t
	}
	return s, nil
}

// Routes returns the UI router. The API is mounted by the caller.
func (s *Server) Routes() http.Handler {
	r := chi.NewRouter()
	static, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", cacheStatic(http.FileServerFS(static))))
	r.Get("/login", s.loginPage)
	r.Post("/login", s.loginSubmit)
	r.Get("/board", s.boardPage)

	r.Group(func(r chi.Router) {
		r.Use(s.requireLogin, s.csrf)
		r.Post("/logout", s.logout)
		r.Get("/", s.dashboard)
		r.Get("/partials/dashboard", s.dashboardLive)
		r.Get("/partials/traffic-window", s.trafficWindow)
		r.Get("/rankings", s.rankings)
		r.Get("/hidden", s.hiddenList)
		r.Get("/search", s.search)
		r.Get("/devices/{id}", s.device)
		r.Get("/mac/{mac}", s.macRedirect)
		r.Get("/merges", s.merges)
		r.Get("/events", s.events)
		r.Get("/events/compare", s.compare)
		r.Get("/events/{slug}", s.event)
		r.Get("/events/{slug}/export.{format}", s.exportEvent)
		r.Get("/switches", s.switches)
		r.Get("/switches/{id}", s.ports)
		r.Get("/account", s.account)
		r.Post("/account/password", s.accountPassword)
		r.Post("/account/language", s.accountLanguage)
		r.Post("/account/theme", s.accountTheme)

		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(store.RoleOperator))
			r.Post("/devices/{id}", s.deviceUpdate)
			r.Post("/devices/{id}/split", s.deviceSplit)
			r.Post("/merges/{id}", s.mergeDecide)
			r.Post("/events/start", s.eventStart)
			r.Post("/events/stop", s.eventStop)
			r.Post("/events/schedules", s.scheduleCreate)
			r.Post("/events/schedules/{id}/start", s.scheduleStartNow)
			r.Post("/events/schedules/{id}/delete", s.scheduleDelete)
			r.Post("/events/{slug}", s.eventUpdate)
			r.Post("/switches/{id}/ports", s.portsUpdate)
		})
		r.Group(func(r chi.Router) {
			r.Use(s.requireRole(store.RoleAdmin))
			r.Get("/switches/new", s.switchNew)
			r.Post("/switches", s.switchCreate)
			r.Post("/switches/test", s.switchTest)
			r.Get("/switches/{id}/edit", s.switchEdit)
			r.Post("/switches/{id}", s.switchUpdate)
			r.Post("/switches/{id}/delete", s.switchDelete)
			r.Get("/admin/users", s.users)
			r.Post("/admin/users", s.userCreate)
			r.Post("/admin/users/{id}", s.userUpdate)
			r.Post("/admin/users/{id}/password", s.userPassword)
			r.Post("/admin/users/{id}/delete", s.userDelete)
			r.Get("/admin/tokens", s.tokens)
			r.Post("/admin/tokens", s.tokenCreate)
			r.Post("/admin/tokens/{id}/revoke", s.tokenRevoke)
			r.Get("/admin/audit", s.audit)
			r.Get("/admin/settings", s.settings)
			r.Get("/admin/dhcp", s.dhcpList)
			r.Get("/admin/dhcp/new", s.dhcpNew)
			r.Post("/admin/dhcp", s.dhcpCreate)
			r.Get("/admin/dhcp/{id}", s.dhcpShow)
			r.Get("/admin/dhcp/{id}/edit", s.dhcpEdit)
			r.Post("/admin/dhcp/{id}", s.dhcpUpdate)
			r.Post("/admin/dhcp/{id}/delete", s.dhcpDelete)
			r.Post("/admin/dhcp/{id}/test", s.dhcpTest)
			r.Post("/admin/dhcp/{id}/trust", s.dhcpTrust)
			r.Post("/admin/dhcp/{id}/untrust", s.dhcpUntrust)
		})
	})
	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
	})
	return r
}

func cacheStatic(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=3600")
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireLogin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if app.User(r.Context()) == nil {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			next := r.URL.RequestURI()
			http.Redirect(w, r, "/login?next="+url.QueryEscape(next), http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requireRole(role string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !app.User(r.Context()).Can(role) {
				s.errorPage(w, r, http.StatusForbidden, "error.forbidden")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// csrf checks the session's token on every state-changing request.
func (s *Server) csrf(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			sess := app.Session(r.Context())
			token := r.Header.Get("X-CSRF-Token")
			if token == "" {
				token = r.PostFormValue("_csrf")
			}
			if sess == nil || subtle.ConstantTimeCompare([]byte(token), []byte(sess.CSRFToken)) != 1 {
				s.errorPage(w, r, http.StatusForbidden, "error.csrf")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// Page is passed to every template.
type Page struct {
	Lang    string
	User    *store.User
	CSRF    string
	Title   string
	Nav     string
	Flash   string
	Error   string
	Version string
	Board   bool
	// Theme is the color theme: auto, light or dark.
	Theme string
	// Path is the current request URI, to return to after a theme change.
	Path string
	Data any
}

// NextTheme is the theme the toggle in the top bar switches to.
func (p *Page) NextTheme() string {
	switch p.Theme {
	case "auto":
		return "light"
	case "light":
		return "dark"
	}
	return "auto"
}

// T translates a key.
func (p *Page) T(key string, args ...any) string { return translate(p.Lang, key, args...) }

// Can reports whether the user has a role (for templates).
func (p *Page) Can(role string) bool { return p.User.Can(role) }

func (s *Server) page(r *http.Request, title, nav string, data any) *Page {
	p := &Page{Lang: language(r), User: app.User(r.Context()), Title: title, Nav: nav, Version: version.Version,
		Board: s.Cfg.Board.Enabled, Theme: theme(r), Path: r.URL.RequestURI(), Data: data}
	if sess := app.Session(r.Context()); sess != nil {
		p.CSRF = sess.CSRFToken
	}
	if ok := r.URL.Query().Get("ok"); ok != "" && hasKey("flash."+ok) {
		p.Flash = p.T("flash." + ok)
	}
	if e := r.URL.Query().Get("err"); e != "" && hasKey("error."+e) {
		p.Error = p.T("error." + e)
	}
	return p
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, name string, p *Page) {
	s.renderStatus(w, r, http.StatusOK, name, "layout", p)
}

func (s *Server) renderStatus(w http.ResponseWriter, r *http.Request, status int, name, tmpl string, p *Page) {
	t, ok := s.pages[name]
	if !ok {
		http.Error(w, "unknown template "+name, http.StatusInternalServerError)
		return
	}
	var buf strings.Builder
	if err := t.ExecuteTemplate(&buf, tmpl, p); err != nil {
		s.Log.Error("render", "template", name, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(buf.String())) // the client may have gone away
}

// partial renders a named block of a page without the layout (htmx).
func (s *Server) partial(w http.ResponseWriter, r *http.Request, name, block string, p *Page) {
	s.renderStatus(w, r, http.StatusOK, name, block, p)
}

func (s *Server) errorPage(w http.ResponseWriter, r *http.Request, status int, key string) {
	p := s.page(r, "", "", nil)
	p.Error = p.T(key)
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		fmt.Fprintf(w, `<p class="alert alert-error">%s</p>`, template.HTMLEscapeString(p.Error))
		return
	}
	s.renderStatus(w, r, status, "error", "layout", p)
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	if errors.Is(err, store.ErrNotFound) {
		s.errorPage(w, r, http.StatusNotFound, "error.not_found")
		return
	}
	s.Log.Error("request failed", "path", r.URL.Path, "err", err)
	s.errorPage(w, r, http.StatusInternalServerError, "error.internal")
}

func redirect(w http.ResponseWriter, r *http.Request, to string) {
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// themeCookie keeps the theme for the login page after logout.
const themeCookie = "ll_theme"

// theme picks the user's theme, else the cookie's, else auto.
func theme(r *http.Request) string {
	if u := app.User(r.Context()); u != nil && store.ValidTheme(u.Theme) {
		return u.Theme
	}
	if c, err := r.Cookie(themeCookie); err == nil && store.ValidTheme(c.Value) {
		return c.Value
	}
	return "auto"
}

// language picks the user's language, else the browser's, else English.
func language(r *http.Request) string {
	if u := app.User(r.Context()); u != nil && (u.Language == "de" || u.Language == "en") {
		return u.Language
	}
	if c, err := r.Cookie("ll_lang"); err == nil && (c.Value == "de" || c.Value == "en") {
		return c.Value
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Accept-Language")), "de") {
		return "de"
	}
	return "en"
}
