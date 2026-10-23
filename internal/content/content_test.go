package content_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"cms/internal/actor"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/fields"
	"cms/internal/jobs"
	"cms/internal/rbac"
	"cms/internal/site"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

type env struct {
	svc  *content.Service
	jobs *jobs.Discard
	q    *dbq.Queries
	ctx  context.Context
}

func setup(t *testing.T, role rbac.Role) *env {
	d := testutil.NewDB(t)
	b := testutil.BlogBundle(t)
	ctx := context.Background()
	if err := site.ApplySchemaForTest(ctx, d.Q, b); err != nil {
		t.Fatal(err)
	}
	uid := uuid.Must(uuid.NewV7())
	if _, err := d.Q.CreateUser(ctx, dbq.CreateUserParams{ID: uid, Email: string(role) + "@x.test", Role: string(role), Status: "active", Locale: "en"}); err != nil {
		t.Fatal(err)
	}
	j := &jobs.Discard{}
	svc := &content.Service{DB: d.DB, Site: func() *site.Bundle { return b }, Jobs: j}
	ctx = actor.With(ctx, &actor.Actor{Kind: actor.KindUser, UserID: uid, Role: role, Label: string(role)})
	return &env{svc: svc, jobs: j, q: d.Q, ctx: ctx}
}

func richText(s string) json.RawMessage {
	b, _ := json.Marshal([]map[string]any{{"id": "b1", "type": "rich-text", "data": map[string]any{
		"body": map[string]any{"type": "doc", "content": []any{map[string]any{"type": "paragraph",
			"content": []any{map[string]any{"type": "text", "text": s}}}}}}}})
	return b
}

func TestWorkingCopyAndPublish(t *testing.T) {
	e := setup(t, rbac.Editor)
	ent, err := e.svc.Create(e.ctx, content.CreateInput{Type: "post", Title: "Hello World"})
	if err != nil {
		t.Fatal(err)
	}
	if ent.DraftSlug != "hello-world" {
		t.Fatalf("slug = %q", ent.DraftSlug)
	}
	// Autosave with missing title is allowed (partial).
	ent, err = e.svc.Save(e.ctx, content.SaveInput{ID: ent.ID, ExpectedVersion: ent.Version, Title: "", Slug: "hello-world",
		Fields: json.RawMessage(`{"subtitle":"sub"}`), Blocks: richText("first"), Autosave: true})
	if err != nil {
		t.Fatal(err)
	}
	// Stale version conflicts.
	_, err = e.svc.Save(e.ctx, content.SaveInput{ID: ent.ID, ExpectedVersion: ent.Version - 1, Title: "x"})
	var ce *content.ErrConflict
	if !errors.As(err, &ce) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// Publishing with no title fails validation.
	if _, err := e.svc.Publish(e.ctx, ent.ID, ent.Version); err == nil {
		t.Fatal("published without a title")
	}
	ent, err = e.svc.Save(e.ctx, content.SaveInput{ID: ent.ID, ExpectedVersion: ent.Version, Title: "Hello World",
		Slug: "hello-world", Fields: json.RawMessage(`{"subtitle":"sub"}`), Blocks: richText("first")})
	if err != nil {
		t.Fatal(err)
	}
	pub, err := e.svc.Publish(e.ctx, ent.ID, ent.Version)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Slug != "hello-world" || pub.HasUnpublishedChanges() {
		t.Fatalf("after publish: slug=%q dirty=%v", pub.Slug, pub.HasUnpublishedChanges())
	}
	live, err := e.q.PublishedBySlug(e.ctx, dbq.PublishedBySlugParams{Type: "post", Locale: "en", Slug: "hello-world"})
	if err != nil || len(live) != 1 || live[0].Title != "Hello World" {
		t.Fatalf("live: %v %v", live, err)
	}
	// Edit after publish: live stays unchanged.
	ent, err = e.svc.Save(e.ctx, content.SaveInput{ID: pub.ID, ExpectedVersion: pub.Version, Title: "Hello Again",
		Slug: "hello-again", Blocks: richText("second")})
	if err != nil {
		t.Fatal(err)
	}
	if !ent.HasUnpublishedChanges() {
		t.Fatal("expected unpublished changes")
	}
	live, _ = e.q.PublishedBySlug(e.ctx, dbq.PublishedBySlugParams{Type: "post", Locale: "en", Slug: "hello-world"})
	if len(live) != 1 || live[0].Title != "Hello World" {
		t.Fatal("working copy leaked to live")
	}
	// Publishing the slug change creates an automatic redirect.
	if _, err := e.svc.Publish(e.ctx, ent.ID, ent.Version); err != nil {
		t.Fatal(err)
	}
	reds, _ := e.q.ListRedirects(e.ctx)
	if len(reds) != 1 || reds[0].Source != "/blog/hello-world/" || reds[0].Target != "/blog/hello-again/" {
		t.Fatalf("redirects: %+v", reds)
	}
	// Search finds the published text.
	hits, err := e.q.PublishedSearch(e.ctx, dbq.PublishedSearchParams{Cfg: "english", Query: "second", Types: []string{"post"}, Lim: 10})
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %d %v", len(hits), err)
	}
	// Revisions: save, publish, save, publish (autosave made none).
	revs, _ := e.svc.Revisions(e.ctx, ent.ID, 50)
	if len(revs) != 4 {
		t.Fatalf("revisions = %d", len(revs))
	}
	// Render invalidation was enqueued.
	found := false
	for _, j := range e.jobs.Jobs {
		if ri, ok := j.(jobs.RenderInvalidate); ok && len(ri.Paths) == 2 {
			found = true
		}
	}
	if !found {
		t.Fatal("expected invalidation with old and new paths")
	}
}

