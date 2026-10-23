package render

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/andybalholm/brotli"
)

// Disk writes rendered output under <root>/<host>/ for Caddy (or Go in dev)
// to serve directly. Every write is atomic: a temp file is renamed into place,
// so readers never see a partial page. Brotli and gzip siblings are written
// once at render time, not per request (spec 8.3).
type Disk struct {
	Root string // e.g. /var/lib/cms/public/example.com
}

// FileFor maps a URL path to its file: directories get index.html.
func (d *Disk) FileFor(urlPath string) (string, error) {
	clean := path.Clean("/" + urlPath)
	if strings.Contains(clean, "\x00") || strings.Contains(clean, "..") {
		return "", errors.New("render: bad path")
	}
	rel := strings.TrimPrefix(clean, "/")
	if strings.HasSuffix(urlPath, "/") || rel == "" {
		rel = path.Join(rel, "index.html")
	}
	p := filepath.Join(d.Root, filepath.FromSlash(rel))
	if r, err := filepath.Rel(d.Root, p); err != nil || strings.HasPrefix(r, "..") {
		return "", errors.New("render: path escapes root")
	}
	return p, nil
}

// Write stores body (and compressed copies) for a URL path.
func (d *Disk) Write(urlPath string, body []byte) error {
	p, err := d.FileFor(urlPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	// If a directory index replaces a stale file of the same name, remove it.
	if err := atomicWrite(p, body); err != nil {
		return err
	}
	if len(body) < 256 {
		_ = os.Remove(p + ".br")
		_ = os.Remove(p + ".gz")
		return nil
	}
	var br bytes.Buffer
	bw := brotli.NewWriterLevel(&br, 6)
	if _, err := bw.Write(body); err != nil {
		return err
	}
	if err := bw.Close(); err != nil {
		return err
	}
	if err := atomicWrite(p+".br", br.Bytes()); err != nil {
		return err
	}
	var gz bytes.Buffer
	gw, _ := gzip.NewWriterLevel(&gz, 7)
	if _, err := gw.Write(body); err != nil {
		return err
	}
	if err := gw.Close(); err != nil {
		return err
	}
	return atomicWrite(p+".gz", gz.Bytes())
}

// Delete removes a path's files so the next request falls through to Go.
func (d *Disk) Delete(urlPath string) error {
	p, err := d.FileFor(urlPath)
	if err != nil {
		return err
	}
	for _, f := range []string{p, p + ".br", p + ".gz"} {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	// Tidy empty directories up to the root.
	for dir := filepath.Dir(p); dir != d.Root && strings.HasPrefix(dir, d.Root); dir = filepath.Dir(dir) {
		if os.Remove(dir) != nil {
			break
		}
	}
	return nil
}

// Exists reports whether a rendered file exists for a URL path.
func (d *Disk) Exists(urlPath string) bool {
	p, err := d.FileFor(urlPath)
	if err != nil {
		return false
	}
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// Walk lists every rendered URL path (for sweeping stale files after a rebuild).
func (d *Disk) Walk(fn func(urlPath string)) error {
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if e.IsDir() || strings.HasSuffix(p, ".br") || strings.HasSuffix(p, ".gz") || strings.Contains(e.Name(), ".tmp-") {
			return nil
		}
		rel, _ := filepath.Rel(d.Root, p)
		u := "/" + filepath.ToSlash(rel)
		if strings.HasSuffix(u, "/index.html") {
			u = strings.TrimSuffix(u, "index.html")
		}
		fn(u)
		return nil
	})
	return err
}

func atomicWrite(p string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), p)
}
