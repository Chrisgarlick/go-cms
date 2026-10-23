// Package fields defines field types (spec 4.2) and validates stored values.
// Values are JSON; Validate returns a cleaned copy (unknown keys dropped,
// defaults applied) plus every reference to media, entries, terms and users so
// callers can track usage and render dependencies.
package fields

import (
	"encoding/json"
	"fmt"
	"math"
	"net/mail"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"cms/internal/richtext"
)

type Type string

const (
	Text        Type = "text"
	Textarea    Type = "textarea"
	Number      Type = "number"
	Range       Type = "range"
	Email       Type = "email"
	URL         Type = "url"
	Slug        Type = "slug"
	RichText    Type = "richtext"
	Markdown    Type = "markdown"
	Code        Type = "code"
	OEmbed      Type = "oembed"
	HTML        Type = "html"
	Select      Type = "select"
	MultiSelect Type = "multiselect"
	Checkbox    Type = "checkbox"
	Radio       Type = "radio"
	Boolean     Type = "boolean"
	Date        Type = "date"
	DateTime    Type = "datetime"
	TimeOfDay   Type = "time"
	Image       Type = "image"
	File        Type = "file"
	Gallery     Type = "gallery"
	EntryRef    Type = "entry"
	Entries     Type = "entries"
	TermRef     Type = "term"
	Terms       Type = "terms"
	UserRef     Type = "user"
	Group       Type = "group"
	Repeater    Type = "repeater"
	Link        Type = "link"
	Color       Type = "color"
	KeyValue    Type = "keyvalue"
	Message     Type = "message" // help text only; stores nothing
)

var knownTypes = map[Type]bool{
	Text: true, Textarea: true, Number: true, Range: true, Email: true, URL: true, Slug: true,
	RichText: true, Markdown: true, Code: true, OEmbed: true, HTML: true, Select: true,
	MultiSelect: true, Checkbox: true, Radio: true, Boolean: true, Date: true, DateTime: true,
	TimeOfDay: true, Image: true, File: true, Gallery: true, EntryRef: true, Entries: true,
	TermRef: true, Terms: true, UserRef: true, Group: true, Repeater: true, Link: true,
	Color: true, KeyValue: true, Message: true,
}

// Option is one choice for select/radio/checkbox fields. YAML accepts either a
// plain string or {value, label}.
type Option struct {
	Value string `yaml:"value" json:"value"`
	Label string `yaml:"label" json:"label"`
}

func (o *Option) UnmarshalYAML(unmarshal func(any) error) error {
	var s string
	if err := unmarshal(&s); err == nil {
		o.Value, o.Label = s, humanize(s)
		return nil
	}
	type plain Option
	var p plain
	if err := unmarshal(&p); err != nil {
		return err
	}
	*o = Option(p)
	if o.Label == "" {
		o.Label = humanize(o.Value)
	}
	return nil
}

type Condition struct {
	Field    string `yaml:"field" json:"field"`
	Equals   any    `yaml:"equals" json:"equals,omitempty"`
	NotEmpty bool   `yaml:"not_empty" json:"not_empty,omitempty"`
}

