package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"cms/internal/config"
	"cms/internal/netx"
)

// Release feeds may mix several streams: in a monorepo the same GitHub
// releases list holds core-style versions (v1.2.3) and several sites' bundles
// (docs-…, site-…). Selection is therefore by tag pattern, never "the newest
// release", and fails closed when nothing matches (kritano plans/ci.md §6).

// release is the subset of the GitHub releases API we use.
type release struct {
	TagName    string `json:"tag_name"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Body       string `json:"body"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"assets"`
}

var (
	versionTag   = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)
	sitePrefixRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,30}$`)
)

// feedFilter says which releases in a feed belong to this install.
type feedFilter struct {
	// SitePrefix selects site bundles tagged <prefix>-YYYYMMDD-HHMMSS-<sha7>.
	// Empty means core releases (vX.Y.Z) filtered by Channel.
	SitePrefix string
	Channel    string
}

func (f feedFilter) siteTag() (*regexp.Regexp, error) {
	if !sitePrefixRe.MatchString(f.SitePrefix) || versionTag.MatchString(f.SitePrefix) || regexp.MustCompile(`^v\d`).MatchString(f.SitePrefix) {
		return nil, fmt.Errorf("site tag prefix %q is not valid (lower-case letters, digits and hyphens; must not look like a version)", f.SitePrefix)
	}
	return regexp.MustCompile(`^` + regexp.QuoteMeta(f.SitePrefix) + `-\d{8}-\d{6}-[0-9a-f]{7}$`), nil
}

var channelRank = map[string]int{"edge": 0, "canary": 1, "stable": 2}

// releaseChannel reads the channel marker the promotion job writes into the
// release notes; a release without one is on edge.
func releaseChannel(body string) string {
	best := "edge"
	for name, n := range channelRank {
		if strings.Contains(body, "channel:"+name) && n >= channelRank[best] {
			best = name
		}
	}
	return best
}

// selectRelease returns the first (newest) release in rels that this install
// may use, or nil. rels must be newest first, as the GitHub API returns them.
func selectRelease(rels []release, f feedFilter) (*release, error) {
	var siteRe *regexp.Regexp
	if f.SitePrefix != "" {
		var err error
		if siteRe, err = f.siteTag(); err != nil {
			return nil, err
		}
	}
	want, ok := channelRank[f.Channel]
	if f.SitePrefix == "" && !ok {
		return nil, fmt.Errorf("unknown channel %q (edge, canary or stable)", f.Channel)
	}
	for i := range rels {
		r := &rels[i]
		if r.Draft || r.Prerelease {
			continue
		}
		if siteRe != nil {
			if siteRe.MatchString(r.TagName) {
				return r, nil
			}
			continue
		}
		if versionTag.MatchString(r.TagName) && channelRank[releaseChannel(r.Body)] >= want {
			return r, nil
		}
	}
	return nil, nil
}

const feedMaxPages = 5

// latestRelease walks the feed (100 per page, up to feedMaxPages pages) and
// returns the newest release matching f. No match is an error: the caller
// never falls back to "whatever is newest".
func latestRelease(ctx context.Context, cfg *config.Config, feedURL string, f feedFilter) (*release, error) {
	c, tok := feedClient(cfg)
	next, err := withPerPage(feedURL)
	if err != nil {
		return nil, err
	}
	for page := 0; page < feedMaxPages && next != ""; page++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, next, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.Do(req)
		if err != nil {
			return nil, err
		}
		body, err := netx.ReadLimited(resp.Body, 8<<20)
		resp.Body.Close()
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("release feed returned %d", resp.StatusCode)
		}
		var rels []release
		if err := json.Unmarshal(body, &rels); err != nil {
			return nil, err
		}
		rel, err := selectRelease(rels, f)
		if err != nil {
			return nil, err
		}
		if rel != nil {
			return rel, nil
		}
		next = nextLink(resp.Header.Get("Link"))
	}
	if f.SitePrefix != "" {
		return nil, fmt.Errorf("no site bundle tagged %s-… in the feed", f.SitePrefix)
	}
	return nil, errors.New("no release found for channel " + f.Channel)
}

func withPerPage(feedURL string) (string, error) {
	u, err := url.Parse(feedURL)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if q.Get("per_page") == "" {
		q.Set("per_page", "100")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

// nextLink extracts rel="next" from a GitHub Link header.
func nextLink(h string) string {
	for _, part := range strings.Split(h, ",") {
		seg := strings.Split(part, ";")
		if len(seg) < 2 {
			continue
		}
		for _, p := range seg[1:] {
			if strings.TrimSpace(p) == `rel="next"` {
				return strings.Trim(strings.TrimSpace(seg[0]), "<>")
			}
		}
	}
	return ""
}

// sitePrefix is the configured site tag prefix. "site" is the default, which
// is what single-site repos have always published.
func sitePrefix(cfg *config.Config) string {
	if cfg.Upgrade.SiteTagPrefix != "" {
		return cfg.Upgrade.SiteTagPrefix
	}
	return "site"
}

// bundleInfo is bundle.json, written into every site bundle by the site
// workflow.
type bundleInfo struct {
	Prefix  string `json:"prefix"`
	Site    string `json:"site"`
	Commit  string `json:"commit"`
	BuiltAt string `json:"built_at"`
}

// checkBundleIdentity refuses a bundle built for another site: its prefix must
// match this install's, and its site.yaml name must match the bundle that is
// serving now (skipped on the first install). Bundles from before bundle.json
// existed pass the prefix check only through the tag filter.
func checkBundleIdentity(newDir, currentDir, prefix string) error {
	var bi bundleInfo
	if b, err := os.ReadFile(filepath.Join(newDir, "bundle.json")); err == nil {
		if err := json.Unmarshal(b, &bi); err != nil {
			return fmt.Errorf("bundle.json: %w", err)
		}
		if bi.Prefix != prefix {
			return fmt.Errorf("this bundle was built for %q, but this site installs %q bundles", bi.Prefix, prefix)
		}
	}
	newName, err := siteName(newDir)
	if err != nil {
		return fmt.Errorf("bundle has no readable site.yaml: %w", err)
	}
	if bi.Site != "" && bi.Site != newName {
		return fmt.Errorf("bundle.json names site %q but its site.yaml says %q", bi.Site, newName)
	}
	if currentDir == "" {
		return nil // first install
	}
	curName, err := siteName(currentDir)
	if err != nil {
		return nil // nothing readable is serving; nothing to compare against
	}
	if curName != newName {
		return fmt.Errorf("bundle is for site %q, but this server runs %q", newName, curName)
	}
	return nil
}

func siteName(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, "site.yaml"))
	if err != nil {
		return "", err
	}
	var s struct {
		Name string `yaml:"name"`
	}
	if err := yaml.Unmarshal(b, &s); err != nil {
		return "", err
	}
	return s.Name, nil
}
