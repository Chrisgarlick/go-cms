package redirects

import (
	"testing"

	"github.com/google/uuid"

	"cms/internal/counters"
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
