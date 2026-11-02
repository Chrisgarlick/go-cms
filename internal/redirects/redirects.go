// Package redirects matches request paths against redirect rules (spec 4.5).
// Exact rules live in a map, prefix rules in a sorted list searched longest
// first, regex rules are compiled once (RE2: linear time). Tables rebuild on
// RedirectsChanged events; hit counts are batched.
package redirects

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/counters"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/events"
	"cms/internal/fields"
	"cms/internal/jobs"
	"cms/internal/rbac"
)

const (
	MaxRegexRules   = 500
	MaxPatternLen   = 512
	maxChainFollows = 10
)

// Match is a redirect decision.
type Match struct {
	ID     uuid.UUID
	Status int
	Target string // absolute path or URL; empty for 410/451
}

type rule struct {
	id            uuid.UUID
	host          string
	source        string
	re            *regexp.Regexp
	target        string
	status        int
	preserveQuery bool
	params        []param // exact rules with a query: what the query must hold
}

// param is one part of a query rule: key=literal, or key={name}, which
// captures the value for the target.
type param struct {
	key, value, capture string
}

type table struct {
	exact  map[string]rule   // host + "\x00" + path
	query  map[string][]rule // host + "\x00" + path: rules with a query, most specific first
	prefix []rule            // sorted by source length desc
	regex  []rule
}

type Service struct {
	DB   *db.DB
	Jobs jobs.Enqueuer
	// AllowedHosts may be redirect targets besides the site itself.
	AllowedHosts []string
	SiteHost     string

	t        atomic.Pointer[table]
	mu       sync.Mutex
	hits     *counters.Counter[uuid.UUID]
	notFound *counters.Counter[[3]string]
}

func (s *Service) init() {
	s.mu.Lock()
	if s.hits == nil {
		s.hits = counters.New[uuid.UUID](20000)
		s.notFound = counters.New[[3]string](10000)
	}
	s.mu.Unlock()
}

// Reload rebuilds the match tables from the database.
func (s *Service) Reload(ctx context.Context) error {
	s.init()
	rows, err := s.DB.Q.ListRedirects(ctx)
	if err != nil {
		return err
	}
	s.t.Store(build(rows))
	return nil
}

// build turns stored rules into match tables.
func build(rows []dbq.Redirect) *table {
	t := &table{exact: map[string]rule{}, query: map[string][]rule{}}
	for _, r := range rows {
		ru := rule{id: r.ID, host: r.Host, source: r.Source, target: r.Target, status: int(r.StatusCode), preserveQuery: r.PreserveQuery}
		switch r.MatchType {
		case "exact":
			if path, q, ok := strings.Cut(r.Source, "?"); ok {
				ps, err := parseQueryPattern(q)
				if err != nil {
					continue // validated on save
				}
				ru.params = ps
				k := r.Host + "\x00" + normalise(path)
				t.query[k] = append(t.query[k], ru)
				continue
			}
			t.exact[r.Host+"\x00"+normalise(r.Source)] = ru
		case "prefix":
			ru.source = normalise(r.Source)
			t.prefix = append(t.prefix, ru)
		case "regex":
			re, err := regexp.Compile("^(?:" + r.Source + ")$")
			if err != nil {
				continue // validated on save; skip anything that slipped through
			}
			ru.re = re
			t.regex = append(t.regex, ru)
		}
	}
	sort.Slice(t.prefix, func(i, j int) bool { return len(t.prefix[i].source) > len(t.prefix[j].source) })
	// Most specific first: more parts, then more literal values (page=1
	// before page={n}).
	literals := func(r rule) int {
		n := 0
		for _, p := range r.params {
			if p.capture == "" {
				n++
			}
		}
		return n
	}
	for _, rs := range t.query {
		sort.SliceStable(rs, func(i, j int) bool {
			if len(rs[i].params) != len(rs[j].params) {
				return len(rs[i].params) > len(rs[j].params)
			}
			return literals(rs[i]) > literals(rs[j])
		})
	}
	return t
}

