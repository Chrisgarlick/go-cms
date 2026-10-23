// Package content implements entries, working copies, publishing, revisions
// and taxonomies (spec 4.1). Editors change only the working copy; visitors
// see only the published revision.
package content

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/blocks"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/events"
	"cms/internal/fields"
	"cms/internal/jobs"
	"cms/internal/rbac"
	"cms/internal/richtext"
	"cms/internal/site"
)

var (
	ErrNotFound    = errors.New("entry not found")
	ErrTrashed     = errors.New("entry is in the trash")
	ErrUnknownType = errors.New("unknown content type")
)

// ErrConflict means someone else saved since the editor loaded the entry.
type ErrConflict struct {
	Current  int32
	Expected int32
}

func (e *ErrConflict) Error() string {
	return fmt.Sprintf("this entry was changed by someone else (version %d, you have %d)", e.Current, e.Expected)
}

// Meta is the non-field working-copy data stored in draft_meta / revision meta.
type Meta struct {
	Excerpt       string              `json:"excerpt,omitempty"`
	FeaturedImage string              `json:"featured_image,omitempty"`
	ParentID      string              `json:"parent_id,omitempty"`
	Terms         map[string][]string `json:"terms,omitempty"`
	SEO           SEO                 `json:"seo"`
	Template      string              `json:"template,omitempty"`
	Uncached      bool                `json:"uncached,omitempty"` // derived: logged-in blocks etc.
}

type SEO struct {
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Canonical   string `json:"canonical,omitempty"`
	NoIndex     bool   `json:"noindex,omitempty"`
	OGImage     string `json:"og_image,omitempty"`
}

func ParseMeta(raw json.RawMessage) Meta {
	var m Meta
	_ = json.Unmarshal(raw, &m)
	return m
}

// Entry is the admin view of one entry (working copy plus status).
type Entry struct {
	dbq.Entry
	Meta Meta
}

// HasUnpublishedChanges reports a working copy ahead of the published revision.
func (e *Entry) HasUnpublishedChanges() bool {
	return e.PublishedVersion != nil && *e.PublishedVersion != e.Version
}

func (e *Entry) IsPublished() bool { return e.PublishedRevisionID != nil }

type Service struct {
	DB   *db.DB
	Site func() *site.Bundle
	Jobs jobs.Enqueuer
	// FrontPage returns the entry id shown at "/", if any (settings).
	FrontPage func(ctx context.Context) uuid.UUID
	Now       func() time.Time
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) typeOf(name string) (*site.ContentType, error) {
	t := s.Site().Types[name]
	if t == nil {
		return nil, fmt.Errorf("%w %q", ErrUnknownType, name)
	}
	return t, nil
}

// CreateInput starts a new draft.
type CreateInput struct {
	Type     string
	Title    string
	Slug     string
	ParentID *uuid.UUID
	Locale   string
}

func (s *Service) Create(ctx context.Context, in CreateInput) (*Entry, error) {
	a, err := actor.Require(ctx, rbac.ContentEdit)
	if err != nil {
		return nil, err
	}
	ct, err := s.typeOf(in.Type)
	if err != nil {
		return nil, err
	}
	locale := in.Locale
	if locale == "" {
		locale = s.Site().Config.Locale
	}
	title := strings.TrimSpace(in.Title)
	id := uuid.Must(uuid.NewV7())
	meta := Meta{}
	if in.ParentID != nil && ct.Hierarchical {
		meta.ParentID = in.ParentID.String()
	}
	metaJSON, _ := json.Marshal(meta)
	var author *uuid.UUID
	if a.IsUser() {
		u := a.UserID
		author = &u
	}
	var out *Entry
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		slug, err := s.uniqueDraftSlug(ctx, q, ct, locale, nil, id, firstNonEmpty(in.Slug, title, "untitled"))
		if err != nil {
			return err
		}
		// Live slug starts as a placeholder unique to this entry; it becomes the
		// real slug on first publish.
		e, err := q.CreateEntry(ctx, dbq.CreateEntryParams{
			ID: id, Type: ct.Name, Slug: "~" + id.String(), ParentID: nil, AuthorID: author, Locale: locale,
			DraftTitle: title, DraftSlug: slug, DraftMeta: metaJSON,
			DraftFields: json.RawMessage("{}"), DraftBlocks: blocks.Defaults(ct),
		})
		if err != nil {
			return err
		}
		out = &Entry{Entry: e, Meta: meta}
		return audit.Record(ctx, q, audit.Event{Action: "entry.create", ResourceType: "entry", ResourceID: id.String(),
			Diff: map[string]any{"type": ct.Name}})
	})
	return out, err
}

func (s *Service) Get(ctx context.Context, id uuid.UUID) (*Entry, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, err
	}
	e, err := s.DB.Q.GetEntry(ctx, id)
	if db.IsNotFound(err) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &Entry{Entry: e, Meta: ParseMeta(e.DraftMeta)}, nil
}

// SaveInput is a working-copy write from the editor, API or CLI.
type SaveInput struct {
	ID              uuid.UUID
	ExpectedVersion int32
	Title           string
	Slug            string
	Meta            Meta
	Fields          json.RawMessage
	Blocks          json.RawMessage
	// Autosave skips required checks and does not create a revision.
	Autosave bool
}

