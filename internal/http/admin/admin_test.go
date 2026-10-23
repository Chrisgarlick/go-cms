package admin_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"cms/internal/app"
	"cms/internal/auth"
	"cms/internal/content"
	"cms/internal/http/admin"
	"cms/internal/http/api"
	"cms/internal/rbac"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

type browser struct {
	t    *testing.T
	c    *http.Client
	base string
	csrf string
}

func newBrowser(t *testing.T, srv *httptest.Server) *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: t, base: srv.URL, c: &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

var csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)

func (b *browser) get(path string) (int, string) {
	resp, err := b.c.Get(b.base + path)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if m := csrfRe.FindStringSubmatch(string(body)); m != nil {
		b.csrf = m[1]
	}
	return resp.StatusCode, string(body)
}

func (b *browser) post(path string, form url.Values, origin bool) (*http.Response, string) {
	req, _ := http.NewRequest(http.MethodPost, b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if origin {
		req.Header.Set("Origin", b.base)
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return resp, string(body)
}

func (b *browser) json(method, path string, v any) (int, map[string]any) {
	data, _ := json.Marshal(v)
	req, _ := http.NewRequest(method, b.base+path, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-CSRF-Token", b.csrf)
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	resp, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func setup(t *testing.T) (*app.App, *httptest.Server) {
	a, _ := testutil.NewApp(t, "blog")
	srv := httptest.NewServer(a.Handler(admin.New(a), api.New(a)))
	t.Cleanup(srv.Close)
	a.Cfg.Hosts = append(a.Cfg.Hosts, "127.0.0.1")
	return a, srv
}

// login signs in a user with a confirmed authenticator, returning a browser.
func login(t *testing.T, a *app.App, srv *httptest.Server, email string, role rbac.Role) *browser {
	sys := app.SystemCtx(context.Background(), "test")
	u, err := a.Auth.CreateUserDirect(sys, email, "Tester", "a perfectly long passphrase", role)
	if err != nil {
		t.Fatal(err)
	}
	b := newBrowser(t, srv)
	// Enrol 2FA directly so the flow exercises the MFA step.
	en, err := a.Auth.BeginEnroll(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessTok, err := a.Auth.Login(context.Background(), email, "a perfectly long passphrase", netipZero(), "")
	if err != nil {
		t.Fatal(err)
	}
	s, _ := a.Auth.Authenticate(context.Background(), sessTok.Token)
	code, _ := totp.GenerateCode(en.Secret, time.Now().Add(-30*time.Second))
	if _, _, err := a.Auth.ConfirmEnroll(context.Background(), s, code, netipZero(), ""); err != nil {
		t.Fatal(err)
	}
	// Real sign-in through the forms.
	b.get("/admin/login")
	resp, _ := b.post("/admin/login", url.Values{"email": {email}, "password": {"a perfectly long passphrase"}, "next": {"/admin/"}}, true)
	if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/admin/login/2fa") {
		t.Fatalf("login redirected to %q, want 2fa", loc)
	}
	b.get("/admin/login/2fa")
	code, _ = totp.GenerateCode(en.Secret, time.Now())
	resp, _ = b.post("/admin/login/2fa", url.Values{"code": {code}, "csrf": {b.csrf}, "next": {"/admin/"}}, true)
	if resp.Header.Get("Location") != "/admin/" {
		t.Fatalf("2fa redirected to %q", resp.Header.Get("Location"))
	}
	if st, _ := b.get("/admin/"); st != 200 {
		t.Fatalf("dashboard status %d", st)
	}
	return b
}

func TestAdminPagesAndEditorFlow(t *testing.T) {
	a, srv := setup(t)
	b := login(t, a, srv, "owner@example.test", rbac.Owner)
	for _, p := range []string{"/admin/", "/admin/content/post/", "/admin/content/page/", "/admin/media/", "/admin/terms/category/",
		"/admin/menus/", "/admin/menus/primary", "/admin/redirects/", "/admin/users/", "/admin/settings/", "/admin/account",
		"/admin/security/", "/admin/system/"} {
		if st, body := b.get(p); st != 200 {
			t.Errorf("%s: status %d: %.300s", p, st, body)
		}
	}
	// Create, save (JSON API) and publish.
	b.get("/admin/content/post/")
	resp, _ := b.post("/admin/content/post/new", url.Values{"csrf": {b.csrf}}, true)
	loc := resp.Header.Get("Location")
	if !strings.HasPrefix(loc, "/admin/entries/") {
		t.Fatalf("new entry redirect %q", loc)
	}
	id := strings.TrimPrefix(loc, "/admin/entries/")
	if st, body := b.get(loc); st != 200 || !strings.Contains(body, `id="editor-data"`) {
		t.Fatalf("editor: %d", st)
	}
	blocks := []map[string]any{{"id": "a1", "type": "rich-text", "data": map[string]any{"body": map[string]any{"type": "doc",
		"content": []any{map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "Admin body"}}}}}}}}
	st, res := b.json("POST", "/admin/api/entries/"+id+"/save", map[string]any{"version": 1, "title": "From the admin", "slug": "", "blocks": blocks})
	if st != 200 {
		t.Fatalf("save: %d %v", st, res)
	}
	ver := res["entry"].(map[string]any)["version"]
	// Stale version: conflict.
	if st, _ := b.json("POST", "/admin/api/entries/"+id+"/save", map[string]any{"version": 1, "title": "x"}); st != 409 {
		t.Fatalf("stale save status %d, want 409", st)
	}
	st, res = b.json("POST", "/admin/api/entries/"+id+"/action/publish", map[string]any{"version": ver})
	if st != 200 || res["live_url"] != "/blog/from-the-admin/" {
		t.Fatalf("publish: %d %v", st, res)
	}
	if st, body := b.get("/blog/from-the-admin/"); st != 200 || !strings.Contains(body, "Admin body") {
		t.Fatalf("live page: %d", st)
	}
	// Validation errors come back as fields.
	st, res = b.json("POST", "/admin/api/entries/"+id+"/save", map[string]any{"version": ver.(float64) + 1, "title": "x",
		"fields": map[string]any{}, "blocks": []any{map[string]any{"type": "image", "data": map[string]any{}}}})
	if st != 422 {
		t.Fatalf("invalid block: %d %v", st, res)
	}
}

func TestAdminCSRFAndRoles(t *testing.T) {
	a, srv := setup(t)
	b := login(t, a, srv, "ed@example.test", rbac.Editor)
	// Editors cannot open user management or security.
	for _, p := range []string{"/admin/users/", "/admin/settings/", "/admin/security/", "/admin/redirects/"} {
		if st, _ := b.get(p); st != 403 {
			t.Errorf("editor %s: status %d, want 403", p, st)
		}
	}
	b.get("/admin/content/post/")
	// Missing CSRF token.
	if resp, _ := b.post("/admin/content/post/new", url.Values{}, true); resp.StatusCode != 403 {
		t.Errorf("missing csrf: %d", resp.StatusCode)
	}
	// Cross-origin request with a valid token is still refused.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/admin/content/post/new", strings.NewReader("csrf="+b.csrf))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	resp, _ := b.c.Do(req)
	if resp.StatusCode != 403 {
		t.Errorf("cross-site post: %d", resp.StatusCode)
	}
	// Anonymous admin access redirects to login; JSON gets 401.
	anon := newBrowser(t, srv)
	if st, _ := anon.get("/admin/content/post/"); st != 303 {
		t.Errorf("anonymous: %d", st)
	}
	if st, _ := anon.json("GET", "/admin/api/media", nil); st != 401 {
		t.Errorf("anonymous api: %d", st)
	}
	_ = content.ErrNotFound
	_ = auth.StageFull
}
