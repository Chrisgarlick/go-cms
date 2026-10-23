// Package public serves the public site: redirects first, then pre-rendered
// files (when Go serves them itself), then dynamic routes and cache misses,
// which are rendered, sent, and written to disk for next time (spec 2.5).
package public

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"cms/internal/auth"
	"cms/internal/content"
	"cms/internal/redirects"
	"cms/internal/render"
	"cms/internal/web"
)

type Handler struct {
	Renderer  *render.Renderer
	Redirects *redirects.Service
	Auth      *auth.Service
	Content   *content.Service
	Log       *slog.Logger
	// ServeStatic: Go serves rendered files and media itself (dev, or no Caddy).
	ServeStatic bool
	MediaDir    string // <data>/media/public
	CSP         func() web.CSP
	Preview     func(ctx context.Context, token string) (*content.Entry, error)

	writeSem chan struct{}
	once     sync.Once
}

func (h *Handler) init() {
	h.once.Do(func() { h.writeSem = make(chan struct{}, 4) })
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.init()
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	p := r.URL.Path
	if strings.Contains(p, "\x00") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	host := r.Host
	if i := strings.LastIndex(host, ":"); i > 0 && !strings.Contains(host[i:], "]") {
		host = host[:i]
	}
	// 1. Redirect rules win over everything.
	if m, ok := h.Redirects.Lookup(host, p, r.URL.RawQuery); ok {
		if m.Target == "" {
			h.status(w, r, m.Status)
			return
		}
		target := m.Target
		if strings.HasPrefix(target, "/") && !strings.HasPrefix(target, "//") {
			target = h.Renderer.Site().Config.Out(target)
		}
		http.Redirect(w, r, target, m.Status)
		return
	}
	switch {
	case strings.HasPrefix(p, "/assets/"):
		h.asset(w, r)
		return
	case strings.HasPrefix(p, "/media/"):
		if h.ServeStatic {
			h.media(w, r)
			return
		}
		h.notFound(w, r)
		return
	case p == "/search/" || p == "/search":
		h.search(w, r)
		return
	case strings.HasPrefix(p, "/preview/"):
		h.preview(w, r)
		return
	}
	// 2. Canonical trailing slash for page-like paths. Internally paths always
	// end in a slash; a site with trailing_slash: false publishes them without.
	if path.Ext(p) == "" && p != "/" {
		slash := h.Renderer.Site().Config.Slash()
		if strings.HasSuffix(p, "/") != slash {
			target := strings.TrimSuffix(p, "/")
			if slash {
				target = p + "/"
			}
			target = web.LocalPath(target)
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently) //nolint:gosec // a local path
			return
		}
		if !slash {
			p += "/"
		}
	}
	// 3. Pre-rendered file (Caddy normally serves these before Go sees them).
	if h.ServeStatic && h.serveFile(w, r, p) {
		return
	}
	// 4. Render, respond, and store for next time.
	res, err := h.Renderer.Render(r.Context(), p)
	if err != nil {
		h.Log.Error("render failed", "path", p, "err", err, "request_id", web.RequestID(r.Context()))
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if res.Status == 404 {
		h.Redirects.RecordNotFound(host, p, r.Referer())
	}
	h.write(w, r, res)
	if res.Status == 200 && res.Cacheable {
		h.store(p)
	}
}

// store writes the page to disk in the background (bounded concurrency).
func (h *Handler) store(p string) {
	select {
	case h.writeSem <- struct{}{}:
	default:
		return // busy: the next miss will store it
	}
	go func() {
		defer func() { <-h.writeSem }()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := h.Renderer.Refresh(ctx, p); err != nil {
			h.Log.Warn("store rendered page failed", "path", p, "err", err)
		}
	}()
}

func (h *Handler) headers(w http.ResponseWriter, r *http.Request, res *render.Result) {
	hd := w.Header()
	hd.Set("Content-Type", res.ContentType)
	if h.CSP != nil && strings.HasPrefix(res.ContentType, "text/html") {
		hd.Set("Content-Security-Policy", h.CSP().String())
	}
	if res.Cacheable && res.Status == 200 {
		hd.Set("Cache-Control", "public, max-age=0, must-revalidate")
	} else {
		hd.Set("Cache-Control", "no-store")
	}
}

func (h *Handler) write(w http.ResponseWriter, r *http.Request, res *render.Result) {
	h.headers(w, r, res)
	if res.Status == 200 {
		sum := sha256.Sum256(res.Body)
		etag := `"` + hex.EncodeToString(sum[:8]) + `"`
		w.Header().Set("ETag", etag)
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Body)))
	w.WriteHeader(res.Status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(res.Body) //nolint:gosec // rendered page, escaped by the templates
	}
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request, code int) {
	res, err := h.Renderer.Render(r.Context(), "/__status__/")
	if err != nil || res == nil {
		http.Error(w, http.StatusText(code), code)
		return
	}
	res.Status = code
	res.Cacheable = false
	h.write(w, r, res)
}

