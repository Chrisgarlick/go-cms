package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cms/internal/config"
)

// A monorepo feed: app versions, two sites' bundles, a draft and a prerelease,
// newest first.
func mixedFeed() []release {
	return []release{
		{TagName: "docs-20261022-120500-aaaaaaa"},
		{TagName: "v1.4.0-rc.1", Prerelease: true},
		{TagName: "site-20261022-120000-bbbbbbb", Draft: true},
		{TagName: "v1.3.0", Body: "notes\nchannel:edge"},
		{TagName: "docs-20261021-090000-ccccccc"},
		{TagName: "site-20261021-080000-ddddddd"},
		{TagName: "v1.2.0", Body: "channel:stable"},
		{TagName: "sites-20261020-080000-eeeeeee"}, // a different prefix that shares a stem
		{TagName: "site-20261020-070000-fffffff"},
		{TagName: "v1.1.0", Body: "channel:canary"},
		{TagName: "v2"}, // not a full version
	}
}

func TestSelectRelease(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    feedFilter
		want string // "" = no match
	}{
		{"docs bundle", feedFilter{SitePrefix: "docs"}, "docs-20261022-120500-aaaaaaa"},
		{"site bundle skips the draft", feedFilter{SitePrefix: "site"}, "site-20261021-080000-ddddddd"},
		{"prefix must match exactly", feedFilter{SitePrefix: "sites"}, "sites-20261020-080000-eeeeeee"},
		{"edge takes any version", feedFilter{Channel: "edge"}, "v1.3.0"},
		{"stable", feedFilter{Channel: "stable"}, "v1.2.0"},
		{"canary takes canary or stable", feedFilter{Channel: "canary"}, "v1.2.0"},
		{"unknown prefix finds nothing", feedFilter{SitePrefix: "blog"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := selectRelease(mixedFeed(), tc.f)
			if err != nil {
				t.Fatal(err)
			}
			gotTag := ""
			if got != nil {
				gotTag = got.TagName
			}
			if gotTag != tc.want {
				t.Fatalf("got %q, want %q", gotTag, tc.want)
			}
		})
	}
	// Core never picks a bundle, even when bundles are all that is newest.
	onlyBundles := []release{{TagName: "docs-20261022-120500-aaaaaaa"}, {TagName: "site-20261022-120000-bbbbbbb"}}
	if r, _ := selectRelease(onlyBundles, feedFilter{Channel: "edge"}); r != nil {
		t.Fatalf("core picked %s", r.TagName)
	}
	for _, bad := range []string{"v1", "v2-docs", "Docs", "", "a b"} {
		if bad == "" {
			continue // empty means core
		}
		if _, err := selectRelease(mixedFeed(), feedFilter{SitePrefix: bad}); err == nil {
			t.Errorf("prefix %q accepted", bad)
		}
	}
	if _, err := selectRelease(mixedFeed(), feedFilter{Channel: "nightly"}); err == nil {
		t.Error("unknown channel accepted")
	}
}

// The feed is paged: a site bundle 150 releases deep must still be found, and
// a missing one must fail rather than fall back to anything.
func TestLatestReleasePaginates(t *testing.T) {
	var all []release
	for i := 0; i < 150; i++ {
		all = append(all, release{TagName: fmt.Sprintf("docs-20261022-%06d-aaaaaaa", 150-i)})
	}
	all = append(all, release{TagName: "site-20261001-000000-1234567"})
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("per_page = %q", r.URL.Query().Get("per_page"))
		}
		page := 1
		fmt.Sscan(r.URL.Query().Get("page"), &page)
		start, end := (page-1)*100, page*100
		if end > len(all) {
			end = len(all)
		}
		if end < len(all) {
			w.Header().Set("Link", fmt.Sprintf(`<%s/releases?per_page=100&page=%d>; rel="next", <%s/releases?page=9>; rel="last"`, srv.URL, page+1, srv.URL))
		}
		_ = json.NewEncoder(w).Encode(all[start:end])
	}))
	defer srv.Close()
	old := httpClient
	httpClient = srv.Client
	defer func() { httpClient = old }()
	cfg := &config.Config{}
	ctx := context.Background()

	rel, err := latestRelease(ctx, cfg, srv.URL+"/releases", feedFilter{SitePrefix: "site"})
	if err != nil || rel.TagName != "site-20261001-000000-1234567" {
		t.Fatalf("site on page 2: %v %v", rel, err)
	}
	if _, err := latestRelease(ctx, cfg, srv.URL+"/releases", feedFilter{SitePrefix: "blog"}); err == nil || !strings.Contains(err.Error(), "blog") {
		t.Fatalf("missing prefix must fail closed, got %v", err)
	}
	if _, err := latestRelease(ctx, cfg, srv.URL+"/releases", feedFilter{Channel: "stable"}); err == nil {
		t.Fatal("a feed of bundles has no core release")
	}
}

func TestNextLink(t *testing.T) {
	h := `<https://api.github.com/repositories/1/releases?page=2>; rel="next", <https://api.github.com/repositories/1/releases?page=5>; rel="last"`
	if got := nextLink(h); got != "https://api.github.com/repositories/1/releases?page=2" {
		t.Fatal(got)
	}
	if nextLink(`<x>; rel="prev"`) != "" || nextLink("") != "" {
		t.Fatal("no next link expected")
	}
}

func TestCheckBundleIdentity(t *testing.T) {
	mk := func(name, bundleJSON string) string {
		d := t.TempDir()
		_ = os.WriteFile(filepath.Join(d, "site.yaml"), []byte("name: "+name+"\n"), 0o644)
		if bundleJSON != "" {
			_ = os.WriteFile(filepath.Join(d, "bundle.json"), []byte(bundleJSON), 0o644)
		}
		return d
	}
	docsNow := mk("Kritano Docs", "")
	docsNew := mk("Kritano Docs", `{"prefix":"docs","site":"Kritano Docs"}`)
	siteNew := mk("Kritano", `{"prefix":"site","site":"Kritano"}`)
	legacy := mk("Kritano Docs", "") // built before bundle.json existed

	if err := checkBundleIdentity(docsNew, docsNow, "docs"); err != nil {
		t.Fatalf("same site: %v", err)
	}
	if err := checkBundleIdentity(siteNew, docsNow, "docs"); err == nil {
		t.Fatal("kritano.com bundle accepted by the docs server")
	}
	if err := checkBundleIdentity(mk("Kritano", `{"prefix":"docs","site":"Kritano"}`), docsNow, "docs"); err == nil {
		t.Fatal("right prefix but different site name accepted")
	}
	if err := checkBundleIdentity(mk("Kritano Docs", `{"prefix":"docs","site":"Other"}`), "", "docs"); err == nil {
		t.Fatal("bundle.json disagreeing with site.yaml accepted")
	}
	if err := checkBundleIdentity(siteNew, "", "site"); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if err := checkBundleIdentity(legacy, docsNow, "docs"); err != nil {
		t.Fatalf("legacy bundle for the same site: %v", err)
	}
}