// normalise lowercases nothing (paths are case-sensitive) but ensures a
// leading slash and removes a trailing slash except for "/".
func normalise(p string) string {
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if len(p) > 1 {
		p = strings.TrimRight(p, "/")
	}
	return p
}

// SameSource reports whether a stored source and a source as written (in a
// fixture, say) are the same rule, after the normalising Create applies.
func SameSource(stored, written, matchType string) bool {
	if matchType == "regex" {
		return stored == strings.TrimSpace(written)
	}
	c, err := canonicalSource(strings.TrimSpace(written), matchType)
	return err == nil && stored == c
}

var captureRe = regexp.MustCompile(`^\{([a-z_][a-z0-9_]*)\}$`)

// parseQueryPattern reads "category={c}&page=1": every key must be there,
// with that literal value or any non-empty value (captured).
func parseQueryPattern(q string) ([]param, error) {
	var out []param
	seen := map[string]bool{}
	for _, part := range strings.Split(q, "&") {
		k, v, _ := strings.Cut(part, "=")
		k, err := url.QueryUnescape(k)
		if err != nil || k == "" || seen[k] {
			return nil, fmt.Errorf("query part %q: each key once, as key=value or key={name}", part)
		}
		seen[k] = true
		p := param{key: k}
		if m := captureRe.FindStringSubmatch(v); m != nil {
			p.capture = m[1]
		} else if v == "" || strings.ContainsAny(v, "{}") {
			return nil, fmt.Errorf("query part %q: a value, or {name} to capture it", part)
		} else if p.value, err = url.QueryUnescape(v); err != nil {
			return nil, fmt.Errorf("query part %q: %v", part, err)
		}
		out = append(out, p)
	}
	return out, nil
}

// canonicalSource is the stored form of an exact or prefix source: the
// path normalised and, for an exact rule, a query pattern with sorted keys.
func canonicalSource(src, matchType string) (string, error) {
	u, err := url.Parse(src)
	if err != nil || u.Scheme != "" || u.Host != "" {
		return "", errors.New("must be a path such as /old-page")
	}
	path := normalise(u.Path)
	if u.RawQuery == "" {
		return path, nil
	}
	if matchType != "exact" {
		return "", errors.New("only an exact rule can match a query string")
	}
	ps, err := parseQueryPattern(u.RawQuery)
	if err != nil {
		return "", err
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].key < ps[j].key })
	parts := make([]string, len(ps))
	for i, p := range ps {
		v := "{" + p.capture + "}"
		if p.capture == "" {
			v = url.QueryEscape(p.value)
		}
		parts[i] = url.QueryEscape(p.key) + "=" + v
	}
	return path + "?" + strings.Join(parts, "&"), nil
}

// matchQuery reports whether a request's query holds every part of a rule
// and returns the captured values.
func matchQuery(ps []param, q url.Values) (map[string]string, bool) {
	caps := map[string]string{}
	for _, p := range ps {
		vs := q[p.key]
		if len(vs) != 1 || vs[0] == "" {
			return nil, false
		}
		if p.capture != "" {
			caps[p.capture] = vs[0]
		} else if vs[0] != p.value {
			return nil, false
		}
	}
	return caps, true
}

