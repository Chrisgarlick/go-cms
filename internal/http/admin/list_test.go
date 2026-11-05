package admin_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cms/internal/app"
	"cms/internal/content"
	"cms/internal/rbac"
)

// A private type with list_columns and a filterable select: the admin list
// shows the field columns and filters by them, and the public API hides it.
func TestListColumnsAndFilters(t *testing.T) {
	a, srv := setup(t)
	ct := `name: roadmap
label: { singular: Roadmap item, plural: Roadmap }
public: false
supports: [title, body]
sort: { field: menu_order, order: asc }
list_columns: [title, release, state, promised, updated_at]
fields:
  - { name: release, type: select, filterable: true, options: [v1, { value: v1.1, label: "v1.1" }, later] }
  - { name: state, type: select, filterable: true, options: [{ value: planned, label: Planned }, { value: doing, label: In progress }] }
  - { name: promised, type: boolean }
`
	if err := os.WriteFile(filepath.Join(a.Cfg.Paths.Site, "content-types", "roadmap.yaml"), []byte(ct), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadBundle(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Site().Apply(context.Background(), a.DB.Q); err != nil {
		t.Fatal(err)
	}
	sys := app.SystemCtx(context.Background(), "test")
	for _, it := range []struct{ title, release, state string }{{"MCP server", "v1.1", "planned"}, {"Dark mode", "v1", "doing"}} {
		e, err := a.Content.Create(sys, content.CreateInput{Type: "roadmap", Title: it.title})
		if err != nil {
			t.Fatal(err)
		}
		f, _ := json.Marshal(map[string]any{"release": it.release, "state": it.state, "promised": it.release == "v1.1"})
		if _, err := a.Content.Save(sys, content.SaveInput{ID: e.ID, ExpectedVersion: e.Version, Title: it.title, Slug: e.Slug, Fields: f}); err != nil {
			t.Fatal(err)
		}
	}

	b := login(t, a, srv, "editor@example.com", rbac.Editor)
	st, body := b.get("/admin/content/roadmap/")
	if st != 200 {
		t.Fatalf("list status %d", st)
	}
	for _, want := range []string{`<th scope="col">Release</th>`, `<th scope="col">State</th>`, `<th scope="col">Promised</th>`,
		"In progress", "Planned", "<td>Yes</td>", "MCP server", "Dark mode", `aria-label="Filter by release"`, `href="?release=v1.1"`} {
		if !strings.Contains(body, want) {
			t.Errorf("list is missing %q", want)
		}
	}
	if strings.Contains(body, `<th scope="col">Author</th>`) {
		t.Error("author column shown although list_columns leaves it out")
	}

	_, body = b.get("/admin/content/roadmap/?release=v1.1&status=draft")
	if !strings.Contains(body, "MCP server") || strings.Contains(body, "Dark mode") {
		t.Error("release=v1.1 should list only MCP server")
	}
	// The status tabs keep the filter, and the filter links keep the status.
	for _, want := range []string{`href="?release=v1.1&amp;status=published"`, `href="?release=v1&amp;status=draft"`} {
		if !strings.Contains(body, want) {
			t.Errorf("filtered list is missing %q", want)
		}
	}

	// Filters combine.
	_, body = b.get("/admin/content/roadmap/?release=v1.1&state=doing")
	if strings.Contains(body, "MCP server") || strings.Contains(body, "Dark mode") {
		t.Error("release=v1.1 and state=doing should list nothing")
	}
	_, body = b.get("/admin/content/roadmap/?release=v1&state=doing")
	if !strings.Contains(body, "Dark mode") || strings.Contains(body, "MCP server") {
		t.Error("release=v1 and state=doing should list only Dark mode")
	}

	for _, p := range []string{"/api/v1/content/roadmap", "/api/v1/content/roadmap/mcp-server"} {
		if st, _ := newBrowser(t, srv).get(p); st != 404 {
			t.Errorf("GET %s = %d, want 404 for a private type", p, st)
		}
	}
}

// A type sorted by a datetime field lists by that field, and shows it in
// the site's time zone.
func TestListSortsByDateField(t *testing.T) {
	a, srv := setup(t)
	ct := `name: social
label: { singular: Social post, plural: Social }
public: false
supports: [title]
sort: { field: goes_out, order: desc }
list_columns: [title, goes_out]
fields:
  - { name: goes_out, type: datetime }
`
	if err := os.WriteFile(filepath.Join(a.Cfg.Paths.Site, "content-types", "social.yaml"), []byte(ct), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.LoadBundle(); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Site().Apply(context.Background(), a.DB.Q); err != nil {
		t.Fatal(err)
	}
	sys := app.SystemCtx(context.Background(), "test")
	// Saved in this order, so updated_at alone would list them the other way round.
	for _, it := range []struct{ title, when string }{{"Tuesday reel", "2026-11-10T08:00:00Z"}, {"Monday carousel", "2026-11-09T18:30:00Z"}, {"No date yet", ""}} {
		e, err := a.Content.Create(sys, content.CreateInput{Type: "social", Title: it.title})
		if err != nil {
			t.Fatal(err)
		}
		m := map[string]any{}
		if it.when != "" {
			m["goes_out"] = it.when
		}
		f, _ := json.Marshal(m)
		if _, err := a.Content.Save(sys, content.SaveInput{ID: e.ID, ExpectedVersion: e.Version, Title: it.title, Slug: e.Slug, Fields: f}); err != nil {
			t.Fatal(err)
		}
	}

	b := login(t, a, srv, "editor@example.com", rbac.Editor)
	_, body := b.get("/admin/content/social/")
	tue, mon, none := strings.Index(body, "Tuesday reel"), strings.Index(body, "Monday carousel"), strings.Index(body, "No date yet")
	if tue < 0 || mon < 0 || none < 0 || !(tue < mon && mon < none) {
		t.Errorf("want Tuesday, Monday, then the undated post; got positions %d, %d, %d", tue, mon, none)
	}
	tz := a.Site().Config.Timezone
	loc, _ := time.LoadLocation(tz)
	want := time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC).In(loc).Format("Mon 2 Jan 2006, 15:04")
	if !strings.Contains(body, want) {
		t.Errorf("list is missing the date %q (site time zone %s)", want, tz)
	}
}
