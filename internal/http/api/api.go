// Package api is the read-only REST/JSON API under /api/v1 (spec 4.14).
// Write endpoints arrive in v1.x; until then writes go through the admin and CLI.
package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"cms/internal/actor"
	"cms/internal/app"
	"cms/internal/auth"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/media"
	"cms/internal/rbac"
	"cms/internal/web"
)

type Handler struct {
	a       *app.App
	mux     *http.ServeMux
	limiter *auth.Limiter
	origins map[string]bool
}

func New(a *app.App) http.Handler {
	h := &Handler{a: a, mux: http.NewServeMux(), limiter: auth.NewLimiter(600, time.Minute), origins: map[string]bool{}}
	for _, o := range a.Cfg.API.CORSOrigins {
		h.origins[strings.TrimRight(o, "/")] = true
	}
	h.mux.HandleFunc("GET /api/openapi.json", h.openapi)
	h.mux.HandleFunc("GET /api/v1/types", h.types)
	h.mux.HandleFunc("GET /api/v1/content/{type}", h.list)
	h.mux.HandleFunc("GET /api/v1/content/{type}/{ref}", h.get)
	h.mux.HandleFunc("GET /api/v1/taxonomies/{tax}/terms", h.terms)
	h.mux.HandleFunc("GET /api/v1/menus/{name}", h.menu)
	h.mux.HandleFunc("GET /api/v1/search", h.search)
	h.mux.HandleFunc("GET /api/v1/site", h.siteInfo)
	h.mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions {
			writeErr(w, http.StatusMethodNotAllowed, "the v1 API is read-only; use the admin or CLI to change content")
			return
		}
		writeErr(w, http.StatusNotFound, "not found")
	})
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.a.Cfg.API.Disabled {
		http.NotFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Content-Type", "application/json")
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	if origin := r.Header.Get("Origin"); origin != "" && h.origins[origin] {
		hd.Set("Access-Control-Allow-Origin", origin)
		hd.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		hd.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		hd.Set("Access-Control-Max-Age", "600")
		hd.Add("Vary", "Origin")
	}
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	key := web.ClientIP(r.Context()).String()
	// Token authentication (optional for public reads).
	if authz := r.Header.Get("Authorization"); strings.HasPrefix(authz, "Bearer ") {
		a, err := h.a.Auth.AuthenticateToken(r.Context(), strings.TrimPrefix(authz, "Bearer "))
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		key = "token:" + a.TokenID.String()
		r = r.WithContext(actor.With(r.Context(), a))
	}
	if !h.limiter.Allow(key) {
		hd.Set("Retry-After", "60")
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded")
		return
	}
	if actor.From(r.Context()).Kind == actor.KindAnonymous {
		hd.Set("Cache-Control", "public, max-age=60")
	} else {
		hd.Set("Cache-Control", "private, no-store")
	}
	h.mux.ServeHTTP(w, r)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"status": status, "message": msg}})
}

func writeJSON(w http.ResponseWriter, v any) {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(true)
	_ = enc.Encode(v)
}

// sys gives read queries a system actor scoped to reading content: public
// content is public; authorisation for drafts is checked separately.
func sys(ctx context.Context) context.Context {
	return actor.With(ctx, actor.SystemWith("api", rbac.ContentRead))
}

type entryJSON struct {
	ID            uuid.UUID           `json:"id"`
	Type          string              `json:"type"`
	Title         string              `json:"title"`
	Slug          string              `json:"slug"`
	URL           string              `json:"url"`
	Excerpt       string              `json:"excerpt,omitempty"`
	PublishedAt   *time.Time          `json:"published_at"`
	UpdatedAt     time.Time           `json:"updated_at"`
	Author        *authorJSON         `json:"author,omitempty"`
	FeaturedImage *mediaJSON          `json:"featured_image,omitempty"`
	Terms         map[string][]termJS `json:"terms,omitempty"`
	Fields        json.RawMessage     `json:"fields,omitempty"`
	Blocks        json.RawMessage     `json:"blocks,omitempty"`
	HTML          string              `json:"html,omitempty"`
	SEO           *content.SEO        `json:"seo,omitempty"`
}

type authorJSON struct {
	Name string `json:"name"`
	Bio  string `json:"bio,omitempty"`
}

type termJS struct {
	ID   uuid.UUID `json:"id"`
	Name string    `json:"name"`
	Slug string    `json:"slug"`
}

type mediaJSON struct {
	ID       uuid.UUID        `json:"id"`
	URL      string           `json:"url"`
	Alt      string           `json:"alt"`
	Width    int              `json:"width,omitempty"`
	Height   int              `json:"height,omitempty"`
	Variants map[string]vJSON `json:"variants,omitempty"`
}