// Lookup finds the rule for a request. host is the request host.
func (s *Service) Lookup(host, path, rawQuery string) (Match, bool) {
	t := s.t.Load()
	if t == nil {
		return Match{}, false
	}
	np := normalise(path)
	var hit *rule
	// Query rules first: /blog?category=news is more specific than /blog.
	// The request's other parameters are dropped.
	if rawQuery != "" && len(t.query) > 0 {
		if q, err := url.ParseQuery(rawQuery); err == nil {
			for _, h := range []string{host, ""} {
				for i := range t.query[h+"\x00"+np] {
					r := &t.query[h+"\x00"+np][i]
					caps, ok := matchQuery(r.params, q)
					if !ok {
						continue
					}
					target := r.target
					for name, v := range caps {
						target = strings.ReplaceAll(target, "{"+name+"}", url.PathEscape(v))
					}
					if r.status == 410 || r.status == 451 {
						target = ""
					}
					s.hits.Add(r.id, 1)
					return Match{ID: r.id, Status: r.status, Target: target}, true
				}
			}
		}
	}
	for _, h := range []string{host, ""} {
		if r, ok := t.exact[h+"\x00"+np]; ok {
			hit = &r
			break
		}
	}
	target := ""
	if hit == nil {
		for i := range t.prefix {
			r := &t.prefix[i]
			if (r.host == "" || r.host == host) && (np == r.source || strings.HasPrefix(np, r.source+"/")) {
				hit = r
				rest := strings.TrimPrefix(np, r.source)
				target = strings.TrimRight(r.target, "/") + rest
				break
			}
		}
	}
	if hit == nil {
		for i := range t.regex {
			r := &t.regex[i]
			if (r.host == "" || r.host == host) && r.re.MatchString(np) {
				hit = r
				target = r.re.ReplaceAllString(np, r.target)
				break
			}
		}
	}
	if hit == nil {
		return Match{}, false
	}
	if target == "" {
		target = hit.target
	}
	if hit.status == 410 || hit.status == 451 {
		target = ""
	} else if hit.preserveQuery && rawQuery != "" {
		sep := "?"
		if strings.Contains(target, "?") {
			sep = "&"
		}
		target += sep + rawQuery
	}
	s.hits.Add(hit.id, 1)
	return Match{ID: hit.id, Status: hit.status, Target: target}, true
}

// RecordNotFound counts a 404 (batched; at most 10k distinct paths per window).
func (s *Service) RecordNotFound(host, path, referrer string) {
	s.init()
	if len(path) > 512 {
		path = path[:512]
	}
	if len(referrer) > 512 {
		referrer = referrer[:512]
	}
	s.notFound.Add([3]string{host, path, referrer}, 1)
}

// Flush writes batched counters (called every minute).
func (s *Service) Flush(ctx context.Context) error {
	s.init()
	err1 := s.hits.Flush(ctx, func(ctx context.Context, id uuid.UUID, n int64) error {
		return s.DB.Q.AddRedirectHits(ctx, dbq.AddRedirectHitsParams{N: n, ID: id})
	})
	err2 := s.notFound.Flush(ctx, func(ctx context.Context, k [3]string, n int64) error {
		return s.DB.Q.RecordNotFound(ctx, dbq.RecordNotFoundParams{Host: k[0], Path: k[1], Referrer: k[2], N: n})
	})
	return errors.Join(err1, err2)
}

// Input is a redirect from the admin, API, CLI or CSV import.
type Input struct {
	Host          string
	Source        string
	MatchType     string
	Target        string
	Status        int
	PreserveQuery bool
	Note          string
}

