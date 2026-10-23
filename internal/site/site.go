// Package site loads and validates a site bundle (spec 2.6): site.yaml,
// content-types/, taxonomies/, field-groups/, blocks/ and theme/theme.yaml.
// The bundle is the source of truth for the schema; the database mirrors it
// only so `cms schema diff` can detect drift.
package site

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"cms/internal/fields"
)

// Bundle is a loaded, validated site.
type Bundle struct {
	FS          fs.FS `json:"-"`
	Dir         string
	Config      Config
	Types       map[string]*ContentType
	TypeOrder   []string
	Taxonomies  map[string]*Taxonomy
	FieldGroups map[string]*FieldGroup
	Blocks      map[string]*BlockDef
	Options     map[string]*OptionsPage
	Forms       map[string]*Form
	Theme       Theme
	// Hash covers every file in the bundle; it versions rendered output.
	Hash string
}

type Config struct {
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description"`
	Locale      string `yaml:"locale" json:"locale"`
	Timezone    string `yaml:"timezone" json:"timezone"`
	DateFormat  string `yaml:"date_format" json:"date_format"`
	Core        struct {
		Channel    string `yaml:"channel" json:"channel"`
		MinVersion string `yaml:"min_version" json:"min_version"`
	} `yaml:"core" json:"core"`
	PerPage int `yaml:"per_page" json:"per_page"`
	// TrailingSlash false publishes URLs as /about rather than /about/. Paths stay
	// slash-terminated inside the CMS (routing, disk cache, dependencies); only
	// the URLs written into pages, feeds and sitemaps, and the canonical
	// redirect, change.
	TrailingSlash *bool `yaml:"trailing_slash" json:"trailing_slash"`
}

// Slash reports whether published URLs end in a slash (the default).
func (c Config) Slash() bool { return c.TrailingSlash == nil || *c.TrailingSlash }

// Out converts an internal path (always slash-terminated) to its published form.
// Query strings and fragments are kept; paths with a file extension are untouched.
func (c Config) Out(p string) string {
	if c.Slash() || p == "" || p == "/" || !strings.HasPrefix(p, "/") {
		return p
	}
	rest := ""
	if i := strings.IndexAny(p, "?#"); i >= 0 {
		p, rest = p[:i], p[i:]
	}
	if len(p) > 1 && strings.HasSuffix(p, "/") {
		p = strings.TrimSuffix(p, "/")
	}
	return p + rest
}

type Label struct {
	Singular string `yaml:"singular" json:"singular"`
	Plural   string `yaml:"plural" json:"plural"`
}

type ContentType struct {
	Name         string         `yaml:"name" json:"name"`
	Label        Label          `yaml:"label" json:"label"`
	Icon         string         `yaml:"icon" json:"icon"`
	Hierarchical bool           `yaml:"hierarchical" json:"hierarchical"`
	Public       *bool          `yaml:"public" json:"public"`
	URL          string         `yaml:"url" json:"url"`         // single entry pattern, e.g. /blog/{slug}
	Archive      string         `yaml:"archive" json:"archive"` // archive path, e.g. /blog
	Supports     []string       `yaml:"supports" json:"supports"`
	Taxonomies   []string       `yaml:"taxonomies" json:"taxonomies"`
	Fields       []fields.Field `yaml:"fields" json:"fields"`
	FieldGroups  []string       `yaml:"field_groups" json:"field_groups"`
	Blocks       BlockPolicy    `yaml:"blocks" json:"blocks"`
	Sort         Sort           `yaml:"sort" json:"sort"`
	PerPage      int            `yaml:"per_page" json:"per_page"`
	ListColumns  []string       `yaml:"list_columns" json:"list_columns"`
	Cache        string         `yaml:"cache" json:"cache"` // static | uncached
	Feed         bool           `yaml:"feed" json:"feed"`
	Sitemap      *bool          `yaml:"sitemap" json:"sitemap"`
	Search       *bool          `yaml:"search" json:"search"`
	MenuPosition int            `yaml:"menu_position" json:"menu_position"`
	Template     string         `yaml:"template" json:"template"`

	pattern *Pattern
}

