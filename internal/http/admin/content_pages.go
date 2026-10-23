package admin

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/fields"
	"cms/internal/rbac"
	"cms/internal/site"
)

type dashboardData struct {
	Recent     []dbq.RecentEntriesRow
	Scheduled  []dbq.ScheduledEntriesRow
	Counts     []typeCount
	Findings   int
	Critical   int
	MissingAlt int64
	Unused     int64
}

type typeCount struct {
	Type      *site.ContentType
	Published int64
	Drafts    int64
	Scheduled int64
}

func (h *Handler) dashboard(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := h.a.DB.Q
	d := dashboardData{}
	d.Recent, _ = q.RecentEntries(ctx, 8)
	d.Scheduled, _ = q.ScheduledEntries(ctx, 5)
	counts, _ := q.CountEntriesByStatus(ctx)
	by := map[string]*typeCount{}
	for _, name := range h.a.Site().TypeOrder {
		tc := &typeCount{Type: h.a.Site().Types[name]}
		by[name] = tc
		d.Counts = append(d.Counts, *tc)
	}
	for _, c := range counts {
		tc := by[c.Type]
		if tc == nil {
			continue
		}
		switch c.Status {
		case "published":
			tc.Published += c.N
		case "draft":
			tc.Drafts += c.N
		case "scheduled":
			tc.Scheduled += c.N
		}
	}
	d.Counts = d.Counts[:0]
	for _, name := range h.a.Site().TypeOrder {
		d.Counts = append(d.Counts, *by[name])
	}
	if s := sessionFrom(ctx); rbac.Has(s.Role, rbac.SecuritySummary) {
		if fs, err := q.ListFindings(ctx, false); err == nil {
			for _, f := range fs {
				if f.AckUntil != nil && f.AckUntil.After(time.Now()) {
					continue
				}
				d.Findings++
				if f.Severity == "critical" || f.Severity == "high" {
					d.Critical++
				}
			}
		}
	}
	d.MissingAlt, _ = q.MediaMissingAlt(ctx)
	d.Unused, _ = q.UnusedMediaCount(ctx)
	h.render(w, r, "dashboard", http.StatusOK, h.page(r, "Dashboard", "dashboard", d))
}

type listData struct {
	Type   *site.ContentType
	Rows   []dbq.ListEntriesRow
	Total  int64
	Status string
	Query  string
	Page   int
	Pages  int
	Counts map[string]int64
}

