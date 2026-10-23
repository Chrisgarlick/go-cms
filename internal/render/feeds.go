package render

import (
	"bytes"
	"context"
	"encoding/xml"
	"fmt"
	"strings"
	"time"

	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/jobs"
	"cms/internal/settings"
	"cms/internal/site"
)

func (r *Renderer) robots(ctx context.Context) *Result {
	var b strings.Builder
	b.WriteString("User-agent: *\n")
	if r.Settings.Bool(ctx, settings.DiscourageBots) {
		b.WriteString("Disallow: /\n")
	} else {
		b.WriteString("Disallow: /admin/\nDisallow: /api/\nDisallow: /preview/\nDisallow: /search/\n")
	}
	if extra := r.Settings.String(ctx, settings.RobotsExtra, ""); extra != "" {
		b.WriteString("\n" + strings.TrimSpace(extra) + "\n")
	}
	fmt.Fprintf(&b, "\nSitemap: %s/sitemap.xml\n", strings.TrimRight(r.BaseURL, "/"))
	return &Result{Status: 200, Body: []byte(b.String()), ContentType: "text/plain; charset=utf-8", Cacheable: true,
		Deps: []jobs.Dep{{Type: "settings", ID: "all"}}}
}

type urlset struct {
	XMLName xml.Name `xml:"urlset"`
	NS      string   `xml:"xmlns,attr"`
	URLs    []smURL  `xml:"url"`
}

type smURL struct {
	Loc     string `xml:"loc"`
	LastMod string `xml:"lastmod,omitempty"`
}

type smIndex struct {
	XMLName  xml.Name `xml:"sitemapindex"`
	NS       string   `xml:"xmlns,attr"`
	Sitemaps []smURL  `xml:"sitemap"`
}

func (r *Renderer) sitemap(ctx context.Context, which string) (*Result, error) {
	b := r.Site()
	base := strings.TrimRight(r.BaseURL, "/")
	var out bytes.Buffer
	out.WriteString(xml.Header)
	enc := xml.NewEncoder(&out)
	var deps []jobs.Dep
	if r.Settings.Bool(ctx, settings.DiscourageBots) {
		which = "empty"
	}
	switch which {
	case "index":
		idx := smIndex{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
		for _, name := range b.TypeOrder {
			if b.Types[name].InSitemap() {
				idx.Sitemaps = append(idx.Sitemaps, smURL{Loc: base + "/sitemap-" + name + ".xml"})
			}
		}
		if len(b.Taxonomies) > 0 {
			idx.Sitemaps = append(idx.Sitemaps, smURL{Loc: base + "/sitemap-taxonomies.xml"})
		}
		if err := enc.Encode(idx); err != nil {
			return nil, err
		}
	case "taxonomies":
		set := urlset{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
		for name, tx := range b.Taxonomies {
			if tx.Pattern() == nil {
				continue
			}
			deps = append(deps, jobs.Dep{Type: "taxonomy", ID: name})
			terms, err := r.DB.Q.ListTerms(ctx, name)
			if err != nil {
				return nil, err
			}
			for _, t := range terms {
				if t.PublishedCount > 0 {
					set.URLs = append(set.URLs, smURL{Loc: base + r.out(r.termURL(name, t.Slug))})
				}
			}
		}
		if err := enc.Encode(set); err != nil {
			return nil, err
		}
	case "empty":
		if err := enc.Encode(urlset{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}); err != nil {
			return nil, err
		}
	default:
		ct := b.Types[which]
		deps = append(deps, jobs.Dep{Type: "type", ID: which})
		rows, err := r.DB.Q.PublishedIDsByType(ctx, which)
		if err != nil {
			return nil, err
		}
		set := urlset{NS: "http://www.sitemaps.org/schemas/sitemap/0.9"}
		front := r.Settings.FrontPageID(ctx)
		for _, row := range rows {
			u, err := content.BuildURL(ctx, r.DB.Q, b, ct, row.Slug, row.ParentID, deref(row.PublishedAt))
			if err != nil || u == "" {
				continue
			}
			if row.ID == front {
				u = "/"
			}
			set.URLs = append(set.URLs, smURL{Loc: base + r.out(u), LastMod: row.UpdatedAt.UTC().Format("2006-01-02")})
		}
		if ct.Archive != "" {
			set.URLs = append(set.URLs, smURL{Loc: base + r.out(strings.TrimRight(ct.Archive, "/")+"/")})
		}
		if err := enc.Encode(set); err != nil {
			return nil, err
		}
	}
	return &Result{Status: 200, Body: out.Bytes(), ContentType: "application/xml; charset=utf-8", Cacheable: true, Deps: deps}, nil
}

type rss struct {
	XMLName xml.Name   `xml:"rss"`
	Version string     `xml:"version,attr"`
	Atom    string     `xml:"xmlns:atom,attr"`
	Channel rssChannel `xml:"channel"`
}

type rssChannel struct {
	Title       string    `xml:"title"`
	Link        string    `xml:"link"`
	Description string    `xml:"description"`
	Language    string    `xml:"language"`
	LastBuild   string    `xml:"lastBuildDate"`
	Self        atomLink  `xml:"atom:link"`
	Items       []rssItem `xml:"item"`
}

type atomLink struct {
	Href string `xml:"href,attr"`
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
}

type rssItem struct {
	Title       string  `xml:"title"`
	Link        string  `xml:"link"`
	GUID        rssGUID `xml:"guid"`
	PubDate     string  `xml:"pubDate"`
	Description string  `xml:"description"`
	Creator     string  `xml:"author,omitempty"`
}

type rssGUID struct {
	Value       string `xml:",chardata"`
	IsPermaLink bool   `xml:"isPermaLink,attr"`
}

func (r *Renderer) feed(ctx context.Context, ct *site.ContentType, path string) (*Result, error) {
	b := r.Site()
	base := strings.TrimRight(r.BaseURL, "/")
	rows, err := r.DB.Q.PublishedList(ctx, dbq.PublishedListParams{Type: ct.Name, Locale: b.Config.Locale, Sort: "newest", Lim: 20})
	if err != nil {
		return nil, err
	}
	p := r.newPage(ctx, "feed", path)
	views := r.listViews(p, rows)
	ch := rssChannel{Title: r.Settings.SiteName(ctx), Link: base + "/", Description: firstNonEmpty(b.Config.Description, r.Settings.SiteName(ctx)),
		Language: b.Config.Locale, LastBuild: time.Now().UTC().Format(time.RFC1123Z),
		Self: atomLink{Href: base + path, Rel: "self", Type: "application/rss+xml"}}
	for _, e := range views {
		ch.Items = append(ch.Items, rssItem{Title: e.Title, Link: base + e.URL, GUID: rssGUID{Value: e.ID.String(), IsPermaLink: false},
			PubDate: e.Date.UTC().Format(time.RFC1123Z), Description: e.Excerpt()})
	}
	var out bytes.Buffer
	out.WriteString(xml.Header)
	if err := xml.NewEncoder(&out).Encode(rss{Version: "2.0", Atom: "http://www.w3.org/2005/Atom", Channel: ch}); err != nil {
		return nil, err
	}
	return &Result{Status: 200, Body: out.Bytes(), ContentType: "application/rss+xml; charset=utf-8", Cacheable: true,
		Deps: []jobs.Dep{{Type: "type", ID: ct.Name}}}, nil
}
