package render

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"html/template"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/microcosm-cc/bluemonday"

	"cms/internal/blocks"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/jobs"
	"cms/internal/media"
	"cms/internal/menus"
	"cms/internal/richtext"
	"cms/internal/site"
)

// Page is the data every page template receives. Its methods fetch more data
// on demand and record what was read, so the page can be re-rendered when it
// changes (spec 4.12 dependency tracking).
type Page struct {
	Kind        string // single | archive | taxonomy | home | search | 404 | preview
	Path        string
	Site        SiteView
	Entry       *EntryView
	Entries     []*EntryView
	Pagination  *Pagination
	Type        *site.ContentType
	Taxonomy    *site.Taxonomy
	Term        *TermView
	SearchQuery string
	Total       int64
	Title       string // document title for this page (before the site suffix)
	Description string
	Status      int
	Now         time.Time

	r        *Renderer
	ctx      context.Context
	tmpl     *template.Template
	deps     map[jobs.Dep]bool
	depMu    sync.Mutex
	uncached bool
	nonce    string
	preview  bool
	mediaMu  sync.Mutex
	mediaC   map[uuid.UUID]*media.View
}

type SiteView struct {
	Name        string
	Tagline     string
	Description string
	URL         string // base URL without trailing slash
	Locale      string
	Language    string
}

type Pagination struct {
	Page     int
	Pages    int
	Total    int64
	PrevURL  string
	NextURL  string
	BasePath string
	out      func(string) string
}

// Numbers returns page numbers to show (with 0 as a gap marker).
func (p *Pagination) Numbers() []int {
	if p == nil || p.Pages <= 1 {
		return nil
	}
	var out []int
	for i := 1; i <= p.Pages; i++ {
		if i == 1 || i == p.Pages || (i >= p.Page-2 && i <= p.Page+2) {
			out = append(out, i)
		} else if len(out) > 0 && out[len(out)-1] != 0 {
			out = append(out, 0)
		}
	}
	return out
}

func (p *Pagination) URL(n int) string {
	u := p.BasePath
	if n > 1 {
		u = fmt.Sprintf("%spage/%d/", p.BasePath, n)
	}
	if p.out != nil {
		return p.out(u)
	}
	return u
}

// EntryView is one published entry (or a working copy in preview).
type EntryView struct {
	ID         uuid.UUID
	Type       string
	Title      string
	Slug       string
	URL        string
	Date       time.Time
	Updated    time.Time
	AuthorName string
	AuthorBio  string
	Meta       content.Meta
	FieldsRaw  json.RawMessage
	BlocksRaw  json.RawMessage
	terms      map[string][]*TermView
	fields     map[string]any
	page       *Page
}

func (e *EntryView) Fields() map[string]any {
	if e.fields == nil {
		e.fields = map[string]any{}
		_ = json.Unmarshal(e.FieldsRaw, &e.fields)
	}
	return e.fields
}

// Field returns one field value.
func (e *EntryView) Field(name string) any { return e.Fields()[name] }

// Excerpt is the manual excerpt, or the start of the text.
func (e *EntryView) Excerpt() string {
	if e.Meta.Excerpt != "" {
		return e.Meta.Excerpt
	}
	return truncate(160, e.PlainText())
}

// PlainText of the entry's rich-text blocks.
func (e *EntryView) PlainText() string {
	var parts []string
	blocks.Walk(blocks.Parse(e.BlocksRaw), func(b *blocks.Block) {
		var data map[string]json.RawMessage
		if json.Unmarshal(b.Data, &data) != nil {
			return
		}
		for _, k := range []string{"body", "text", "heading", "subheading", "answer"} {
			if raw, ok := data[k]; ok {
				var s string
				if json.Unmarshal(raw, &s) == nil {
					parts = append(parts, s)
				} else {
					parts = append(parts, richtext.PlainTextJSON(raw))
				}
			}
		}
	})
	return strings.Join(parts, " ")
}

// ReadingTime in minutes at 230 words per minute (minimum 1).
func (e *EntryView) ReadingTime() int {
	words := len(strings.Fields(e.PlainText()))
	return int(math.Max(1, math.Round(float64(words)/230)))
}

// Featured returns the featured image (nil if none).
func (e *EntryView) Featured() *media.View {
	if e.page == nil || e.Meta.FeaturedImage == "" {
		return nil
	}
	return e.page.Media(e.Meta.FeaturedImage)
}