type BlockPolicy struct {
	Allowed  []string `yaml:"allowed" json:"allowed"`
	Default  []string `yaml:"default" json:"default"`
	MaxDepth int      `yaml:"max_depth" json:"max_depth"`
}

type Sort struct {
	Field string `yaml:"field" json:"field"` // published_at | title | menu_order | updated_at
	Order string `yaml:"order" json:"order"` // asc | desc
}

type Taxonomy struct {
	Name         string         `yaml:"name" json:"name"`
	Label        Label          `yaml:"label" json:"label"`
	Hierarchical bool           `yaml:"hierarchical" json:"hierarchical"`
	URL          string         `yaml:"url" json:"url"` // e.g. /category/{slug}
	Fields       []fields.Field `yaml:"fields" json:"fields"`
	Types        []string       `yaml:"-" json:"types"`
	pattern      *Pattern
}

type Location struct {
	Type        string `yaml:"type" json:"type,omitempty"`
	Taxonomy    string `yaml:"taxonomy" json:"taxonomy,omitempty"`
	OptionsPage string `yaml:"options_page" json:"options_page,omitempty"`
	Media       bool   `yaml:"media" json:"media,omitempty"`
}

type FieldGroup struct {
	Name     string         `yaml:"name" json:"name"`
	Label    string         `yaml:"label" json:"label"`
	Position string         `yaml:"position" json:"position"` // main | sidebar
	Location []Location     `yaml:"location" json:"location"`
	Fields   []fields.Field `yaml:"fields" json:"fields"`
}

// Form is a public form (forms/<name>.yaml). Submissions are validated against
// Fields, stored encrypted and emailed to Notify (spec 4.8).
type Form struct {
	Name   string         `yaml:"name" json:"name"`
	Label  string         `yaml:"label" json:"label"`
	Fields []fields.Field `yaml:"fields" json:"fields"`
	// Notify lists email addresses told about each submission.
	Notify []string `yaml:"notify" json:"notify"`
	// Subject of the notification email; {label} and any {field} are filled in.
	Subject string `yaml:"subject" json:"subject"`
	// Success is the message shown after a successful submission.
	Success string `yaml:"success" json:"success"`
	// RetentionDays deletes submissions older than this (default 365).
	RetentionDays int `yaml:"retention_days" json:"retention_days"`
	// ReplyTo names an email field whose value becomes the notification's Reply-To.
	ReplyTo string `yaml:"reply_to" json:"reply_to"`
}

var formFieldTypes = map[fields.Type]bool{fields.Text: true, fields.Textarea: true, fields.Email: true, fields.URL: true,
	fields.Number: true, fields.Select: true, fields.MultiSelect: true, fields.Radio: true, fields.Checkbox: true,
	fields.Boolean: true, fields.Date: true}

func (f *Form) check() error {
	if !nameRe.MatchString(f.Name) {
		return fmt.Errorf("form name %q must be lower-case letters, digits and hyphens", f.Name)
	}
	if f.Label == "" {
		f.Label = fields.Humanize(f.Name)
	}
	if len(f.Fields) == 0 {
		return errors.New("a form needs at least one field")
	}
	for _, fd := range f.Fields {
		if !formFieldTypes[fd.Type] {
			return fmt.Errorf("field %q: type %q cannot be used in a public form", fd.Name, fd.Type)
		}
		if strings.HasPrefix(fd.Name, "_") {
			return fmt.Errorf("field %q: names starting with _ are reserved", fd.Name)
		}
	}
	if f.RetentionDays <= 0 {
		f.RetentionDays = 365
	}
	if f.Success == "" {
		f.Success = "Thank you. Your message has been sent."
	}
	if f.Subject == "" {
		f.Subject = "New {label} submission"
	}
	return nil
}

type OptionsPage struct {
	Name   string
	Label  string
	Groups []*FieldGroup
}

