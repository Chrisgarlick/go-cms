package render

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Theme is the layered template set: the site theme overrides the base theme
// file by file. Templates are named by path without extension, e.g.
// "partials/head", "blocks/hero", "templates/single-post".
type Theme struct {
	layers []fs.FS // highest priority first: site theme, site blocks, base
	base   *template.Template
	mu     sync.Mutex
	pages  map[string]*template.Template // compiled page templates by name
	names  map[string]bool               // every template file known (for hierarchy lookups)
	assets map[string]string             // asset path -> content hash (8 hex)
	static map[string]fs.FS              // static/ file path -> the layer that provides it
	css    map[string][]byte             // stylesheets with url() references fingerprinted
	Hash   string                        // hash of all theme files
}

// LoadTheme builds the template set. siteTheme may be nil (base only);
// siteBlocks holds the site's block templates (blocks/<name>/<name>.gohtml).
func LoadTheme(base, siteTheme, siteBlocks fs.FS, funcs template.FuncMap) (*Theme, error) {
	t := &Theme{pages: map[string]*template.Template{}, names: map[string]bool{}, assets: map[string]string{}, static: map[string]fs.FS{}, css: map[string][]byte{}}
	if siteTheme != nil {
		t.layers = append(t.layers, siteTheme)
	}
	t.layers = append(t.layers, base)

	root := template.New("root").Funcs(funcs).Option("missingkey=zero")
	// Collect files per name: lowest layer first so higher layers override.
	type file struct{ name, src string }
	var shared []file
	all := map[string]bool{}
	hasher := sha256.New()
	for i := len(t.layers) - 1; i >= 0; i-- {
		layer := t.layers[i]
		err := fs.WalkDir(layer, ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			data, err := fs.ReadFile(layer, p)
			if err != nil {
				return err
			}
			fmt.Fprintf(hasher, "%d:%s:%d\n", i, p, len(data))
			hasher.Write(data)
			if strings.HasPrefix(p, "static/") {
				// Served as-is at the site root (favicon.ico, og images, llms.txt).
				t.static[strings.TrimPrefix(p, "static/")] = layer
				return nil
			}
			if strings.HasPrefix(p, "assets/") {
				// Layers are walked lowest first, so a site asset replaces the base one.
				h := sha256.Sum256(data)
				t.assets[strings.TrimPrefix(p, "assets/")] = hex.EncodeToString(h[:4])
				return nil
			}
			if !strings.HasSuffix(p, ".gohtml") {
				return nil
			}
			name := strings.TrimSuffix(p, ".gohtml")
			all[name] = true
			if !strings.HasPrefix(name, "templates/") {
				shared = append(shared, file{name, string(data)})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	t.fingerprintCSS()
	// Site block templates live outside the theme dir: blocks/<n>/<n>.gohtml.
	if siteBlocks != nil {
		if entries, err := fs.ReadDir(siteBlocks, "blocks"); err == nil {
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				p := path.Join("blocks", e.Name(), e.Name()+".gohtml")
				data, err := fs.ReadFile(siteBlocks, p)
				if err != nil {
					continue
				}
				hasher.Write(data)
				name := "blocks/" + e.Name()
				all[name] = true
				shared = append(shared, file{name, string(data)})
			}
		}
	}
	for _, f := range shared {
		if _, err := root.New(f.name).Parse(f.src); err != nil {
			return nil, fmt.Errorf("theme: %s.gohtml: %w", f.name, err)
		}
	}
	if root.Lookup("layout") == nil {
		return nil, errors.New(`theme: no "layout" template defined (layouts/default.gohtml)`)
	}
	t.base = root
	t.names = all
	t.Hash = hex.EncodeToString(hasher.Sum(nil))[:12]
	return t, nil
}

// Has reports whether a page template exists in any layer.
func (t *Theme) Has(name string) bool { return t.names[name] }

// Page returns the compiled template set for a page template (cached).
func (t *Theme) Page(name string) (*template.Template, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if p, ok := t.pages[name]; ok {
		return p, nil
	}
	var src []byte
	for _, l := range t.layers {
		if b, err := fs.ReadFile(l, name+".gohtml"); err == nil {
			src = b
			break
		}
	}
	if src == nil {
		return nil, fmt.Errorf("theme: template %s not found", name)
	}
	clone, err := t.base.Clone()
	if err != nil {
		return nil, err
	}
	if _, err := clone.New(name).Parse(string(src)); err != nil {
		return nil, fmt.Errorf("theme: %s.gohtml: %w", name, err)
	}
	t.pages[name] = clone
	return clone, nil
}

// First returns the first existing template from candidates.
func (t *Theme) First(candidates ...string) (string, bool) {
	for _, c := range candidates {
		if c != "" && t.Has(c) {
			return c, true
		}
	}
	return "", false
}

// Shared returns the base set (for rendering blocks outside a page).
func (t *Theme) Shared() *template.Template { return t.base }

// AssetPath returns the fingerprinted URL path for a theme asset.
func (t *Theme) AssetPath(p string) (string, bool) {
	p = strings.TrimPrefix(path.Clean("/"+p), "/")
	h, ok := t.assets[p]
	if !ok {
		return "", false
	}
	return "/assets/" + h + "/" + p, true
}

// Static returns a file from a theme's static/ folder, served at the site
// root: "/favicon.ico" → static/favicon.ico.
func (t *Theme) Static(urlPath string) ([]byte, bool) {
	p := strings.TrimPrefix(path.Clean("/"+urlPath), "/")
	layer, ok := t.static[p]
	if !ok {
		return nil, false
	}
	b, err := fs.ReadFile(layer, "static/"+p)
	return b, err == nil
}

// HasStatic reports whether the theme's static/ folder has a file.
func (t *Theme) HasStatic(p string) bool {
	_, ok := t.static[strings.TrimPrefix(path.Clean("/"+p), "/")]
	return ok
}

// OpenAsset resolves "/assets/<hash>/<path>" to file contents.
func (t *Theme) OpenAsset(urlPath string) ([]byte, string, bool) {
	rest := strings.TrimPrefix(urlPath, "/assets/")
	h, p, ok := strings.Cut(rest, "/")
	if !ok || strings.Contains(p, "..") {
		return nil, "", false
	}
	if want, exists := t.assets[p]; !exists || want != h {
		return nil, "", false
	}
	if b, ok := t.css[p]; ok {
		return b, p, true
	}
	for _, l := range t.layers {
		if b, err := fs.ReadFile(l, path.Join("assets", p)); err == nil {
			return b, p, true
		}
	}
	return nil, "", false
}

var cssURL = regexp.MustCompile(`url\(\s*(['"]?)([^'")]+)['"]?\s*\)`)

// fingerprintCSS rewrites relative url() references in theme stylesheets
// (fonts, images) to the referenced assets' fingerprinted URLs, so a
// stylesheet can say url(fonts/inter.woff2) and still be cached forever.
// Each stylesheet's own hash is then taken over the rewritten content.
func (t *Theme) fingerprintCSS() {
	for p := range t.assets {
		if !strings.HasSuffix(p, ".css") {
			continue
		}
		var src []byte
		for _, l := range t.layers {
			if b, err := fs.ReadFile(l, path.Join("assets", p)); err == nil {
				src = b
				break
			}
		}
		changed := false
		out := cssURL.ReplaceAllFunc(src, func(m []byte) []byte {
			ref := string(cssURL.FindSubmatch(m)[2])
			if strings.Contains(ref, ":") || strings.HasPrefix(ref, "/") || strings.HasPrefix(ref, "#") {
				return m // absolute, data: or external
			}
			suffix := ""
			if i := strings.IndexAny(ref, "?#"); i >= 0 {
				ref, suffix = ref[:i], ref[i:]
			}
			target := path.Join(path.Dir(p), ref)
			h, ok := t.assets[target]
			if !ok || strings.HasSuffix(target, ".css") {
				return m
			}
			changed = true
			return []byte(`url("/assets/` + h + "/" + target + suffix + `")`)
		})
		if changed {
			sum := sha256.Sum256(out)
			t.css[p] = out
			t.assets[p] = hex.EncodeToString(sum[:4])
		}
	}
}

// Assets lists every asset path (for copying into the public directory).
func (t *Theme) Assets() []string {
	out := make([]string, 0, len(t.assets))
	for p := range t.assets {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// Validate parses every page template, for `cms theme validate`.
func (t *Theme) Validate() []error {
	var errs []error
	names := make([]string, 0, len(t.names))
	for n := range t.names {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if strings.HasPrefix(n, "templates/") {
			if _, err := t.Page(n); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errs
}
