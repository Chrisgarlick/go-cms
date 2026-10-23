package site

import "testing"

func TestOut(t *testing.T) {
	off := false
	noSlash := Config{TrailingSlash: &off}
	for in, want := range map[string]string{
		"/":                "/",
		"/about/":          "/about",
		"/blog/post/":      "/blog/post",
		"/blog/page/2/":    "/blog/page/2",
		"/search/?q=x":     "/search?q=x",
		"/about/#team":     "/about#team",
		"/feed.xml":        "/feed.xml",
		"https://x.com/a/": "https://x.com/a/",
		"":                 "",
	} {
		if got := noSlash.Out(in); got != want {
			t.Errorf("Out(%q) = %q, want %q", in, got, want)
		}
	}
	if got := (Config{}).Out("/about/"); got != "/about/" {
		t.Errorf("default Out = %q, want the path unchanged", got)
	}
}
