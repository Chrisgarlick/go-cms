// Package render turns published content into HTML, XML and text using the
// layered theme, writes cacheable output to disk for Caddy to serve, and keeps
// it fresh through dependency tracking (spec 4.12, 8.1).
package render

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"html/template"
	"io/fs"
	"log/slog"
	"mime"
	"net/url"
	pathpkg "path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"cms/internal/content"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/jobs"
	"cms/internal/media"
	"cms/internal/menus"
	"cms/internal/settings"
	"cms/internal/site"
)

type Renderer struct {
	DB       *db.DB
	Site     func() *site.Bundle
	Content  *content.Service
	Media    *media.Service
	Menus    *menus.Service
	Settings *settings.Service
	BaseFS   fs.FS  // embedded base theme
	BaseURL  string // https://example.com
	Disk     *Disk
	Log      *slog.Logger
	Version  string

	log     *slog.Logger
	themeMu sync.RWMutex
	th      *Theme
	themeOf string // bundle hash the theme was built from
}

// Result of rendering one path.
type Result struct {
	Status      int
	Body        []byte
	ContentType string
	Cacheable   bool
	Deps        []jobs.Dep
	Redirect    string // for front-page canonicalisation
}

var ErrNotFound = errors.New("not found")

func (r *Renderer) init() {
	if r.log == nil {
		r.log = r.Log
		if r.log == nil {
			r.log = slog.Default()
		}
	}
}

// theme returns the current theme, rebuilding it when the bundle changed.
func (r *Renderer) theme() *Theme {
	r.init()
	b := r.Site()
	r.themeMu.RLock()
	t, of := r.th, r.themeOf
	r.themeMu.RUnlock()
	if t != nil && of == b.Hash {
		return t
	}
	t, err := r.LoadTheme(b)
	if err != nil {
		r.log.Error("theme failed to load", "err", err)
		r.themeMu.RLock()
		defer r.themeMu.RUnlock()
		return r.th
	}
	return t
}

// LoadTheme (re)builds the theme for a bundle and installs it.
func (r *Renderer) LoadTheme(b *site.Bundle) (*Theme, error) {
	var siteTheme fs.FS
	if sub, err := fs.Sub(b.FS, "theme"); err == nil {
		if _, err := fs.Stat(b.FS, "theme"); err == nil {
			siteTheme = sub
		}
	}
	t, err := LoadTheme(r.BaseFS, siteTheme, b.FS, Funcs())
	if err != nil {
		return nil, err
	}
	r.themeMu.Lock()
	r.th, r.themeOf = t, b.Hash
	r.themeMu.Unlock()
	return t, nil
}

// ThemeHash identifies the current theme (part of cache keys).
func (r *Renderer) ThemeHash() string {
	if t := r.theme(); t != nil {
		return t.Hash
	}
	return ""
}

func (r *Renderer) newPage(ctx context.Context, kind, path string) *Page {
	b := r.Site()
	name := r.Settings.SiteName(ctx)
	lang := strings.SplitN(b.Config.Locale, "-", 2)[0]
	return &Page{
		Kind: kind, Path: path, Status: 200, Now: time.Now(),
		Site: SiteView{Name: name, Tagline: r.Settings.String(ctx, settings.SiteTagline, ""), Description: b.Config.Description,
			URL: strings.TrimRight(r.BaseURL, "/"), Locale: b.Config.Locale, Language: lang},
		r: r, ctx: ctx, deps: map[jobs.Dep]bool{}, mediaC: map[uuid.UUID]*media.View{},
	}
}

// route is the outcome of matching a path.
type route struct {
	kind    string
	ct      *site.ContentType
	tax     *site.Taxonomy
	term    *dbq.Term
	entry   *dbq.PublishedBySlugRow
	page    int
	sitemap string // "" | "index" | type name | "taxonomies"
}

