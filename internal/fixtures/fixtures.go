// Package fixtures loads sample content from a site bundle's
// fixtures/content.yaml so `cms dev` and new sites are never empty.
package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"cms/internal/blocks"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/fields"
	"cms/internal/media"
	"cms/internal/menus"
	"cms/internal/redirects"
	"cms/internal/richtext"
	"cms/internal/settings"
)

type File struct {
	Settings map[string]any            `yaml:"settings"`
	Terms    map[string][]Term         `yaml:"terms"`
	Entries  []Entry                   `yaml:"entries"`
	Menus    map[string][]MenuItem     `yaml:"menus"`
	Options  map[string]map[string]any `yaml:"options"`
	// Redirects are created as permanent (or the given status) exact-path rules.
	Redirects []Redirect `yaml:"redirects"`
}

type Redirect struct {
	From   string `yaml:"from"`
	To     string `yaml:"to"`
	Status int    `yaml:"status"`
	// Match is exact (default) or prefix: /blog → /article sends /blog/x to /article/x.
	Match string `yaml:"match"`
}

type Term struct {
	Name        string `yaml:"name"`
	Slug        string `yaml:"slug"`
	Description string `yaml:"description"`
}

type Entry struct {
	Type    string `yaml:"type"`
	Title   string `yaml:"title"`
	Slug    string `yaml:"slug"`
	Excerpt string `yaml:"excerpt"`
	// Body is Markdown, or a ProseMirror document (a map) used as it is.
	Body      any                 `yaml:"body"`
	Blocks    []map[string]any    `yaml:"blocks"`
	Fields    map[string]any      `yaml:"fields"`
	Terms     map[string][]string `yaml:"terms"`  // term names
	Parent    string              `yaml:"parent"` // slug of a parent of the same type
	Publish   bool                `yaml:"publish"`
	DaysAgo   int                 `yaml:"days_ago"`
	FrontPage bool                `yaml:"front_page"`
	// PublishedAt (RFC 3339) sets the publication date exactly; it wins over DaysAgo.
	PublishedAt string      `yaml:"published_at"`
	SEO         content.SEO `yaml:"seo"`
	Template    string      `yaml:"template"`
	// Image is the featured image: a file path, relative to the content
	// file (fixtures/ for a site's own fixtures). It's uploaded to the
	// media library; the same file again reuses the upload.
	Image    string `yaml:"image"`
	ImageAlt string `yaml:"image_alt"`
}

type MenuItem struct {
	Label  string     `yaml:"label"`
	URL    string     `yaml:"url"`
	Entry  string     `yaml:"entry"` // slug
	Target string     `yaml:"target"`
	Items  []MenuItem `yaml:"items"`
}

type Services struct {
	Content  *content.Service
	Menus    *menus.Service
	Settings *settings.Service
	Q        *dbq.Queries
	// Redirects is optional: without it, a file's redirects are skipped.
	Redirects *redirects.Service
	// Media and Files are optional: with both, entries' images (paths in
	// Files) are uploaded; without, images are skipped.
	Media *media.Service
	Files fs.FS
}

// Load imports a site's fixtures (cms dev on an empty database, cms check):
// fixtures/content.yaml first, then any other fixtures/*.yaml in name order,
// such as a generated reference beside hand-written pages. Returns entries
// created.
func Load(ctx context.Context, fsys fs.FS, s Services) (int, error) {
	files, _ := fs.Glob(fsys, "fixtures/*.yaml")
	// Images are relative to the content file, as with cms content import.
	if s.Files != nil {
		if sub, err := fs.Sub(s.Files, "fixtures"); err == nil {
			s.Files = sub
		}
	}
	sort.Slice(files, func(i, j int) bool {
		if (files[i] == "fixtures/content.yaml") != (files[j] == "fixtures/content.yaml") {
			return files[i] == "fixtures/content.yaml"
		}
		return files[i] < files[j]
	})
	n := 0
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return n, err
		}
		r, err := Import(ctx, data, s, ImportOptions{})
		n += r.Created
		if err != nil {
			return n, fmt.Errorf("%s: %w", f, err)
		}
	}
	return n, nil
}

// ImportOptions control how an import treats what's already there.
type ImportOptions struct {
	// SkipExisting leaves entries whose type and slug already exist alone.
	SkipExisting bool
	// Update brings existing entries (matched by type and slug) in line with
	// the file: title, excerpt, SEO, template, terms, fields and blocks. An
	// entry that already matches is left untouched (no new revision).
	Update bool
	// SkipFields are field names an update never writes: values edited by
	// hand on an otherwise generated entry (e.g. "notes").
	SkipFields []string
	// Prune lists types whose entries not in the file are moved to the
	// trash (generated reference pages whose source went away).
	Prune []string
}

