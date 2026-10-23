package site

import (
	"fmt"
	"strings"
)

// Pattern is a URL pattern like /blog/{year}/{slug} or /{path}.
type Pattern struct {
	Raw      string
	segs     []seg
	HasPath  bool
	Literals int
}

type seg struct {
	lit   string
	token string // slug | path | year | month | day
}

var tokens = map[string]bool{"slug": true, "path": true, "year": true, "month": true, "day": true}

func ParsePattern(raw string) (*Pattern, error) {
	if !strings.HasPrefix(raw, "/") {
		return nil, fmt.Errorf("must start with /")
	}
	p := &Pattern{Raw: raw}
	parts := strings.Split(strings.Trim(raw, "/"), "/")
	hasSlug := false
	for i, part := range parts {
		if part == "" {
			return nil, fmt.Errorf("empty segment in %q", raw)
		}
		if strings.HasPrefix(part, "{") && strings.HasSuffix(part, "}") {
			tok := part[1 : len(part)-1]
			if !tokens[tok] {
				return nil, fmt.Errorf("unknown token {%s} (slug, path, year, month, day)", tok)
			}
			if tok == "path" {
				if i != len(parts)-1 {
					return nil, fmt.Errorf("{path} must be the last segment")
				}
				p.HasPath = true
			}
			if tok == "slug" || tok == "path" {
				if hasSlug {
					return nil, fmt.Errorf("only one {slug} or {path} allowed")
				}
				hasSlug = true
			}
			p.segs = append(p.segs, seg{token: tok})
			continue
		}
		if strings.ContainsAny(part, "{}") {
			return nil, fmt.Errorf("tokens must fill a whole segment: %q", part)
		}
		p.segs = append(p.segs, seg{lit: part})
		p.Literals++
	}
	if !hasSlug {
		return nil, fmt.Errorf("must contain {slug} or {path}")
	}
	return p, nil
}

// Key is the pattern's shape, used to detect clashes.
func (p *Pattern) Key() string {
	var b strings.Builder
	for _, s := range p.segs {
		b.WriteByte('/')
		switch {
		case s.lit != "":
			b.WriteString(s.lit)
		case s.token == "path":
			b.WriteString("**")
		default:
			b.WriteString("*")
		}
	}
	return b.String()
}

// Match extracts token values from a request path (no trailing slash handling
// beyond trimming). Year/month/day must be numeric.
func (p *Pattern) Match(path string) (map[string]string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) == 1 && parts[0] == "" {
		return nil, false
	}
	vals := map[string]string{}
	for i, s := range p.segs {
		if s.token == "path" {
			if i >= len(parts) {
				return nil, false
			}
			rest := parts[i:]
			for _, r := range rest {
				if r == "" {
					return nil, false
				}
			}
			vals["path"] = strings.Join(rest, "/")
			vals["slug"] = rest[len(rest)-1]
			return vals, true
		}
		if i >= len(parts) {
			return nil, false
		}
		part := parts[i]
		if s.lit != "" {
			if part != s.lit {
				return nil, false
			}
			continue
		}
		switch s.token {
		case "year":
			if len(part) != 4 || !digits(part) {
				return nil, false
			}
		case "month", "day":
			if len(part) != 2 || !digits(part) {
				return nil, false
			}
		}
		vals[s.token] = part
	}
	if len(parts) != len(p.segs) {
		return nil, false
	}
	return vals, true
}

// Build fills the pattern. Always returns a path with a trailing slash.
func (p *Pattern) Build(vals map[string]string) string {
	var b strings.Builder
	for _, s := range p.segs {
		b.WriteByte('/')
		if s.lit != "" {
			b.WriteString(s.lit)
		} else {
			b.WriteString(vals[s.token])
		}
	}
	b.WriteByte('/')
	return b.String()
}

func digits(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}