type vJSON struct {
	URL    string `json:"url"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

func toMedia(v *media.View) *mediaJSON {
	if v == nil {
		return nil
	}
	m := &mediaJSON{ID: v.ID, URL: v.URL, Alt: v.Alt, Width: v.Width, Height: v.Height, Variants: map[string]vJSON{}}
	for _, x := range v.Variants {
		m.Variants[x.Preset] = vJSON{x.URL, x.Width, x.Height}
	}
	return m
}

func (h *Handler) publicType(w http.ResponseWriter, name string) bool {
	ct := h.a.Site().Types[name]
	if ct == nil || !ct.IsPublic() {
		writeErr(w, http.StatusNotFound, "unknown content type")
		return false
	}
	return true
}

func (h *Handler) types(w http.ResponseWriter, r *http.Request) {
	type t struct {
		Name       string   `json:"name"`
		Label      string   `json:"label"`
		URL        string   `json:"url_pattern,omitempty"`
		Archive    string   `json:"archive,omitempty"`
		Taxonomies []string `json:"taxonomies,omitempty"`
	}
	var out []t
	b := h.a.Site()
	for _, n := range b.TypeOrder {
		ct := b.Types[n]
		if ct.IsPublic() {
			out = append(out, t{n, ct.Label.Plural, ct.URL, ct.Archive, ct.Taxonomies})
		}
	}
	writeJSON(w, map[string]any{"data": out})
}

// list returns published entries, newest first, with an opaque cursor.
func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	if !h.publicType(w, typ) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := decodeCursor(q.Get("cursor"))
	sort := "newest"
	switch q.Get("sort") {
	case "oldest", "title", "menu_order":
		sort = q.Get("sort")
	}
	params := dbq.PublishedListParams{Type: typ, Locale: h.a.Site().Config.Locale, Sort: sort, Lim: int32(limit), Off: int32(offset)} //nolint:gosec // limit <= 100, offset <= 100000
	if tax, slug := q.Get("taxonomy"), q.Get("term"); tax != "" && slug != "" {
		t, err := h.a.DB.Q.GetTermBySlug(r.Context(), dbq.GetTermBySlugParams{Taxonomy: tax, Slug: slug})
		if err != nil {
			writeJSON(w, map[string]any{"data": []any{}, "next_cursor": nil})
			return
		}
		params.TermID = &t.ID
	}
	rows, err := h.a.DB.Q.PublishedList(r.Context(), params)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	fieldsFilter := splitFields(q.Get("fields"))
	ids := make([]uuid.UUID, 0, len(rows))
	var mids []uuid.UUID
	for _, row := range rows {
		ids = append(ids, row.ID)
		if id, err := uuid.Parse(content.ParseMeta(row.Meta).FeaturedImage); err == nil {
			mids = append(mids, id)
		}
	}
	terms := h.termsFor(r.Context(), ids)
	views, _ := h.a.Media.Lookup(r.Context(), mids)
	out := make([]map[string]any, 0, len(rows))
	var total int64
	for _, row := range rows {
		total = row.Total
		pr := dbq.PublishedBySlugRow{ID: row.ID, Type: row.Type, Slug: row.Slug, ParentID: row.ParentID, PublishedAt: row.PublishedAt}
		m := content.ParseMeta(row.Meta)
		e := entryJSON{ID: row.ID, Type: row.Type, Title: row.Title, Slug: row.Slug, URL: h.a.Renderer.EntryURL(r.Context(), &pr),
			Excerpt: m.Excerpt, PublishedAt: row.PublishedAt, UpdatedAt: row.RevisedAt, Terms: terms[row.ID], Fields: row.Fields}
		if row.AuthorName != "" {
			e.Author = &authorJSON{Name: row.AuthorName}
		}
		if id, err := uuid.Parse(m.FeaturedImage); err == nil {
			e.FeaturedImage = toMedia(views[id])
		}
		out = append(out, filterFields(e, fieldsFilter))
	}
	var next any
	if int64(offset+limit) < total {
		next = encodeCursor(offset + limit)
	}
	writeJSON(w, map[string]any{"data": out, "total": total, "next_cursor": next})
}

func (h *Handler) get(w http.ResponseWriter, r *http.Request) {
	typ := r.PathValue("type")
	if !h.publicType(w, typ) {
		return
	}
	ref := r.PathValue("ref")
	ctx := r.Context()
	var row *dbq.PublishedBySlugRow
	if id, err := uuid.Parse(ref); err == nil {
		x, err := h.a.DB.Q.PublishedByID(ctx, id)
		if err == nil && x.Type == typ {
			pr := dbq.PublishedBySlugRow(x)
			row = &pr
		}
	} else {
		rows, err := h.a.DB.Q.PublishedBySlug(ctx, dbq.PublishedBySlugParams{Type: typ, Locale: h.a.Site().Config.Locale, Slug: ref})
		if err == nil && len(rows) > 0 {
			row = &rows[0]
		}
	}
	if row == nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	m := content.ParseMeta(row.Meta)
	e := entryJSON{ID: row.ID, Type: row.Type, Title: row.Title, Slug: row.Slug, URL: h.a.Renderer.EntryURL(ctx, row),
		Excerpt: m.Excerpt, PublishedAt: row.PublishedAt, UpdatedAt: row.RevisedAt, Fields: row.Fields, Blocks: row.Blocks, SEO: &m.SEO,
		Terms: h.termsFor(ctx, []uuid.UUID{row.ID})[row.ID]}
	if row.AuthorName != "" {
		e.Author = &authorJSON{Name: row.AuthorName, Bio: row.AuthorBio}
	}
	if id, err := uuid.Parse(m.FeaturedImage); err == nil {
		views, _ := h.a.Media.Lookup(ctx, []uuid.UUID{id})
		e.FeaturedImage = toMedia(views[id])
	}
	if r.URL.Query().Get("render") == "html" {
		html, err := h.a.Renderer.EntryHTML(ctx, row)
		if err == nil {
			e.HTML = html
		}
	}
	writeJSON(w, map[string]any{"data": filterFields(e, splitFields(r.URL.Query().Get("fields")))})
}

func (h *Handler) termsFor(ctx context.Context, ids []uuid.UUID) map[uuid.UUID]map[string][]termJS {
	out := map[uuid.UUID]map[string][]termJS{}
	if len(ids) == 0 {
		return out
	}
	rows, err := h.a.DB.Q.TermsForEntries(ctx, ids)
	if err != nil {
		return out
	}
	for _, t := range rows {
		if out[t.EntryID] == nil {
			out[t.EntryID] = map[string][]termJS{}
		}
		out[t.EntryID][t.Taxonomy] = append(out[t.EntryID][t.Taxonomy], termJS{t.ID, t.Name, t.Slug})
	}
	return out
}

func (h *Handler) terms(w http.ResponseWriter, r *http.Request) {
	tax := r.PathValue("tax")
	if h.a.Site().Taxonomies[tax] == nil {
		writeErr(w, http.StatusNotFound, "unknown taxonomy")
		return
	}
	rows, err := h.a.Content.Terms(sys(r.Context()), tax)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "query failed")
		return
	}
	type t struct {
		ID          uuid.UUID  `json:"id"`
		Name        string     `json:"name"`
		Slug        string     `json:"slug"`
		Description string     `json:"description,omitempty"`
		ParentID    *uuid.UUID `json:"parent_id,omitempty"`
		Count       int64      `json:"count"`
	}
	out := make([]t, 0, len(rows))
	for _, x := range rows {
		out = append(out, t{x.ID, x.Name, x.Slug, x.Description, x.ParentID, x.PublishedCount})
	}
	writeJSON(w, map[string]any{"data": out})
}

func (h *Handler) menu(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := h.a.Menus.Locations()[name]; !ok {
		writeErr(w, http.StatusNotFound, "unknown menu")
		return
	}
	writeJSON(w, map[string]any{"data": h.a.Renderer.MenuItems(r.Context(), name, "/")})
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len(q) > 200 {
		writeErr(w, http.StatusBadRequest, "q is required (at most 200 characters)")
		return
	}
	var types []string
	for _, n := range h.a.Site().TypeOrder {
		if h.a.Site().Types[n].Searchable() {
			types = append(types, n)
		}
	}
	rows, err := h.a.DB.Q.PublishedSearch(r.Context(), dbq.PublishedSearchParams{Cfg: content.SearchConfig(h.a.Site().Config.Locale),
		Query: q, Types: types, Lim: 20})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "search failed")
		return
	}
	type hit struct {
		ID    uuid.UUID `json:"id"`
		Type  string    `json:"type"`
		Title string    `json:"title"`
		URL   string    `json:"url"`
		Rank  float32   `json:"rank"`
	}
	out := make([]hit, 0, len(rows))
	for _, x := range rows {
		pr := dbq.PublishedBySlugRow{ID: x.ID, Type: x.Type, Slug: x.Slug, ParentID: x.ParentID, PublishedAt: x.PublishedAt}
		out = append(out, hit{x.ID, x.Type, x.Title, h.a.Renderer.EntryURL(r.Context(), &pr), x.Rank})
	}
	writeJSON(w, map[string]any{"data": out})
}

func (h *Handler) siteInfo(w http.ResponseWriter, r *http.Request) {
	b := h.a.Site()
	writeJSON(w, map[string]any{"data": map[string]any{"name": h.a.Settings.SiteName(r.Context()), "description": b.Config.Description,
		"locale": b.Config.Locale, "url": h.a.Cfg.BaseURL}})
}

func encodeCursor(off int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("o:" + strconv.Itoa(off)))
}

func decodeCursor(c string) int {
	b, err := base64.RawURLEncoding.DecodeString(c)
	if err != nil {
		return 0
	}
	n, _ := strconv.Atoi(strings.TrimPrefix(string(b), "o:"))
	if n < 0 || n > 100000 {
		return 0
	}
	return n
}

func splitFields(s string) map[string]bool {
	if s == "" {
		return nil
	}
	m := map[string]bool{"id": true}
	for _, f := range strings.Split(s, ",") {
		m[strings.TrimSpace(f)] = true
	}
	return m
}

// filterFields keeps only the requested top-level keys (?fields=title,url).
func filterFields(e entryJSON, keep map[string]bool) map[string]any {
	b, _ := json.Marshal(e)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if keep == nil {
		return m
	}
	for k := range m {
		if !keep[k] {
			delete(m, k)
		}
	}
	return m
}