// Resolve maps a request path to a route.
func (r *Renderer) resolve(ctx context.Context, path string) (*route, error) {
	b := r.Site()
	switch path {
	case "/robots.txt":
		return &route{kind: "robots"}, nil
	case "/sitemap.xml":
		return &route{kind: "sitemap", sitemap: "index"}, nil
	case "/feed.xml":
		if ct := r.homeType(ctx); ct != nil && ct.Feed {
			return &route{kind: "feed", ct: ct}, nil
		}
		return nil, ErrNotFound
	}
	if strings.HasPrefix(path, "/sitemap-") && strings.HasSuffix(path, ".xml") {
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/sitemap-"), ".xml")
		if name == "taxonomies" {
			return &route{kind: "sitemap", sitemap: name}, nil
		}
		if ct := b.Types[name]; ct != nil && ct.InSitemap() {
			return &route{kind: "sitemap", sitemap: name}, nil
		}
		return nil, ErrNotFound
	}
	if !strings.HasSuffix(path, "/") {
		return nil, ErrNotFound
	}
	base, page := splitPage(path)
	if page < 1 {
		return nil, ErrNotFound
	}
	if base == "/" {
		if page == 1 {
			if id := r.Settings.FrontPageID(ctx); id != uuid.Nil {
				row, err := r.DB.Q.PublishedByID(ctx, id)
				if err == nil {
					pr := dbq.PublishedBySlugRow(row)
					return &route{kind: "front", entry: &pr, ct: b.Types[row.Type]}, nil
				}
			}
		}
		return &route{kind: "home", page: page, ct: r.homeType(ctx)}, nil
	}
	// Archives (and their feeds).
	for _, name := range b.TypeOrder {
		ct := b.Types[name]
		if ct.Archive == "" || !ct.IsPublic() {
			continue
		}
		arch := strings.TrimRight(ct.Archive, "/") + "/"
		if base == arch {
			return &route{kind: "archive", ct: ct, page: page}, nil
		}
	}
	// Taxonomy terms.
	for _, tx := range b.Taxonomies {
		p := tx.Pattern()
		if p == nil {
			continue
		}
		vals, ok := p.Match(base)
		if !ok {
			continue
		}
		t, err := r.DB.Q.GetTermBySlug(ctx, dbq.GetTermBySlugParams{Taxonomy: tx.Name, Slug: vals["slug"]})
		if err != nil {
			continue
		}
		return &route{kind: "taxonomy", tax: tx, term: &t, page: page}, nil
	}
	if page > 1 {
		return nil, ErrNotFound
	}
	// Single entries: most specific patterns first.
	types := make([]*site.ContentType, 0, len(b.Types))
	for _, ct := range b.Types {
		if ct.Pattern() != nil && ct.IsPublic() {
			types = append(types, ct)
		}
	}
	sort.Slice(types, func(i, j int) bool {
		pi, pj := types[i].Pattern(), types[j].Pattern()
		if pi.Literals != pj.Literals {
			return pi.Literals > pj.Literals
		}
		if pi.HasPath != pj.HasPath {
			return !pi.HasPath
		}
		return types[i].Name < types[j].Name
	})
	for _, ct := range types {
		vals, ok := ct.Pattern().Match(base)
		if !ok {
			continue
		}
		rows, err := r.DB.Q.PublishedBySlug(ctx, dbq.PublishedBySlugParams{Type: ct.Name, Locale: b.Config.Locale, Slug: vals["slug"]})
		if err != nil {
			return nil, err
		}
		for i := range rows {
			row := rows[i]
			u, err := content.BuildURL(ctx, r.DB.Q, b, ct, row.Slug, row.ParentID, deref(row.PublishedAt))
			if err == nil && u == base {
				return &route{kind: "single", ct: ct, entry: &row}, nil
			}
		}
	}
	return nil, ErrNotFound
}

func splitPage(path string) (string, int) {
	i := strings.LastIndex(strings.TrimSuffix(path, "/"), "/page/")
	if i < 0 {
		return path, 1
	}
	n, err := strconv.Atoi(strings.TrimSuffix(path[i+len("/page/"):], "/"))
	if err != nil {
		return path, 1
	}
	if n < 2 {
		return path, 0 // /page/1/ is not canonical
	}
	return path[:i+1], n
}