// Image renders the featured image at a preset ("" if none).
func (e *EntryView) Image(preset string, attrs ...string) template.HTML {
	if e.page == nil || e.Meta.FeaturedImage == "" {
		return ""
	}
	return e.page.Img(e.Meta.FeaturedImage, preset, attrs...)
}

// Terms returns the entry's terms in a taxonomy.
func (e *EntryView) Terms(taxonomy string) []*TermView {
	if e.terms == nil && e.page != nil {
		e.page.loadTerms([]*EntryView{e})
	}
	return e.terms[taxonomy]
}

// HasUpdate reports a meaningful update after publication (> 1 day later).
// Blocks returns the entry's top-level blocks (type and data), for templates
// that lay a block out themselves, e.g. a home page reading its hero.
func (e *EntryView) Blocks() []*BlockView {
	var out []*BlockView
	for i, bl := range blocks.Parse(e.BlocksRaw) {
		data := map[string]any{}
		_ = json.Unmarshal(bl.Data, &data)
		out = append(out, &BlockView{ID: bl.ID, Type: bl.Type, Index: i, Data: data, Settings: bl.Settings, Entry: e, Page: e.page,
			children: bl.Children})
	}
	return out
}

// Refs follows an entry-reference field to the published entries it names.
func (e *EntryView) Refs(field string) []*EntryView {
	if e.page == nil {
		return nil
	}
	return e.page.Refs(e.Field(field))
}

func (e *EntryView) HasUpdate() bool { return e.Updated.Sub(e.Date) > 24*time.Hour }

type TermView struct {
	ID          uuid.UUID
	Taxonomy    string
	Name        string
	Slug        string
	Description string
	URL         string
	Count       int64
	Fields      map[string]any
}

// MenuItem is a resolved menu item.
type MenuItem struct {
	Label    string
	URL      string
	Target   string
	Rel      string
	Class    string
	Active   bool
	Current  bool
	Children []*MenuItem
}

func (p *Page) dep(t, id string) {
	p.depMu.Lock()
	p.deps[jobs.Dep{Type: t, ID: id}] = true
	p.depMu.Unlock()
}

// Deps returns everything this page read.
func (p *Page) Deps() []jobs.Dep {
	p.depMu.Lock()
	defer p.depMu.Unlock()
	out := make([]jobs.Dep, 0, len(p.deps))
	for d := range p.deps {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type+out[i].ID < out[j].Type+out[j].ID })
	return out
}

// Content renders the current entry's blocks.
func (p *Page) Content() template.HTML {
	if p.Entry == nil {
		return ""
	}
	return p.RenderBlocks(p.Entry)
}

// RenderBlocks renders any entry's block tree.
func (p *Page) RenderBlocks(e *EntryView) template.HTML {
	if e == nil {
		return ""
	}
	tree := blocks.Parse(e.BlocksRaw)
	var b strings.Builder
	p.renderTree(&b, tree, e, 0)
	return template.HTML(b.String())
}

// BlockView is the data each block template receives.
type BlockView struct {
	ID       string
	Type     string
	Index    int
	Data     map[string]any
	Settings blocks.Settings
	Entry    *EntryView
	Page     *Page
	children []blocks.Block
	depth    int
}

// Inner renders the block's children (columns and other containers).
func (bv *BlockView) Inner() template.HTML {
	var b strings.Builder
	bv.Page.renderTree(&b, bv.children, bv.Entry, bv.depth+1)
	return template.HTML(b.String())
}

// Children renders each child separately (for per-column wrappers).
func (bv *BlockView) Children() []template.HTML {
	out := make([]template.HTML, 0, len(bv.children))
	for _, c := range bv.children {
		var b strings.Builder
		bv.Page.renderTree(&b, []blocks.Block{c}, bv.Entry, bv.depth+1)
		out = append(out, template.HTML(b.String()))
	}
	return out
}