// Field is one field definition, shared by content types, field groups,
// blocks, taxonomies and options pages.
type Field struct {
	Name         string     `yaml:"name" json:"name"`
	Label        string     `yaml:"label" json:"label"`
	Type         Type       `yaml:"type" json:"type"`
	Help         string     `yaml:"help" json:"help,omitempty"`
	Placeholder  string     `yaml:"placeholder" json:"placeholder,omitempty"`
	Required     bool       `yaml:"required" json:"required,omitempty"`
	Default      any        `yaml:"default" json:"default,omitempty"`
	Min          *float64   `yaml:"min" json:"min,omitempty"`
	Max          *float64   `yaml:"max" json:"max,omitempty"`
	Step         float64    `yaml:"step" json:"step,omitempty"`
	MinLength    int        `yaml:"min_length" json:"min_length,omitempty"`
	MaxLength    int        `yaml:"max_length" json:"max_length,omitempty"`
	Pattern      string     `yaml:"pattern" json:"pattern,omitempty"`
	Options      []Option   `yaml:"options" json:"options,omitempty"`
	Fields       []Field    `yaml:"fields" json:"fields,omitempty"`       // group, repeater
	MinItems     int        `yaml:"min_items" json:"min_items,omitempty"` // repeater, gallery, multi refs
	MaxItems     int        `yaml:"max_items" json:"max_items,omitempty"`
	Types        []string   `yaml:"types" json:"types,omitempty"`       // entry refs: allowed content types
	Taxonomy     string     `yaml:"taxonomy" json:"taxonomy,omitempty"` // term refs
	Accept       []string   `yaml:"accept" json:"accept,omitempty"`     // file: mime prefixes
	Language     string     `yaml:"language" json:"language,omitempty"` // code
	Searchable   bool       `yaml:"searchable" json:"searchable,omitempty"`
	Filterable   bool       `yaml:"filterable" json:"filterable,omitempty"`
	Translatable bool       `yaml:"translatable" json:"translatable,omitempty"`
	Width        int        `yaml:"width" json:"width,omitempty"` // admin column width, percent
	Rows         int        `yaml:"rows" json:"rows,omitempty"`
	Condition    *Condition `yaml:"condition" json:"condition,omitempty"`
	Capability   string     `yaml:"capability" json:"capability,omitempty"` // e.g. content.raw_html

	re *regexp.Regexp
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// reserved names collide with core entry attributes in templates.
var reserved = map[string]bool{"id": true, "type": true, "status": true, "title": true, "slug": true, "url": true, "blocks": true}

// Check validates entry-level definitions: core attribute names are reserved.
func Check(fs []Field, path string) error { return check(fs, path, path == "") }

// CheckBlock validates block field definitions, where no names are reserved.
func CheckBlock(fs []Field) error { return check(fs, "", false) }

func check(fs []Field, path string, reserve bool) error {
	seen := map[string]bool{}
	for i := range fs {
		f := &fs[i]
		p := path + f.Name
		if !nameRe.MatchString(f.Name) {
			return fmt.Errorf("%s: field name %q must be lowercase letters, digits and underscores", path, f.Name)
		}
		if reserve && reserved[f.Name] {
			return fmt.Errorf("field name %q is reserved", f.Name)
		}
		if seen[f.Name] {
			return fmt.Errorf("%s: duplicate field %q", path, f.Name)
		}
		seen[f.Name] = true
		if f.Type == "" {
			f.Type = Text
		}
		if !knownTypes[f.Type] {
			return fmt.Errorf("%s: unknown field type %q", p, f.Type)
		}
		if f.Label == "" {
			f.Label = humanize(f.Name)
		}
		if f.Pattern != "" {
			re, err := regexp.Compile(f.Pattern) // RE2: linear time
			if err != nil {
				return fmt.Errorf("%s: bad pattern: %w", p, err)
			}
			f.re = re
		}
		switch f.Type {
		case Select, MultiSelect, Checkbox, Radio:
			if len(f.Options) == 0 {
				return fmt.Errorf("%s: %s field needs options", p, f.Type)
			}
		case Group, Repeater:
			if len(f.Fields) == 0 {
				return fmt.Errorf("%s: %s field needs sub-fields", p, f.Type)
			}
			if err := check(f.Fields, p+".", false); err != nil {
				return err
			}
		case TermRef, Terms:
			if f.Taxonomy == "" {
				return fmt.Errorf("%s: %s field needs a taxonomy", p, f.Type)
			}
		case HTML:
			if f.Capability == "" {
				f.Capability = "content.raw_html"
			}
		}
		if f.Condition != nil && !seen[f.Condition.Field] && !contains(fs, f.Condition.Field) {
			return fmt.Errorf("%s: condition refers to unknown field %q", p, f.Condition.Field)
		}
	}
	return nil
}

func contains(fs []Field, name string) bool {
	for _, f := range fs {
		if f.Name == name {
			return true
		}
	}
	return false
}

// Refs collects references found while validating.
type Refs struct {
	Media   map[string]string // media id -> field path
	Entries []uuid.UUID
	Terms   []uuid.UUID
	Users   []uuid.UUID
	// Text is the searchable plain text of fields marked searchable.
	Text []string
}

func newRefs() *Refs { return &Refs{Media: map[string]string{}} }

// Errors maps a field path ("hero.cta.url", "items.2.title") to a message.
type Errors map[string]string

func (e Errors) Error() string {
	keys := make([]string, 0, len(e))
	for k := range e {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteString("; ")
		}
		b.WriteString(k + ": " + e[k])
	}
	return b.String()
}

