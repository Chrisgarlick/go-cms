// Package starters embeds the site templates used by `cms new`.
package starters

import (
	"embed"
	"io/fs"
	"sort"
)

//go:embed all:blank all:blog all:brochure
var embedded embed.FS

// Info describes a starter.
type Info struct {
	Name        string
	Description string
	Planned     bool
}

var list = []Info{
	{"blog", "Posts, categories, tags, RSS and a reading-focused theme", false},
	{"brochure", "Home, services, case studies, team and contact pages for a business site", false},
	{"blank", "One page type and the base theme. Build from scratch.", false},
	{"shop", "Products with Stripe Checkout (planned, see spec 4.21)", true},
}

func List() []Info { return list }

// Get returns a starter's files.
func Get(name string) (fs.FS, bool) {
	for _, s := range list {
		if s.Name == name && !s.Planned {
			sub, err := fs.Sub(embedded, name)
			return sub, err == nil
		}
	}
	return nil, false
}

// Names lists available starters.
func Names() []string {
	var out []string
	for _, s := range list {
		if !s.Planned {
			out = append(out, s.Name)
		}
	}
	sort.Strings(out)
	return out
}
