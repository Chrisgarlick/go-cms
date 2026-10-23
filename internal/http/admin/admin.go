// Package admin is the server-rendered admin UI (spec 4.13). Pages are
// html/template; the entry editor is a small JS app fed by a JSON data block.
// Every action goes through the same services as the CLI and API.
package admin

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"cms/internal/actor"
	"cms/internal/app"
	"cms/internal/auth"
	"cms/internal/content"
	"cms/internal/fields"
	"cms/internal/rbac"
	"cms/internal/web"
	webassets "cms/web"
)

//go:embed templates
var tmplFS embed.FS

type Handler struct {
	a      *app.App
	mux    *http.ServeMux
	tmpl   map[string]*template.Template
	secure bool
	cookie string
	assets fs.FS
	csrfMW *http.CrossOriginProtection
}

type ctxKey int

const keySession ctxKey = 1

// New builds the admin handler (mounted at /admin/).
func New(a *app.App) http.Handler {
	u, _ := url.Parse(a.Cfg.BaseURL)
	h := &Handler{a: a, mux: http.NewServeMux(), secure: u.Scheme == "https", assets: webassets.Dist()}
	h.cookie = "cms_session"
	if h.secure {
		h.cookie = "__Host-cms_session"
	}
	h.csrfMW = http.NewCrossOriginProtection()
	h.loadTemplates()
	h.routes()
	return h
}

func (h *Handler) loadTemplates() {
	funcs := template.FuncMap{
		"ago":      ago,
		"date":     func(t time.Time) string { return t.Local().Format("2 Jan 2006, 15:04") },
		"dateOnly": func(t time.Time) string { return t.Local().Format("2 Jan 2006") },
		"deref": func(t *time.Time) time.Time {
			if t == nil {
				return time.Time{}
			}
			return *t
		},
		"json": func(v any) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
		"can":       func(s *Session, c string) bool { return s != nil && rbac.Has(s.Role, rbac.Cap(c)) },
		"add":       func(a, b int) int { return a + b },
		"sub":       func(a, b int) int { return a - b },
		"title":     func(s string) string { return fields.Humanize(s) },
		"lower":     strings.ToLower,
		"eq2":       func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) },
		"kb":        func(n int64) string { return humanBytes(n) },
		"str":       func(v any) string { return fmt.Sprint(v) },
		"hasPrefix": strings.HasPrefix,
		"deref2": func(v *int32) int32 {
			if v == nil {
				return 0
			}
			return *v
		},
		"uuidStr": func(v any) string {
			switch t := v.(type) {
			case uuid.UUID:
				return t.String()
			case *uuid.UUID:
				if t != nil {
					return t.String()
				}
			}
			return ""
		},
		"dict": func(kv ...any) map[string]any {
			m := map[string]any{}
			for i := 0; i+1 < len(kv); i += 2 {
				m[kv[i].(string)] = kv[i+1]
			}
			return m
		},
		"seq": func(n int) []int {
			s := make([]int, n)
			for i := range s {
				s[i] = i + 1
			}
			return s
		},
	}
	base := template.Must(template.New("base").Funcs(funcs).ParseFS(tmplFS, "templates/layout.gohtml", "templates/partials/*.gohtml"))
	h.tmpl = map[string]*template.Template{}
	pages, _ := fs.Glob(tmplFS, "templates/pages/*.gohtml")
	for _, p := range pages {
		name := strings.TrimSuffix(strings.TrimPrefix(p, "templates/pages/"), ".gohtml")
		t := template.Must(template.Must(base.Clone()).ParseFS(tmplFS, p))
		h.tmpl[name] = t
	}
}

// Session is the signed-in user for one request.
type Session struct {
	*auth.Session
	Token string
}