// Fields returns every field of the page across its groups.
func (o *OptionsPage) Fields() []fields.Field {
	var out []fields.Field
	for _, g := range o.Groups {
		out = append(out, g.Fields...)
	}
	return out
}

type BlockDef struct {
	Name      string         `yaml:"name" json:"name"`
	Label     string         `yaml:"label" json:"label"`
	Icon      string         `yaml:"icon" json:"icon"`
	Category  string         `yaml:"category" json:"category"`
	Help      string         `yaml:"help" json:"help"`
	Fields    []fields.Field `yaml:"fields" json:"fields"`
	Template  string         `yaml:"template" json:"template"` // path within bundle; default blocks/<name>/<name>.gohtml
	AllowedIn []string       `yaml:"allowed_in" json:"allowed_in"`
	Children  *ChildPolicy   `yaml:"children" json:"children,omitempty"`
	Cache     string         `yaml:"cache" json:"cache"` // static | uncached
	Version   int            `yaml:"version" json:"version"`
	// Wrapper false renders the block's template output as it is, without the
	// <div class="b b-<name>"> around it (for themes whose CSS uses child
	// selectors). The editor's class and anchor settings then do nothing.
	Wrapper *bool `yaml:"wrapper" json:"wrapper,omitempty"`
	Builtin bool  `yaml:"-" json:"builtin"`
}

// Wrapped reports whether rendered output gets the wrapping div (the default).
func (d *BlockDef) Wrapped() bool { return d == nil || d.Wrapper == nil || *d.Wrapper }

type ChildPolicy struct {
	Allowed []string `yaml:"allowed" json:"allowed"`
	Max     int      `yaml:"max" json:"max"`
	Zones   []string `yaml:"zones" json:"zones"` // e.g. columns: [left, right]
}

type ImageSize struct {
	Width  int    `yaml:"width" json:"width"`
	Height int    `yaml:"height" json:"height"`
	Fit    string `yaml:"fit" json:"fit"` // cover | contain | inside
}

type Theme struct {
	Name       string               `yaml:"name" json:"name"`
	Extends    string               `yaml:"extends" json:"extends"`
	Menus      map[string]string    `yaml:"menus" json:"menus"`
	ImageSizes map[string]ImageSize `yaml:"image_sizes" json:"image_sizes"`
	CSP        CSP                  `yaml:"csp" json:"csp"`
	Settings   []fields.Field       `yaml:"settings" json:"settings"`
}

// CSP lists extra sources a theme needs on the public site.
type CSP struct {
	ScriptSrc  []string `yaml:"script_src" json:"script_src"`
	StyleSrc   []string `yaml:"style_src" json:"style_src"`
	ImgSrc     []string `yaml:"img_src" json:"img_src"`
	FontSrc    []string `yaml:"font_src" json:"font_src"`
	FrameSrc   []string `yaml:"frame_src" json:"frame_src"`
	ConnectSrc []string `yaml:"connect_src" json:"connect_src"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,47}$`)

// LoadDir loads a bundle from a directory.
func LoadDir(dir string) (*Bundle, error) {
	if _, err := os.Stat(path.Join(dir, "site.yaml")); err != nil {
		return nil, fmt.Errorf("site bundle %s: site.yaml not found", dir)
	}
	b, err := Load(os.DirFS(dir))
	if err != nil {
		return nil, err
	}
	b.Dir = dir
	return b, nil
}

