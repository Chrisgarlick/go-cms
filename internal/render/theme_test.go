package render

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestThemeStaticAndCSSFingerprints(t *testing.T) {
	base := fstest.MapFS{"layouts/default.gohtml": {Data: []byte(`{{define "layout"}}{{end}}`)}}
	site := fstest.MapFS{
		"assets/app.css":           {Data: []byte(`@font-face{src:url(fonts/a.woff2) format("woff2")}b{background:url("data:image/svg+xml,x")}i{background:url(/abs.png)}`)},
		"assets/fonts/a.woff2":     {Data: []byte("font")},
		"static/favicon.ico":       {Data: []byte("ico")},
		"static/og/home.png":       {Data: []byte("png")},
		"static/.well-known/a.txt": {Data: []byte("txt")},
	}
	th, err := LoadTheme(base, site, nil, Funcs())
	if err != nil {
		t.Fatal(err)
	}
	fontURL, _ := th.AssetPath("fonts/a.woff2")
	cssURL, _ := th.AssetPath("app.css")
	css, _, ok := th.OpenAsset(cssURL)
	if !ok {
		t.Fatalf("stylesheet not served at its fingerprinted URL %s", cssURL)
	}
	if !strings.Contains(string(css), `url("`+fontURL+`")`) {
		t.Errorf("font reference not fingerprinted: %s", css)
	}
	if !strings.Contains(string(css), `url("data:image/svg+xml,x")`) || !strings.Contains(string(css), `url(/abs.png)`) {
		t.Errorf("absolute and data: URLs must be left alone: %s", css)
	}
	if b, ok := th.Static("/favicon.ico"); !ok || string(b) != "ico" {
		t.Error("static file not found")
	}
	if !th.HasStatic("og/home.png") || !th.HasStatic(".well-known/a.txt") || th.HasStatic("../site.yaml") {
		t.Error("HasStatic")
	}
}

func TestInline(t *testing.T) {
	for in, want := range map[string]string{
		"Websites that *stay* fast": "Websites that <em>stay</em> fast",
		"**Bold** and *em*":         "<strong>Bold</strong> and <em>em</em>",
		"<script>*x*</script>":      "&lt;script&gt;<em>x</em>&lt;/script&gt;",
		"2 * 3 * 4":                 "2 * 3 * 4",
	} {
		if got := string(inline(in)); got != want {
			t.Errorf("inline(%q) = %q, want %q", in, got, want)
		}
	}
}