// Context controls permission-dependent validation.
type Context struct {
	// CanUse reports whether the actor holds a field's capability (raw HTML etc).
	CanUse func(capability string) bool
	// Partial allows required fields to be empty (autosave of a draft).
	Partial bool
}

// Validate checks values against defs and returns the cleaned values.
func Validate(defs []Field, raw json.RawMessage, c Context) (json.RawMessage, *Refs, error) {
	in := map[string]any{}
	if len(raw) > 0 && string(raw) != "null" {
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.UseNumber()
		if err := dec.Decode(&in); err != nil {
			return nil, nil, Errors{"": "fields must be a JSON object"}
		}
	}
	refs := newRefs()
	errs := Errors{}
	out := validateObject(defs, in, "", c, refs, errs)
	if len(errs) > 0 {
		return nil, refs, errs
	}
	b, err := json.Marshal(out)
	return b, refs, err
}

func validateObject(defs []Field, in map[string]any, path string, c Context, refs *Refs, errs Errors) map[string]any {
	out := map[string]any{}
	for i := range defs {
		f := &defs[i]
		if f.Type == Message {
			continue
		}
		p := path + f.Name
		v, present := in[f.Name]
		if f.Condition != nil && !conditionMet(f.Condition, in) {
			continue // hidden fields are not stored
		}
		if f.Capability != "" && c.CanUse != nil && !c.CanUse(f.Capability) {
			// Keep nothing the actor is not allowed to set; existing values are
			// merged back by the caller.
			continue
		}
		if !present || isEmpty(v) {
			if f.Default != nil && !present {
				v = f.Default
			} else {
				if f.Required && !c.Partial {
					errs[p] = "is required"
				}
				continue
			}
		}
		cv, err := validateValue(f, v, p, c, refs, errs)
		if err != "" {
			errs[p] = err
			continue
		}
		if cv != nil {
			out[f.Name] = cv
		}
	}
	return out
}

func conditionMet(cd *Condition, in map[string]any) bool {
	v := in[cd.Field]
	if cd.NotEmpty {
		return !isEmpty(v)
	}
	return fmt.Sprint(v) == fmt.Sprint(cd.Equals)
}

func isEmpty(v any) bool {
	switch t := v.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(t) == ""
	case []any:
		return len(t) == 0
	case map[string]any:
		return len(t) == 0
	}
	return false
}