func sessionFrom(ctx context.Context) *Session {
	s, _ := ctx.Value(keySession).(*Session)
	return s
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Cache-Control", "no-store")
	hd.Set("X-Frame-Options", "DENY")
	hd.Set("X-Robots-Tag", "noindex, nofollow")
	media := ""
	if h.a.Cfg.MediaURL != "" {
		if mu, err := url.Parse(h.a.Cfg.MediaURL); err == nil {
			media = mu.Scheme + "://" + mu.Host
		}
	}
	csp := web.CSP{Img: []string{"blob:"}, FrameAncestors: "'none'"}
	if media != "" {
		csp.Img = append(csp.Img, media)
		csp.Media = []string{media}
	}
	hd.Set("Content-Security-Policy", csp.String())
	// Cross-origin protection (Sec-Fetch-Site / Origin) on every unsafe method.
	if err := h.csrfMW.Check(r); err != nil {
		http.Error(w, "Cross-origin request blocked", http.StatusForbidden)
		return
	}
	// Session lookup (any stage); handlers decide what each stage may do.
	if c, err := r.Cookie(h.cookie); err == nil {
		if s, err := h.a.Auth.Authenticate(r.Context(), c.Value); err == nil {
			sess := &Session{Session: s, Token: c.Value}
			ctx := context.WithValue(r.Context(), keySession, sess)
			if s.Stage == auth.StageFull {
				ctx = actor.With(ctx, s.Actor(web.ClientIP(r.Context())))
			}
			r = r.WithContext(ctx)
		}
	}
	// Per-session CSRF token on every unsafe method (in addition to the origin check).
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		if s := sessionFrom(r.Context()); s != nil {
			tok := r.Header.Get("X-CSRF-Token")
			if tok == "" {
				tok = r.PostFormValue("csrf")
			}
			if tok != s.CSRF {
				http.Error(w, "Your session form token is invalid; reload the page and try again.", http.StatusForbidden)
				return
			}
		}
	}
	h.mux.ServeHTTP(w, r)
}

// authed wraps handlers that need a full session (and optionally a capability).
func (h *Handler) authed(capability rbac.Cap, fn http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s := sessionFrom(r.Context())
		if s == nil {
			h.redirectLogin(w, r)
			return
		}
		switch s.Stage {
		case auth.StageMFA:
			http.Redirect(w, r, "/admin/login/2fa", http.StatusSeeOther)
			return
		case auth.StageEnroll:
			http.Redirect(w, r, "/admin/enroll", http.StatusSeeOther)
			return
		}
		if capability != "" && !rbac.Has(s.Role, capability) {
			h.error(w, r, http.StatusForbidden, "You don't have permission to do that.")
			return
		}
		fn(w, r)
	}
}

func (h *Handler) redirectLogin(w http.ResponseWriter, r *http.Request) {
	if isJSON(r) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Your session has ended. Sign in again."})
		return
	}
	next := r.URL.RequestURI()
	if r.Method != http.MethodGet || !strings.HasPrefix(next, "/admin/") {
		next = "/admin/"
	}
	http.Redirect(w, r, "/admin/login?next="+url.QueryEscape(next), http.StatusSeeOther)
}

func isJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json") || strings.HasPrefix(r.URL.Path, "/admin/api/")
}

// Page is the data every admin template receives.
type Page struct {
	Title   string
	Section string
	Session *Session
	CSRF    string
	Flash   string
	Error   string
	Site    string
	SiteURL string
	Nav     []NavGroup
	Data    any
	Version string
	Dev     bool
	Path    string
}

type NavGroup struct {
	Label string
	Items []NavItem
}

type NavItem struct {
	Label  string
	URL    string
	Icon   string
	Active bool
}

var flashes = map[string]string{
	"saved": "Saved.", "published": "Published.", "deleted": "Deleted.", "invited": "Invitation sent.",
	"password": "Password changed.", "enrolled": "Two-factor authentication is set up.", "signedout": "Signed out of every other device.",
	"revoked": "Revoked.", "rebuild": "The site is being rebuilt in the background.", "audit": "Security checks finished.",
	"reset-sent": "If that email has an account, a reset link is on its way.", "reset-done": "Password updated. Sign in with your new password.",
	"restored": "Restored.", "uploaded": "Uploaded.", "created": "Created.",
}

