// Package fixtures loads sample content from a site bundle's
// fixtures/content.yaml so `cms dev` and new sites are never empty.
package fixtures

import (
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.yaml.in/yaml/v3"

	"cms/internal/blocks"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/fields"
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
}

// Load imports fixtures when the site has no entries yet. Returns entries created.
func Load(ctx context.Context, fsys fs.FS, s Services) (int, error) {
	data, err := fs.ReadFile(fsys, "fixtures/content.yaml")
	if err != nil {
		return 0, nil
	}
	n, err := Import(ctx, data, s, false)
	if err != nil {
		return n, fmt.Errorf("fixtures/content.yaml: %w", err)
	}
	return n, nil
}

// Import loads a content file (the fixtures format) into the database. With
// skipExisting, entries whose type and slug already exist are left alone, so
// an import can be re-run (used by `cms content import` at a migration).
func Import(ctx context.Context, data []byte, s Services, skipExisting bool) (int, error) {
	var f File
	if err := yaml.Unmarshal(data, &f); err != nil {
		return 0, err
	}
	for k, v := range f.Settings {
		if err := s.Settings.Set(ctx, k, v); err != nil {
			return 0, err
		}
	}
	termIDs := map[string]map[string]string{}
	for tax, list := range f.Terms {
		termIDs[tax] = map[string]string{}
		for _, t := range list {
			tm, err := s.Content.CreateTerm(ctx, content.TermInput{Taxonomy: tax, Name: t.Name, Slug: t.Slug, Description: t.Description})
			if err != nil {
				return 0, fmt.Errorf("term %s/%s: %w", tax, t.Name, err)
			}
			termIDs[tax][strings.ToLower(t.Name)] = tm.ID.String()
		}
	}
	// Two passes: create every entry first, so entry fields can point at
	// entries later in the file (services ↔ projects), then fill them in.
	slugIDs := map[string]uuid.UUID{}
	created := make([]*content.Entry, len(f.Entries))
	parents := make([]*uuid.UUID, len(f.Entries))
	for i, fe := range f.Entries {
		if skipExisting && fe.Slug != "" {
			if ex, err := s.Q.EntryByTypeSlug(ctx, dbq.EntryByTypeSlugParams{Type: fe.Type, DraftSlug: fe.Slug}); err == nil {
				slugIDs[fe.Type+"/"+fe.Slug] = ex.ID
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
			return 0, fmt.Errorf("entry %q: %w", fe.Title, err)
		}
		created[i], parents[i] = e, in.ParentID
		slugIDs[fe.Type+"/"+e.DraftSlug] = e.ID
	}
	n := 0
	for i, fe := range f.Entries {
		e := created[i]
		if e == nil {
			continue // already existed
		}
		in := content.CreateInput{ParentID: parents[i]}
		meta := content.Meta{Excerpt: fe.Excerpt, SEO: fe.SEO, Template: fe.Template}
		if in.ParentID != nil {
			meta.ParentID = in.ParentID.String()
		}
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
			if body != "" {
				tree = append(tree, map[string]any{"id": blocks.NewID(), "type": "rich-text",
					"data": map[string]any{"body": richtext.FromMarkdown(body)}})
			}
		case map[string]any:
			tree = append(tree, map[string]any{"id": blocks.NewID(), "type": "rich-text", "data": map[string]any{"body": body}})
		case nil:
		default:
			return n, fmt.Errorf("entry %q: body must be Markdown or a ProseMirror document", fe.Title)
		}
		for _, b := range fe.Blocks {
			if b["id"] == nil {
				b["id"] = blocks.NewID()
			}
			tree = append(tree, b)
		}
		bj, _ := json.Marshal(tree)
		resolveRefs(s.Content.Site().FieldsFor(fe.Type), fe.Fields, slugIDs)
		fj, _ := json.Marshal(fe.Fields)
		e, err := s.Content.Save(ctx, content.SaveInput{ID: e.ID, ExpectedVersion: e.Version, Title: fe.Title, Slug: e.DraftSlug,
			Meta: meta, Fields: fj, Blocks: bj})
		if err != nil {
			return n, fmt.Errorf("entry %q: %w", fe.Title, err)
		}
		if fe.Publish {
			if _, err := s.Content.Publish(ctx, e.ID, e.Version); err != nil {
				return n, fmt.Errorf("publish %q: %w", fe.Title, err)
			}
			var at time.Time
			if fe.PublishedAt != "" {
				if at, err = time.Parse(time.RFC3339, fe.PublishedAt); err != nil {
					return n, fmt.Errorf("entry %q: published_at: %w", fe.Title, err)
				}
			} else if fe.DaysAgo > 0 {
				at = time.Now().Add(-time.Duration(fe.DaysAgo) * 24 * time.Hour)
			}
			if !at.IsZero() {
				if err := s.Q.SetPublishedAt(ctx, dbq.SetPublishedAtParams{ID: e.ID, PublishedAt: &at}); err != nil {
					return n, err
				}
			}
		}
		if fe.FrontPage {
			if err := s.Settings.Set(ctx, settings.FrontPage, e.ID.String()); err != nil {
				return n, err
			}
		}
		// Keep the file's order for types sorted by menu_order.
		if err := s.Q.SetEntryOrder(ctx, dbq.SetEntryOrderParams{ID: e.ID, MenuOrder: int32(n)}); err != nil {
			return n, err
		}
		n++
	}
	for name, items := range f.Menus {
		conv := convertMenu(items, slugIDs)
		if err := s.Menus.Save(ctx, name, name, conv); err != nil {
			return n, fmt.Errorf("menu %s: %w", name, err)
		}
	}
	if s.Redirects != nil {
		for _, rd := range f.Redirects {
			status := rd.Status
			if status == 0 {
				status = 301
			}
			match := rd.Match
			if match == "" {
				match = "exact"
			}
			_, err := s.Redirects.Create(ctx, redirects.Input{Source: rd.From, Target: rd.To, MatchType: match, Status: status,
				PreserveQuery: true, Note: "imported"})
			if err != nil {
				return n, fmt.Errorf("redirect %s: %w", rd.From, err)
			}
		}
	}
	for page, vals := range f.Options {
		b, _ := json.Marshal(vals)
		if err := s.Settings.SaveOptions(ctx, page, b); err != nil {
			return n, fmt.Errorf("options %s: %w", page, err)
		}
	}
	return n, nil
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
		}
	}
}
