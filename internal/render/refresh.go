package render

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/jobs"
)

// Refresh renders one path and stores it (or removes it when it is no longer
// a cacheable 200), recording its dependencies.
func (r *Renderer) Refresh(ctx context.Context, path string) error {
	res, err := r.Render(ctx, path)
	if err != nil {
		return err
	}
	if res.Status != 200 || !res.Cacheable {
		if err := r.Disk.Delete(path); err != nil {
			return err
		}
		return r.DB.Q.ClearRenderDeps(ctx, path)
	}
	if err := r.Disk.Write(path, res.Body); err != nil {
		return err
	}
	return r.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.ClearRenderDeps(ctx, path); err != nil {
			return err
		}
		for _, d := range res.Deps {
			if err := q.AddRenderDep(ctx, dbq.AddRenderDepParams{PageKey: path, DepType: d.Type, DepID: d.ID}); err != nil {
				return err
			}
		}
		return nil
	})
}

// Invalidate re-renders every page that read one of the deps, plus paths.
// Old files are deleted first so a failing render never leaves stale content.
func (r *Renderer) Invalidate(ctx context.Context, args jobs.RenderInvalidate) error {
	r.init()
	keys := map[string]bool{}
	for _, p := range args.Paths {
		if p != "" {
			keys[p] = true
		}
	}
	byType := map[string][]string{}
	for _, d := range args.Deps {
		byType[d.Type] = append(byType[d.Type], d.ID)
	}
	for t, ids := range byType {
		pages, err := r.DB.Q.PagesDependingOn(ctx, dbq.PagesDependingOnParams{DepType: t, Ids: ids})
		if err != nil {
			return err
		}
		for _, p := range pages {
			keys[p] = true
		}
		// A changed type also changes its feeds and sitemaps, even if never rendered yet.
		if t == "type" {
			for _, name := range ids {
				if ct := r.Site().Types[name]; ct != nil {
					if ct.InSitemap() {
						keys["/sitemap-"+name+".xml"] = true
					}
					if ct.Archive != "" {
						keys[strings.TrimRight(ct.Archive, "/")+"/"] = true
					}
				}
			}
			keys["/feed.xml"] = true
			keys["/"] = true
		}
		if t == "term" || t == "taxonomy" {
			keys["/sitemap-taxonomies.xml"] = true
		}
	}
	var firstErr error
	for p := range keys {
		_ = r.Disk.Delete(p)
		if err := r.Refresh(ctx, p); err != nil {
			r.log.Error("re-render failed", "path", p, "err", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// Rebuild renders the whole site, writes theme assets, then removes any file
// that is no longer part of the site. Old files serve until replaced.
func (r *Renderer) Rebuild(ctx context.Context) (int, error) {
	r.init()
	start := time.Now()
	paths, err := r.AllPaths(ctx)
	if err != nil {
		return 0, err
	}
	if err := r.writeAssets(); err != nil {
		return 0, err
	}
	keep := map[string]bool{}
	n := 0
	var firstErr error
	for _, p := range paths {
		if err := r.Refresh(ctx, p); err != nil {
			r.log.Error("rebuild: render failed", "path", p, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		keep[p] = true
		n++
	}
	if firstErr == nil {
		var stale []string
		_ = r.Disk.Walk(func(u string) {
			if strings.HasPrefix(u, "/assets/") {
				return
			}
			if !keep[u] {
				stale = append(stale, u)
			}
		})
		for _, u := range stale {
			_ = r.Disk.Delete(u)
			_ = r.DB.Q.ClearRenderDeps(ctx, u)
		}
		r.sweepAssets()
	}
	r.log.Info("site rebuilt", "pages", n, "took", time.Since(start).Round(time.Millisecond))
	return n, firstErr
}

// AllPaths lists every public path of the site (also used by `cms build`).
func (r *Renderer) AllPaths(ctx context.Context) ([]string, error) {
	b := r.Site()
	seen := map[string]bool{}
	var out []string
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	add("/")
	add("/robots.txt")
	add("/sitemap.xml")
	if len(b.Taxonomies) > 0 {
		add("/sitemap-taxonomies.xml")
	}
	if ct := r.homeType(ctx); ct != nil && ct.Feed {
		add("/feed.xml")
	}
	front := r.Settings.FrontPageID(ctx)
	for _, name := range b.TypeOrder {
		ct := b.Types[name]
		if !ct.IsPublic() {
			continue
		}
		if ct.InSitemap() {
			add("/sitemap-" + name + ".xml")
		}
		rows, err := r.DB.Q.PublishedIDsByType(ctx, name)
		if err != nil {
			return nil, err
		}
		if ct.Archive != "" {
			base := strings.TrimRight(ct.Archive, "/") + "/"
			add(base)
			for i := 2; i <= pagesFor(len(rows), ct.PerPage); i++ {
				add(fmt.Sprintf("%spage/%d/", base, i))
			}
		}
		if ct.Pattern() == nil {
			continue
		}
		for _, row := range rows {
			if row.ID == front {
				continue
			}
			u, err := content.BuildURL(ctx, r.DB.Q, b, ct, row.Slug, row.ParentID, deref(row.PublishedAt))
			if err == nil {
				add(u)
			}
		}
	}
	// Home pagination when the home page lists entries.
	if front.String() == "00000000-0000-0000-0000-000000000000" {
		if ct := r.homeType(ctx); ct != nil {
			rows, err := r.DB.Q.PublishedIDsByType(ctx, ct.Name)
			if err == nil {
				for i := 2; i <= pagesFor(len(rows), ct.PerPage); i++ {
					add(fmt.Sprintf("/page/%d/", i))
				}
			}
		}
	}
	for name, tx := range b.Taxonomies {
		if tx.Pattern() == nil {
			continue
		}
		terms, err := r.DB.Q.ListTerms(ctx, name)
		if err != nil {
			return nil, err
		}
		per := b.Config.PerPage
		if len(tx.Types) > 0 {
			per = b.Types[tx.Types[0]].PerPage
		}
		for _, t := range terms {
			if t.PublishedCount == 0 {
				continue
			}
			base := r.termURL(name, t.Slug)
			add(base)
			for i := 2; i <= pagesFor(int(t.PublishedCount), per); i++ {
				add(fmt.Sprintf("%spage/%d/", base, i))
			}
		}
	}
	return out, nil
}

func pagesFor(n, per int) int {
	if per <= 0 {
		per = 10
	}
	return (n + per - 1) / per
}

// writeAssets copies fingerprinted theme assets into the public directory.
func (r *Renderer) writeAssets() error {
	th := r.theme()
	if th == nil {
		return nil
	}
	for _, p := range th.Assets() {
		u, _ := th.AssetPath(p)
		if r.Disk.Exists(u) {
			continue
		}
		data, _, ok := th.OpenAsset(u)
		if !ok {
			continue
		}
		if err := r.Disk.Write(u, data); err != nil {
			return err
		}
	}
	return nil
}

// sweepAssets removes asset directories of previous theme versions.
func (r *Renderer) sweepAssets() {
	th := r.theme()
	if th == nil {
		return
	}
	live := map[string]bool{}
	for _, p := range th.Assets() {
		u, _ := th.AssetPath(p)
		live[strings.Split(strings.TrimPrefix(u, "/assets/"), "/")[0]] = true
	}
	dir := filepath.Join(r.Disk.Root, "assets")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		// Keep a grace period: cached pages may still reference old hashes.
		if info, err := e.Info(); err == nil && !live[e.Name()] && time.Since(info.ModTime()) > 24*time.Hour {
			_ = os.RemoveAll(filepath.Join(dir, e.Name()))
		}
	}
}