// Load reads and validates a bundle from fsys. Built-in blocks (from the core)
// are merged in by the caller via AddBuiltinBlocks before Finalize.
func Load(fsys fs.FS) (*Bundle, error) {
	b := &Bundle{
		FS:          fsys,
		Types:       map[string]*ContentType{},
		Taxonomies:  map[string]*Taxonomy{},
		FieldGroups: map[string]*FieldGroup{},
		Blocks:      map[string]*BlockDef{},
		Options:     map[string]*OptionsPage{},
		Forms:       map[string]*Form{},
	}
	var errs []error
	if err := readYAML(fsys, "site.yaml", &b.Config); err != nil {
		return nil, err
	}
	if b.Config.Name == "" {
		errs = append(errs, errors.New("site.yaml: name is required"))
	}
	if b.Config.Locale == "" {
		b.Config.Locale = "en"
	}
	if b.Config.Timezone == "" {
		b.Config.Timezone = "UTC"
	}
	if b.Config.DateFormat == "" {
		b.Config.DateFormat = "2 January 2006"
	}
	if b.Config.PerPage <= 0 {
		b.Config.PerPage = 10
	}
	if b.Config.Core.Channel == "" {
		b.Config.Core.Channel = "stable"
	}

	errs = append(errs, eachYAML(fsys, "content-types", func(p string, data []byte) error {
		var t ContentType
		if err := decode(data, &t); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if _, dup := b.Types[t.Name]; dup {
			return fmt.Errorf("%s: duplicate content type %q", p, t.Name)
		}
		b.Types[t.Name] = &t
		return nil
	})...)
	errs = append(errs, eachYAML(fsys, "taxonomies", func(p string, data []byte) error {
		var t Taxonomy
		if err := decode(data, &t); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if _, dup := b.Taxonomies[t.Name]; dup {
			return fmt.Errorf("%s: duplicate taxonomy %q", p, t.Name)
		}
		b.Taxonomies[t.Name] = &t
		return nil
	})...)
	errs = append(errs, eachYAML(fsys, "field-groups", func(p string, data []byte) error {
		var g FieldGroup
		if err := decode(data, &g); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if _, dup := b.FieldGroups[g.Name]; dup {
			return fmt.Errorf("%s: duplicate field group %q", p, g.Name)
		}
		b.FieldGroups[g.Name] = &g
		return nil
	})...)
	errs = append(errs, eachYAML(fsys, "forms", func(p string, data []byte) error {
		var f Form
		if err := decode(data, &f); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if err := f.check(); err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		if _, dup := b.Forms[f.Name]; dup {
			return fmt.Errorf("%s: duplicate form %q", p, f.Name)
		}
		b.Forms[f.Name] = &f
		return nil
	})...)
	// Blocks: blocks/<name>/<name>.yaml (or blocks/<name>.yaml)
	if entries, err := fs.ReadDir(fsys, "blocks"); err == nil {
		for _, e := range entries {
			var p string
			if e.IsDir() {
				p = path.Join("blocks", e.Name(), e.Name()+".yaml")
			} else if strings.HasSuffix(e.Name(), ".yaml") {
				p = path.Join("blocks", e.Name())
			} else {
				continue
			}
			data, err := fs.ReadFile(fsys, p)
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p, err))
				continue
			}
			var d BlockDef
			if err := decode(data, &d); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", p, err))
				continue
			}
			if d.Template == "" {
				d.Template = path.Join("blocks", d.Name, d.Name+".gohtml")
			}
			b.Blocks[d.Name] = &d
		}
	}
	if _, err := fs.Stat(fsys, "theme/theme.yaml"); err == nil {
		if err := readYAML(fsys, "theme/theme.yaml", &b.Theme); err != nil {
			errs = append(errs, err)
		}
	}
	if b.Theme.Extends == "" {
		b.Theme.Extends = "base"
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	h, err := hashFS(fsys)
	if err != nil {
		return nil, err
	}
	b.Hash = h
	return b, nil
}

// AddBuiltinBlocks merges core blocks; site blocks with the same name win.
func (b *Bundle) AddBuiltinBlocks(defs []*BlockDef) {
	for _, d := range defs {
		if _, ok := b.Blocks[d.Name]; !ok {
			c := *d
			c.Builtin = true
			b.Blocks[d.Name] = &c
		}
	}
}