func (h *Handler) page(r *http.Request, title, section string, data any) *Page {
	s := sessionFrom(r.Context())
	p := &Page{Title: title, Section: section, Session: s, Data: data, Site: h.a.Site().Config.Name, SiteURL: h.a.Cfg.BaseURL,
		Version: versionString(), Dev: h.a.Cfg.Dev(), Path: r.URL.Path}
	if name := h.a.Settings.SiteName(r.Context()); name != "" {
		p.Site = name
	}
	if s != nil {
		p.CSRF = s.CSRF
	}
	if f := r.URL.Query().Get("ok"); f != "" {
		p.Flash = flashes[f]
	}
	if s != nil && s.Stage == auth.StageFull {
		p.Nav = h.nav(s, r.URL.Path)
	}
	return p
}

func (h *Handler) nav(s *Session, path string) []NavGroup {
	b := h.a.Site()
	var contentItems []NavItem
	types := append([]string{}, b.TypeOrder...)
	for _, name := range types {
		ct := b.Types[name]
		u := "/admin/content/" + name + "/"
		contentItems = append(contentItems, NavItem{Label: ct.Label.Plural, URL: u, Icon: firstNonEmpty(ct.Icon, "file"), Active: strings.HasPrefix(path, u)})
	}
	contentItems = append(contentItems, NavItem{Label: "Media", URL: "/admin/media/", Icon: "image", Active: strings.HasPrefix(path, "/admin/media")})
	if len(b.Forms) > 0 && rbac.Has(s.Role, rbac.FormsView) {
		contentItems = append(contentItems, NavItem{Label: "Form submissions", URL: "/admin/forms/", Icon: "mail", Active: strings.HasPrefix(path, "/admin/forms")})
	}
	taxNames := make([]string, 0, len(b.Taxonomies))
	for n := range b.Taxonomies {
		taxNames = append(taxNames, n)
	}
	sort.Strings(taxNames)
	var org []NavItem
	for _, n := range taxNames {
		u := "/admin/terms/" + n + "/"
		org = append(org, NavItem{Label: b.Taxonomies[n].Label.Plural, URL: u, Icon: "tag", Active: strings.HasPrefix(path, u)})
	}
	org = append(org, NavItem{Label: "Menus", URL: "/admin/menus/", Icon: "menu", Active: strings.HasPrefix(path, "/admin/menus")})
	for _, name := range sortedKeys(b.Options) {
		u := "/admin/options/" + name
		org = append(org, NavItem{Label: b.Options[name].Label, URL: u, Icon: "sliders", Active: path == u})
	}
	groups := []NavGroup{{Label: "Content", Items: contentItems}, {Label: "Organise", Items: org}}
	var site []NavItem
	if rbac.Has(s.Role, rbac.RedirectManage) {
		site = append(site, NavItem{Label: "Redirects", URL: "/admin/redirects/", Icon: "redirect", Active: strings.HasPrefix(path, "/admin/redirects")})
	}
	if rbac.Has(s.Role, rbac.UsersManage) {
		site = append(site, NavItem{Label: "Users", URL: "/admin/users/", Icon: "users", Active: strings.HasPrefix(path, "/admin/users")})
	}
	if rbac.Has(s.Role, rbac.SettingsManage) {
		site = append(site, NavItem{Label: "Settings", URL: "/admin/settings/", Icon: "settings", Active: strings.HasPrefix(path, "/admin/settings")})
	}
	if rbac.Has(s.Role, rbac.SecuritySummary) {
		site = append(site, NavItem{Label: "Security", URL: "/admin/security/", Icon: "shield", Active: strings.HasPrefix(path, "/admin/security")})
	}
	if rbac.Has(s.Role, rbac.SystemView) {
		site = append(site, NavItem{Label: "System", URL: "/admin/system/", Icon: "activity", Active: strings.HasPrefix(path, "/admin/system")})
	}
	if len(site) > 0 {
		groups = append(groups, NavGroup{Label: "Site", Items: site})
	}
	return groups
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (h *Handler) render(w http.ResponseWriter, r *http.Request, name string, status int, p *Page) {
	t := h.tmpl[name]
	if t == nil {
		http.Error(w, "missing template "+name, http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		h.a.Log.Error("admin template failed", "template", name, "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// error renders a friendly error page (or JSON for API calls).
func (h *Handler) error(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if isJSON(r) {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	section := ""
	if s := sessionFrom(r.Context()); s == nil || s.Stage != auth.StageFull {
		section = "auth"
	}
	p := h.page(r, http.StatusText(status), section, map[string]any{"Status": status, "Message": msg})
	h.render(w, r, "error", status, p)
}

// fail maps service errors to responses.
func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	var fe fields.Errors
	var forbidden *actor.ErrForbidden
	var conflict *content.ErrConflict
	switch {
	case errors.Is(err, actor.ErrSudoRequired):
		if isJSON(r) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Confirm it's you to continue.", "sudo": true})
			return
		}
		http.Redirect(w, r, "/admin/sudo?next="+url.QueryEscape(refererPath(r)), http.StatusSeeOther)
	case errors.As(err, &fe):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "Some fields need attention.", "fields": fe})
	case errors.As(err, &forbidden):
		h.error(w, r, http.StatusForbidden, "You don't have permission to do that.")
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "current_version": conflict.Current})
	case errors.Is(err, content.ErrNotFound), errors.Is(err, auth.ErrUserNotFound):
		h.error(w, r, http.StatusNotFound, "Not found.")
	default:
		h.a.Log.Error("admin action failed", "path", r.URL.Path, "err", err, "request_id", web.RequestID(r.Context()))
		msg := err.Error()
		if len(msg) > 300 {
			msg = "Something went wrong. The error has been logged."
		}
		h.error(w, r, http.StatusBadRequest, msg)
	}
}

