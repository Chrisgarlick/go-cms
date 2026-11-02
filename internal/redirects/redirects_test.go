package redirects

import (
	"testing"

	"github.com/google/uuid"

	"cms/internal/counters"
	"cms/internal/db/dbq"
)

func newTest(rules []rule, prefix []rule, re []rule) *Service {
	s := &Service{SiteHost: "example.test", hits: counters.New[uuid.UUID](10), notFound: counters.New[[3]string](10)}
	t := &table{exact: map[string]rule{}, prefix: prefix, regex: re}
	for _, r := range rules {
		t.exact[r.host+"\x00"+normalise(r.source)] = r
	}
	s.t.Store(t)
	return s
}

func TestLookup(t *testing.T) {
	s := newTest(
		[]rule{{id: uuid.New(), source: "/old/", target: "/new/", status: 301, preserveQuery: true},
			{id: uuid.New(), source: "/gone", status: 410}},
		[]rule{{id: uuid.New(), source: "/docs", target: "/help", status: 308}},
		nil,
	)
	if m, ok := s.Lookup("example.test", "/old", "a=1"); !ok || m.Target != "/new/?a=1" || m.Status != 301 {
		t.Fatalf("exact: %+v %v", m, ok)
	}
	if m, ok := s.Lookup("example.test", "/gone/", ""); !ok || m.Status != 410 || m.Target != "" {
		t.Fatalf("410: %+v", m)
	}
	if m, ok := s.Lookup("example.test", "/docs/a/b", ""); !ok || m.Target != "/help/a/b" {
		t.Fatalf("prefix: %+v", m)
	}
	if _, ok := s.Lookup("example.test", "/docsx", ""); ok {
		t.Fatal("prefix matched a sibling path")
	}
}

func TestOpenRedirectBlocked(t *testing.T) {
	s := &Service{SiteHost: "example.test", AllowedHosts: []string{"partner.test"}}
	for _, bad := range []string{"//evil.test/x", "https://evil.test/", "javascript:alert(1)", "/ok\r\nSet-Cookie: x"} {
		in := Input{Source: "/a", Target: bad}
		if err := s.validate(&in); err == nil {
			t.Errorf("accepted target %q", bad)
		}
	}
	for _, good := range []string{"/b", "https://example.test/c", "https://partner.test/d"} {
		in := Input{Source: "/a", Target: good}
		if err := s.validate(&in); err != nil {
			t.Errorf("rejected %q: %v", good, err)
		}
	}
}

// Query rules: /blog?category=x → /blog/category/x, the most specific rule
// first, other parameters dropped.
func TestQueryRedirects(t *testing.T) {
	s := &Service{SiteHost: "example.test", hits: counters.New[uuid.UUID](10), notFound: counters.New[[3]string](10)}
	var rows []dbq.Redirect
	for _, in := range []Input{
		{Source: "/blog?category={c}", Target: "/blog/category/{c}"},
		{Source: "/blog?page={n}&category={c}", Target: "/blog/category/{c}/page/{n}"},
		{Source: "/blog?page={n}", Target: "/blog/page/{n}"},
		{Source: "/blog/?page=1", Target: "/blog"},
		{Source: "/blog?tag={t}", Target: "/blog/tag/{t}", Status: 308},
	} {
		if err := s.validate(&in); err != nil {
			t.Fatalf("%s: %v", in.Source, err)
		}
		rows = append(rows, dbq.Redirect{ID: uuid.New(), Source: in.Source, MatchType: in.MatchType, Target: in.Target,
			StatusCode: int32(in.Status), PreserveQuery: in.PreserveQuery})
	}
	if rows[1].Source != "/blog?category={c}&page={n}" || rows[3].Source != "/blog?page=1" {
		t.Errorf("canonical sources: %q %q", rows[1].Source, rows[3].Source)
	}
	s.t.Store(build(rows))
	for _, c := range []struct{ path, query, want string }{
		{"/blog", "category=news", "/blog/category/news"},
		{"/blog", "category=news&utm_source=x", "/blog/category/news"}, // others dropped
		{"/blog", "page=3&category=news", "/blog/category/news/page/3"},
		{"/blog", "page=1", "/blog"}, // the literal beats the capture
		{"/blog", "page=4", "/blog/page/4"},
		{"/blog/", "tag=go", "/blog/tag/go"},
		{"/blog", "category=a%2Fb", "/blog/category/a%2Fb"}, // escaped into the path
	} {
		m, ok := s.Lookup("example.test", c.path, c.query)
		if !ok || m.Target != c.want {
			t.Errorf("%s?%s → %q %v, want %q", c.path, c.query, m.Target, ok, c.want)
		}
	}
	for _, q := range []string{"", "utm_source=x", "category=", "category=a&category=b"} {
		if m, ok := s.Lookup("example.test", "/blog", q); ok {
			t.Errorf("/blog?%s matched %+v", q, m)
		}
	}

	for _, bad := range []Input{
		{Source: "/blog?category={c}", Target: "/blog/{x}"},               // {x} isn't captured
		{Source: "/blog?category={c}", Target: "/c", MatchType: "prefix"}, // prefix rules can't have a query
		{Source: "/blog?category", Target: "/c"},                          // no value
		{Source: "/blog?a=1&a=2", Target: "/c"},                           // a key twice
	} {
		if err := s.validate(&bad); err == nil {
			t.Errorf("accepted %s → %s (%s)", bad.Source, bad.Target, bad.MatchType)
		}
	}
	if !SameSource("/blog?category={c}&page={n}", "/blog/?page={n}&category={c}", "exact") {
		t.Error("SameSource doesn't see a reordered query as the same rule")
	}
}