// Finalize applies defaults and cross-checks references. Call after
// AddBuiltinBlocks.
func (b *Bundle) Finalize() error {
	var errs []error
	if len(b.Types) == 0 {
		b.Types["page"] = &ContentType{Name: "page", Hierarchical: true, URL: "/{path}"}
	}
	patterns := map[string]string{}
	for name, t := range b.Types {
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("content type %q: name must be lowercase letters, digits, - or _", name))
			continue
		}
		applyTypeDefaults(t, b.Config.PerPage)
		if t.URL != "" {
			p, err := ParsePattern(t.URL)
			if err != nil {
				errs = append(errs, fmt.Errorf("content type %s: url: %w", name, err))
			} else {
				t.pattern = p
				if other, dup := patterns[p.Key()]; dup {
					errs = append(errs, fmt.Errorf("content types %s and %s have the same url pattern %q", other, name, t.URL))
				}
				patterns[p.Key()] = name
				if p.HasPath && !t.Hierarchical {
					errs = append(errs, fmt.Errorf("content type %s: {path} requires hierarchical: true", name))
				}
			}
		}
		if t.Archive != "" && !strings.HasPrefix(t.Archive, "/") {
			errs = append(errs, fmt.Errorf("content type %s: archive must start with /", name))
		}
		if err := fields.Check(t.Fields, ""); err != nil {
			errs = append(errs, fmt.Errorf("content type %s: %w", name, err))
		}
		for _, tx := range t.Taxonomies {
			tax, ok := b.Taxonomies[tx]
			if !ok {
				errs = append(errs, fmt.Errorf("content type %s: unknown taxonomy %q", name, tx))
				continue
			}
			tax.Types = append(tax.Types, name)
		}
		for _, g := range t.FieldGroups {
			if _, ok := b.FieldGroups[g]; !ok {
				errs = append(errs, fmt.Errorf("content type %s: unknown field group %q", name, g))
			}
		}
		for _, bl := range append(append([]string{}, t.Blocks.Allowed...), t.Blocks.Default...) {
			if _, ok := b.Blocks[bl]; !ok {
				errs = append(errs, fmt.Errorf("content type %s: unknown block %q", name, bl))
			}
		}
	}
	for name, tx := range b.Taxonomies {
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("taxonomy %q: invalid name", name))
			continue
		}
		if tx.Label.Singular == "" {
			tx.Label.Singular = fields.Humanize(name)
		}
		if tx.Label.Plural == "" {
			tx.Label.Plural = plural(tx.Label.Singular)
		}
		if tx.URL != "" {
			p, err := ParsePattern(tx.URL)
			if err != nil {
				errs = append(errs, fmt.Errorf("taxonomy %s: url: %w", name, err))
			} else {
				tx.pattern = p
				if other, dup := patterns[p.Key()]; dup {
					errs = append(errs, fmt.Errorf("taxonomy %s and %s have the same url pattern", name, other))
				}
				patterns[p.Key()] = "taxonomy:" + name
			}
		}
		if err := fields.Check(tx.Fields, ""); err != nil {
			errs = append(errs, fmt.Errorf("taxonomy %s: %w", name, err))
		}
		sort.Strings(tx.Types)
	}
	for name, g := range b.FieldGroups {
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("field group %q: invalid name", name))
			continue
		}
		if g.Label == "" {
			g.Label = fields.Humanize(name)
		}
		if g.Position == "" {
			g.Position = "main"
		}
		if err := fields.Check(g.Fields, ""); err != nil {
			errs = append(errs, fmt.Errorf("field group %s: %w", name, err))
		}
		for _, l := range g.Location {
			switch {
			case l.Type != "":
				if _, ok := b.Types[l.Type]; !ok {
					errs = append(errs, fmt.Errorf("field group %s: unknown content type %q", name, l.Type))
				}
			case l.Taxonomy != "":
				if _, ok := b.Taxonomies[l.Taxonomy]; !ok {
					errs = append(errs, fmt.Errorf("field group %s: unknown taxonomy %q", name, l.Taxonomy))
				}
			case l.OptionsPage != "":
				op := b.Options[l.OptionsPage]
				if op == nil {
					op = &OptionsPage{Name: l.OptionsPage, Label: g.Label}
					b.Options[l.OptionsPage] = op
				}
				op.Groups = append(op.Groups, g)
			}
		}
	}
	for name, d := range b.Blocks {
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Errorf("block %q: invalid name", name))
			continue
		}
		if d.Label == "" {
			d.Label = fields.Humanize(name)
		}
		if d.Version == 0 {
			d.Version = 1
		}
		if d.Category == "" {
			d.Category = "content"
		}
		if d.Cache == "" {
			d.Cache = "static"
		}
		if err := fields.CheckBlock(d.Fields); err != nil {
			errs = append(errs, fmt.Errorf("block %s: %w", name, err))
		}
		if d.Children != nil {
			for _, c := range d.Children.Allowed {
				if _, ok := b.Blocks[c]; !ok {
					errs = append(errs, fmt.Errorf("block %s: unknown child block %q", name, c))
				}
			}
		}
		if !d.Builtin {
			if _, err := fs.Stat(b.FS, d.Template); err != nil {
				errs = append(errs, fmt.Errorf("block %s: template %s not found", name, d.Template))
			}
		}
	}
	for name, s := range b.Theme.ImageSizes {
		if s.Width <= 0 && s.Height <= 0 {
			errs = append(errs, fmt.Errorf("theme image size %s: width or height required", name))
		}
	}
	if err := fields.Check(b.Theme.Settings, ""); err != nil {
		errs = append(errs, fmt.Errorf("theme settings: %w", err))
	}
	b.TypeOrder = b.TypeOrder[:0]
	for name := range b.Types {
		b.TypeOrder = append(b.TypeOrder, name)
	}
	sort.Slice(b.TypeOrder, func(i, j int) bool {
		ti, tj := b.Types[b.TypeOrder[i]], b.Types[b.TypeOrder[j]]
		if ti.MenuPosition != tj.MenuPosition {
			return ti.MenuPosition < tj.MenuPosition
		}
		return ti.Name < tj.Name
	})
	return errors.Join(errs...)
}