func (p *Page) renderTree(b *strings.Builder, tree []blocks.Block, e *EntryView, depth int) {
	now := p.Now
	for i, bl := range tree {
		s := bl.Settings
		if s.Hidden {
			continue
		}
		if s.From != "" {
			if t, err := time.Parse(time.RFC3339, s.From); err == nil && now.Before(t) {
				continue
			}
		}
		if s.Until != "" {
			if t, err := time.Parse(time.RFC3339, s.Until); err == nil && !now.Before(t) {
				continue
			}
		}
		if s.LoggedIn && !p.preview {
			p.uncached = true
			continue // members-only content is never in a cached page
		}
		data := map[string]any{}
		_ = json.Unmarshal(bl.Data, &data)
		bv := &BlockView{ID: bl.ID, Type: bl.Type, Index: i, Data: data, Settings: s, Entry: e, Page: p, children: bl.Children, depth: depth}
		name := "blocks/" + bl.Type
		t := p.tmpl.Lookup(name)
		if t == nil {
			if p.preview {
				fmt.Fprintf(b, `<div class="cms-missing-block">Missing template for block %q</div>`, html.EscapeString(bl.Type))
			}
			continue
		}
		wrapped := p.r.Site().Blocks[bl.Type].Wrapped()
		if wrapped {
			cls := "b b-" + bl.Type
			if s.Class != "" {
				cls += " " + s.Class
			}
			b.WriteString(`<div class="` + html.EscapeString(cls) + `"`)
			if s.Anchor != "" {
				b.WriteString(` id="` + html.EscapeString(s.Anchor) + `"`)
			}
			b.WriteString(">")
		}
		var buf bytes.Buffer
		if err := t.Execute(&buf, bv); err != nil {
			p.r.log.Error("block render failed", "block", bl.Type, "err", err)
			if p.preview {
				fmt.Fprintf(b, `<div class="cms-missing-block">Block %q failed: %s</div>`, html.EscapeString(bl.Type), html.EscapeString(err.Error()))
			}
		} else {
			b.Write(buf.Bytes())
		}
		if wrapped {
			b.WriteString("</div>")
		}
		b.WriteString("\n")
	}
}

