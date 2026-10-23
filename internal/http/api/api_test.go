package api_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"cms/internal/app"
	"cms/internal/content"
	"cms/internal/fixtures"
	"cms/internal/http/api"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func TestReadAPI(t *testing.T) {
	a, _ := testutil.NewApp(t, "blog")
	sys := app.SystemCtx(context.Background(), "test")
	if _, err := fixtures.Load(sys, a.Site().FS, fixtures.Services{Content: a.Content, Menus: a.Menus, Settings: a.Settings, Q: a.DB.Q}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(a.Handler(nil, api.New(a)))
	defer srv.Close()
	get := func(path string) (int, map[string]any) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		b, _ := io.ReadAll(resp.Body)
		_ = json.Unmarshal(b, &m)
		return resp.StatusCode, m
	}
	st, m := get("/api/v1/content/post?limit=2")
	if st != 200 || len(m["data"].([]any)) != 2 || m["next_cursor"] == nil {
		t.Fatalf("list: %d %v", st, m)
	}
	st, m = get("/api/v1/content/post?limit=2&cursor=" + m["next_cursor"].(string))
	if st != 200 || len(m["data"].([]any)) != 2 {
		t.Fatalf("page 2: %d %v", st, m)
	}
	st, m = get("/api/v1/content/post/why-this-site-is-fast?render=html")
	d, _ := m["data"].(map[string]any)
	if st != 200 || !strings.Contains(d["html"].(string), "<h2") || d["url"] != "/blog/why-this-site-is-fast/" {
		t.Fatalf("get: %d %v", st, m)
	}
	st, m = get("/api/v1/content/post?fields=title")
	first := m["data"].([]any)[0].(map[string]any)
	if _, ok := first["excerpt"]; ok || first["title"] == nil {
		t.Fatalf("fields filter: %v", first)
	}
	if st, m = get("/api/v1/taxonomies/category/terms"); st != 200 || len(m["data"].([]any)) != 3 {
		t.Fatalf("terms: %d %v", st, m)
	}
	if st, m = get("/api/v1/search?q=accessibility"); st != 200 || len(m["data"].([]any)) == 0 {
		t.Fatalf("search: %d %v", st, m)
	}
	if st, _ = get("/api/v1/menus/primary"); st != 200 {
		t.Fatalf("menu: %d", st)
	}
	if st, m = get("/api/openapi.json"); st != 200 || m["openapi"] != "3.1.0" {
		t.Fatalf("openapi: %d", st)
	}
	if st, _ = get("/api/v1/content/nope"); st != 404 {
		t.Fatalf("unknown type: %d", st)
	}
	resp, _ := http.Post(srv.URL+"/api/v1/content/post", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 405 {
		t.Fatalf("write: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/api/v1/content/post", nil)
	req.Header.Set("Authorization", "Bearer cms_pat_bogus")
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	_ = content.ErrNotFound
}