// ImportResult counts what an import did.
type ImportResult struct {
	Created, Updated, Unchanged, Pruned int
}

// Import loads a content file (the fixtures format) into the database. Terms
// and redirects that already exist are reused or updated, so any import can
// be re-run; see ImportOptions for entries.
func Import(ctx context.Context, data []byte, s Services, opt ImportOptions) (ImportResult, error) {
	var res ImportResult
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return res, err
	}
	for k, v := range f.Settings {
		if err := s.Settings.Set(ctx, k, v); err != nil {
			return res, err
		}
	}
	termIDs := map[string]map[string]string{}
	for tax, list := range f.Terms {
		termIDs[tax] = map[string]string{}
		existing, err := s.Content.Terms(ctx, tax)
		if err != nil {
			return res, fmt.Errorf("terms %s: %w", tax, err)
		}
		for _, t := range list {
			var found *dbq.ListTermsRow
			for i := range existing {
				if strings.EqualFold(existing[i].Name, t.Name) || (t.Slug != "" && existing[i].Slug == t.Slug) {
					found = &existing[i]
					break
				}
			}
			if found != nil {
				if opt.Update && t.Description != "" && t.Description != found.Description {
					if _, err := s.Content.UpdateTerm(ctx, found.ID, content.TermInput{Taxonomy: tax, Name: found.Name, Slug: found.Slug,
						Description: t.Description}); err != nil {
						return res, fmt.Errorf("term %s/%s: %w", tax, t.Name, err)
					}
				}
				termIDs[tax][strings.ToLower(t.Name)] = found.ID.String()
				continue
			}
			tm, err := s.Content.CreateTerm(ctx, content.TermInput{Taxonomy: tax, Name: t.Name, Slug: t.Slug, Description: t.Description})
			if err != nil {
				return res, fmt.Errorf("term %s/%s: %w", tax, t.Name, err)
			}
			termIDs[tax][strings.ToLower(t.Name)] = tm.ID.String()
		}
	}
	// Two passes: create every entry first, so entry fields can point at
	// entries later in the file (services ↔ projects), then fill them in.
	slugIDs := map[string]uuid.UUID{}
	targets := make([]*content.Entry, len(f.Entries))
	isNew := make([]bool, len(f.Entries))
	parents := make([]*uuid.UUID, len(f.Entries))
	inFile := map[string]bool{}
	for i, fe := range f.Entries {
		if fe.Slug != "" && (opt.SkipExisting || opt.Update) {
			if ex, err := s.Q.EntryByTypeSlug(ctx, dbq.EntryByTypeSlugParams{Type: fe.Type, DraftSlug: fe.Slug}); err == nil {
				slugIDs[fe.Type+"/"+fe.Slug] = ex.ID
				inFile[ex.ID.String()] = true
				if opt.Update {
					e, err := s.Content.Get(ctx, ex.ID)
					if err != nil {
						return res, fmt.Errorf("entry %q: %w", fe.Title, err)
					}
					targets[i] = e
				}
				continue
			}
		}
		in := content.CreateInput{Type: fe.Type, Title: fe.Title, Slug: fe.Slug}
		if fe.Parent != "" {
			if pid, ok := slugIDs[fe.Type+"/"+fe.Parent]; ok {
				in.ParentID = &pid
			}
		}
		e, err := s.Content.Create(ctx, in)
		if err != nil {
			return res, fmt.Errorf("entry %q: %w", fe.Title, err)
		}
		targets[i], isNew[i], parents[i] = e, true, in.ParentID
		slugIDs[fe.Type+"/"+e.DraftSlug] = e.ID
		inFile[e.ID.String()] = true
	}
	skip := map[string]bool{}
	for _, f := range opt.SkipFields {
		skip[f] = true
	}
	order := 0
	for i, fe := range f.Entries {
		e := targets[i]
		if e == nil {
			order++
			continue // existing, left alone
		}
		parent := parents[i]
		if !isNew[i] && fe.Parent != "" {
			if pid, ok := slugIDs[fe.Type+"/"+fe.Parent]; ok {
				parent = &pid
			}
		}
		meta := content.Meta{Excerpt: fe.Excerpt, SEO: fe.SEO, Template: fe.Template}
		if !isNew[i] {
			// Keep what the file doesn't set: the featured image chosen in
			// the admin, say.
			meta = e.Meta
			meta.Excerpt, meta.SEO, meta.Template = fe.Excerpt, fe.SEO, fe.Template
			meta.ParentID = ""
		}
		if parent != nil {
			meta.ParentID = parent.String()
		}
		if fe.Image != "" && s.Media != nil && s.Files != nil {
			id, err := uploadImage(ctx, s, fe.Image, fe.ImageAlt)
			if err != nil {
				return res, fmt.Errorf("entry %q: image %s: %w", fe.Title, fe.Image, err)
			}
			meta.FeaturedImage = id
		}
		meta.Terms = nil
		if len(fe.Terms) > 0 {
			meta.Terms = map[string][]string{}
			for tax, names := range fe.Terms {
				for _, name := range names {
					if id := termIDs[tax][strings.ToLower(name)]; id != "" {
						meta.Terms[tax] = append(meta.Terms[tax], id)
					}
				}
			}
		}
		var tree []map[string]any
		switch body := fe.Body.(type) {
		case string:
			tree = append(tree, markdownBlocks(body)...)
		case map[string]any:
			tree = append(tree, map[string]any{"id": blocks.NewID(), "type": "rich-text", "data": map[string]any{"body": body}})
		case nil:
		default:
			return res, fmt.Errorf("entry %q: body must be Markdown or a ProseMirror document", fe.Title)
		}
		for _, b := range fe.Blocks {
			if b["id"] == nil {
				b["id"] = blocks.NewID()
			}
			tree = append(tree, b)
		}
		resolveRefs(s.Content.Site().FieldsFor(fe.Type), fe.Fields, slugIDs)
		fieldsMap := map[string]any{}
		for k, v := range fe.Fields {
			fieldsMap[k] = v
		}
		if !isNew[i] && len(skip) > 0 {
			var cur map[string]any
			_ = json.Unmarshal(e.DraftFields, &cur)
			for name := range skip {
				if v, ok := cur[name]; ok {
					fieldsMap[name] = v
				} else {
					delete(fieldsMap, name)
				}
			}
		}
		bj, _ := json.Marshal(tree)
		fj, _ := json.Marshal(fieldsMap)
		if !isNew[i] && sameEntry(e, fe.Title, meta, fj, bj) {
			if fe.Publish && (!e.IsPublished() || e.HasUnpublishedChanges()) {
				if _, err := s.Content.Publish(ctx, e.ID, e.Version); err != nil {
					return res, fmt.Errorf("publish %q: %w", fe.Title, err)
				}
			}
			res.Unchanged++
			order++
			continue
		}
		e, err := s.Content.Save(ctx, content.SaveInput{ID: e.ID, ExpectedVersion: e.Version, Title: fe.Title, Slug: e.DraftSlug,
			Meta: meta, Fields: fj, Blocks: bj})
		if err != nil {
			return res, fmt.Errorf("entry %q: %w", fe.Title, err)
		}
		if fe.Publish {
			if _, err := s.Content.Publish(ctx, e.ID, e.Version); err != nil {
				return res, fmt.Errorf("publish %q: %w", fe.Title, err)
			}
			var at time.Time
			if fe.PublishedAt != "" {
				if at, err = time.Parse(time.RFC3339, fe.PublishedAt); err != nil {
					return res, fmt.Errorf("entry %q: published_at: %w", fe.Title, err)
				}
			} else if fe.DaysAgo > 0 && isNew[i] {
				at = time.Now().Add(-time.Duration(fe.DaysAgo) * 24 * time.Hour)
			}
			if !at.IsZero() {
				if err := s.Q.SetPublishedAt(ctx, dbq.SetPublishedAtParams{ID: e.ID, PublishedAt: &at}); err != nil {
					return res, err
				}
			}
		}
		if fe.FrontPage {
			if err := s.Settings.Set(ctx, settings.FrontPage, e.ID.String()); err != nil {
				return res, err
			}
		}
		// Keep the file's order for types sorted by menu_order.
		if err := s.Q.SetEntryOrder(ctx, dbq.SetEntryOrderParams{ID: e.ID, MenuOrder: int32(order)}); err != nil { //nolint:gosec // small
			return res, err
		}
		order++
		if isNew[i] {
			res.Created++
		} else {
			res.Updated++
		}
	}
	for _, typ := range opt.Prune {
		n, err := prune(ctx, s, typ, inFile)
		res.Pruned += n
		if err != nil {
			return res, fmt.Errorf("prune %s: %w", typ, err)
		}
	}
	for name, items := range f.Menus {
		conv := convertMenu(items, slugIDs)
		if err := s.Menus.Save(ctx, name, name, conv); err != nil {
			return res, fmt.Errorf("menu %s: %w", name, err)
		}
	}
	if s.Redirects != nil && len(f.Redirects) > 0 {
		if err := importRedirects(ctx, s, f.Redirects); err != nil {
			return res, err
		}
	}
	for page, vals := range f.Options {
		b, _ := json.Marshal(vals)
		if err := s.Settings.SaveOptions(ctx, page, b); err != nil {
			return res, fmt.Errorf("options %s: %w", page, err)
		}
	}
	return res, nil
}