func (s *Service) validate(in *Input) error {
	errs := fields.Errors{}
	in.Source = strings.TrimSpace(in.Source)
	in.Target = strings.TrimSpace(in.Target)
	in.Host = strings.ToLower(strings.TrimSpace(in.Host))
	if in.MatchType == "" {
		in.MatchType = "exact"
	}
	if in.Status == 0 {
		in.Status = 301
	}
	switch in.Status {
	case 301, 302, 307, 308, 410, 451:
	default:
		errs["status"] = "must be 301, 302, 307, 308, 410 or 451"
	}
	if len(in.Source) == 0 || len(in.Source) > MaxPatternLen {
		errs["source"] = fmt.Sprintf("is required (at most %d characters)", MaxPatternLen)
	}
	switch in.MatchType {
	case "exact", "prefix":
		if c, err := canonicalSource(in.Source, in.MatchType); err != nil {
			errs["source"] = err.Error()
		} else {
			in.Source = c
		}
	case "regex":
		if _, err := regexp.Compile("^(?:" + in.Source + ")$"); err != nil {
			errs["source"] = "is not a valid regular expression: " + err.Error()
		}
	default:
		errs["match_type"] = "must be exact, prefix or regex"
	}
	if in.Status != 410 && in.Status != 451 {
		if in.Target == "" {
			errs["target"] = "is required"
		} else if err := s.checkTarget(in.Target); err != nil {
			errs["target"] = err.Error()
		}
	} else {
		in.Target = ""
	}
	if _, q, isQuery := strings.Cut(in.Source, "?"); isQuery && errs["source"] == "" && in.MatchType == "exact" {
		// Every {name} in the target must be captured by the query.
		ps, _ := parseQueryPattern(q)
		names := map[string]bool{}
		for _, p := range ps {
			if p.capture != "" {
				names[p.capture] = true
			}
		}
		for _, m := range regexp.MustCompile(`\{([^}]*)\}`).FindAllStringSubmatch(in.Target, -1) {
			if !names[m[1]] {
				errs["target"] = "{" + m[1] + "} isn't captured by the source's query"
			}
		}
		// The request's other parameters are dropped, and a rule may point at
		// its own path (/blog?page=1 → /blog): the query is what changes.
		in.PreserveQuery = false
	} else if in.MatchType == "exact" && in.Target != "" && normalise(in.Target) == in.Source {
		errs["target"] = "points at itself"
	}
	if len(errs) > 0 {
		return errs
	}
	return nil
}

// checkTarget blocks open redirects: relative paths, the site's own host, or
// explicitly allow-listed hosts only (spec 4.5).
func (s *Service) checkTarget(t string) error {
	if strings.HasPrefix(t, "//") || strings.ContainsAny(t, "\r\n\\") {
		return errors.New("must be a path or an allowed URL")
	}
	if strings.HasPrefix(t, "/") {
		return nil
	}
	u, err := url.Parse(t)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return errors.New("must start with / or be an http(s) URL")
	}
	h := strings.ToLower(u.Hostname())
	if h == s.SiteHost {
		return nil
	}
	for _, a := range s.AllowedHosts {
		if h == strings.ToLower(a) {
			return nil
		}
	}
	return fmt.Errorf("external host %s is not in the redirect allow-list", h)
}

func (s *Service) List(ctx context.Context) ([]dbq.Redirect, error) {
	if _, err := actor.Require(ctx, rbac.RedirectManage); err != nil {
		return nil, err
	}
	return s.DB.Q.ListRedirects(ctx)
}

func (s *Service) Create(ctx context.Context, in Input) (dbq.Redirect, error) {
	a, err := actor.Require(ctx, rbac.RedirectManage)
	if err != nil {
		return dbq.Redirect{}, err
	}
	if err := s.validate(&in); err != nil {
		return dbq.Redirect{}, err
	}
	var out dbq.Redirect
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if in.MatchType == "regex" {
			rows, err := q.ListRedirects(ctx)
			if err != nil {
				return err
			}
			n := 0
			for _, r := range rows {
				if r.MatchType == "regex" {
					n++
				}
			}
			if n >= MaxRegexRules {
				return fmt.Errorf("at most %d regex redirects are allowed", MaxRegexRules)
			}
		}
		var by *uuid.UUID
		if a.IsUser() {
			u := a.UserID
			by = &u
		}
		var err error
		out, err = q.CreateRedirect(ctx, dbq.CreateRedirectParams{ID: uuid.Must(uuid.NewV7()), Host: in.Host, Source: in.Source,
			MatchType: in.MatchType, Target: in.Target, StatusCode: int32(in.Status), PreserveQuery: in.PreserveQuery, Note: in.Note, CreatedBy: by})
		if err != nil {
			return err
		}
		if in.MatchType == "exact" && in.Target != "" {
			// Flatten chains that ended at this source.
			if _, err := q.RetargetRedirects(ctx, dbq.RetargetRedirectsParams{NewTarget: in.Target, OldTarget: in.Source + "/"}); err != nil {
				return err
			}
		}
		if err := s.afterChange(ctx, tx, q, in.Source); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "redirect.create", ResourceType: "redirect", ResourceID: out.ID.String(),
			Diff: map[string]any{"source": in.Source, "target": in.Target, "status": in.Status}})
	})
	return out, err
}