func applyTypeDefaults(t *ContentType, perPage int) {
	if t.Label.Singular == "" {
		t.Label.Singular = fields.Humanize(t.Name)
	}
	if t.Label.Plural == "" {
		t.Label.Plural = plural(t.Label.Singular)
	}
	if t.Public == nil {
		v := true
		t.Public = &v
	}
	if len(t.Supports) == 0 {
		t.Supports = []string{"title", "slug", "excerpt", "featured_image", "blocks", "revisions", "scheduling", "seo"}
	}
	if t.Sort.Field == "" {
		if t.Hierarchical {
			t.Sort = Sort{Field: "menu_order", Order: "asc"}
		} else {
			t.Sort = Sort{Field: "published_at", Order: "desc"}
		}
	}
	if t.Sort.Order == "" {
		t.Sort.Order = "desc"
	}
	if t.PerPage <= 0 {
		t.PerPage = perPage
	}
	if t.Cache == "" {
		t.Cache = "static"
	}
	if len(t.ListColumns) == 0 {
		t.ListColumns = []string{"title", "status", "author", "updated_at"}
	}
	if t.Sitemap == nil {
		v := *t.Public
		t.Sitemap = &v
	}
	if t.Search == nil {
		v := *t.Public
		t.Search = &v
	}
	if t.Blocks.MaxDepth <= 0 {
		t.Blocks.MaxDepth = 4
	}
}

// Has reports whether the type supports a feature (title, excerpt, blocks ...).
func (t *ContentType) Has(feature string) bool {
	for _, s := range t.Supports {
		if s == feature {
			return true
		}
	}
	return false
}

func (t *ContentType) Pattern() *Pattern { return t.pattern }
func (t *ContentType) IsPublic() bool    { return t.Public == nil || *t.Public }
func (t *ContentType) InSitemap() bool {
	return t.Sitemap != nil && *t.Sitemap && t.IsPublic() && t.URL != ""
}
func (t *ContentType) Searchable() bool { return t.Search != nil && *t.Search && t.IsPublic() }
func (t *ContentType) Cacheable() bool  { return t.Cache != "uncached" }