// Save writes the working copy. It never changes the live site.
func (s *Service) Save(ctx context.Context, in SaveInput) (*Entry, error) {
	a, err := actor.Require(ctx, rbac.ContentEdit)
	if err != nil {
		return nil, err
	}
	var out *Entry
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := s.lock(ctx, q, in.ID)
		if err != nil {
			return err
		}
		if cur.Version != in.ExpectedVersion {
			return &ErrConflict{Current: cur.Version, Expected: in.ExpectedVersion}
		}
		ct, err := s.typeOf(cur.Type)
		if err != nil {
			return err
		}
		wc, err := s.clean(ctx, q, a, ct, cur, in, in.Autosave)
		if err != nil {
			return err
		}
		e, err := q.SaveWorkingCopy(ctx, dbq.SaveWorkingCopyParams{
			ID: cur.ID, DraftTitle: wc.title, DraftSlug: wc.slug, DraftMeta: wc.meta,
			DraftFields: wc.fields, DraftBlocks: wc.blocks, ExpectedVersion: in.ExpectedVersion,
		})
		if db.IsNotFound(err) {
			return &ErrConflict{Current: cur.Version + 1, Expected: in.ExpectedVersion}
		}
		if err != nil {
			return err
		}
		if !in.Autosave {
			if ct.Has("revisions") {
				if _, err := s.revision(ctx, q, a, &e, "save"); err != nil {
					return err
				}
			}
			if err := audit.Record(ctx, q, audit.Event{Action: "entry.save", ResourceType: "entry",
				ResourceID: e.ID.String(), Diff: map[string]any{"version": e.Version}}); err != nil {
				return err
			}
		}
		out = &Entry{Entry: e, Meta: ParseMeta(e.DraftMeta)}
		return nil
	})
	return out, err
}

type workingCopy struct {
	title, slug          string
	meta, fields, blocks json.RawMessage
	refs                 *fields.Refs
	blockText            []string
	metaV                Meta
}

// clean validates and normalises a working copy.
func (s *Service) clean(ctx context.Context, q *dbq.Queries, a *actor.Actor, ct *site.ContentType,
	cur dbq.Entry, in SaveInput, partial bool) (*workingCopy, error) {
	b := s.Site()
	fc := fields.Context{CanUse: func(c string) bool { return a.Can(rbac.Cap(c)) }, Partial: partial}
	errs := fields.Errors{}

	title := strings.TrimSpace(in.Title)
	if ct.Has("title") && title == "" && !partial {
		errs["title"] = "is required"
	}
	if len([]rune(title)) > 300 {
		errs["title"] = "must be at most 300 characters"
	}

	// Capability-gated values the actor may not edit are carried over unchanged.
	inFields, inBlocks := in.Fields, in.Blocks
	if !a.Can(rbac.RawHTML) {
		inFields = preserveGatedFields(b.FieldsFor(ct.Name), cur.DraftFields, inFields)
		inBlocks = preserveGatedBlocks(b, cur.DraftBlocks, inBlocks)
	}

	fieldsJSON, refs, err := fields.Validate(b.FieldsFor(ct.Name), inFields, fc)
	if err != nil {
		mergeErrs(errs, err, "fields.")
	}
	if refs == nil {
		refs = &fields.Refs{Media: map[string]string{}}
	}
	blocksJSON := json.RawMessage("[]")
	var blockText []string
	uncached := false
	if ct.Has("blocks") {
		res, err := blocks.Validate(b, ct, inBlocks, fc)
		if err != nil {
			mergeErrs(errs, err, "")
		}
		if res != nil {
			blocksJSON, _ = json.Marshal(res.Blocks)
			for k, v := range res.Refs.Media {
				refs.Media[k] = v
			}
			refs.Entries = append(refs.Entries, res.Refs.Entries...)
			refs.Terms = append(refs.Terms, res.Refs.Terms...)
			blockText = res.Text
			uncached = res.Uncached
		}
	}

	m := in.Meta
	m.Excerpt = strings.TrimSpace(m.Excerpt)
	if len([]rune(m.Excerpt)) > 1000 {
		errs["meta.excerpt"] = "must be at most 1000 characters"
	}
	if m.FeaturedImage != "" {
		if _, err := uuid.Parse(m.FeaturedImage); err != nil {
			errs["meta.featured_image"] = "is not a valid image"
		} else {
			refs.Media[m.FeaturedImage] = "featured_image"
		}
	}
	if m.SEO.OGImage != "" {
		if _, err := uuid.Parse(m.SEO.OGImage); err != nil {
			errs["meta.seo.og_image"] = "is not a valid image"
		} else {
			refs.Media[m.SEO.OGImage] = "seo.og_image"
		}
	}
	if m.SEO.Canonical != "" {
		if u, ok := richtext.SafeURL(m.SEO.Canonical); ok {
			m.SEO.Canonical = u
		} else {
			errs["meta.seo.canonical"] = "must be a URL"
		}
	}
	m.SEO.Title = strings.TrimSpace(m.SEO.Title)
	m.SEO.Description = strings.TrimSpace(m.SEO.Description)
	if m.ParentID != "" {
		pid, err := uuid.Parse(m.ParentID)
		switch {
		case !ct.Hierarchical:
			m.ParentID = ""
		case err != nil || pid == cur.ID:
			errs["meta.parent_id"] = "is not a valid parent"
		default:
			anc, err := q.EntryAncestors(ctx, pid)
			if err != nil {
				return nil, err
			}
			for _, x := range anc {
				if x.ID == cur.ID {
					errs["meta.parent_id"] = "would create a loop"
				}
			}
			p, err := q.GetEntry(ctx, pid)
			if err != nil || p.Type != ct.Name {
				errs["meta.parent_id"] = "must be an entry of the same type"
			}
		}
	}
	// Terms: only taxonomies attached to the type, ids must exist in that taxonomy.
	if len(m.Terms) > 0 {
		clean := map[string][]string{}
		var ids []uuid.UUID
		for tax, list := range m.Terms {
			if !containsStr(ct.Taxonomies, tax) {
				continue
			}
			for _, t := range list {
				id, err := uuid.Parse(t)
				if err != nil {
					errs["meta.terms."+tax] = "contains an invalid term"
					continue
				}
				ids = append(ids, id)
				clean[tax] = append(clean[tax], id.String())
			}
		}
		if len(ids) > 0 {
			found, err := q.TermsByIDs(ctx, ids)
			if err != nil {
				return nil, err
			}
			byID := map[string]string{}
			for _, t := range found {
				byID[t.ID.String()] = t.Taxonomy
			}
			// Drop ids of deleted terms (or terms of another taxonomy) silently:
			// a term deleted elsewhere must not block publishing.
			for tax, list := range clean {
				kept := list[:0]
				for _, id := range list {
					if byID[id] == tax {
						kept = append(kept, id)
					}
				}
				clean[tax] = kept
			}
		}
		m.Terms = clean
	}
	if m.Template != "" && !validTemplateName(m.Template) {
		errs["meta.template"] = "is not a valid template name"
	}
	m.Uncached = uncached

	slug := strings.TrimSpace(in.Slug)
	if slug == "" {
		slug = richtext.Slugify(title)
	} else {
		slug = richtext.Slugify(slug)
	}
	if slug == "" {
		slug = "untitled"
	}
	if len(errs) > 0 {
		return nil, errs
	}
	var parent *uuid.UUID
	if m.ParentID != "" {
		p := uuid.MustParse(m.ParentID)
		parent = &p
	}
	slug, err = s.uniqueDraftSlug(ctx, q, ct, cur.Locale, parent, cur.ID, slug)
	if err != nil {
		return nil, err
	}
	metaJSON, _ := json.Marshal(m)
	return &workingCopy{title: title, slug: slug, meta: metaJSON, fields: fieldsJSON, blocks: blocksJSON,
		refs: refs, blockText: blockText, metaV: m}, nil
}

