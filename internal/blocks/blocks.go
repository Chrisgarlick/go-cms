// Package blocks validates block trees stored as
// [{id, type, version, data, settings, children}] (spec 4.3).
package blocks

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"cms/internal/fields"
	"cms/internal/site"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// Builtin returns the core's starter block definitions.
func Builtin() []*site.BlockDef {
	files, _ := fs.Glob(builtinFS, "builtin/*.yaml")
	out := make([]*site.BlockDef, 0, len(files))
	for _, f := range files {
		data, _ := builtinFS.ReadFile(f)
		var d site.BlockDef
		if err := yaml.Unmarshal(data, &d); err != nil {
			panic(fmt.Sprintf("builtin block %s: %v", f, err))
		}
		d.Template = path.Join("blocks", d.Name+".gohtml")
		d.Builtin = true
		out = append(out, &d)
	}
	return out
}

// Block is one node in a block tree.
type Block struct {
	ID       string          `json:"id"`
	Type     string          `json:"type"`
	Version  int             `json:"version,omitempty"`
	Data     json.RawMessage `json:"data"`
	Settings Settings        `json:"settings,omitempty"`
	Children []Block         `json:"children,omitempty"`
}

// Settings are common to every block.
type Settings struct {
	Anchor string `json:"anchor,omitempty"`
	Class  string `json:"class,omitempty"`
	// Visibility window; publishing re-renders at the edges (scheduler).
	From  string `json:"from,omitempty"`
	Until string `json:"until,omitempty"`
	// LoggedIn makes the whole page uncacheable (spec 4.12).
	LoggedIn bool `json:"logged_in,omitempty"`
	Hidden   bool `json:"hidden,omitempty"`
}

var (
	anchorRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
	classRe  = regexp.MustCompile(`^[a-zA-Z0-9_ -]{0,128}$`)
	idRe     = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,32}$`)
)

const maxBlocks = 2000

// Result of validating a tree.
type Result struct {
	Blocks   []Block
	Refs     *fields.Refs
	Uncached bool
	Text     []string
	Types    map[string]bool
}

// Validate checks a tree for a content type and returns a cleaned copy.
func Validate(b *site.Bundle, ct *site.ContentType, raw json.RawMessage, c fields.Context) (*Result, error) {
	var tree []Block
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &tree); err != nil {
			return nil, fields.Errors{"blocks": "must be a list of blocks"}
		}
	}
	res := &Result{Refs: &fields.Refs{Media: map[string]string{}}, Types: map[string]bool{}}
	errs := fields.Errors{}
	count := 0
	seenIDs := map[string]bool{}
	res.Blocks = validateList(b, ct, tree, nil, 1, "blocks", c, res, errs, &count, seenIDs)
	if len(errs) > 0 {
		return res, errs
	}
	return res, nil
}

func validateList(b *site.Bundle, ct *site.ContentType, in []Block, parent *site.BlockDef, depth int,
	path string, c fields.Context, res *Result, errs fields.Errors, count *int, seen map[string]bool) []Block {
	out := make([]Block, 0, len(in))
	if parent != nil && parent.Children.Max > 0 && len(in) > parent.Children.Max {
		errs[path] = fmt.Sprintf("may contain at most %d blocks", parent.Children.Max)
	}
	for i, bl := range in {
		p := fmt.Sprintf("%s.%d", path, i)
		*count++
		if *count > maxBlocks {
			errs["blocks"] = fmt.Sprintf("a page may contain at most %d blocks", maxBlocks)
			return out
		}
		def := b.Blocks[bl.Type]
		if def == nil {
			errs[p] = fmt.Sprintf("unknown block type %q", bl.Type)
			continue
		}
		if parent == nil {
			if !ct.AllowsBlock(bl.Type, def) {
				errs[p] = fmt.Sprintf("%s blocks are not allowed in %s", def.Label, ct.Label.Plural)
				continue
			}
		} else if !contains(parent.Children.Allowed, bl.Type) {
			errs[p] = fmt.Sprintf("%s blocks are not allowed inside %s", def.Label, parent.Label)
			continue
		}
		if bl.ID == "" || !idRe.MatchString(bl.ID) || seen[bl.ID] {
			bl.ID = NewID()
		}
		seen[bl.ID] = true
		data, refs, err := fields.Validate(def.Fields, bl.Data, c)
		if err != nil {
			if fe := (fields.Errors)(nil); errors.As(err, &fe) {
				for k, v := range fe {
					errs[p+".data."+k] = v
				}
			} else {
				errs[p] = err.Error()
			}
			continue
		}
		mergeRefs(res.Refs, refs, p)
		res.Text = append(res.Text, refs.Text...)
		res.Types[bl.Type] = true
		s := bl.Settings
		if s.Anchor != "" && !anchorRe.MatchString(s.Anchor) {
			errs[p+".settings.anchor"] = "must start with a letter and contain only letters, digits, - and _"
		}
		if !classRe.MatchString(s.Class) {
			errs[p+".settings.class"] = "may contain only letters, digits, spaces, - and _"
		}
		s.Class = strings.Join(strings.Fields(s.Class), " ")
		if s.LoggedIn || def.Cache == "uncached" {
			res.Uncached = true
		}
		nb := Block{ID: bl.ID, Type: bl.Type, Version: def.Version, Data: data, Settings: s}
		if len(bl.Children) > 0 {
			if def.Children == nil {
				errs[p] = fmt.Sprintf("%s blocks cannot contain other blocks", def.Label)
				continue
			}
			if depth >= ct.Blocks.MaxDepth {
				errs[p] = fmt.Sprintf("blocks may be nested at most %d deep", ct.Blocks.MaxDepth)
				continue
			}
			nb.Children = validateList(b, ct, bl.Children, def, depth+1, p+".children", c, res, errs, count, seen)
		}
		out = append(out, nb)
	}
	return out
}

func mergeRefs(dst, src *fields.Refs, prefix string) {
	for k, v := range src.Media {
		dst.Media[k] = prefix + "." + v
	}
	dst.Entries = append(dst.Entries, src.Entries...)
	dst.Terms = append(dst.Terms, src.Terms...)
	dst.Users = append(dst.Users, src.Users...)
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// NewID returns a short random block id.
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Defaults builds the initial tree for a new entry from the type's default list.
func Defaults(ct *site.ContentType) json.RawMessage {
	tree := make([]Block, 0, len(ct.Blocks.Default))
	for _, name := range ct.Blocks.Default {
		tree = append(tree, Block{ID: NewID(), Type: name, Data: json.RawMessage("{}")})
	}
	b, _ := json.Marshal(tree)
	return b
}

// Walk visits every block depth-first.
func Walk(tree []Block, fn func(b *Block)) {
	for i := range tree {
		fn(&tree[i])
		Walk(tree[i].Children, fn)
	}
}

// Parse decodes a stored tree (already validated).
func Parse(raw json.RawMessage) []Block {
	var tree []Block
	_ = json.Unmarshal(raw, &tree)
	return tree
}