// uploadImage adds a file to the media library (or finds the same file
// there) and returns its id.
func uploadImage(ctx context.Context, s Services, name, alt string) (string, error) {
	f, err := s.Files.Open(strings.TrimPrefix(path.Clean("/"+name), "/"))
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	m, _, err := s.Media.Upload(ctx, media.UploadInput{Filename: path.Base(name), Body: f, Alt: alt})
	if err != nil {
		return "", err
	}
	return m.ID.String(), nil
}

var tableDelim = regexp.MustCompile(`^\|?\s*:?-{2,}:?\s*(\|\s*:?-{2,}:?\s*)*\|?$`)

// markdownBlocks turns a Markdown body into blocks: rich text, with each
// GFM table (outside code fences) as a table block, since rich text has no
// tables.
func markdownBlocks(body string) []map[string]any {
	var out []map[string]any
	var text []string
	flush := func() {
		md := strings.TrimSpace(strings.Join(text, "\n"))
		text = nil
		if md != "" {
			out = append(out, map[string]any{"id": blocks.NewID(), "type": "rich-text",
				"data": map[string]any{"body": richtext.FromMarkdown(md)}})
		}
	}
	lines := strings.Split(strings.ReplaceAll(body, "\r\n", "\n"), "\n")
	fence := false
	for i := 0; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if strings.HasPrefix(t, "```") {
			fence = !fence
		}
		if fence || !strings.HasPrefix(t, "|") || i+1 >= len(lines) || !tableDelim.MatchString(strings.TrimSpace(lines[i+1])) {
			text = append(text, lines[i])
			continue
		}
		flush()
		rows := []any{map[string]any{"cells": strings.Join(tableCells(t), "\n")}}
		for i += 2; i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|"); i++ {
			rows = append(rows, map[string]any{"cells": strings.Join(tableCells(strings.TrimSpace(lines[i])), "\n")})
		}
		i--
		out = append(out, map[string]any{"id": blocks.NewID(), "type": "table", "data": map[string]any{"header": true, "rows": rows}})
	}
	flush()
	return out
}