func validTemplateName(s string) bool {
	if len(s) > 64 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func mergeErrs(dst fields.Errors, err error, prefix string) {
	var fe fields.Errors
	if errors.As(err, &fe) {
		for k, v := range fe {
			dst[prefix+k] = v
		}
		return
	}
	dst[strings.TrimSuffix(prefix, ".")] = err.Error()
}

// uniqueDraftSlug appends -2, -3 ... until no other entry uses the slug.
func (s *Service) uniqueDraftSlug(ctx context.Context, q *dbq.Queries, ct *site.ContentType, locale string,
	parent *uuid.UUID, id uuid.UUID, want string) (string, error) {
	base := richtext.Slugify(want)
	if base == "" {
		base = "untitled"
	}
	slug := base
	for i := 2; i < 1000; i++ {
		taken, err := q.DraftSlugTaken(ctx, dbq.DraftSlugTakenParams{Type: ct.Name, Locale: locale, Slug: slug, ID: id, ParentID: parent})
		if err != nil {
			return "", err
		}
		if !taken && !reservedSlug(ct, slug) {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
	return "", errors.New("could not find a free slug")
}

// reservedSlug blocks slugs that would shadow system routes for root-level types.
func reservedSlug(ct *site.ContentType, slug string) bool {
	p := ct.Pattern()
	if p == nil || p.Literals > 0 {
		return false
	}
	switch slug {
	case "admin", "api", "media", "search", "preview", "assets", "feed", "sitemap", "robots", "_cms", "healthz", "readyz", "metrics":
		return true
	}
	return false
}

func (s *Service) lock(ctx context.Context, q *dbq.Queries, id uuid.UUID) (dbq.Entry, error) {
	e, err := q.GetEntryForUpdate(ctx, id)
	if db.IsNotFound(err) {
		return e, ErrNotFound
	}
	if err != nil {
		return e, err
	}
	if e.Status == "trashed" {
		return e, ErrTrashed
	}
	return e, nil
}

func (s *Service) revision(ctx context.Context, q *dbq.Queries, a *actor.Actor, e *dbq.Entry, kind string) (dbq.EntryRevision, error) {
	n, err := q.NextRevisionNumber(ctx, e.ID)
	if err != nil {
		return dbq.EntryRevision{}, err
	}
	var author *uuid.UUID
	if a.IsUser() {
		u := a.UserID
		author = &u
	}
	return q.CreateRevision(ctx, dbq.CreateRevisionParams{
		ID: uuid.Must(uuid.NewV7()), EntryID: e.ID, Number: n, Kind: kind, AuthorID: author,
		Title: e.DraftTitle, Slug: e.DraftSlug, Meta: e.DraftMeta, Fields: e.DraftFields, Blocks: e.DraftBlocks,
	})
}

// Publish validates the working copy fully and makes it live.
func (s *Service) Publish(ctx context.Context, id uuid.UUID, expectedVersion int32) (*Entry, error) {
	a, err := actor.Require(ctx, rbac.ContentPublish)
	if err != nil {
		return nil, err
	}
	var out *Entry
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		e, err := s.publishTx(ctx, tx, q, a, id, expectedVersion, "publish")
		out = e
		return err
	})
	return out, err
}

func (s *Service) publishTx(ctx context.Context, tx pgx.Tx, q *dbq.Queries, a *actor.Actor, id uuid.UUID,
	expectedVersion int32, kind string) (*Entry, error) {
	cur, err := s.lock(ctx, q, id)
	if err != nil {
		return nil, err
	}
	if expectedVersion != 0 && cur.Version != expectedVersion {
		return nil, &ErrConflict{Current: cur.Version, Expected: expectedVersion}
	}
	ct, err := s.typeOf(cur.Type)
	if err != nil {
		return nil, err
	}
	// Full validation of what is about to go live.
	wc, err := s.clean(ctx, q, a, ct, cur, SaveInput{
		Title: cur.DraftTitle, Slug: cur.DraftSlug, Meta: ParseMeta(cur.DraftMeta),
		Fields: cur.DraftFields, Blocks: cur.DraftBlocks,
	}, false)
	if err != nil {
		return nil, err
	}
	var parent *uuid.UUID
	if wc.metaV.ParentID != "" {
		p := uuid.MustParse(wc.metaV.ParentID)
		pe, err := q.GetEntry(ctx, p)
		if err != nil || pe.PublishedRevisionID == nil {
			return nil, fields.Errors{"meta.parent_id": "the parent must be published first"}
		}
		parent = &p
	}
	taken, err := q.SlugTaken(ctx, dbq.SlugTakenParams{Type: ct.Name, Locale: cur.Locale, Slug: wc.slug, ID: cur.ID, ParentID: parent})
	if err != nil {
		return nil, err
	}
	if taken {
		return nil, fields.Errors{"slug": "is already used by another published entry"}
	}
	// Persist the cleaned working copy (it may differ from what was stored).
	saved, err := q.SaveWorkingCopy(ctx, dbq.SaveWorkingCopyParams{
		ID: cur.ID, DraftTitle: wc.title, DraftSlug: wc.slug, DraftMeta: wc.meta,
		DraftFields: wc.fields, DraftBlocks: wc.blocks, ExpectedVersion: cur.Version,
	})
	if err != nil {
		return nil, err
	}
	oldURL := ""
	if cur.PublishedRevisionID != nil {
		oldURL, _ = s.urlFor(ctx, q, ct, cur.ID, cur.Slug, cur.ParentID, deref(cur.PublishedAt))
	}
	rev, err := s.revision(ctx, q, a, &saved, kind)
	if err != nil {
		return nil, err
	}
	pub, err := q.PublishEntry(ctx, dbq.PublishEntryParams{
		ID: cur.ID, Slug: wc.slug, ParentID: parent, PublishedRevisionID: &rev.ID, PublishedAt: s.now(),
	})
	if err != nil {
		if db.IsUniqueViolation(err, "entries_route_key") {
			return nil, fields.Errors{"slug": "is already used by another published entry"}
		}
		return nil, err
	}
	if err := s.syncLive(ctx, q, &pub, wc); err != nil {
		return nil, err
	}
	newURL, err := s.urlFor(ctx, q, ct, pub.ID, pub.Slug, pub.ParentID, deref(pub.PublishedAt))
	if err != nil {
		return nil, err
	}
	paths := []string{newURL}
	if oldURL != "" && oldURL != newURL {
		paths = append(paths, oldURL)
		if err := s.autoRedirect(ctx, tx, q, a, oldURL, newURL); err != nil {
			return nil, err
		}
	}
	if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: s.depsFor(ct, &pub, wc.metaV), Paths: paths},
		jobs.WebhookEvent{Event: "entry.published", Payload: map[string]any{"id": pub.ID, "type": pub.Type, "title": wc.title, "url": newURL}}); err != nil {
		return nil, err
	}
	if err := audit.Record(ctx, q, audit.Event{Action: "entry." + kind, ResourceType: "entry", ResourceID: pub.ID.String(),
		Diff: map[string]any{"version": pub.Version, "revision": rev.Number, "url": newURL}}); err != nil {
		return nil, err
	}
	return &Entry{Entry: pub, Meta: wc.metaV}, nil
}