func (r *Renderer) homeType(ctx context.Context) *site.ContentType {
	b := r.Site()
	if name := r.Settings.String(ctx, settings.PostsType, ""); name != "" {
		if ct := b.Types[name]; ct != nil {
			return ct
		}
	}
	if ct := b.Types["post"]; ct != nil {
		return ct
	}
	for _, name := range b.TypeOrder {
		if ct := b.Types[name]; ct.Archive != "" {
			return ct
		}
	}
	return nil
}

// Render produces the response for a public path.
func (r *Renderer) Render(ctx context.Context, path string) (*Result, error) {
	r.init()
	rt, err := r.resolve(ctx, path)
	if errors.Is(err, ErrNotFound) {
		if res := r.static(path); res != nil {
			return res, nil
		}
		return r.render404(ctx, path)
	}
	if err != nil {
		return nil, err
	}
	switch rt.kind {
	case "robots":
		return r.robots(ctx), nil
	case "sitemap":
		return r.sitemap(ctx, rt.sitemap)
	case "feed":
		return r.feed(ctx, rt.ct, path)
	}
	p := r.newPage(ctx, rt.kind, path)
	var candidates []string
	switch rt.kind {
	case "single", "front":
		ev := r.entryView(p, rt.entry)
		p.Entry, p.Type = ev, rt.ct
		p.dep("entry", ev.ID.String())
		p.Title = firstNonEmpty(ev.Meta.SEO.Title, ev.Title)
		p.Description = firstNonEmpty(ev.Meta.SEO.Description, ev.Excerpt())
		if ev.Meta.Uncached || (rt.ct != nil && !rt.ct.Cacheable()) {
			p.uncached = true
		}
		if rt.kind == "front" {
			candidates = append(candidates, "templates/front-page")
		}
		if ev.Meta.Template != "" {
			candidates = append(candidates, "templates/"+ev.Meta.Template)
		}
		candidates = append(candidates, "templates/single-"+ev.Type+"-"+ev.Slug, "templates/single-"+ev.Type, "templates/single")
	case "archive", "home":
		ct := rt.ct
		if ct == nil {
			p.Title = p.Site.Name
			candidates = []string{"templates/home", "templates/archive"}
			break
		}
		p.Type = ct
		if err := r.fillList(p, ct, nil, rt.page, ct.Archive); err != nil {
			return nil, err
		}
		if rt.page > p.Pagination.Pages && rt.page > 1 {
			return r.render404(ctx, path)
		}
		if rt.kind == "home" {
			p.Title = p.Site.Name
			if p.Pagination.BasePath == "" || rt.ct.Archive == "" {
				p.Pagination.BasePath = "/"
			}
			candidates = []string{"templates/home", "templates/archive-" + ct.Name, "templates/archive"}
		} else {
			p.Title = ct.Label.Plural
			candidates = []string{"templates/archive-" + ct.Name, "templates/archive"}
		}
	case "taxonomy":
		p.Taxonomy = rt.tax
		p.Term = r.termView(rt.term)
		p.dep("term", rt.term.ID.String())
		p.Title = rt.term.Name
		p.Description = rt.term.Description
		var ct *site.ContentType
		if len(rt.tax.Types) > 0 {
			ct = r.Site().Types[rt.tax.Types[0]]
		}
		if ct != nil {
			p.Type = ct
			if err := r.fillList(p, ct, &rt.term.ID, rt.page, strings.TrimSuffix(p.Term.URL, "/")); err != nil {
				return nil, err
			}
			if rt.page > p.Pagination.Pages && rt.page > 1 {
				return r.render404(ctx, path)
			}
		}
		candidates = []string{"templates/taxonomy-" + rt.tax.Name + "-" + rt.term.Slug, "templates/taxonomy-" + rt.tax.Name,
			"templates/taxonomy", "templates/archive"}
	}
	return r.execute(p, candidates)
}