func (h *Handler) notFound(w http.ResponseWriter, r *http.Request) {
	res, err := h.Renderer.Render(r.Context(), "/__not_found__/")
	if err != nil {
		http.NotFound(w, r)
		return
	}
	h.write(w, r, res)
}

// serveFile serves a rendered file with precompressed negotiation, as Caddy would.
func (h *Handler) serveFile(w http.ResponseWriter, r *http.Request, p string) bool {
	f, err := h.Renderer.Disk.FileFor(p)
	if err != nil {
		return false
	}
	st, err := os.Stat(f) //nolint:gosec // path confined to the output/media root above
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	ae := r.Header.Get("Accept-Encoding")
	name, enc := f, ""
	switch {
	case strings.Contains(ae, "br") && exists(f+".br"):
		name, enc = f+".br", "br"
	case strings.Contains(ae, "gzip") && exists(f+".gz"):
		name, enc = f+".gz", "gzip"
	}
	data, err := os.ReadFile(name) //nolint:gosec // path confined to the output/media root above
	if err != nil {
		return false
	}
	ct := mime.TypeByExtension(filepath.Ext(f))
	if ct == "" {
		ct = "application/octet-stream"
	}
	res := &render.Result{Status: 200, ContentType: ct, Cacheable: true, Body: data}
	h.headers(w, r, res)
	if enc != "" {
		w.Header().Set("Content-Encoding", enc)
		w.Header().Add("Vary", "Accept-Encoding")
	}
	http.ServeContent(w, r, "", st.ModTime(), bytes.NewReader(data))
	return true
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil } //nolint:gosec // path confined to the output/media root above

// asset serves fingerprinted theme assets (immutable).
func (h *Handler) asset(w http.ResponseWriter, r *http.Request) {
	data, name, ok := h.Renderer.Asset(r.URL.Path)
	if !ok {
		h.notFound(w, r)
		return
	}
	ct := mime.TypeByExtension(path.Ext(name))
	if ct == "" {
		ct = "application/octet-stream"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
}

// media serves public media files in development (Caddy serves them in production).
func (h *Handler) media(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(path.Clean(r.URL.Path), "/media/")
	if rel == "" || strings.HasPrefix(rel, "..") || !(strings.HasPrefix(rel, "v/") || strings.HasPrefix(rel, "f/")) {
		h.notFound(w, r)
		return
	}
	f := filepath.Join(h.MediaDir, filepath.FromSlash(rel))
	st, err := os.Stat(f) //nolint:gosec // path confined to the output/media root above
	if err != nil || !st.Mode().IsRegular() {
		h.notFound(w, r)
		return
	}
	hd := w.Header()
	hd.Set("Cache-Control", "public, max-age=31536000, immutable")
	hd.Set("Content-Security-Policy", "default-src 'none'; img-src 'self'; media-src 'self'; style-src 'unsafe-inline'; sandbox")
	if strings.HasPrefix(rel, "f/") {
		ext := strings.ToLower(path.Ext(rel))
		if ext != ".pdf" && ext != ".mp4" && ext != ".webm" && ext != ".mp3" && ext != ".ogg" && ext != ".svg" {
			hd.Set("Content-Disposition", "attachment")
		}
	}
	http.ServeFile(w, r, f) //nolint:gosec // path confined to the output/media root above
}

func (h *Handler) search(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/search" {
		http.Redirect(w, r, "/search/?"+r.URL.RawQuery, http.StatusMovedPermanently) //nolint:gosec // fixed path
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	res, err := h.Renderer.Search(r.Context(), r.URL.Query().Get("q"), page)
	if err != nil {
		h.Log.Error("search failed", "err", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	h.write(w, r, res)
}

// preview renders a working copy from a signed, expiring token.
func (h *Handler) preview(w http.ResponseWriter, r *http.Request) {
	tok := strings.Trim(strings.TrimPrefix(r.URL.Path, "/preview/"), "/")
	if h.Preview == nil || tok == "" {
		h.notFound(w, r)
		return
	}
	e, err := h.Preview(r.Context(), tok)
	if err != nil {
		if errors.Is(err, content.ErrNotFound) {
			h.notFound(w, r)
			return
		}
		http.Error(w, "This preview link is invalid or has expired.", http.StatusGone)
		return
	}
	res, err := h.Renderer.Preview(r.Context(), e)
	if err != nil {
		h.Log.Error("preview failed", "err", err)
		http.Error(w, "Preview failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("X-Robots-Tag", "noindex")
	h.write(w, r, res)
}
