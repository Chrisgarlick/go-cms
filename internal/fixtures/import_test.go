package fixtures_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"testing/fstest"

	"cms/internal/app"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/fixtures"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func services(a *app.App) fixtures.Services {
	return fixtures.Services{Content: a.Content, Menus: a.Menus, Settings: a.Settings, Q: a.DB.Q, Redirects: a.Redirects}
}

const services1 = `
entries:
  - type: service
    title: Audits
    slug: audits
    publish: true
    fields: { icon: strategy, summary: We check everything. }
    body: The first version.
  - type: service
    title: Fixes
    slug: fixes
    publish: true
    fields: { icon: design, summary: We fix it. }
`

func TestImportUpdate(t *testing.T) {
	a, _ := testutil.NewApp(t, "brochure")
	ctx := app.SystemCtx(context.Background(), "test")
	s := services(a)
	r, err := fixtures.Import(ctx, []byte(services1), s, fixtures.ImportOptions{})
	if err != nil || r.Created != 2 {
		t.Fatalf("first import: %+v %v", r, err)
	}
	get := func(slug string) *content.Entry {
		t.Helper()
		row, err := a.DB.Q.EntryByTypeSlug(ctx, dbqParams("service", slug))
		if err != nil {
			t.Fatal(err)
		}
		e, err := a.Content.Get(ctx, row.ID)
		if err != nil {
			t.Fatal(err)
		}
		return e
	}
	before := get("audits")

	// The same file again, with --update: nothing changes, no new revision.
	r, err = fixtures.Import(ctx, []byte(services1), s, fixtures.ImportOptions{Update: true})
	if err != nil || r.Created != 0 || r.Updated != 0 || r.Unchanged != 2 {
		t.Fatalf("re-import: %+v %v", r, err)
	}
	if v := get("audits").Version; v != before.Version {
		t.Errorf("an unchanged entry got a new version: %d → %d", before.Version, v)
	}

	// Someone sets the icon by hand; the file changes the summary and body.
	e := get("audits")
	fj, _ := json.Marshal(map[string]any{"icon": "growth", "summary": "We check everything."})
	if _, err := a.Content.Save(ctx, content.SaveInput{ID: e.ID, ExpectedVersion: e.Version, Title: e.DraftTitle, Slug: e.DraftSlug,
		Meta: e.Meta, Fields: fj, Blocks: e.DraftBlocks}); err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(strings.Replace(services1, "We check everything.", "We check everything twice.", 1), "The first version.", "The second version.", 1)
	r, err = fixtures.Import(ctx, []byte(changed), s, fixtures.ImportOptions{Update: true, SkipFields: []string{"icon"}})
	if err != nil || r.Updated != 1 || r.Unchanged != 1 {
		t.Fatalf("update: %+v %v", r, err)
	}
	e = get("audits")
	var f map[string]any
	_ = json.Unmarshal(e.DraftFields, &f)
	if f["icon"] != "growth" || f["summary"] != "We check everything twice." {
		t.Errorf("fields after update: %v", f)
	}
	if !strings.Contains(string(e.DraftBlocks), "second version") || !e.IsPublished() || e.HasUnpublishedChanges() {
		t.Errorf("body not updated and published: %s", e.DraftBlocks)
	}

	// Pruning: an entry the file no longer has goes to the trash.
	only := services1[:strings.Index(services1, "  - type: service\n    title: Fixes")]
	r, err = fixtures.Import(ctx, []byte(only), s, fixtures.ImportOptions{Update: true, Prune: []string{"service"}})
	if err != nil || r.Pruned != 1 {
		t.Fatalf("prune: %+v %v", r, err)
	}
	if _, err := a.DB.Q.EntryByTypeSlug(ctx, dbqParams("service", "fixes")); err == nil {
		t.Error("a pruned entry is still there")
	}
}

const blog = `
terms:
  category: [ { name: Engineering, description: How things are built. } ]
entries:
  - type: post
    title: Hello
    slug: hello
    publish: true
    terms: { category: [Engineering] }
    body: Hi.
redirects:
  - { from: /old-hello, to: /blog/hello }
`

// Terms and redirects that exist are reused, so a plain re-run works.
func TestImportRerun(t *testing.T) {
	a, _ := testutil.NewApp(t, "blog")
	ctx := app.SystemCtx(context.Background(), "test")
	for i := range 2 {
		if _, err := fixtures.Import(ctx, []byte(blog), services(a), fixtures.ImportOptions{Update: true}); err != nil {
			t.Fatalf("run %d: %v", i+1, err)
		}
	}
	moved := strings.Replace(blog, "to: /blog/hello", "to: /blog/hello-again", 1)
	if _, err := fixtures.Import(ctx, []byte(moved), services(a), fixtures.ImportOptions{Update: true}); err != nil {
		t.Fatal(err)
	}
	rs, _ := a.DB.Q.ListRedirects(ctx)
	if len(rs) != 1 || rs[0].Target != "/blog/hello-again" {
		t.Errorf("redirects: %+v", rs)
	}
	terms, _ := a.Content.Terms(ctx, "category")
	n := 0
	for _, tm := range terms {
		if tm.Name == "Engineering" {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d Engineering terms", n)
	}
}

func dbqParams(typ, slug string) dbq.EntryByTypeSlugParams {
	return dbq.EntryByTypeSlugParams{Type: typ, DraftSlug: slug}
}

// Load reads content.yaml first, then the other fixture files in order.
func TestLoadAllFixtureFiles(t *testing.T) {
	a, _ := testutil.NewApp(t, "blog")
	ctx := app.SystemCtx(context.Background(), "test")
	fsys := fstest.MapFS{
		"fixtures/content.yaml":   {Data: []byte("entries:\n  - { type: page, title: First, slug: first, publish: true }\n")},
		"fixtures/reference.yaml": {Data: []byte(`{"entries": [{"type": "page", "title": "Second", "slug": "second", "publish": true}]}`)},
		"fixtures/notes.txt":      {Data: []byte("not a fixture")},
	}
	n, err := fixtures.Load(ctx, fsys, services(a))
	if err != nil || n != 2 {
		t.Fatalf("loaded %d: %v", n, err)
	}
}