func (r *Renderer) fillList(p *Page, ct *site.ContentType, termID *uuid.UUID, pageNum int, basePath string) error {
	p.dep("type", ct.Name)
	per := int32(ct.PerPage)
	sort := "newest"
	switch ct.Sort.Field {
	case "title":
		sort = "title"
	case "menu_order":
		sort = "menu_order"
	default:
		if ct.Sort.Order == "asc" {
			sort = "oldest"
		}
	}
	rows, err := r.DB.Q.PublishedList(p.ctx, dbq.PublishedListParams{Type: ct.Name, Locale: p.Site.Locale, TermID: termID,
		Sort: sort, Lim: per, Off: int32(pageNum-1) * per})
	if err != nil {
		return err
	}
	p.Entries = r.listViews(p, rows)
	var total int64
	if len(rows) > 0 {
		total = rows[0].Total
	}
	p.Total = total
	pages := int((total + int64(per) - 1) / int64(per))
	if pages < 1 {
		pages = 1
	}
	bp := strings.TrimRight(basePath, "/") + "/"
	pg := &Pagination{Page: pageNum, Pages: pages, Total: total, BasePath: bp, out: r.out}
	if pageNum > 1 {
		pg.PrevURL = pg.URL(pageNum - 1)
	}
	if pageNum < pages {
		pg.NextURL = pg.URL(pageNum + 1)
	}
	p.Pagination = pg
	return nil
}

func (r *Renderer) listViews(p *Page, rows []dbq.PublishedListRow) []*EntryView {
	b := r.Site()
	out := make([]*EntryView, 0, len(rows))
	var mediaIDs []uuid.UUID
	for _, row := range rows {
		ct := b.Types[row.Type]
		u := ""
		if ct != nil {
			u, _ = content.BuildURL(p.ctx, r.DB.Q, b, ct, row.Slug, row.ParentID, deref(row.PublishedAt))
		}
		m := content.ParseMeta(row.Meta)
		if id, err := uuid.Parse(m.FeaturedImage); err == nil {
			mediaIDs = append(mediaIDs, id)
		}
		out = append(out, &EntryView{ID: row.ID, Type: row.Type, Title: row.Title, Slug: row.Slug, URL: r.out(u),
			Date: deref(row.PublishedAt), Updated: row.RevisedAt, AuthorName: row.AuthorName, Meta: m, FieldsRaw: row.Fields, page: p})
	}
	p.preloadMedia(mediaIDs)
	p.loadTerms(out)
	return out
}

func (r *Renderer) entryView(p *Page, row *dbq.PublishedBySlugRow) *EntryView {
	b := r.Site()
	ct := b.Types[row.Type]
	u := ""
	if ct != nil {
		u, _ = r.Content.URLFor(p.ctx, &dbq.Entry{ID: row.ID, Type: row.Type, Slug: row.Slug, ParentID: row.ParentID, PublishedAt: row.PublishedAt})
	}
	return &EntryView{ID: row.ID, Type: row.Type, Title: row.Title, Slug: row.Slug, URL: r.out(u), Date: deref(row.PublishedAt),
		Updated: row.RevisedAt, AuthorName: row.AuthorName, AuthorBio: row.AuthorBio, Meta: content.ParseMeta(row.Meta),
		FieldsRaw: row.Fields, BlocksRaw: row.Blocks, page: p}
}

func (r *Renderer) termView(t *dbq.Term) *TermView {
	tv := &TermView{ID: t.ID, Taxonomy: t.Taxonomy, Name: t.Name, Slug: t.Slug, Description: t.Description, URL: r.out(r.termURL(t.Taxonomy, t.Slug))}
	_ = json.Unmarshal(t.Fields, &tv.Fields)
	return tv
}

// out converts an internal path to the form published in pages (site.yaml trailing_slash).
func (r *Renderer) out(p string) string { return r.Site().Config.Out(p) }

func (r *Renderer) termURL(tax, slug string) string {
	tx := r.Site().Taxonomies[tax]
	if tx == nil || tx.Pattern() == nil {
		return ""
	}
	return tx.Pattern().Build(map[string]string{"slug": slug, "path": slug})
}

// ogStatic finds a theme social image for a page path: og/<path>.png|jpg,
// then og/default.png.
func (r *Renderer) ogStatic(p string) string {
	t := r.Theme()
	if t == nil {
		return ""
	}
	key := strings.Trim(p, "/")
	if key == "" {
		key = "home"
	}
	for _, c := range []string{"og/" + key + ".png", "og/" + key + ".jpg", "og/default.png"} {
		if t.HasStatic(c) {
			return "/" + c
		}
	}
	return ""
}