// tableCells splits a "| a | b |" row; `\|` is a literal pipe.
func tableCells(row string) []string {
	row = strings.TrimSuffix(strings.TrimPrefix(row, "|"), "|")
	row = strings.ReplaceAll(row, `\|`, "\x00")
	parts := strings.Split(row, "|")
	for i, p := range parts {
		parts[i] = strings.ReplaceAll(strings.TrimSpace(p), "\x00", "|")
	}
	return parts
}

// sameEntry reports whether an entry's working copy already holds this
// title, meta, fields and blocks (block ids and versions aside: ids are
// minted per import and saving stamps the version).
func sameEntry(e *content.Entry, title string, meta content.Meta, fields, tree []byte) bool {
	if e.DraftTitle != title {
		return false
	}
	mj, _ := json.Marshal(meta)
	cm, _ := json.Marshal(e.Meta)
	return jsonEqual(mj, cm, false) && jsonEqual(fields, e.DraftFields, false) && jsonEqual(tree, e.DraftBlocks, true)
}

func jsonEqual(a, b []byte, ignoreIDs bool) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	if ignoreIDs {
		x, y = stripIDs(x), stripIDs(y)
	}
	x, y = emptyAsNil(x), emptyAsNil(y)
	xa, _ := json.Marshal(x)
	ya, _ := json.Marshal(y)
	return string(xa) == string(ya)
}