func (s *Service) Update(ctx context.Context, id uuid.UUID, in Input) (dbq.Redirect, error) {
	if _, err := actor.Require(ctx, rbac.RedirectManage); err != nil {
		return dbq.Redirect{}, err
	}
	if err := s.validate(&in); err != nil {
		return dbq.Redirect{}, err
	}
	var out dbq.Redirect
	err := s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var err error
		out, err = q.UpdateRedirect(ctx, dbq.UpdateRedirectParams{ID: id, Host: in.Host, Source: in.Source, MatchType: in.MatchType,
			Target: in.Target, StatusCode: int32(in.Status), PreserveQuery: in.PreserveQuery, Note: in.Note})
		if db.IsNotFound(err) {
			return errors.New("redirect not found")
		}
		if db.IsUniqueViolation(err, "") {
			return fields.Errors{"source": "another redirect already uses this source"}
		}
		if err != nil {
			return err
		}
		if err := s.afterChange(ctx, tx, q, in.Source); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "redirect.update", ResourceType: "redirect", ResourceID: id.String()})
	})
	return out, err
}

func (s *Service) Delete(ctx context.Context, id uuid.UUID) error {
	if _, err := actor.Require(ctx, rbac.RedirectManage); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		r, err := q.GetRedirect(ctx, id)
		if err != nil {
			return errors.New("redirect not found")
		}
		if err := q.DeleteRedirect(ctx, id); err != nil {
			return err
		}
		if err := s.afterChange(ctx, tx, q, r.Source); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "redirect.delete", ResourceType: "redirect", ResourceID: id.String()})
	})
}

// afterChange: a new exact redirect must win over a rendered file at its
// source, so that file is re-evaluated (spec 4.5); every process reloads.
func (s *Service) afterChange(ctx context.Context, tx pgx.Tx, q *dbq.Queries, source string) error {
	if strings.HasPrefix(source, "/") && !strings.ContainsAny(source, "()[]*+?^$|\\") {
		p := source
		if !strings.Contains(p, ".") {
			p = strings.TrimRight(p, "/") + "/"
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Paths: []string{p}}); err != nil {
			return err
		}
	}
	return events.Notify(ctx, tx, events.Event{Topic: events.RedirectsChanged})
}

// Test reports what a path resolves to, following chains (admin/CLI tool).
func (s *Service) Test(path string) []Match {
	var out []Match
	seen := map[string]bool{}
	cur := path
	for i := 0; i < maxChainFollows; i++ {
		u, _ := url.Parse(cur)
		if u == nil || u.Host != "" {
			break
		}
		m, ok := s.Lookup(s.SiteHost, u.Path, u.RawQuery)
		if !ok {
			break
		}
		out = append(out, m)
		if m.Target == "" || seen[m.Target] {
			break
		}
		seen[m.Target] = true
		cur = m.Target
	}
	return out
}

// Problems lists redirect loops and chains (`cms redirects check`).
func (s *Service) Problems(ctx context.Context) ([]string, error) {
	rows, err := s.DB.Q.ListRedirects(ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if r.MatchType != "exact" {
			continue
		}
		chain := s.Test(r.Source)
		if len(chain) >= maxChainFollows {
			out = append(out, fmt.Sprintf("loop: %s", r.Source))
		} else if len(chain) > 1 {
			out = append(out, fmt.Sprintf("chain of %d: %s -> %s", len(chain), r.Source, chain[len(chain)-1].Target))
		}
	}
	return out, nil
}

func (s *Service) NotFound(ctx context.Context, limit int32) ([]dbq.NotFoundLog, error) {
	if _, err := actor.Require(ctx, rbac.RedirectManage); err != nil {
		return nil, err
	}
	return s.DB.Q.TopNotFound(ctx, limit)
}
