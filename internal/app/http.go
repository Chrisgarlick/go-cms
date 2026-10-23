package app

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"time"

	"cms/internal/content"
	"cms/internal/http/public"
	"cms/internal/render"
	"cms/internal/version"
	"cms/internal/web"
)

// Handler builds the full HTTP handler: admin, API, health, then the public site.
func (a *App) Handler(admin, api http.Handler) http.Handler {
	u, _ := url.Parse(a.Cfg.BaseURL)
	pub := &public.Handler{Renderer: a.Renderer, Redirects: a.Redirects, Auth: a.Auth, Content: a.Content, Log: a.Log,
		ServeStatic: a.Cfg.HTTP.ServeStatic, MediaDir: a.Cfg.MediaDir() + "/public", CSP: a.PublicCSP, Preview: a.PreviewEntry}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /readyz", a.readyz)
	mux.HandleFunc("GET /metrics", a.metrics)
	mux.HandleFunc("GET /forms/{name}/token", a.formToken)
	mux.HandleFunc("POST /forms/{name}", a.formSubmit)
	if admin != nil {
		mux.Handle("/admin/", admin)
		mux.Handle("/admin", http.RedirectHandler("/admin/", http.StatusMovedPermanently))
	}
	if api != nil {
		mux.Handle("/api/", api)
	}
	if a.Cfg.API.Headless {
		// Headless: no public pages, only the API (and media in development).
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/media/") || strings.HasPrefix(r.URL.Path, "/preview/") {
				pub.ServeHTTP(w, r)
				return
			}
			http.NotFound(w, r)
		}))
	} else {
		mux.Handle("/", pub)
	}
	var h http.Handler = mux
	h = web.HostCheck(a.Cfg.Hosts, h)
	h = web.SecurityHeaders(u.Scheme == "https", h)
	h = web.Base(a.Cfg, a.Log, h)
	return h
}

// PublicCSP is the public site's Content-Security-Policy (theme extras added).
func (a *App) PublicCSP() web.CSP {
	t := a.Site().Theme.CSP
	c := web.CSP{Script: t.ScriptSrc, Style: t.StyleSrc, Img: t.ImgSrc, Font: t.FontSrc, Connect: t.ConnectSrc,
		Frame: append(append([]string{}, render.EmbedFrameSources...), t.FrameSrc...)}
	if a.Cfg.MediaURL != "" {
		if mu, err := url.Parse(a.Cfg.MediaURL); err == nil {
			origin := mu.Scheme + "://" + mu.Host
			c.Img = append(c.Img, origin)
			c.Media = append(c.Media, origin)
		}
	}
	return c
}

// PreviewEntry resolves a signed preview token (see PreviewToken).
func (a *App) PreviewEntry(ctx context.Context, token string) (*content.Entry, error) {
	id, err := a.verifyPreview(token)
	if err != nil {
		return nil, err
	}
	return a.Content.Get(SystemCtx(ctx, "preview"), id)
}

func (a *App) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	if err := a.DB.Pool.Ping(ctx); err != nil {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
		return
	}
	if a.Renderer.Theme() == nil {
		http.Error(w, "theme failed to load", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "version": version.Version, "uptime_s": int(a.Uptime().Seconds())})
}

// metrics exposes Prometheus text format; loopback only unless a token is set.
func (a *App) metrics(w http.ResponseWriter, r *http.Request) {
	ip := web.ClientIP(r.Context())
	if !ip.IsLoopback() {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		want := a.Cfg.HTTP.MetricsToken()
		if want == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(want)) != 1 {
			http.NotFound(w, r)
			return
		}
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	st := a.DB.Pool.Stat()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# TYPE cms_up gauge\ncms_up 1\n")
	fmt.Fprintf(w, "# TYPE cms_uptime_seconds gauge\ncms_uptime_seconds %d\n", int(a.Uptime().Seconds()))
	fmt.Fprintf(w, "# TYPE go_memstats_heap_alloc_bytes gauge\ngo_memstats_heap_alloc_bytes %d\n", ms.HeapAlloc)
	fmt.Fprintf(w, "# TYPE go_memstats_sys_bytes gauge\ngo_memstats_sys_bytes %d\n", ms.Sys)
	fmt.Fprintf(w, "# TYPE go_gc_cycles_total counter\ngo_gc_cycles_total %d\n", ms.NumGC)
	fmt.Fprintf(w, "# TYPE go_goroutines gauge\ngo_goroutines %d\n", runtime.NumGoroutine())
	fmt.Fprintf(w, "# TYPE cms_db_pool_acquired gauge\ncms_db_pool_acquired %d\n", st.AcquiredConns())
	fmt.Fprintf(w, "# TYPE cms_db_pool_total gauge\ncms_db_pool_total %d\n", st.TotalConns())
	fmt.Fprintf(w, "# TYPE cms_db_pool_wait_total counter\ncms_db_pool_wait_total %d\n", st.EmptyAcquireCount())
	web.WriteRequestMetrics(w)
}

// Serve runs the HTTP server until ctx is cancelled, then drains connections.
func (a *App) Serve(ctx context.Context, handler http.Handler, addr string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: a.Cfg.HTTP.ReadHeaderTimeout,
		ReadTimeout:       2 * time.Minute, // uploads
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       90 * time.Second,
		MaxHeaderBytes:    64 << 10,
		BaseContext:       func(net.Listener) context.Context { return context.WithoutCancel(ctx) },
	}
	errc := make(chan error, 1)
	go func() {
		a.Log.Info("listening", "addr", addr, "url", a.Cfg.BaseURL, "version", version.Version)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}
	a.Log.Info("shutting down", "drain", a.Cfg.HTTP.ShutdownTimeout)
	sctx, cancel := context.WithTimeout(context.Background(), a.Cfg.HTTP.ShutdownTimeout)
	defer cancel()
	return srv.Shutdown(sctx)
}