// static serves a file from the theme's static/ folder (paths with an extension only).
func (r *Renderer) static(p string) *Result {
	ext := pathpkg.Ext(p)
	if ext == "" || strings.HasSuffix(p, "/") {
		return nil
	}
	t := r.Theme()
	if t == nil {
		return nil
	}
	body, ok := t.Static(p)
	if !ok {
		return nil
	}
	ct := mime.TypeByExtension(ext)
	if ct == "" {
		ct = "application/octet-stream"
	}
	return &Result{Status: 200, Body: body, ContentType: ct, Cacheable: true}
}

func (r *Renderer) render404(ctx context.Context, path string) (*Result, error) {
	p := r.newPage(ctx, "404", path)
	p.Status = 404
	p.Title = "Page not found"
	res, err := r.execute(p, []string{"templates/404"})
	if err != nil {
		return nil, err
	}
	res.Status = 404
	res.Cacheable = false
	return res, nil
}

// Search renders the dynamic search page (never cached).
func (r *Renderer) Search(ctx context.Context, q string, pageNum int) (*Result, error) {
	p := r.newPage(ctx, "search", "/search/")
	p.SearchQuery = strings.TrimSpace(q)
	if len(p.SearchQuery) > 200 {
		p.SearchQuery = p.SearchQuery[:200]
	}
	p.Title = "Search"
	if p.SearchQuery != "" {
		var types []string
		for _, name := range r.Site().TypeOrder {
			if r.Site().Types[name].Searchable() {
				types = append(types, name)
			}
		}
		if pageNum < 1 || pageNum > 50 {
			pageNum = 1
		}
		const per = 10
		rows, err := r.DB.Q.PublishedSearch(ctx, dbq.PublishedSearchParams{Cfg: content.SearchConfig(p.Site.Locale), Query: p.SearchQuery,
			Types: types, Lim: per, Off: int32((pageNum - 1) * per)})
		if err != nil {
			return nil, err
		}
		b := r.Site()
		for _, row := range rows {
			ct := b.Types[row.Type]
			u, _ := content.BuildURL(ctx, r.DB.Q, b, ct, row.Slug, row.ParentID, deref(row.PublishedAt))
			p.Entries = append(p.Entries, &EntryView{ID: row.ID, Type: row.Type, Title: row.Title, Slug: row.Slug, URL: r.out(u),
				Date: deref(row.PublishedAt), Meta: content.ParseMeta(row.Meta), page: p})
			p.Total = row.Total
		}
		pages := int((p.Total + per - 1) / per)
		pg := &Pagination{Page: pageNum, Pages: max(pages, 1), Total: p.Total, BasePath: "/search/"}
		qs := "?q=" + url.QueryEscape(p.SearchQuery)
		if pageNum > 1 {
			pg.PrevURL = fmt.Sprintf("/search/%s&page=%d", qs, pageNum-1)
		}
		if pageNum < pages {
			pg.NextURL = fmt.Sprintf("/search/%s&page=%d", qs, pageNum+1)
		}
		p.Pagination = pg
	}
	res, err := r.execute(p, []string{"templates/search", "templates/archive"})
	if err != nil {
		return nil, err
	}
	res.Cacheable = false
	return res, nil
}

