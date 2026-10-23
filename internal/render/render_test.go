package render_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"cms/internal/actor"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/jobs"
	"cms/internal/media"
	"cms/internal/menus"
	"cms/internal/rbac"
	"cms/internal/render"
	"cms/internal/settings"
	"cms/internal/site"
	"cms/internal/testutil"
	"cms/themes"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func richText(s string) json.RawMessage {
	b, _ := json.Marshal([]map[string]any{{"id": "b1", "type": "rich-text", "data": map[string]any{
		"body": map[string]any{"type": "doc", "content": []any{
			map[string]any{"type": "heading", "attrs": map[string]any{"level": 2}, "content": []any{map[string]any{"type": "text", "text": "Section <one>"}}},
			map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": s}}}}}}}})
	return b
}

func TestRenderPublishedSite(t *testing.T) {
	d := testutil.NewDB(t)
	b := testutil.BlogBundle(t)
	ctx := context.Background()
	if err := site.ApplySchemaForTest(ctx, d.Q, b); err != nil {
		t.Fatal(err)
	}
	uid := uuid.Must(uuid.NewV7())
	if _, err := d.Q.CreateUser(ctx, dbq.CreateUserParams{ID: uid, Email: "a@x.test", Role: "owner", DisplayName: "Ada", Status: "active", Locale: "en"}); err != nil {
		t.Fatal(err)
	}
	actx := actor.With(ctx, &actor.Actor{Kind: actor.KindUser, UserID: uid, Role: rbac.Owner})
	j := &jobs.Discard{}
	bundle := func() *site.Bundle { return b }
	set := &settings.Service{DB: d.DB, Site: bundle, Jobs: j}
	cs := &content.Service{DB: d.DB, Site: bundle, Jobs: j, FrontPage: set.FrontPageID}
	ms := &media.Service{DB: d.DB, Site: bundle, BaseURL: "https://example.test/media"}
	root := t.TempDir()
	r := &render.Renderer{DB: d.DB, Site: bundle, Content: cs, Media: ms, Menus: &menus.Service{DB: d.DB, Site: bundle, Jobs: j},
		Settings: set, BaseFS: themes.Base(), BaseURL: "https://example.test", Disk: &render.Disk{Root: root}}

	var first *content.Entry
	for i, title := range []string{"Hello World", "Second Post"} {
		e, err := cs.Create(actx, content.CreateInput{Type: "post", Title: title})
		if err != nil {
			t.Fatal(err)
		}
		e, err = cs.Save(actx, content.SaveInput{ID: e.ID, ExpectedVersion: e.Version, Title: title, Slug: e.DraftSlug,
			Meta: content.Meta{Excerpt: "Excerpt " + title}, Blocks: richText("Body text <b>" + title + "</b>")})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cs.Publish(actx, e.ID, e.Version); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = e
		}
	}

	res, err := r.Render(ctx, "/blog/hello-world/")
	if err != nil {
		t.Fatal(err)
	}
	html := string(res.Body)
	if res.Status != 200 || !res.Cacheable {
		t.Fatalf("status %d cacheable %v", res.Status, res.Cacheable)
	}
	for _, want := range []string{"<title>Hello World | Test Blog</title>", `<link rel="canonical" href="https://example.test/blog/hello-world/">`,
		`<h2 id="section-one">Section &lt;one&gt;</h2>`, "Body text &lt;b&gt;Hello World&lt;/b&gt;", `"@type":"BlogPosting"`, "Ada", `href="/assets/`} {
		if !strings.Contains(html, want) {
			t.Errorf("single page missing %q", want)
		}
	}
	if strings.Contains(html, "<b>Hello") {
		t.Error("unescaped HTML in output")
	}
	deps := map[string]bool{}
	for _, dd := range res.Deps {
		deps[dd.Type+":"+dd.ID] = true
	}
	if !deps["entry:"+first.ID.String()] {
		t.Errorf("deps missing entry: %v", res.Deps)
	}

	for path, want := range map[string]string{
		"/blog/":            "Second Post",
		"/":                 "Hello World",
		"/sitemap.xml":      "sitemap-post.xml",
		"/sitemap-post.xml": "https://example.test/blog/hello-world/",
		"/feed.xml":         "<title>Second Post</title>",
		"/robots.txt":       "Sitemap: https://example.test/sitemap.xml",
	} {
		res, err := r.Render(ctx, path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if res.Status != 200 || !strings.Contains(string(res.Body), want) {
			t.Errorf("%s: status %d, missing %q", path, res.Status, want)
		}
	}
	res, _ = r.Render(ctx, "/blog/nope/")
	if res.Status != 404 || res.Cacheable {
		t.Errorf("404: status %d cacheable %v", res.Status, res.Cacheable)
	}

	// Rebuild writes files with compressed siblings and records deps.
	n, err := r.Rebuild(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n < 6 {
		t.Fatalf("rebuilt %d pages", n)
	}
	for _, f := range []string{"blog/hello-world/index.html", "blog/hello-world/index.html.br", "blog/hello-world/index.html.gz", "index.html", "sitemap.xml"} {
		if _, err := os.Stat(filepath.Join(root, f)); err != nil {
			t.Errorf("missing %s", f)
		}
	}
	// Unpublishing invalidates: the file disappears, the archive no longer lists it.
	if err := cs.Unpublish(actx, first.ID); err != nil {
		t.Fatal(err)
	}
	var inv jobs.RenderInvalidate
	for _, jb := range j.Jobs {
		if ri, ok := jb.(jobs.RenderInvalidate); ok {
			inv = ri
		}
	}
	if err := r.Invalidate(ctx, inv); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "blog/hello-world/index.html")); !os.IsNotExist(err) {
		t.Error("unpublished page still on disk")
	}
	arch, _ := os.ReadFile(filepath.Join(root, "blog/index.html"))
	if strings.Contains(string(arch), "Hello World") {
		t.Error("archive still lists the unpublished entry")
	}
}