// syncLive writes live term assignments, relations, media usage and search.
func (s *Service) syncLive(ctx context.Context, q *dbq.Queries, e *dbq.Entry, wc *workingCopy) error {
	if err := q.SetEntryTerms(ctx, e.ID); err != nil {
		return err
	}
	for _, list := range wc.metaV.Terms {
		for _, t := range list {
			if err := q.AddEntryTerm(ctx, dbq.AddEntryTermParams{EntryID: e.ID, TermID: uuid.MustParse(t)}); err != nil {
				return err
			}
		}
	}
	if err := q.ClearEntryRelations(ctx, e.ID); err != nil {
		return err
	}
	for i, target := range wc.refs.Entries {
		if target == e.ID {
			continue
		}
		if err := q.AddEntryRelation(ctx, dbq.AddEntryRelationParams{SourceID: e.ID, Field: "ref", Position: int32(i), TargetID: target}); err != nil {
			return err
		}
	}
	if err := q.ClearMediaUsage(ctx, e.ID); err != nil {
		return err
	}
	for mid, path := range wc.refs.Media {
		id, err := uuid.Parse(mid)
		if err != nil {
			continue
		}
		if err := q.AddMediaUsage(ctx, dbq.AddMediaUsageParams{MediaID: id, EntryID: e.ID, FieldPath: path}); err != nil {
			return err
		}
	}
	ct := s.Site().Types[e.Type]
	if ct != nil && ct.Searchable() {
		body := strings.Join(append(wc.refs.Text, wc.blockText...), " ")
		if len(body) > 512*1024 {
			body = body[:512*1024]
		}
		return q.UpdateSearchVector(ctx, dbq.UpdateSearchVectorParams{
			Cfg: SearchConfig(e.Locale), Title: wc.title, Summary: wc.metaV.Excerpt, Body: body, ID: e.ID,
		})
	}
	return q.DeleteSearchVector(ctx, e.ID)
}