// Preview renders an entry's working copy (signed preview links, live preview pane).
func (r *Renderer) Preview(ctx context.Context, e *content.Entry) (*Result, error) {
	r.init()
	ct := r.Site().Types[e.Type]
	if ct == nil {
		return nil, content.ErrUnknownType
	}
	p := r.newPage(ctx, "single", "/")
	p.preview = true
	date := deref(e.PublishedAt)
	if date.IsZero() {
		date = time.Now()
	}
	var parent *uuid.UUID
	if e.Meta.ParentID != "" {
		if id, err := uuid.Parse(e.Meta.ParentID); err == nil {
			parent = &id
		}
	}
	u, _ := content.BuildURL(ctx, r.DB.Q, r.Site(), ct, e.DraftSlug, parent, date)
	p.Path = u
	p.Entry = &EntryView{ID: e.ID, Type: e.Type, Title: e.DraftTitle, Slug: e.DraftSlug, URL: r.out(u), Date: date, Updated: e.UpdatedAt,
		Meta: e.Meta, FieldsRaw: e.DraftFields, BlocksRaw: e.DraftBlocks, page: p, terms: map[string][]*TermView{}}
	// Draft term assignments.
	var tids []uuid.UUID
	for _, list := range e.Meta.Terms {
		for _, s := range list {
			if id, err := uuid.Parse(s); err == nil {
				tids = append(tids, id)
			}
		}
	}
	if len(tids) > 0 {
		if ts, err := r.DB.Q.TermsByIDs(ctx, tids); err == nil {
			for i := range ts {
				p.Entry.terms[ts[i].Taxonomy] = append(p.Entry.terms[ts[i].Taxonomy], r.termView(&ts[i]))
			}
		}
	}
	if e.AuthorID != nil {
		if a, err := r.DB.Q.GetUser(ctx, *e.AuthorID); err == nil {
			p.Entry.AuthorName, p.Entry.AuthorBio = a.DisplayName, a.Bio
		}
	}
	p.Type = ct
	p.Title = firstNonEmpty(e.Meta.SEO.Title, e.DraftTitle)
	p.Description = firstNonEmpty(e.Meta.SEO.Description, p.Entry.Excerpt())
	cands := []string{}
	if e.Meta.Template != "" {
		cands = append(cands, "templates/"+e.Meta.Template)
	}
	cands = append(cands, "templates/single-"+e.Type+"-"+e.DraftSlug, "templates/single-"+e.Type, "templates/single")
	res, err := r.execute(p, cands)
	if err != nil {
		return nil, err
	}
	res.Cacheable = false
	return res, nil
}

func (r *Renderer) execute(p *Page, candidates []string) (*Result, error) {
	th := r.theme()
	if th == nil {
		return nil, errors.New("render: no theme loaded")
	}
	name, ok := th.First(candidates...)
	if !ok {
		return nil, fmt.Errorf("render: none of the templates %v exist", candidates)
	}
	t, err := th.Page(name)
	if err != nil {
		return nil, err
	}
	p.tmpl = t
	var buf bytes.Buffer
	buf.Grow(32 << 10)
	if err := t.ExecuteTemplate(&buf, "layout", p); err != nil {
		return nil, fmt.Errorf("render %s with %s: %w", p.Path, name, err)
	}
	return &Result{Status: p.Status, Body: buf.Bytes(), ContentType: "text/html; charset=utf-8",
		Cacheable: !p.uncached && !p.preview, Deps: p.Deps()}, nil
}