var (
	colorRe = regexp.MustCompile(`^#(?:[0-9a-fA-F]{3}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
	slugRe  = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)
)

const maxString = 1 << 20 // 1 MiB per string value

func validateValue(f *Field, v any, p string, c Context, refs *Refs, errs Errors) (any, string) {
	switch f.Type {
	case Text, Textarea, Markdown, Code, Slug, Email, URL, OEmbed, Color, HTML:
		s, ok := v.(string)
		if !ok {
			return nil, "must be text"
		}
		if f.Type == Text || f.Type == Slug || f.Type == Email || f.Type == Color {
			s = strings.TrimSpace(s)
			if strings.ContainsAny(s, "\r\n") {
				return nil, "must be a single line"
			}
		}
		if len(s) > maxString {
			return nil, "is too long"
		}
		n := len([]rune(s))
		if f.MinLength > 0 && n < f.MinLength {
			return nil, fmt.Sprintf("must be at least %d characters", f.MinLength)
		}
		if f.MaxLength > 0 && n > f.MaxLength {
			return nil, fmt.Sprintf("must be at most %d characters", f.MaxLength)
		}
		if f.re != nil && !f.re.MatchString(s) {
			return nil, "has the wrong format"
		}
		switch f.Type {
		case Email:
			a, err := mail.ParseAddress(s)
			if err != nil || a.Address != s {
				return nil, "must be an email address"
			}
		case URL, OEmbed:
			u, ok := richtext.SafeURL(s)
			if !ok {
				return nil, "must be an http(s), mailto, tel or relative URL"
			}
			s = u
		case Slug:
			if !slugRe.MatchString(s) {
				return nil, "must contain only lowercase letters, digits and hyphens"
			}
		case Color:
			if !colorRe.MatchString(s) {
				return nil, "must be a hex colour like #1a2b3c"
			}
		}
		if f.Searchable {
			refs.Text = append(refs.Text, s)
		}
		return s, ""
	case RichText:
		b, err := json.Marshal(v)
		if err != nil {
			return nil, "must be a rich text document"
		}
		n, err := richtext.Parse(b)
		if err != nil {
			return nil, err.Error()
		}
		if f.Searchable {
			refs.Text = append(refs.Text, richtext.PlainText(n))
		}
		return n, ""
	case Number, Range:
		var x float64
		switch t := v.(type) {
		case json.Number:
			fv, err := t.Float64()
			if err != nil {
				return nil, "must be a number"
			}
			x = fv
		case float64:
			x = t
		case int:
			x = float64(t)
		case string:
			fv, err := strconv.ParseFloat(strings.TrimSpace(t), 64)
			if err != nil {
				return nil, "must be a number"
			}
			x = fv
		default:
			return nil, "must be a number"
		}
		if math.IsNaN(x) || math.IsInf(x, 0) {
			return nil, "must be a number"
		}
		if f.Min != nil && x < *f.Min {
			return nil, fmt.Sprintf("must be at least %v", *f.Min)
		}
		if f.Max != nil && x > *f.Max {
			return nil, fmt.Sprintf("must be at most %v", *f.Max)
		}
		return x, ""
	case Boolean:
		switch t := v.(type) {
		case bool:
			return t, ""
		case string:
			return t == "true" || t == "on" || t == "1", ""
		}
		return nil, "must be true or false"
	case Select, Radio:
		s, ok := v.(string)
		if !ok || !hasOption(f, s) {
			return nil, "is not one of the allowed options"
		}
		return s, ""
	case MultiSelect, Checkbox:
		list, ok := toStrings(v)
		if !ok {
			return nil, "must be a list of options"
		}
		for _, s := range list {
			if !hasOption(f, s) {
				return nil, fmt.Sprintf("%q is not an allowed option", s)
			}
		}
		return list, ""
	case Date:
		s, _ := v.(string)
		if _, err := time.Parse("2006-01-02", s); err != nil {
			return nil, "must be a date (YYYY-MM-DD)"
		}
		return s, ""
	case DateTime:
		s, _ := v.(string)
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			if t, err = time.Parse("2006-01-02T15:04", s); err != nil {
				return nil, "must be a date and time"
			}
		}
		return t.UTC().Format(time.RFC3339), ""
	case TimeOfDay:
		s, _ := v.(string)
		if _, err := time.Parse("15:04", s); err != nil {
			return nil, "must be a time (HH:MM)"
		}
		return s, ""
	case Image, File, EntryRef, TermRef, UserRef:
		s, _ := v.(string)
		id, err := uuid.Parse(s)
		if err != nil {
			return nil, "must reference an item"
		}
		addRef(f.Type, id, p, refs)
		return id.String(), ""
	case Gallery, Entries, Terms:
		list, ok := toStrings(v)
		if !ok {
			return nil, "must be a list"
		}
		if f.MaxItems > 0 && len(list) > f.MaxItems {
			return nil, fmt.Sprintf("must have at most %d items", f.MaxItems)
		}
		if f.MinItems > 0 && len(list) < f.MinItems && !c.Partial {
			return nil, fmt.Sprintf("must have at least %d items", f.MinItems)
		}
		out := make([]string, 0, len(list))
		for i, s := range list {
			id, err := uuid.Parse(s)
			if err != nil {
				return nil, "contains an invalid reference"
			}
			addRef(f.Type, id, fmt.Sprintf("%s.%d", p, i), refs)
			out = append(out, id.String())
		}
		return out, ""
	case Link:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, "must be a link"
		}
		href, _ := m["url"].(string)
		u, ok := richtext.SafeURL(href)
		if !ok {
			return nil, "link URL is not allowed"
		}
		label, _ := m["label"].(string)
		out := map[string]any{"url": u, "label": strings.TrimSpace(label)}
		if t, _ := m["target"].(string); t == "_blank" {
			out["target"] = "_blank"
		}
		return out, ""
	case Group:
		m, ok := v.(map[string]any)
		if !ok {
			return nil, "must be a group of fields"
		}
		return validateObject(f.Fields, m, p+".", c, refs, errs), ""
	case Repeater:
		list, ok := v.([]any)
		if !ok {
			return nil, "must be a list"
		}
		if f.MaxItems > 0 && len(list) > f.MaxItems {
			return nil, fmt.Sprintf("must have at most %d items", f.MaxItems)
		}
		if f.MinItems > 0 && len(list) < f.MinItems && !c.Partial {
			return nil, fmt.Sprintf("must have at least %d items", f.MinItems)
		}
		out := make([]any, 0, len(list))
		for i, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, "items must be groups of fields"
			}
			out = append(out, validateObject(f.Fields, m, fmt.Sprintf("%s.%d.", p, i), c, refs, errs))
		}
		return out, ""
	case KeyValue:
		list, ok := v.([]any)
		if !ok {
			return nil, "must be a list of key/value pairs"
		}
		out := make([]map[string]string, 0, len(list))
		for _, item := range list {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, "must be a list of key/value pairs"
			}
			k, _ := m["key"].(string)
			val, _ := m["value"].(string)
			if strings.TrimSpace(k) == "" {
				continue
			}
			out = append(out, map[string]string{"key": strings.TrimSpace(k), "value": val})
		}
		return out, ""
	}
	return nil, "unsupported field type"
}

func addRef(t Type, id uuid.UUID, path string, refs *Refs) {
	switch t {
	case Image, File, Gallery:
		refs.Media[id.String()] = path
	case EntryRef, Entries:
		refs.Entries = append(refs.Entries, id)
	case TermRef, Terms:
		refs.Terms = append(refs.Terms, id)
	case UserRef:
		refs.Users = append(refs.Users, id)
	}
}

func hasOption(f *Field, s string) bool {
	for _, o := range f.Options {
		if o.Value == s {
			return true
		}
	}
	return false
}

func toStrings(v any) ([]string, bool) {
	switch t := v.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			if s != "" {
				out = append(out, s)
			}
		}
		return out, true
	case string:
		if t == "" {
			return nil, true
		}
		return []string{t}, true
	}
	return nil, false
}

func humanize(s string) string {
	s = strings.NewReplacer("_", " ", "-", " ").Replace(s)
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Humanize is exported for schema loaders.
func Humanize(s string) string { return humanize(s) }