func refererPath(r *http.Request) string {
	if ref, err := url.Parse(r.Referer()); err == nil && strings.HasPrefix(ref.Path, "/admin/") {
		return ref.RequestURI()
	}
	return "/admin/"
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	return dec.Decode(v)
}

// safeNext only allows local admin paths as post-login destinations.
func safeNext(next string) string {
	if strings.HasPrefix(next, "/admin/") && !strings.HasPrefix(next, "//") && !strings.ContainsAny(next, "\\\r\n") {
		return next
	}
	return "/admin/"
}

func (h *Handler) setSessionCookie(w http.ResponseWriter, token string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{Name: h.cookie, Value: token, Path: "/", HttpOnly: true, Secure: h.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: int(maxAge.Seconds())})
}

func (h *Handler) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: h.cookie, Value: "", Path: "/", HttpOnly: true, Secure: h.secure,
		SameSite: http.SameSiteStrictMode, MaxAge: -1})
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 7*24*time.Hour:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
	return t.Local().Format("2 Jan 2006")
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KB", n>>10)
	}
	return fmt.Sprintf("%d B", n)
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// assetsHandler serves the embedded admin bundle, gzip-compressed once at
// first use and kept in memory (the editor bundle shrinks ~70%).
func (h *Handler) assetsHandler() http.Handler {
	type asset struct {
		raw, gz []byte
		ctype   string
	}
	var mu sync.Mutex
	cache := map[string]*asset{}
	load := func(name string) *asset {
		mu.Lock()
		defer mu.Unlock()
		if a, ok := cache[name]; ok {
			return a
		}
		raw, err := fs.ReadFile(h.assets, name)
		if err != nil {
			cache[name] = nil
			return nil
		}
		var buf bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
		_, _ = zw.Write(raw)
		_ = zw.Close()
		ct := mime.TypeByExtension(path.Ext(name))
		if ct == "" {
			ct = "application/octet-stream"
		}
		a := &asset{raw: raw, gz: buf.Bytes(), ctype: ct}
		cache[name] = a
		return a
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/admin/assets/")
		if name == "" || strings.Contains(name, "..") {
			http.NotFound(w, r)
			return
		}
		a := load(name)
		if a == nil {
			http.NotFound(w, r)
			return
		}
		hd := w.Header()
		hd.Set("Content-Type", a.ctype)
		hd.Set("Cache-Control", "public, max-age=3600")
		if r.URL.Query().Get("v") != "" {
			hd.Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		hd.Add("Vary", "Accept-Encoding")
		body := a.raw
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && len(a.gz) < len(a.raw) {
			hd.Set("Content-Encoding", "gzip")
			body = a.gz
		}
		hd.Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})
}
