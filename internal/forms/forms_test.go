package forms_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"cms/internal/app"
	"cms/internal/forms"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

func post(t *testing.T, h http.Handler, form string, v url.Values) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest("POST", "/forms/"+form, strings.NewReader(v.Encode()))
	req.Host = "localhost"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestSubmit(t *testing.T) {
	a, mails := testutil.NewApp(t, "brochure")
	ctx := context.Background()
	h := a.Handler(nil, nil)
	now := time.Now()
	a.Forms.Now = func() time.Time { return now }

	// The token endpoint.
	req := httptest.NewRequest("GET", "/forms/contact/token", nil)
	req.Host = "localhost"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var tok struct{ Token string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &tok) != nil || tok.Token == "" {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("token Cache-Control = %q", cc)
	}

	good := url.Values{"_token": {tok.Token}, "name": {"Ada Lovelace"}, "email": {"ada@example.com"}, "message": {"Hello\nthere"}, "unknown": {"dropped"}}

	// Too fast: a person can't fill the form in within two seconds.
	if code, _ := post(t, h, "contact", good); code != http.StatusBadRequest {
		t.Fatalf("instant submit = %d, want 400", code)
	}
	now = now.Add(5 * time.Second)

	// Validation errors come back per field.
	bad := url.Values{"_token": {tok.Token}, "name": {""}, "email": {"not-an-email"}, "message": {"x"}}
	code, body := post(t, h, "contact", bad)
	errs, _ := body["errors"].(map[string]any)
	if code != http.StatusUnprocessableEntity || errs["name"] == nil || errs["email"] == nil {
		t.Fatalf("invalid submit = %d %v", code, body)
	}

	// A filled honeypot looks like success but stores nothing.
	spam := url.Values{"_token": {tok.Token}, forms.Honeypot: {"http://spam"}, "name": {"Bot"}, "email": {"b@example.com"}, "message": {"buy"}}
	if code, _ := post(t, h, "contact", spam); code != 200 {
		t.Fatalf("honeypot = %d", code)
	}

	// A forged token is rejected.
	forged := url.Values{"_token": {"123.abc"}, "name": {"x"}, "email": {"x@example.com"}, "message": {"x"}}
	if code, _ := post(t, h, "contact", forged); code != http.StatusBadRequest {
		t.Fatalf("forged token = %d", code)
	}

	code, body = post(t, h, "contact", good)
	if code != 200 || body["message"] != a.Site().Forms["contact"].Success {
		t.Fatalf("submit = %d %v", code, body)
	}
	subs, err := a.Forms.List(ctx, "contact", 10, 0)
	if err != nil || len(subs) != 1 {
		t.Fatalf("stored = %d, %v (honeypot must not store)", len(subs), err)
	}
	sub := subs[0]
	if sub.Values["email"] != "ada@example.com" || sub.Values["unknown"] != nil || sub.Summary != "Ada Lovelace" {
		t.Fatalf("stored values = %v summary %q", sub.Values, sub.Summary)
	}
	// Stored encrypted: the plaintext is nowhere in the row.
	var raw []byte
	if err := a.DB.Pool.QueryRow(ctx, "SELECT data_enc FROM form_submissions WHERE id = $1", sub.ID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "ada@example.com") {
		t.Fatal("submission stored in plaintext")
	}

	// The notification email.
	if err := a.NotifyForm(ctx, sub.ID); err != nil {
		t.Fatal(err)
	}
	m, ok := mails.Last()
	if !ok || m.To != "hello@example.com" || m.Subject != "New enquiry from Ada Lovelace" || m.ReplyTo != "ada@example.com" ||
		!strings.Contains(m.Text, "How can we help?:\nHello\nthere") {
		t.Fatalf("notification = %+v", m)
	}

	// Rate limit: five per form per address per window.
	for range 4 {
		post(t, h, "contact", good)
	}
	if code, _ := post(t, h, "contact", good); code != http.StatusTooManyRequests {
		t.Fatalf("sixth submit = %d, want 429", code)
	}

	// Unknown forms are 404s.
	if code, _ := post(t, h, "nope", good); code != http.StatusNotFound {
		t.Fatalf("unknown form = %d", code)
	}

	// Delete is audited.
	if err := a.Forms.Delete(app.SystemCtx(ctx, "test"), sub.ID); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := a.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM audit_log WHERE action = 'form.submission.delete'").Scan(&n); err != nil || n != 1 {
		t.Fatalf("audit rows = %d, %v", n, err)
	}
}

func TestPrune(t *testing.T) {
	a, _ := testutil.NewApp(t, "brochure")
	ctx := context.Background()
	now := time.Now().Add(-time.Minute)
	a.Forms.Now = func() time.Time { return now }
	tok := a.Forms.Token("contact")
	now = now.Add(time.Minute)
	if _, err := a.Forms.Submit(ctx, "contact", map[string]any{"name": "Old", "email": "o@example.com", "message": "m"}, tok, "", netip.MustParseAddr("192.0.2.1")); err != nil {
		t.Fatal(err)
	}
	if _, err := a.DB.Pool.Exec(ctx, "UPDATE form_submissions SET created_at = now() - interval '400 days'"); err != nil {
		t.Fatal(err)
	}
	n, err := a.Forms.Prune(ctx)
	if err != nil || n != 1 {
		t.Fatalf("pruned %d, %v", n, err)
	}
}