// head builds <title>, description, canonical, robots, Open Graph, Twitter and JSON-LD.
func (r *Renderer) head(p *Page) template.HTML {
	ctx := p.ctx
	var b strings.Builder
	title := p.Title
	full := p.Site.Name
	if title != "" && title != p.Site.Name {
		format := r.Settings.String(ctx, settings.TitleFormat, "{title} | {site}")
		full = strings.NewReplacer("{title}", title, "{site}", p.Site.Name).Replace(format)
		if p.Pagination != nil && p.Pagination.Page > 1 {
			full = fmt.Sprintf("%s (page %d)", full, p.Pagination.Page)
		}
	}
	esc := html.EscapeString
	fmt.Fprintf(&b, "<title>%s</title>\n", esc(full))
	desc := truncate(300, strings.Join(strings.Fields(firstNonEmpty(p.Description, p.Site.Tagline, p.Site.Description)), " "))
	if desc != "" {
		fmt.Fprintf(&b, `<meta name="description" content="%s">`+"\n", esc(desc))
	}
	canonical := p.Abs(r.out(p.Path))
	noindex := p.Status == 404 || p.Kind == "search" || p.preview || r.Settings.Bool(ctx, settings.DiscourageBots)
	if p.Entry != nil {
		if p.Entry.Meta.SEO.Canonical != "" {
			canonical = p.Abs(p.Entry.Meta.SEO.Canonical)
		}
		noindex = noindex || p.Entry.Meta.SEO.NoIndex
	}
	if p.Status == 200 && !p.preview {
		fmt.Fprintf(&b, `<link rel="canonical" href="%s">`+"\n", esc(canonical))
	}
	if noindex {
		b.WriteString(`<meta name="robots" content="noindex">` + "\n")
	}
	if p.Pagination != nil && p.Kind != "search" {
		if p.Pagination.PrevURL != "" {
			fmt.Fprintf(&b, `<link rel="prev" href="%s">`+"\n", esc(p.Abs(p.Pagination.PrevURL)))
		}
		if p.Pagination.NextURL != "" {
			fmt.Fprintf(&b, `<link rel="next" href="%s">`+"\n", esc(p.Abs(p.Pagination.NextURL)))
		}
	}
	ogType := "website"
	if p.Kind == "single" && p.Entry != nil && !p.Entry.Date.IsZero() && p.Type != nil && !p.Type.Hierarchical {
		ogType = "article"
	}
	fmt.Fprintf(&b, `<meta property="og:type" content="%s">`+"\n", ogType)
	fmt.Fprintf(&b, `<meta property="og:title" content="%s">`+"\n", esc(firstNonEmpty(title, p.Site.Name)))
	if desc != "" {
		fmt.Fprintf(&b, `<meta property="og:description" content="%s">`+"\n", esc(desc))
	}
	fmt.Fprintf(&b, `<meta property="og:url" content="%s">`+"\n", esc(canonical))
	fmt.Fprintf(&b, `<meta property="og:site_name" content="%s">`+"\n", esc(p.Site.Name))
	fmt.Fprintf(&b, `<meta property="og:locale" content="%s">`+"\n", esc(strings.ReplaceAll(p.Site.Locale, "-", "_")))
	var img *media.View
	if p.Entry != nil {
		if p.Entry.Meta.SEO.OGImage != "" {
			img = p.Media(p.Entry.Meta.SEO.OGImage)
		}
		if img == nil {
			img = p.Entry.Featured()
		}
	}
	card := "summary"
	if img == nil {
		// Convention: theme/static/og/<path>.png (og/home.png for "/") is the
		// page's social image when it has none of its own.
		if u := r.ogStatic(p.Path); u != "" {
			card = "summary_large_image"
			fmt.Fprintf(&b, `<meta property="og:image" content="%s">`+"\n", esc(p.Abs(u)))
		}
	}
	if img != nil {
		if v := img.Variant("large"); v != nil {
			card = "summary_large_image"
			fmt.Fprintf(&b, `<meta property="og:image" content="%s">`+"\n", esc(p.Abs(v.URL)))
			fmt.Fprintf(&b, `<meta property="og:image:width" content="%d"><meta property="og:image:height" content="%d">`+"\n", v.Width, v.Height)
			if img.Alt != "" {
				fmt.Fprintf(&b, `<meta property="og:image:alt" content="%s">`+"\n", esc(img.Alt))
			}
		}
	}
	fmt.Fprintf(&b, `<meta name="twitter:card" content="%s">`+"\n", card)
	if ogType == "article" {
		fmt.Fprintf(&b, `<meta property="article:published_time" content="%s">`+"\n", p.Entry.Date.UTC().Format(time.RFC3339))
		if p.Entry.HasUpdate() {
			fmt.Fprintf(&b, `<meta property="article:modified_time" content="%s">`+"\n", p.Entry.Updated.UTC().Format(time.RFC3339))
		}
	}
	// Feeds.
	if ct := r.homeType(ctx); ct != nil && ct.Feed {
		fmt.Fprintf(&b, `<link rel="alternate" type="application/rss+xml" title="%s" href="%s">`+"\n", esc(p.Site.Name), esc(p.Abs("/feed.xml")))
	}
	b.WriteString(string(r.jsonLD(p, canonical, img)))
	if r.Version != "" {
		fmt.Fprintf(&b, `<meta name="generator" content="cms">`+"\n")
	}
	return template.HTML(b.String())
}

