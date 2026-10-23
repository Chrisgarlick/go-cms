// Package menus stores navigation menus (spec 4.6). Items link to entries and
// terms by id, so slug changes update menus automatically.
package menus

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/blocks"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/events"
	"cms/internal/jobs"
	"cms/internal/rbac"
	"cms/internal/richtext"
	"cms/internal/site"
)

// Item is a stored menu item.
type Item struct {
	ID       string `json:"id"`
	Type     string `json:"type"` // entry | term | url | label
	Ref      string `json:"ref,omitempty"`
	URL      string `json:"url,omitempty"`
	Label    string `json:"label"`
	Target   string `json:"target,omitempty"`
	Rel      string `json:"rel,omitempty"`
	Class    string `json:"class,omitempty"`
	Children []Item `json:"children,omitempty"`
}

type Menu struct {
	Name  string
	Label string
	Items []Item
}

const maxItems = 200

type Service struct {
	DB   *db.DB
	Site func() *site.Bundle
	Jobs jobs.Enqueuer
}

// Locations are declared by the theme (theme.yaml menus).
func (s *Service) Locations() map[string]string { return s.Site().Theme.Menus }

func (s *Service) Get(ctx context.Context, name string) (*Menu, error) {
	row, err := s.DB.Q.GetMenu(ctx, name)
	if db.IsNotFound(err) {
		label := s.Locations()[name]
		if label == "" {
			label = name
		}
		return &Menu{Name: name, Label: label}, nil
	}
	if err != nil {
		return nil, err
	}
	var items []Item
	_ = json.Unmarshal(row.Items, &items)
	return &Menu{Name: row.Name, Label: row.Label, Items: items}, nil
}

func (s *Service) List(ctx context.Context) ([]dbq.Menu, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, err
	}
	return s.DB.Q.ListMenus(ctx)
}

// Save validates and stores a menu.
func (s *Service) Save(ctx context.Context, name, label string, items []Item) error {
	if _, err := actor.Require(ctx, rbac.MenusManage); err != nil {
		return err
	}
	if !validName(name) {
		return errors.New("menu name must be lowercase letters, digits and hyphens")
	}
	count := 0
	clean, err := cleanItems(items, 0, &count)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(clean)
	if strings.TrimSpace(label) == "" {
		label = name
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SaveMenu(ctx, dbq.SaveMenuParams{Name: name, Label: strings.TrimSpace(label), Items: b}); err != nil {
			return err
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: []jobs.Dep{{Type: "menu", ID: name}}}); err != nil {
			return err
		}
		if err := events.Notify(ctx, tx, events.Event{Topic: events.MenusChanged, ID: name}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "menu.save", ResourceType: "menu", ResourceID: name,
			Diff: map[string]any{"items": count}})
	})
}

func validName(s string) bool {
	if s == "" || len(s) > 48 {
		return false
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

func cleanItems(in []Item, depth int, count *int) ([]Item, error) {
	if depth > 4 {
		return nil, errors.New("menus can be nested at most 4 levels deep")
	}
	out := make([]Item, 0, len(in))
	for _, it := range in {
		*count++
		if *count > maxItems {
			return nil, fmt.Errorf("a menu can have at most %d items", maxItems)
		}
		it.Label = strings.TrimSpace(it.Label)
		if len([]rune(it.Label)) > 120 {
			return nil, errors.New("menu labels must be at most 120 characters")
		}
		if it.ID == "" {
			it.ID = blocks.NewID()
		}
		switch it.Type {
		case "entry", "term":
			if _, err := uuid.Parse(it.Ref); err != nil {
				return nil, fmt.Errorf("menu item %q: invalid reference", it.Label)
			}
			it.URL = ""
		case "url":
			u, ok := richtext.SafeURL(it.URL)
			if !ok {
				return nil, fmt.Errorf("menu item %q: URL is not allowed", it.Label)
			}
			it.URL = u
			it.Ref = ""
		case "label":
			it.Ref, it.URL = "", ""
			if it.Label == "" {
				return nil, errors.New("label-only menu items need a label")
			}
		default:
			return nil, fmt.Errorf("menu item %q: unknown type %q", it.Label, it.Type)
		}
		if it.Target != "" && it.Target != "_blank" {
			it.Target = ""
		}
		it.Rel = sanitizeTokens(it.Rel)
		it.Class = sanitizeTokens(it.Class)
		kids, err := cleanItems(it.Children, depth+1, count)
		if err != nil {
			return nil, err
		}
		it.Children = kids
		out = append(out, it)
	}
	return out, nil
}

func sanitizeTokens(s string) string {
	var out []string
	for _, f := range strings.Fields(s) {
		ok := len(f) <= 40
		for _, r := range f {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				ok = false
			}
		}
		if ok {
			out = append(out, f)
		}
	}
	return strings.Join(out, " ")
}

func (s *Service) Delete(ctx context.Context, name string) error {
	if _, err := actor.Require(ctx, rbac.MenusManage); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.DeleteMenu(ctx, name); err != nil {
			return err
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: []jobs.Dep{{Type: "menu", ID: name}}}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "menu.delete", ResourceType: "menu", ResourceID: name})
	})
}