// SearchConfig picks a Postgres text search configuration for a locale.
func SearchConfig(locale string) string {
	switch strings.ToLower(strings.SplitN(locale, "-", 2)[0]) {
	case "en":
		return "english"
	case "fr":
		return "french"
	case "de":
		return "german"
	case "es":
		return "spanish"
	case "it":
		return "italian"
	case "nl":
		return "dutch"
	case "pt":
		return "portuguese"
	}
	return "simple"
}

func (s *Service) depsFor(ct *site.ContentType, e *dbq.Entry, m Meta) []jobs.Dep {
	deps := []jobs.Dep{{Type: "entry", ID: e.ID.String()}, {Type: "type", ID: ct.Name}}
	for _, list := range m.Terms {
		for _, t := range list {
			deps = append(deps, jobs.Dep{Type: "term", ID: t})
		}
	}
	if e.ParentID != nil {
		deps = append(deps, jobs.Dep{Type: "entry", ID: e.ParentID.String()})
	}
	return deps
}

// autoRedirect records old -> new and flattens chains (spec 4.5).
func (s *Service) autoRedirect(ctx context.Context, tx pgx.Tx, q *dbq.Queries, a *actor.Actor, from, to string) error {
	// A redirect whose source is the new URL would loop; remove it.
	if err := q.DeleteRedirectBySource(ctx, dbq.DeleteRedirectBySourceParams{Host: "", Source: to}); err != nil {
		return err
	}
	if _, err := q.RetargetRedirects(ctx, dbq.RetargetRedirectsParams{NewTarget: to, OldTarget: from}); err != nil {
		return err
	}
	var by *uuid.UUID
	if a.IsUser() {
		u := a.UserID
		by = &u
	}
	if _, err := q.CreateRedirect(ctx, dbq.CreateRedirectParams{
		ID: uuid.Must(uuid.NewV7()), Host: "", Source: from, MatchType: "exact", Target: to,
		StatusCode: 301, PreserveQuery: true, IsAuto: true, Note: "Created when the URL changed", CreatedBy: by,
	}); err != nil {
		return err
	}
	return events.Notify(ctx, tx, events.Event{Topic: events.RedirectsChanged})
}

// URLFor returns the live URL path of a published entry ("" if it has none).
func (s *Service) URLFor(ctx context.Context, e *dbq.Entry) (string, error) {
	ct, err := s.typeOf(e.Type)
	if err != nil {
		return "", err
	}
	return s.urlFor(ctx, s.DB.Q, ct, e.ID, e.Slug, e.ParentID, deref(e.PublishedAt))
}

func (s *Service) urlFor(ctx context.Context, q *dbq.Queries, ct *site.ContentType, id uuid.UUID, slug string,
	parent *uuid.UUID, publishedAt time.Time) (string, error) {
	if s.FrontPage != nil && s.FrontPage(ctx) == id {
		return "/", nil
	}
	return BuildURL(ctx, q, s.Site(), ct, slug, parent, publishedAt)
}

// BuildURL fills a type's URL pattern. Hierarchical {path} walks live parents.
func BuildURL(ctx context.Context, q *dbq.Queries, b *site.Bundle, ct *site.ContentType, slug string,
	parent *uuid.UUID, publishedAt time.Time) (string, error) {
	p := ct.Pattern()
	if p == nil {
		return "", nil
	}
	vals := map[string]string{"slug": slug}
	if p.HasPath {
		path := slug
		if parent != nil {
			anc, err := q.EntryAncestors(ctx, *parent)
			if err != nil {
				return "", err
			}
			var parts []string
			for _, x := range anc {
				parts = append(parts, x.Slug)
			}
			parts = append(parts, slug)
			path = strings.Join(parts, "/")
		}
		vals["path"] = path
	}
	if publishedAt.IsZero() {
		publishedAt = time.Now()
	}
	loc, err := time.LoadLocation(b.Config.Timezone)
	if err != nil {
		loc = time.UTC
	}
	t := publishedAt.In(loc)
	vals["year"] = t.Format("2006")
	vals["month"] = t.Format("01")
	vals["day"] = t.Format("02")
	return p.Build(vals), nil
}

// Unpublish takes an entry off the live site; the working copy is kept.
func (s *Service) Unpublish(ctx context.Context, id uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.ContentPublish); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := s.lock(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.PublishedRevisionID == nil {
			return nil
		}
		return s.takeDown(ctx, tx, q, cur, "entry.unpublish", q.UnpublishEntry)
	})
}

