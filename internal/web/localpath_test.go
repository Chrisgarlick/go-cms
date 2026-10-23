package web

import "testing"

func TestLocalPath(t *testing.T) {
	for in, want := range map[string]string{
		"/about/":          "/about/",
		"/":                "/",
		"//evil.example/":  "/evil.example/",
		"/\\evil.example/": "/evil.example/",
		"https://x.test/":  "/",
		"about":            "/",
		"/a\r\nSet-Cookie": "/",
	} {
		if got := LocalPath(in); got != want {
			t.Errorf("LocalPath(%q) = %q, want %q", in, got, want)
		}
	}
}
