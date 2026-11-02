package cli

import (
	"strings"
	"testing"
)

func TestNginxConfig(t *testing.T) {
	s := nginxSite{Name: "kritano-docs", Hosts: []string{"docs.kritano.com", "www.docs.kritano.com"},
		Root: "/var/lib/cms/kritano-docs/public/docs.kritano.com", MediaDir: "/var/lib/cms/kritano-docs/media",
		CSP: "default-src 'self'", Upstream: "/etc/nginx/cms/kritano-docs-upstream.conf", MaxUploadMB: 64, TLS: true}
	conf := nginxConfig(s)
	for _, want := range []string{
		"server_name docs.kritano.com www.docs.kritano.com;\n\tlocation /.well-known/acme-challenge/", // port 80 → https
		"server_name www.docs.kritano.com;",                                     // the alias
		"return 308 https://docs.kritano.com$request_uri;",                      // … redirects to the canonical host
		"ssl_certificate /etc/letsencrypt/live/docs.kritano.com/fullchain.pem;", // the canonical host's certificate
		"include /etc/nginx/cms/kritano-docs-upstream.conf;",
		"location ~ ^/(admin|api|forms|preview|search)(/|$) {\n\t\treturn 418;",
		"alias /var/lib/cms/kritano-docs/media/public/;",
		"if ($args != \"\") {\n\t\t\treturn 418;", // query strings reach Go
		"location ~ ^/.+/$ {",                     // no trailing slash: /about/ reaches Go
		"try_files $uri/index.html $uri @cms;",
		`add_header Content-Security-Policy "default-src 'self'" always;`,
		"client_max_body_size 64m;",
	} {
		if !strings.Contains(conf, want) {
			t.Errorf("missing %q in:\n%s", want, conf)
		}
	}
	// Every location that sets headers repeats the security ones (nginx
	// drops the server's add_headers in a location that has its own).
	if n := strings.Count(conf, "add_header Strict-Transport-Security"); n != 3 {
		t.Errorf("HSTS in %d locations, want 3 (media, assets, pages)", n)
	}
	if strings.Count(conf, "{") != strings.Count(conf, "}") {
		t.Error("unbalanced braces")
	}

	s.Slash, s.TLS, s.MediaDir, s.Hosts = true, false, "", []string{"example.com"}
	conf = nginxConfig(s)
	if strings.Contains(conf, "ssl_certificate") || strings.Contains(conf, "/media/") || strings.Contains(conf, "^/.+/$") ||
		!strings.Contains(conf, "listen 80;") || !strings.Contains(conf, "try_files $uri $uri/index.html @cms;") {
		t.Errorf("plain HTTP, trailing slashes, media elsewhere:\n%s", conf)
	}
}