func (s *Service) takeDown(ctx context.Context, tx pgx.Tx, q *dbq.Queries, cur dbq.Entry, action string,
	op func(context.Context, uuid.UUID) error) error {
	ct, err := s.typeOf(cur.Type)
	if err != nil {
		return err
	}
	var paths []string
	if cur.PublishedRevisionID != nil {
		if u, err := s.urlFor(ctx, q, ct, cur.ID, cur.Slug, cur.ParentID, deref(cur.PublishedAt)); err == nil && u != "" {
			paths = append(paths, u)
		}
	}
	if err := op(ctx, cur.ID); err != nil {
		return err
	}
	if err := q.SetEntryTerms(ctx, cur.ID); err != nil {
		return err
	}
	if err := q.DeleteSearchVector(ctx, cur.ID); err != nil {
		return err
	}
	m := ParseMeta(cur.DraftMeta)
	event := "entry.unpublished"
	if action == "entry.trash" {
		event = "entry.trashed"
	}
	if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: s.depsFor(ct, &cur, m), Paths: paths},
		jobs.WebhookEvent{Event: event, Payload: map[string]any{"id": cur.ID, "type": cur.Type}}); err != nil {
		return err
	}
	return audit.Record(ctx, q, audit.Event{Action: action, ResourceType: "entry", ResourceID: cur.ID.String()})
}

// Schedule validates now and publishes at `at` (scheduler sweep, spec 4.15).
func (s *Service) Schedule(ctx context.Context, id uuid.UUID, at time.Time, expectedVersion int32) error {
	a, err := actor.Require(ctx, rbac.ContentPublish)
	if err != nil {
		return err
	}
	if !at.After(s.now()) {
		return fields.Errors{"scheduled_at": "must be in the future"}
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := s.lock(ctx, q, id)
		if err != nil {
			return err
		}
		if expectedVersion != 0 && cur.Version != expectedVersion {
			return &ErrConflict{Current: cur.Version, Expected: expectedVersion}
		}
		ct, err := s.typeOf(cur.Type)
		if err != nil {
			return err
		}
		if _, err := s.clean(ctx, q, a, ct, cur, SaveInput{Title: cur.DraftTitle, Slug: cur.DraftSlug,
			Meta: ParseMeta(cur.DraftMeta), Fields: cur.DraftFields, Blocks: cur.DraftBlocks}, false); err != nil {
			return err
		}
		if err := q.ScheduleEntry(ctx, dbq.ScheduleEntryParams{ID: id, ScheduledAt: &at}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "entry.schedule", ResourceType: "entry", ResourceID: id.String(),
			Diff: map[string]any{"at": at.UTC().Format(time.RFC3339)}})
	})
}

func (s *Service) Unschedule(ctx context.Context, id uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.ContentPublish); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.UnscheduleEntry(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "entry.unschedule", ResourceType: "entry", ResourceID: id.String()})
	})
}

// SetUnpublishAt schedules removal from the live site (nil clears it).
func (s *Service) SetUnpublishAt(ctx context.Context, id uuid.UUID, at *time.Time) error {
	if _, err := actor.Require(ctx, rbac.ContentPublish); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SetUnpublishAt(ctx, dbq.SetUnpublishAtParams{ID: id, UnpublishAt: at}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "entry.unpublish_at", ResourceType: "entry", ResourceID: id.String()})
	})
}

// RunSchedule publishes due scheduled entries and unpublishes expired ones.
// Called by the scheduler sweep as a system actor; idempotent.
func (s *Service) RunSchedule(ctx context.Context) (published, unpublished int, err error) {
	a, err := actor.Require(ctx, rbac.ContentPublish)
	if err != nil {
		return 0, 0, err
	}
	due, err := s.DB.Q.DueScheduled(ctx)
	if err != nil {
		return 0, 0, err
	}
	for _, id := range due {
		err := s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
			cur, err := q.GetEntryForUpdate(ctx, id)
			if err != nil || cur.ScheduledAt == nil || cur.ScheduledAt.After(s.now()) {
				return err
			}
			_, err = s.publishTx(ctx, tx, q, a, id, 0, "scheduled")
			return err
		})
		if err != nil {
			// A scheduled entry that no longer validates goes back to draft and
			// the failure is audited, so the sweep never retries it forever.
			_ = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
				if err := q.UnscheduleEntry(ctx, id); err != nil {
					return err
				}
				return audit.Record(ctx, q, audit.Event{Action: "entry.schedule_failed", ResourceType: "entry",
					ResourceID: id.String(), Diff: map[string]any{"error": err.Error()}})
			})
			continue
		}
		published++
	}
	expired, err := s.DB.Q.DueUnpublish(ctx)
	if err != nil {
		return published, 0, err
	}
	for _, id := range expired {
		if err := s.Unpublish(ctx, id); err == nil {
			unpublished++
		}
	}
	return published, unpublished, nil
}

// Trash soft-deletes an entry and takes it off the live site.
func (s *Service) Trash(ctx context.Context, id uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.ContentDelete); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := s.lock(ctx, q, id)
		if err != nil {
			return err
		}
		return s.takeDown(ctx, tx, q, cur, "entry.trash", q.TrashEntry)
	})
}

