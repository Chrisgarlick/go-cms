// Package web holds HTTP middleware shared by the public site, admin and API.
package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"

	"cms/internal/config"
)

type ctxKey int

const (
	keyRequestID ctxKey = iota
	keyClientIP
	keyNonce
)

// RequestID returns the request's id ("" outside a request).
func RequestID(ctx context.Context) string { s, _ := ctx.Value(keyRequestID).(string); return s }

// ClientIP returns the client address resolved by the trusted-proxy rules.
func ClientIP(ctx context.Context) netip.Addr { a, _ := ctx.Value(keyClientIP).(netip.Addr); return a }

// Nonce returns the per-request CSP nonce.
func Nonce(ctx context.Context) string { s, _ := ctx.Value(keyNonce).(string); return s }

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Base wraps every handler: request id, client IP, nonce, panic recovery and
// access logging.
func Base(cfg *config.Config, log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rid := r.Header.Get("X-Request-Id")
		if len(rid) < 8 || len(rid) > 64 || strings.ContainsAny(rid, " \r\n\"") {
			rid = randomID(12)
		}
		ip := clientIP(cfg, r)
		ctx := context.WithValue(r.Context(), keyRequestID, rid)
		ctx = context.WithValue(ctx, keyClientIP, ip)
		ctx = context.WithValue(ctx, keyNonce, randomID(16))
		w.Header().Set("X-Request-Id", rid)
		sw := &statusWriter{ResponseWriter: w, status: 200}
		defer func() {
			observe(sw.status, time.Since(start))
			if rec := recover(); rec != nil {
				log.Error("panic", "err", rec, "path", r.URL.Path, "request_id", rid, "stack", string(debug.Stack()))
				if !sw.wrote {
					http.Error(sw, "Internal Server Error", http.StatusInternalServerError)
				}
			}
			lvl := slog.LevelDebug
			if sw.status >= 500 {
				lvl = slog.LevelError
			} else if strings.HasPrefix(r.URL.Path, "/admin") || strings.HasPrefix(r.URL.Path, "/api") {
				lvl = slog.LevelInfo
			}
			log.Log(ctx, lvl, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status,
				"bytes", sw.bytes, "ms", time.Since(start).Milliseconds(), "ip", ip.String(), "request_id", rid)
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

// clientIP trusts X-Forwarded-For only from configured proxies, walking the
// header right to left past trusted hops (spec 7.2).
func clientIP(cfg *config.Config, r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	remote = remote.Unmap()
	if !cfg.HTTP.IsTrustedProxy(remote) {
		return remote
	}
	xff := r.Header.Values("X-Forwarded-For")
	parts := strings.Split(strings.Join(xff, ","), ",")
	for i := len(parts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(parts[i]))
		if err != nil {
			break
		}
		a = a.Unmap()
		if !cfg.HTTP.IsTrustedProxy(a) {
			return a
		}
		remote = a
	}
	return remote
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status = code
		w.wrote = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wrote = true
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// HostCheck answers 421 for hosts the site does not serve (cache poisoning).
func HostCheck(hosts []string, next http.Handler) http.Handler {
	allowed := map[string]bool{}
	for _, h := range hosts {
		allowed[strings.ToLower(h)] = true
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := strings.ToLower(r.Host)
		if hh, _, err := net.SplitHostPort(h); err == nil {
			h = hh
		}
		if !allowed[h] && h != "localhost" && h != "127.0.0.1" && h != "::1" {
			http.Error(w, "Misdirected Request", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// SecurityHeaders sets headers common to every response.
func SecurityHeaders(https bool, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=(), usb=(), interest-cohort=()")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		if https {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

// CSP builds a Content-Security-Policy value.
type CSP struct {
	Script, Style, Img, Font, Frame, Connect, Media []string
	FrameAncestors                                  string
	Nonce                                           string
}

func (c CSP) String() string {
	join := func(base string, extra []string) string {
		if len(extra) == 0 {
			return base
		}
		return base + " " + strings.Join(extra, " ")
	}
	script := join("'self'", c.Script)
	if c.Nonce != "" {
		script += " 'nonce-" + c.Nonce + "'"
	}
	fa := c.FrameAncestors
	if fa == "" {
		fa = "'self'"
	}
	return fmt.Sprintf("default-src 'self'; script-src %s; style-src %s; img-src %s; font-src %s; frame-src %s; connect-src %s; media-src %s; object-src 'none'; base-uri 'self'; form-action 'self'; frame-ancestors %s",
		script, join("'self'", c.Style), join("'self' data:", c.Img), join("'self'", c.Font), join("'self'", c.Frame),
		join("'self'", c.Connect), join("'self'", c.Media), fa)
}

// LimitBody caps request bodies (uploads use their own, larger limit).
func LimitBody(n int64, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, n)
		}
		next.ServeHTTP(w, r)
	})
}