func (h *Handler) contentList(w http.ResponseWriter, r *http.Request) {
	ct := h.a.Site().Types[r.PathValue("type")]
	if ct == nil {
		h.error(w, r, http.StatusNotFound, "Unknown content type.")
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	const per = 30
	status := r.URL.Query().Get("status")
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	sort := "updated"
	if ct.Hierarchical {
		sort = "menu_order"
	}
	rows, total, err := h.a.Content.List(r.Context(), content.ListFilter{Type: ct.Name, Status: status, Query: q, Sort: sort,
		Limit: per, Offset: int32((page - 1) * per)})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	counts := map[string]int64{}
	if all, err := h.a.DB.Q.CountEntriesByStatus(r.Context()); err == nil {
		for _, c := range all {
			if c.Type == ct.Name {
				counts[c.Status] = c.N
				if c.Status != "trashed" {
					counts["all"] += c.N
				}
			}
		}
	}
	pages := int((total + per - 1) / per)
	h.render(w, r, "content-list", http.StatusOK, h.page(r, ct.Label.Plural, "content",
		listData{Type: ct, Rows: rows, Total: total, Status: status, Query: q, Page: page, Pages: pages, Counts: counts}))
}

func (h *Handler) contentNew(w http.ResponseWriter, r *http.Request) {
	ct := h.a.Site().Types[r.PathValue("type")]
	if ct == nil {
		h.error(w, r, http.StatusNotFound, "Unknown content type.")
		return
	}
	in := content.CreateInput{Type: ct.Name, Title: r.PostFormValue("title")}
	if pid, err := uuid.Parse(r.PostFormValue("parent_id")); err == nil {
		in.ParentID = &pid
	}
	e, err := h.a.Content.Create(r.Context(), in)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/entries/"+e.ID.String(), http.StatusSeeOther)
}

// editorData is the JSON the editor script reads from a data block.
type editorData struct {
	Entry      editorEntry               `json:"entry"`
	Type       *site.ContentType         `json:"type"`
	Fields     []fields.Field            `json:"fields"`
	Groups     []editorGroup             `json:"groups"`
	Blocks     map[string]*site.BlockDef `json:"blocks"`
	Taxonomies []editorTaxonomy          `json:"taxonomies"`
	Parents    []editorOption            `json:"parents,omitempty"`
	Authors    []editorOption            `json:"authors"`
	Media      map[string]mediaJSON      `json:"media"`
	CSRF       string                    `json:"csrf"`
	CanPublish bool                      `json:"can_publish"`
	CanRawHTML bool                      `json:"can_raw_html"`
	LiveURL    string                    `json:"live_url"`
	SiteURL    string                    `json:"site_url"`
	Templates  []string                  `json:"templates"`
}

type editorEntry struct {
	ID               uuid.UUID       `json:"id"`
	Type             string          `json:"type"`
	Status           string          `json:"status"`
	Version          int32           `json:"version"`
	PublishedVersion *int32          `json:"published_version"`
	Title            string          `json:"title"`
	Slug             string          `json:"slug"`
	LiveSlug         string          `json:"live_slug"`
	Meta             content.Meta    `json:"meta"`
	Fields           json.RawMessage `json:"fields"`
	Blocks           json.RawMessage `json:"blocks"`
	AuthorID         *uuid.UUID      `json:"author_id"`
	PublishedAt      *time.Time      `json:"published_at"`
	ScheduledAt      *time.Time      `json:"scheduled_at"`
	UnpublishAt      *time.Time      `json:"unpublish_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type editorGroup struct {
	Name     string         `json:"name"`
	Label    string         `json:"label"`
	Position string         `json:"position"`
	Fields   []fields.Field `json:"fields"`
}

type editorTaxonomy struct {
	Name         string         `json:"name"`
	Label        site.Label     `json:"label"`
	Hierarchical bool           `json:"hierarchical"`
	Terms        []editorOption `json:"terms"`
}

type editorOption struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Parent string `json:"parent,omitempty"`
}

func (h *Handler) toEditorEntry(e *content.Entry) editorEntry {
	return editorEntry{ID: e.ID, Type: e.Type, Status: e.Status, Version: e.Version, PublishedVersion: e.PublishedVersion,
		Title: e.DraftTitle, Slug: e.DraftSlug, LiveSlug: e.Slug, Meta: e.Meta, Fields: e.DraftFields, Blocks: e.DraftBlocks,
		AuthorID: e.AuthorID, PublishedAt: e.PublishedAt, ScheduledAt: e.ScheduledAt, UnpublishAt: e.UnpublishAt, UpdatedAt: e.UpdatedAt}
}

func (h *Handler) editor(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	ctx := r.Context()
	e, err := h.a.Content.Get(ctx, id)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	b := h.a.Site()
	ct := b.Types[e.Type]
	if ct == nil {
		h.error(w, r, http.StatusNotFound, "This entry's content type no longer exists in the site bundle.")
		return
	}
	s := sessionFrom(ctx)
	d := editorData{Entry: h.toEditorEntry(e), Type: ct, Fields: ct.Fields, Blocks: map[string]*site.BlockDef{},
		CSRF: s.CSRF, CanPublish: rbac.Has(s.Role, rbac.ContentPublish), CanRawHTML: rbac.Has(s.Role, rbac.RawHTML),
		SiteURL: h.a.Cfg.BaseURL, Media: map[string]mediaJSON{}}
	for _, g := range b.GroupsFor(ct.Name) {
		d.Groups = append(d.Groups, editorGroup{Name: g.Name, Label: g.Label, Position: g.Position, Fields: g.Fields})
	}
	for name, def := range b.Blocks {
		if ct.AllowsBlock(name, def) || isChildSomewhere(b, name) {
			d.Blocks[name] = def
		}
	}
	for _, tx := range ct.Taxonomies {
		t := b.Taxonomies[tx]
		et := editorTaxonomy{Name: tx, Label: t.Label, Hierarchical: t.Hierarchical}
		terms, _ := h.a.Content.Terms(ctx, tx)
		for _, term := range terms {
			o := editorOption{ID: term.ID.String(), Label: term.Name}
			if term.ParentID != nil {
				o.Parent = term.ParentID.String()
			}
			et.Terms = append(et.Terms, o)
		}
		d.Taxonomies = append(d.Taxonomies, et)
	}
	if ct.Hierarchical {
		rows, _, _ := h.a.Content.List(ctx, content.ListFilter{Type: ct.Name, Limit: 200, Sort: "title"})
		for _, row := range rows {
			if row.ID != e.ID {
				d.Parents = append(d.Parents, editorOption{ID: row.ID.String(), Label: firstNonEmpty(row.DraftTitle, "(untitled)")})
			}
		}
	}
	if users, err := h.a.DB.Q.ListUsers(ctx); err == nil {
		for _, u := range users {
			if u.Status == "active" {
				d.Authors = append(d.Authors, editorOption{ID: u.ID.String(), Label: firstNonEmpty(u.DisplayName, u.Email)})
			}
		}
	}
	if e.IsPublished() {
		if u, err := h.a.Content.URLFor(ctx, &e.Entry); err == nil {
			d.LiveURL = u
		}
	}
	// Preload media used by the entry so previews show immediately.
	var ids []uuid.UUID
	collectMediaIDs(e, b, &ids)
	if views, err := h.a.Media.Lookup(ctx, ids); err == nil {
		for id, v := range views {
			d.Media[id.String()] = toMediaJSON(v)
		}
	}
	for _, name := range h.templateChoices(ct.Name) {
		d.Templates = append(d.Templates, name)
	}
	p := h.page(r, firstNonEmpty(e.DraftTitle, "Untitled"), "editor", d)
	h.render(w, r, "editor", http.StatusOK, p)
}

func isChildSomewhere(b *site.Bundle, name string) bool {
	for _, d := range b.Blocks {
		if d.Children != nil {
			for _, c := range d.Children.Allowed {
				if c == name {
					return true
				}
			}
		}
	}
	return false
}

// templateChoices lists custom page templates a theme offers for a type
// (templates named "<type>-<variant>" are selectable per entry).
func (h *Handler) templateChoices(typ string) []string {
	return nil
}

// saveRequest is the editor's JSON body.
type saveRequest struct {
	Version  int32           `json:"version"`
	Title    string          `json:"title"`
	Slug     string          `json:"slug"`
	Meta     content.Meta    `json:"meta"`
	Fields   json.RawMessage `json:"fields"`
	Blocks   json.RawMessage `json:"blocks"`
	Autosave bool            `json:"autosave"`
	AuthorID *uuid.UUID      `json:"author_id"`
}

func (h *Handler) entryID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found."})
		return id, false
	}
	return id, true
}

func (h *Handler) entrySave(w http.ResponseWriter, r *http.Request) {
	id, ok := h.entryID(w, r)
	if !ok {
		return
	}
	var req saveRequest
	if err := decodeJSON(r, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request."})
		return
	}
	e, err := h.a.Content.Save(r.Context(), content.SaveInput{ID: id, ExpectedVersion: req.Version, Title: req.Title, Slug: req.Slug,
		Meta: req.Meta, Fields: req.Fields, Blocks: req.Blocks, Autosave: req.Autosave})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if req.AuthorID != nil && (e.AuthorID == nil || *e.AuthorID != *req.AuthorID) {
		if err := h.a.Content.SetAuthor(r.Context(), id, req.AuthorID); err == nil {
			e.AuthorID = req.AuthorID
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": h.toEditorEntry(e)})
}

func (h *Handler) entryAction(w http.ResponseWriter, r *http.Request) {
	id, ok := h.entryID(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	var body struct {
		Version int32      `json:"version"`
		At      *time.Time `json:"at"`
	}
	_ = decodeJSON(r, &body)
	var e *content.Entry
	var err error
	switch r.PathValue("action") {
	case "publish":
		e, err = h.a.Content.Publish(ctx, id, body.Version)
	case "unpublish":
		err = h.a.Content.Unpublish(ctx, id)
	case "schedule":
		if body.At == nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "Choose a date and time."})
			return
		}
		err = h.a.Content.Schedule(ctx, id, *body.At, body.Version)
	case "unschedule":
		err = h.a.Content.Unschedule(ctx, id)
	case "unpublish-at":
		err = h.a.Content.SetUnpublishAt(ctx, id, body.At)
	case "discard":
		e, err = h.a.Content.Discard(ctx, id)
	case "trash":
		err = h.a.Content.Trash(ctx, id)
	case "restore":
		err = h.a.Content.Restore(ctx, id)
	case "delete":
		err = h.a.Content.Delete(ctx, id)
	case "clone":
		e, err = h.a.Content.Clone(ctx, id)
		if err == nil {
			writeJSON(w, http.StatusOK, map[string]any{"redirect": "/admin/entries/" + e.ID.String()})
			return
		}
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Unknown action."})
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if e == nil {
		e, err = h.a.Content.Get(ctx, id)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
	}
	res := map[string]any{"entry": h.toEditorEntry(e)}
	if e.IsPublished() {
		if u, err := h.a.Content.URLFor(ctx, &e.Entry); err == nil {
			res["live_url"] = u
		}
	}
	writeJSON(w, http.StatusOK, res)
}

// entryFormAction handles list-screen buttons (POST forms, then redirect back).
func (h *Handler) entryFormAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	ctx := r.Context()
	switch r.PathValue("action") {
	case "trash":
		err = h.a.Content.Trash(ctx, id)
	case "restore":
		err = h.a.Content.Restore(ctx, id)
	case "delete":
		err = h.a.Content.Delete(ctx, id)
	case "publish":
		_, err = h.a.Content.Publish(ctx, id, 0)
	case "unpublish":
		err = h.a.Content.Unpublish(ctx, id)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, refererPath(r), http.StatusSeeOther)
}

func (h *Handler) revisions(w http.ResponseWriter, r *http.Request) {
	id, ok := h.entryID(w, r)
	if !ok {
		return
	}
	rows, err := h.a.Content.Revisions(r.Context(), id, 100)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"revisions": rows})
}

func (h *Handler) revisionRestore(w http.ResponseWriter, r *http.Request) {
	id, ok := h.entryID(w, r)
	if !ok {
		return
	}
	rid, err := uuid.Parse(r.PathValue("rid"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Not found."})
		return
	}
	e, err := h.a.Content.RestoreRevision(r.Context(), id, rid)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": h.toEditorEntry(e)})
}

func (h *Handler) previewLink(w http.ResponseWriter, r *http.Request) {
	id, ok := h.entryID(w, r)
	if !ok {
		return
	}
	if _, err := h.a.Content.Get(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	ttl := 2 * time.Hour
	if r.URL.Query().Get("share") != "" {
		ttl = 7 * 24 * time.Hour
	}
	tok := h.a.PreviewToken(id, ttl)
	writeJSON(w, http.StatusOK, map[string]any{"url": "/preview/" + tok + "/", "expires_at": time.Now().Add(ttl)})
}

// entryPicker serves the entry/relationship field picker.
func (h *Handler) entryPicker(w http.ResponseWriter, r *http.Request) {
	types := r.URL.Query()["type"]
	if len(types) == 0 {
		types = h.a.Site().TypeOrder
	}
	rows, err := h.a.DB.Q.EntryPickerOptions(r.Context(), dbq.EntryPickerOptionsParams{Types: types, Q: strings.TrimSpace(r.URL.Query().Get("q"))})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
}

func (h *Handler) termCreateJSON(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Taxonomy string `json:"taxonomy"`
		Name     string `json:"name"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request."})
		return
	}
	t, err := h.a.Content.CreateTerm(r.Context(), content.TermInput{Taxonomy: body.Taxonomy, Name: body.Name})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": t.ID, "label": t.Name})
}

func collectMediaIDs(e *content.Entry, b *site.Bundle, ids *[]uuid.UUID) {
	add := func(s string) {
		if id, err := uuid.Parse(s); err == nil {
			*ids = append(*ids, id)
		}
	}
	add(e.Meta.FeaturedImage)
	add(e.Meta.SEO.OGImage)
	// Any uuid-looking string in fields or blocks may be a media id; Lookup
	// ignores ids that are not media.
	var walk func(v any)
	walk = func(v any) {
		switch t := v.(type) {
		case string:
			if len(t) == 36 {
				add(t)
			}
		case []any:
			for _, x := range t {
				walk(x)
			}
		case map[string]any:
			for _, x := range t {
				walk(x)
			}
		}
	}
	var f, bl any
	_ = json.Unmarshal(e.DraftFields, &f)
	_ = json.Unmarshal(e.DraftBlocks, &bl)
	walk(f)
	walk(bl)
	if len(*ids) > 500 {
		*ids = (*ids)[:500]
	}
}