func stripIDs(v any) any {
	switch t := v.(type) {
	case map[string]any:
		_, isBlock := t["data"]
		isBlock = isBlock && t["type"] != nil
		out := map[string]any{}
		for k, val := range t {
			if isBlock && (k == "id" || k == "version") {
				continue
			}
			out[k] = stripIDs(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripIDs(val)
		}
		return out
	}
	return v
}

// emptyAsNil treats {} and [] like a missing value, as storage does.
func emptyAsNil(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if len(t) == 0 {
			return nil
		}
		out := map[string]any{}
		for k, val := range t {
			if n := emptyAsNil(val); n != nil {
				out[k] = n
			}
		}
		return out
	case []any:
		if len(t) == 0 {
			return nil
		}
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = emptyAsNil(val)
		}
		return out
	case string:
		if t == "" {
			return nil
		}
	}
	return v
}

// prune trashes entries of a type that the file didn't mention.
func prune(ctx context.Context, s Services, typ string, keep map[string]bool) (int, error) {
	n := 0
	for {
		rows, err := s.Q.ListEntries(ctx, dbq.ListEntriesParams{Type: typ, Sort: "created", Lim: 500})
		if err != nil {
			return n, err
		}
		trashed := 0
		for _, r := range rows {
			if keep[r.ID.String()] {
				continue
			}
			if err := s.Content.Trash(ctx, r.ID); err != nil {
				return n, err
			}
			n++
			trashed++
		}
		if trashed == 0 || len(rows) < 500 {
			return n, nil
		}
	}
}

// importRedirects creates the file's redirects, updating any that exist
// for the same source and match type.
func importRedirects(ctx context.Context, s Services, list []Redirect) error {
	existing, err := s.Q.ListRedirects(ctx)
	if err != nil {
		return err
	}
	for _, rd := range list {
		status := rd.Status
		if status == 0 {
			status = 301
		}
		match := rd.Match
		if match == "" {
			match = "exact"
		}
		in := redirects.Input{Source: rd.From, Target: rd.To, MatchType: match, Status: status, PreserveQuery: true, Note: "imported"}
		var found *dbq.Redirect
		for i := range existing {
			if existing[i].Host == "" && existing[i].MatchType == match && redirects.SameSource(existing[i].Source, rd.From, match) {
				found = &existing[i]
				break
			}
		}
		if found == nil {
			if _, err := s.Redirects.Create(ctx, in); err != nil {
				return fmt.Errorf("redirect %s: %w", rd.From, err)
			}
			continue
		}
		if found.Target == rd.To && int(found.StatusCode) == status {
			continue
		}
		if _, err := s.Redirects.Update(ctx, found.ID, in); err != nil {
			return fmt.Errorf("redirect %s: %w", rd.From, err)
		}
	}
	return nil
}

func convertMenu(items []MenuItem, slugs map[string]uuid.UUID) []menus.Item {
	var out []menus.Item
	for _, it := range items {
		mi := menus.Item{Label: it.Label, Target: it.Target}
		switch {
		case it.Entry != "":
			for k, id := range slugs {
				if strings.HasSuffix(k, "/"+it.Entry) {
					mi.Type, mi.Ref = "entry", id.String()
					break
				}
			}
			if mi.Type == "" {
				continue
			}
		case it.URL != "":
			mi.Type, mi.URL = "url", it.URL
		default:
			mi.Type = "label"
		}
		mi.Children = convertMenu(it.Items, slugs)
		out = append(out, mi)
	}
	return out
}

// resolveRefs lets a content file point entry fields at other entries by slug
// ("cg-cms" or "project/cg-cms") rather than by ID. Referenced entries must
// appear earlier in the file.
func resolveRefs(defs []fields.Field, vals map[string]any, slugs map[string]uuid.UUID) {
	lookup := func(v any, types []string) any {
		ref, ok := v.(string)
		if !ok {
			return v
		}
		if _, err := uuid.Parse(ref); err == nil {
			return ref
		}
		if id, ok := slugs[ref]; ok {
			return id.String()
		}
		for _, t := range types {
			if id, ok := slugs[t+"/"+ref]; ok {
				return id.String()
			}
		}
		for k, id := range slugs {
			if strings.HasSuffix(k, "/"+ref) {
				return id.String()
			}
		}
		return v // left as is: validation reports it
	}
	for _, d := range defs {
		v, ok := vals[d.Name]
		if !ok {
			continue
		}
		switch d.Type {
		case fields.EntryRef:
			vals[d.Name] = lookup(v, d.Types)
		case fields.Entries:
			if list, ok := v.([]any); ok {
				for i := range list {
					list[i] = lookup(list[i], d.Types)
				}
			}
		case fields.RichText:
			// A content file may give rich text as Markdown.
			if md, ok := v.(string); ok {
				vals[d.Name] = richtext.FromMarkdown(md)
			}
		}
	}
}