func (s *Service) Restore(ctx context.Context, id uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.ContentDelete); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := q.GetEntryForUpdate(ctx, id)
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status != "trashed" {
			return nil
		}
		if err := q.RestoreEntry(ctx, id); err != nil {
			if db.IsUniqueViolation(err, "entries_route_key") {
				return fields.Errors{"slug": "another entry now uses this slug; rename it first"}
			}
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "entry.restore", ResourceType: "entry", ResourceID: id.String()})
	})
}

// Delete permanently removes a trashed entry.
func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.ContentDelete); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := q.GetEntryForUpdate(ctx, id)
		if db.IsNotFound(err) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		if cur.Status != "trashed" {
			return errors.New("move the entry to the trash before deleting it permanently")
		}
		if err := q.DeleteEntry(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "entry.delete", ResourceType: "entry", ResourceID: id.String(),
			Diff: map[string]any{"type": cur.Type}})
	})
}

// Discard resets the working copy to the published revision.
func (s *Service) Discard(ctx context.Context, id uuid.UUID) (*Entry, error) {
	if _, err := actor.Require(ctx, rbac.ContentEdit); err != nil {
		return nil, err
	}
	var out *Entry
	err := s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := s.lock(ctx, q, id)
		if err != nil {
			return err
		}
		if cur.PublishedRevisionID == nil {
			return errors.New("this entry has never been published")
		}
		e, err := q.ResetWorkingCopy(ctx, id)
		if err != nil {
			return err
		}
		out = &Entry{Entry: e, Meta: ParseMeta(e.DraftMeta)}
		return audit.Record(ctx, q, audit.Event{Action: "entry.discard", ResourceType: "entry", ResourceID: id.String()})
	})
	return out, err
}

// RestoreRevision copies a revision into the working copy.
func (s *Service) RestoreRevision(ctx context.Context, id, revID uuid.UUID) (*Entry, error) {
	a, err := actor.Require(ctx, rbac.ContentEdit)
	if err != nil {
		return nil, err
	}
	var out *Entry
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		cur, err := s.lock(ctx, q, id)
		if err != nil {
			return err
		}
		rev, err := q.GetRevision(ctx, revID)
		if err != nil || rev.EntryID != id {
			return ErrNotFound
		}
		e, err := q.SaveWorkingCopy(ctx, dbq.SaveWorkingCopyParams{
			ID: id, DraftTitle: rev.Title, DraftSlug: rev.Slug, DraftMeta: rev.Meta,
			DraftFields: rev.Fields, DraftBlocks: rev.Blocks, ExpectedVersion: cur.Version,
		})
		if err != nil {
			return err
		}
		if _, err := s.revision(ctx, q, a, &e, "restore"); err != nil {
			return err
		}
		out = &Entry{Entry: e, Meta: ParseMeta(e.DraftMeta)}
		return audit.Record(ctx, q, audit.Event{Action: "entry.restore_revision", ResourceType: "entry",
			ResourceID: id.String(), Diff: map[string]any{"revision": rev.Number}})
	})
	return out, err
}

func (s *Service) Revisions(ctx context.Context, id uuid.UUID, limit int32) ([]dbq.ListRevisionsRow, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, err
	}
	return s.DB.Q.ListRevisions(ctx, dbq.ListRevisionsParams{EntryID: id, Limit: limit})
}

func (s *Service) Revision(ctx context.Context, id uuid.UUID) (dbq.EntryRevision, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return dbq.EntryRevision{}, err
	}
	return s.DB.Q.GetRevision(ctx, id)
}

// Clone copies the working copy into a new draft.
func (s *Service) Clone(ctx context.Context, id uuid.UUID) (*Entry, error) {
	a, err := actor.Require(ctx, rbac.ContentEdit)
	if err != nil {
		return nil, err
	}
	src, err := s.DB.Q.GetEntry(ctx, id)
	if err != nil {
		return nil, ErrNotFound
	}
	ct, err := s.typeOf(src.Type)
	if err != nil {
		return nil, err
	}
	nid := uuid.Must(uuid.NewV7())
	var out *Entry
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		m := ParseMeta(src.DraftMeta)
		var parent *uuid.UUID
		if m.ParentID != "" {
			p := uuid.MustParse(m.ParentID)
			parent = &p
		}
		slug, err := s.uniqueDraftSlug(ctx, q, ct, src.Locale, parent, nid, src.DraftSlug+"-copy")
		if err != nil {
			return err
		}
		var author *uuid.UUID
		if a.IsUser() {
			u := a.UserID
			author = &u
		}
		e, err := q.CreateEntry(ctx, dbq.CreateEntryParams{
			ID: nid, Type: src.Type, Slug: "~" + nid.String(), AuthorID: author, Locale: src.Locale, MenuOrder: src.MenuOrder,
			DraftTitle: src.DraftTitle + " (copy)", DraftSlug: slug, DraftMeta: src.DraftMeta,
			DraftFields: src.DraftFields, DraftBlocks: src.DraftBlocks,
		})
		if err != nil {
			return err
		}
		out = &Entry{Entry: e, Meta: m}
		return audit.Record(ctx, q, audit.Event{Action: "entry.clone", ResourceType: "entry", ResourceID: nid.String(),
			Diff: map[string]any{"from": id.String()}})
	})
	return out, err
}