func TestEditorCannotAddRawHTML(t *testing.T) {
	e := setup(t, rbac.Editor)
	ent, _ := e.svc.Create(e.ctx, content.CreateInput{Type: "post", Title: "X"})
	blocksJSON := json.RawMessage(`[{"id":"h1","type":"html","data":{"html":"<script>x</script>"}}]`)
	ent, err := e.svc.Save(e.ctx, content.SaveInput{ID: ent.ID, ExpectedVersion: ent.Version, Title: "X", Blocks: blocksJSON})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(ent.DraftBlocks), "script") {
		t.Fatalf("editor added a raw HTML block: %s", ent.DraftBlocks)
	}
}

func TestScheduleSweep(t *testing.T) {
	e := setup(t, rbac.Admin)
	ent, _ := e.svc.Create(e.ctx, content.CreateInput{Type: "post", Title: "Later"})
	now := time.Now()
	e.svc.Now = func() time.Time { return now }
	if err := e.svc.Schedule(e.ctx, ent.ID, now.Add(time.Hour), ent.Version); err != nil {
		t.Fatal(err)
	}
	if rows, _ := e.q.PublishedBySlug(e.ctx, dbq.PublishedBySlugParams{Type: "post", Locale: "en", Slug: "later"}); len(rows) > 0 {
		t.Fatal("published before its time")
	}
	// Move the schedule into the past and sweep.
	past := now.Add(-time.Minute)
	if err := e.q.ScheduleEntry(e.ctx, dbq.ScheduleEntryParams{ID: ent.ID, ScheduledAt: &past}); err != nil {
		t.Fatal(err)
	}
	sys := actor.With(context.Background(), actor.System("scheduler"))
	n, _, err := e.svc.RunSchedule(sys)
	if err != nil || n != 1 {
		t.Fatalf("sweep: %d %v", n, err)
	}
	if rows, _ := e.q.PublishedBySlug(e.ctx, dbq.PublishedBySlugParams{Type: "post", Locale: "en", Slug: "later"}); len(rows) != 1 {
		t.Fatal("not published by sweep")
	}
}

func TestHierarchicalURLsAndTrash(t *testing.T) {
	e := setup(t, rbac.Admin)
	parent, _ := e.svc.Create(e.ctx, content.CreateInput{Type: "page", Title: "About"})
	parentPub, err := e.svc.Publish(e.ctx, parent.ID, parent.Version)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := e.svc.Create(e.ctx, content.CreateInput{Type: "page", Title: "Team", ParentID: &parentPub.ID})
	childPub, err := e.svc.Publish(e.ctx, child.ID, child.Version)
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.svc.URLFor(e.ctx, &childPub.Entry)
	if err != nil || u != "/about/team/" {
		t.Fatalf("url = %q %v", u, err)
	}
	if err := e.svc.Trash(e.ctx, childPub.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Save(e.ctx, content.SaveInput{ID: childPub.ID, ExpectedVersion: childPub.Version}); !errors.Is(err, content.ErrTrashed) {
		t.Fatalf("saved a trashed entry: %v", err)
	}
	if err := e.svc.Restore(e.ctx, childPub.ID); err != nil {
		t.Fatal(err)
	}
	// Editors cannot delete users or see security; sanity-check capability errors.
	ed := setup(t, rbac.Editor)
	var fe fields.Errors
	_, err = ed.svc.Publish(ed.ctx, uuid.New(), 0)
	if err == nil || errors.As(err, &fe) {
		t.Fatalf("expected not found, got %v", err)
	}
}