func (r *Renderer) jsonLD(p *Page, canonical string, img *media.View) template.HTML {
	var graph []map[string]any
	site := map[string]any{"@type": "WebSite", "@id": p.Site.URL + "/#website", "url": p.Site.URL + "/", "name": p.Site.Name}
	if p.Path == "/" {
		site["potentialAction"] = map[string]any{"@type": "SearchAction",
			"target": p.Site.URL + "/search/?q={query}", "query-input": "required name=query"}
	}
	graph = append(graph, site)
	if p.Kind == "single" && p.Entry != nil && p.Type != nil && !p.Type.Hierarchical {
		art := map[string]any{"@type": "BlogPosting", "headline": truncate(110, p.Entry.Title), "url": canonical,
			"datePublished": p.Entry.Date.UTC().Format(time.RFC3339), "dateModified": p.Entry.Updated.UTC().Format(time.RFC3339),
			"isPartOf": map[string]any{"@id": p.Site.URL + "/#website"}, "mainEntityOfPage": canonical}
		if p.Entry.AuthorName != "" {
			art["author"] = map[string]any{"@type": "Person", "name": p.Entry.AuthorName}
		}
		if img != nil {
			if v := img.Variant("large"); v != nil {
				art["image"] = p.Abs(v.URL)
			}
		}
		graph = append(graph, art)
	}
	// Breadcrumbs for entries in an archive.
	if p.Kind == "single" && p.Entry != nil && p.Type != nil && p.Type.Archive != "" {
		graph = append(graph, map[string]any{"@type": "BreadcrumbList", "itemListElement": []any{
			map[string]any{"@type": "ListItem", "position": 1, "name": "Home", "item": p.Site.URL + "/"},
			map[string]any{"@type": "ListItem", "position": 2, "name": p.Type.Label.Plural, "item": p.Abs(r.out(strings.TrimRight(p.Type.Archive, "/") + "/"))},
			map[string]any{"@type": "ListItem", "position": 3, "name": p.Entry.Title, "item": canonical},
		}})
	}
	// FAQ markup from accordion blocks that opt in.
	if p.Entry != nil {
		var qa []any
		for _, faq := range faqItems(p.Entry.BlocksRaw) {
			qa = append(qa, map[string]any{"@type": "Question", "name": faq[0],
				"acceptedAnswer": map[string]any{"@type": "Answer", "text": faq[1]}})
		}
		if len(qa) > 0 {
			graph = append(graph, map[string]any{"@type": "FAQPage", "mainEntity": qa})
		}
	}
	data, err := json.Marshal(map[string]any{"@context": "https://schema.org", "@graph": graph})
	if err != nil {
		return ""
	}
	// json.Marshal escapes <, > and & as < etc., so the data cannot close the script element.
	return template.HTML(`<script type="application/ld+json">` + string(data) + "</script>\n")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// Asset returns a fingerprinted theme asset for "/assets/<hash>/<path>".
func (r *Renderer) Asset(urlPath string) ([]byte, string, bool) {
	th := r.theme()
	if th == nil {
		return nil, "", false
	}
	return th.OpenAsset(urlPath)
}

// Theme exposes the current theme (validation, CLI).
func (r *Renderer) Theme() *Theme { return r.theme() }

// EntryHTML renders a published entry's blocks to HTML (API "render=html").
func (r *Renderer) EntryHTML(ctx context.Context, row *dbq.PublishedBySlugRow) (string, error) {
	th := r.theme()
	if th == nil {
		return "", errors.New("render: no theme loaded")
	}
	name, ok := th.First("templates/single-"+row.Type, "templates/single")
	if !ok {
		return "", errors.New("render: no single template")
	}
	t, err := th.Page(name)
	if err != nil {
		return "", err
	}
	p := r.newPage(ctx, "single", "/")
	p.tmpl = t
	ev := r.entryView(p, row)
	p.Entry = ev
	return string(p.RenderBlocks(ev)), nil
}

// EntryURL returns the public URL path of a published row.
func (r *Renderer) EntryURL(ctx context.Context, row *dbq.PublishedBySlugRow) string {
	u, _ := r.Content.URLFor(ctx, &dbq.Entry{ID: row.ID, Type: row.Type, Slug: row.Slug, ParentID: row.ParentID, PublishedAt: row.PublishedAt})
	return r.out(u)
}

// MenuItems resolves a menu for the API.
func (r *Renderer) MenuItems(ctx context.Context, name, path string) []*MenuItem {
	p := r.newPage(ctx, "api", path)
	return p.Menu(name)
}