func (s *Service) SetAuthor(ctx context.Context, id uuid.UUID, author *uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.ContentPublish); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SetEntryAuthor(ctx, dbq.SetEntryAuthorParams{ID: id, AuthorID: author}); err != nil {
			return err
		}
		e, err := q.GetEntry(ctx, id)
		if err != nil {
			return err
		}
		if e.PublishedRevisionID != nil {
			if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: []jobs.Dep{{Type: "entry", ID: id.String()}}}); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Event{Action: "entry.author", ResourceType: "entry", ResourceID: id.String()})
	})
}

// Reorder sets menu_order for hierarchical types (drag-reorder).
func (s *Service) Reorder(ctx context.Context, ids []uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.ContentPublish); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		types := map[string]bool{}
		for i, id := range ids {
			if err := q.SetEntryOrder(ctx, dbq.SetEntryOrderParams{ID: id, MenuOrder: int32(i)}); err != nil {
				return err
			}
			if e, err := q.GetEntry(ctx, id); err == nil {
				types[e.Type] = true
			}
		}
		var deps []jobs.Dep
		for t := range types {
			deps = append(deps, jobs.Dep{Type: "type", ID: t})
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: deps}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "entry.reorder", ResourceType: "entry", ResourceID: "many"})
	})
}

// ListFilter for the admin list screen.
type ListFilter struct {
	Type     string
	Status   string
	Query    string
	AuthorID *uuid.UUID
	Sort     string
	Limit    int32
	Offset   int32
}

func (s *Service) List(ctx context.Context, f ListFilter) ([]dbq.ListEntriesRow, int64, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 200 {
		f.Limit = 50
	}
	rows, err := s.DB.Q.ListEntries(ctx, dbq.ListEntriesParams{
		Type: f.Type, Status: nilIfEmpty(f.Status), Q: nilIfEmpty(f.Query), AuthorID: f.AuthorID,
		Sort: f.Sort, Lim: f.Limit, Off: f.Offset,
	})
	if err != nil {
		return nil, 0, err
	}
	var total int64
	if len(rows) > 0 {
		total = rows[0].Total
	}
	return rows, total, nil
}

// preserveGatedFields keeps the stored value of fields the actor may not edit.
func preserveGatedFields(defs []fields.Field, stored, incoming json.RawMessage) json.RawMessage {
	gated := map[string]bool{}
	for _, f := range defs {
		if f.Capability != "" {
			gated[f.Name] = true
		}
	}
	if len(gated) == 0 {
		return incoming
	}
	var st, in map[string]any
	_ = json.Unmarshal(stored, &st)
	_ = json.Unmarshal(incoming, &in)
	if in == nil {
		in = map[string]any{}
	}
	for k := range gated {
		if v, ok := st[k]; ok {
			in[k] = v
		} else {
			delete(in, k)
		}
	}
	b, _ := json.Marshal(in)
	return b
}

// preserveGatedBlocks restores capability-gated blocks (raw HTML) by id and
// drops any new ones, so an editor cannot add or change them.
func preserveGatedBlocks(b *site.Bundle, stored, incoming json.RawMessage) json.RawMessage {
	gatedType := func(t string) bool {
		d := b.Blocks[t]
		if d == nil {
			return false
		}
		for _, f := range d.Fields {
			if f.Capability != "" {
				return true
			}
		}
		return false
	}
	old := map[string]blocks.Block{}
	blocks.Walk(blocks.Parse(stored), func(bl *blocks.Block) {
		if gatedType(bl.Type) {
			old[bl.ID] = *bl
		}
	})
	tree := blocks.Parse(incoming)
	var fix func([]blocks.Block) []blocks.Block
	fix = func(list []blocks.Block) []blocks.Block {
		out := list[:0]
		for _, bl := range list {
			if gatedType(bl.Type) {
				o, ok := old[bl.ID]
				if !ok {
					continue
				}
				bl.Data = o.Data
			}
			bl.Children = fix(bl.Children)
			out = append(out, bl)
		}
		return out
	}
	tree = fix(tree)
	out, _ := json.Marshal(tree)
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func nilIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Reindex rebuilds search vectors for every published entry.
func (s *Service) Reindex(ctx context.Context) (int, error) {
	if _, err := actor.Require(ctx, rbac.SystemManage); err != nil {
		return 0, err
	}
	n := 0
	for _, name := range s.Site().TypeOrder {
		rows, err := s.DB.Q.PublishedIDsByType(ctx, name)
		if err != nil {
			return n, err
		}
		for _, r := range rows {
			e, err := s.DB.Q.GetEntry(ctx, r.ID)
			if err != nil || e.PublishedRevisionID == nil {
				continue
			}
			rev, err := s.DB.Q.GetRevision(ctx, *e.PublishedRevisionID)
			if err != nil {
				continue
			}
			ct, err := s.typeOf(e.Type)
			if err != nil {
				continue
			}
			a := actor.From(ctx)
			wc, err := s.clean(ctx, s.DB.Q, a, ct, e, SaveInput{Title: rev.Title, Slug: rev.Slug, Meta: ParseMeta(rev.Meta),
				Fields: rev.Fields, Blocks: rev.Blocks}, true)
			if err != nil {
				continue
			}
			if err := s.syncLive(ctx, s.DB.Q, &e, wc); err != nil {
				return n, err
			}
			n++
		}
	}
	return n, nil
}