// AllowsBlock reports whether a block type may be used at the top level.
func (t *ContentType) AllowsBlock(name string, d *BlockDef) bool {
	if len(t.Blocks.Allowed) > 0 {
		for _, a := range t.Blocks.Allowed {
			if a == name {
				return true
			}
		}
		return false
	}
	if len(d.AllowedIn) > 0 {
		for _, a := range d.AllowedIn {
			if a == t.Name {
				return true
			}
		}
		return false
	}
	return true
}

func (tx *Taxonomy) Pattern() *Pattern { return tx.pattern }

// FieldsFor returns every field definition that applies to a content type:
// the type's inline fields followed by attached and located field groups.
func (b *Bundle) FieldsFor(typeName string) []fields.Field {
	t := b.Types[typeName]
	if t == nil {
		return nil
	}
	out := append([]fields.Field{}, t.Fields...)
	for _, g := range b.GroupsFor(typeName) {
		out = append(out, g.Fields...)
	}
	return out
}

// GroupsFor returns field groups attached to a type, in a stable order.
func (b *Bundle) GroupsFor(typeName string) []*FieldGroup {
	t := b.Types[typeName]
	if t == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []*FieldGroup
	for _, g := range t.FieldGroups {
		if fg := b.FieldGroups[g]; fg != nil && !seen[g] {
			out = append(out, fg)
			seen[g] = true
		}
	}
	names := make([]string, 0, len(b.FieldGroups))
	for n := range b.FieldGroups {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		g := b.FieldGroups[n]
		for _, l := range g.Location {
			if l.Type == typeName && !seen[n] {
				out = append(out, g)
				seen[n] = true
			}
		}
	}
	return out
}

// TermFields returns field definitions for terms of a taxonomy.
func (b *Bundle) TermFields(tax string) []fields.Field {
	t := b.Taxonomies[tax]
	if t == nil {
		return nil
	}
	out := append([]fields.Field{}, t.Fields...)
	for _, g := range b.FieldGroups {
		for _, l := range g.Location {
			if l.Taxonomy == tax {
				out = append(out, g.Fields...)
			}
		}
	}
	return out
}

// MediaFields returns extra field definitions for media items.
func (b *Bundle) MediaFields() []fields.Field {
	var out []fields.Field
	for _, g := range b.FieldGroups {
		for _, l := range g.Location {
			if l.Media {
				out = append(out, g.Fields...)
			}
		}
	}
	return out
}

func plural(s string) string {
	switch {
	case s == "":
		return s
	case strings.HasSuffix(s, "y") && !strings.HasSuffix(s, "ay") && !strings.HasSuffix(s, "ey") && !strings.HasSuffix(s, "oy"):
		return s[:len(s)-1] + "ies"
	case strings.HasSuffix(s, "s"), strings.HasSuffix(s, "x"), strings.HasSuffix(s, "ch"), strings.HasSuffix(s, "sh"):
		return s + "es"
	}
	return s + "s"
}

func decode(data []byte, v any) error {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

func readYAML(fsys fs.FS, p string, v any) error {
	data, err := fs.ReadFile(fsys, p)
	if err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	if err := decode(data, v); err != nil {
		return fmt.Errorf("%s: %w", p, err)
	}
	return nil
}

func eachYAML(fsys fs.FS, dir string, fn func(p string, data []byte) error) []error {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil // directory is optional
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() || !(strings.HasSuffix(e.Name(), ".yaml") || strings.HasSuffix(e.Name(), ".yml")) {
			continue
		}
		p := path.Join(dir, e.Name())
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := fn(p, data); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// hashFS hashes every regular file (path + content) in sorted order.
func hashFS(fsys fs.FS) (string, error) {
	var files []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		name := d.Name()
		if d.IsDir() && (name == ".git" || name == "node_modules" || name == ".cms") {
			return fs.SkipDir
		}
		if d.Type().IsRegular() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	h := sha256.New()
	for _, f := range files {
		data, err := fs.ReadFile(fsys, f)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", f, len(data))
		h.Write(data)
	}
	return hex.EncodeToString(h.Sum(nil))[:16], nil
}