// RichText renders a stored rich-text value (as returned by .Data or .Field).
func (p *Page) RichText(v any) template.HTML {
	if v == nil {
		return ""
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return richtext.RenderJSON(raw, richtext.Options{AnchorHeadings: true})
}

// TOC returns the headings of the current entry's rich-text blocks.
func (p *Page) TOC() []richtext.Heading {
	if p.Entry == nil {
		return nil
	}
	var out []richtext.Heading
	blocks.Walk(blocks.Parse(p.Entry.BlocksRaw), func(b *blocks.Block) {
		var data map[string]json.RawMessage
		if json.Unmarshal(b.Data, &data) != nil {
			return
		}
		if raw, ok := data["body"]; ok {
			if n, err := richtext.Parse(raw); err == nil {
				out = append(out, richtext.Headings(n)...)
			}
		}
	})
	return out
}

// Media returns a media item by id (string or uuid).
func (p *Page) Media(id any) *media.View {
	uid, ok := toUUID(id)
	if !ok {
		return nil
	}
	p.dep("media", uid.String())
	p.mediaMu.Lock()
	v, cached := p.mediaC[uid]
	p.mediaMu.Unlock()
	if cached {
		return v
	}
	m, err := p.r.Media.Lookup(p.ctx, []uuid.UUID{uid})
	if err != nil {
		return nil
	}
	p.mediaMu.Lock()
	p.mediaC[uid] = m[uid]
	p.mediaMu.Unlock()
	return m[uid]
}

// preloadMedia fetches many media items in one query.
func (p *Page) preloadMedia(ids []uuid.UUID) {
	if len(ids) == 0 {
		return
	}
	m, err := p.r.Media.Lookup(p.ctx, ids)
	if err != nil {
		return
	}
	p.mediaMu.Lock()
	for _, id := range ids {
		p.mediaC[id] = m[id]
	}
	p.mediaMu.Unlock()
}

// Img renders a responsive <img> for a media id and preset.
func (p *Page) Img(id any, preset string, attrs ...string) template.HTML {
	v := p.Media(id)
	if v == nil {
		return ""
	}
	return imgTag(v, preset, attrs...)
}

func imgTag(v *media.View, preset string, attrs ...string) template.HTML {
	extra := map[string]string{}
	for i := 0; i+1 < len(attrs); i += 2 {
		extra[attrs[i]] = attrs[i+1]
	}
	alt := v.Alt
	if a, ok := extra["alt"]; ok {
		alt = a
	}
	var b strings.Builder
	if !v.IsImage {
		if v.Mime == "image/svg+xml" {
			fmt.Fprintf(&b, `<img src="%s" alt="%s" loading="lazy" decoding="async">`, html.EscapeString(v.URL), html.EscapeString(alt))
			return template.HTML(b.String())
		}
		return ""
	}
	vv := v.Variant(preset)
	if vv == nil {
		return "" // variants not generated yet; the page is re-rendered when they are
	}
	fmt.Fprintf(&b, `<img src="%s" width="%d" height="%d" alt="%s"`, html.EscapeString(vv.URL), vv.Width, vv.Height, html.EscapeString(alt))
	if preset != "thumb" {
		if ss := v.Srcset(); ss != "" {
			sizes := extra["sizes"]
			if sizes == "" {
				sizes = "(min-width: 60rem) 60rem, 100vw"
			}
			fmt.Fprintf(&b, ` srcset="%s" sizes="%s"`, html.EscapeString(ss), html.EscapeString(sizes))
		}
	}
	if extra["loading"] == "eager" {
		b.WriteString(` fetchpriority="high"`)
	} else {
		b.WriteString(` loading="lazy"`)
	}
	b.WriteString(` decoding="async"`)
	if c := extra["class"]; c != "" {
		fmt.Fprintf(&b, ` class="%s"`, html.EscapeString(c))
	}
	b.WriteString(">")
	return template.HTML(b.String())
}

// Menu returns a resolved menu for a theme location.
func (p *Page) Menu(name string) []*MenuItem {
	p.dep("menu", name)
	m, err := p.r.Menus.Get(p.ctx, name)
	if err != nil {
		return nil
	}
	var entryIDs, termIDs []uuid.UUID
	var collect func([]menus.Item)
	collect = func(items []menus.Item) {
		for _, it := range items {
			if id, err := uuid.Parse(it.Ref); err == nil {
				if it.Type == "entry" {
					entryIDs = append(entryIDs, id)
				} else if it.Type == "term" {
					termIDs = append(termIDs, id)
				}
			}
			collect(it.Children)
		}
	}
	collect(m.Items)
	urls := map[string]string{}
	for _, id := range entryIDs {
		p.dep("entry", id.String())
		if e, err := p.r.DB.Q.GetEntry(p.ctx, id); err == nil && e.PublishedRevisionID != nil {
			if u, err := p.r.Content.URLFor(p.ctx, &e); err == nil {
				urls[id.String()] = p.r.out(u)
			}
		}
	}
	if len(termIDs) > 0 {
		if ts, err := p.r.DB.Q.TermsByIDs(p.ctx, termIDs); err == nil {
			for _, t := range ts {
				p.dep("term", t.ID.String())
				urls[t.ID.String()] = p.r.out(p.r.termURL(t.Taxonomy, t.Slug))
			}
		}
	}
	here := p.r.out(p.Path)
	var build func([]menus.Item) []*MenuItem
	build = func(items []menus.Item) []*MenuItem {
		var out []*MenuItem
		for _, it := range items {
			mi := &MenuItem{Label: it.Label, Target: it.Target, Rel: it.Rel, Class: it.Class}
			switch it.Type {
			case "url":
				mi.URL = it.URL
			case "entry", "term":
				mi.URL = urls[it.Ref]
				if mi.URL == "" {
					continue // unpublished or deleted target: hide the item
				}
			}
			mi.Children = build(it.Children)
			mi.Current = mi.URL != "" && mi.URL == here
			mi.Active = mi.Current || (mi.URL != "/" && mi.URL != "" && strings.HasPrefix(here, strings.TrimSuffix(mi.URL, "/")+"/"))
			for _, c := range mi.Children {
				if c.Active {
					mi.Active = true
				}
			}
			if mi.Target == "_blank" && mi.Rel == "" {
				mi.Rel = "noopener noreferrer"
			}
			out = append(out, mi)
		}
		return out
	}
	return build(m.Items)
}

// Query lists published entries: (dict "type" "post" "limit" 3 "term" "news" "taxonomy" "category" "order" "newest").
func (p *Page) Query(args map[string]any) []*EntryView {
	typ, _ := args["type"].(string)
	ct := p.r.Site().Types[typ]
	if ct == nil {
		return nil
	}
	p.dep("type", typ)
	limit := int32(10)
	switch v := args["limit"].(type) {
	case int:
		limit = int32(v)
	case float64:
		limit = int32(v)
	}
	if limit < 1 || limit > 100 {
		limit = 10
	}
	params := dbq.PublishedListParams{Type: typ, Locale: p.Site.Locale, Sort: "newest", Lim: limit}
	switch args["order"] {
	case "oldest":
		params.Sort = "oldest"
	case "title":
		params.Sort = "title"
	case "menu_order":
		params.Sort = "menu_order"
	}
	if tax, _ := args["taxonomy"].(string); tax != "" {
		if slug, _ := args["term"].(string); slug != "" {
			t, err := p.r.DB.Q.GetTermBySlug(p.ctx, dbq.GetTermBySlugParams{Taxonomy: tax, Slug: slug})
			if err != nil {
				return nil
			}
			params.TermID = &t.ID
			p.dep("term", t.ID.String())
		}
	}
	if ex, ok := args["exclude"].(*EntryView); ok && ex != nil {
		params.ExcludeID = &ex.ID
	}
	rows, err := p.r.DB.Q.PublishedList(p.ctx, params)
	if err != nil {
		p.r.log.Error("query block failed", "err", err)
		return nil
	}
	return p.r.listViews(p, rows)
}

// Related returns recent entries sharing a term with e (falls back to latest).
func (p *Page) Related(e *EntryView, limit int) []*EntryView {
	if e == nil {
		return nil
	}
	ct := p.r.Site().Types[e.Type]
	if ct == nil {
		return nil
	}
	if limit <= 0 || limit > 12 {
		limit = 3
	}
	for _, tax := range ct.Taxonomies {
		for _, t := range e.Terms(tax) {
			list := p.Query(map[string]any{"type": e.Type, "taxonomy": tax, "term": t.Slug, "limit": limit, "exclude": e})
			if len(list) > 0 {
				return list
			}
		}
	}
	return p.Query(map[string]any{"type": e.Type, "limit": limit, "exclude": e})
}

// Adjacent returns the previous and next entries by date.
func (p *Page) Adjacent() (prev, next *EntryView) {
	if p.Entry == nil || p.Entry.Date.IsZero() {
		return nil, nil
	}
	p.dep("type", p.Entry.Type)
	rows, err := p.r.DB.Q.AdjacentPublished(p.ctx, dbq.AdjacentPublishedParams{Type: p.Entry.Type, At: p.Entry.Date})
	if err != nil {
		return nil, nil
	}
	ct := p.r.Site().Types[p.Entry.Type]
	for _, r := range rows {
		u, _ := content.BuildURL(p.ctx, p.r.DB.Q, p.r.Site(), ct, r.Slug, r.ParentID, deref(r.PublishedAt))
		v := &EntryView{ID: r.ID, Type: p.Entry.Type, Title: r.Title, Slug: r.Slug, URL: p.r.out(u), Date: deref(r.PublishedAt), page: p}
		if r.Dir == "prev" {
			prev = v
		} else {
			next = v
		}
	}
	return prev, next
}

func (p *Page) Prev() *EntryView { prev, _ := p.Adjacent(); return prev }
func (p *Page) Next() *EntryView { _, next := p.Adjacent(); return next }

// TermsOf lists a taxonomy's terms with published counts.
func (p *Page) TermsOf(taxonomy string) []*TermView {
	if p.r.Site().Taxonomies[taxonomy] == nil {
		return nil
	}
	p.dep("taxonomy", taxonomy)
	rows, err := p.r.DB.Q.ListTerms(p.ctx, taxonomy)
	if err != nil {
		return nil
	}
	var out []*TermView
	for _, t := range rows {
		if t.PublishedCount == 0 {
			continue
		}
		out = append(out, &TermView{ID: t.ID, Taxonomy: t.Taxonomy, Name: t.Name, Slug: t.Slug,
			Description: t.Description, URL: p.r.out(p.r.termURL(t.Taxonomy, t.Slug)), Count: t.PublishedCount})
	}
	return out
}

// Option reads one value from an options page.
func (p *Page) Option(page, field string) any {
	p.dep("options", page)
	var m map[string]any
	_ = json.Unmarshal(p.r.Settings.Options(p.ctx, page), &m)
	return m[field]
}

// Setting reads a theme setting.
func (p *Page) Setting(name string) any {
	p.dep("settings", "theme")
	var m map[string]any
	_ = json.Unmarshal(p.r.Settings.Raw(p.ctx, "theme.settings"), &m)
	return m[name]
}

// Refs resolves an entry-reference field value (one ID or a list of IDs)
// to the published entries, in the order given. Unpublished ones are skipped.
func (p *Page) Refs(v any) []*EntryView {
	var ids []string
	switch t := v.(type) {
	case string:
		ids = []string{t}
	case []any:
		for _, x := range t {
			if s, ok := x.(string); ok {
				ids = append(ids, s)
			}
		}
	case []string:
		ids = t
	}
	var out []*EntryView
	for _, s := range ids {
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		p.dep("entry", id.String())
		row, err := p.r.DB.Q.PublishedByID(p.ctx, id)
		if err != nil {
			continue
		}
		pr := dbq.PublishedBySlugRow(row)
		out = append(out, p.r.entryView(p, &pr))
	}
	p.loadTerms(out)
	return out
}

// EntryBySlug returns one published entry of a type, or nil.
func (p *Page) EntryBySlug(typ, slug string) *EntryView {
	p.dep("type", typ)
	rows, err := p.r.DB.Q.PublishedBySlug(p.ctx, dbq.PublishedBySlugParams{Type: typ, Locale: p.Site.Locale, Slug: slug})
	if err != nil || len(rows) == 0 {
		return nil
	}
	v := p.r.entryView(p, &rows[0])
	p.loadTerms([]*EntryView{v})
	return v
}

// Form returns a form definition (forms/<name>.yaml) so a template can render
// its fields; nil if the site has no such form.
func (p *Page) Form(name string) *site.Form { return p.r.Site().Forms[name] }

// Asset returns the fingerprinted URL of a theme asset.
func (p *Page) Asset(path string) string {
	if u, ok := p.r.theme().AssetPath(path); ok {
		return u
	}
	p.r.log.Warn("theme asset not found", "path", path)
	return ""
}

// Abs makes an absolute URL from a path.
func (p *Page) Abs(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	return p.Site.URL + "/" + strings.TrimPrefix(path, "/")
}

// Nonce is the CSP nonce for inline scripts (dynamic pages only).
func (p *Page) Nonce() string { return p.nonce }

// IsPreview reports a working-copy preview.
func (p *Page) IsPreview() bool { return p.preview }

// HTML sanitises owner-supplied raw HTML (the html block) before output.
func (p *Page) HTML(v any) template.HTML {
	s, _ := v.(string)
	return template.HTML(htmlPolicy.Sanitize(s))
}

var htmlPolicy = func() *bluemonday.Policy {
	pol := bluemonday.UGCPolicy()
	pol.AllowAttrs("class").Globally()
	pol.AllowElements("figure", "figcaption", "section", "aside", "details", "summary", "mark", "small")
	return pol
}()

// Embed renders a click-to-load facade for an allow-listed provider.
func (p *Page) Embed(url, title string) template.HTML { return embedFacade(url, title) }

// Head renders the SEO and social meta tags (spec 4.10).
func (p *Page) Head() template.HTML { return p.r.head(p) }

func (p *Page) loadTerms(list []*EntryView) {
	ids := make([]uuid.UUID, 0, len(list))
	byID := map[uuid.UUID]*EntryView{}
	for _, e := range list {
		if e.terms == nil {
			e.terms = map[string][]*TermView{}
			ids = append(ids, e.ID)
			byID[e.ID] = e
		}
	}
	if len(ids) == 0 {
		return
	}
	rows, err := p.r.DB.Q.TermsForEntries(p.ctx, ids)
	if err != nil {
		return
	}
	for _, r := range rows {
		e := byID[r.EntryID]
		if e == nil {
			continue
		}
		p.dep("term", r.ID.String())
		e.terms[r.Taxonomy] = append(e.terms[r.Taxonomy], &TermView{ID: r.ID, Taxonomy: r.Taxonomy, Name: r.Name, Slug: r.Slug,
			URL: p.r.out(p.r.termURL(r.Taxonomy, r.Slug))})
	}
}

func toUUID(v any) (uuid.UUID, bool) {
	switch t := v.(type) {
	case uuid.UUID:
		return t, t != uuid.Nil
	case string:
		id, err := uuid.Parse(t)
		return id, err == nil
	}
	return uuid.Nil, false
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}
