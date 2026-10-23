// Package settings stores site settings (JSON values by key) and options pages,
// cached in memory and invalidated across processes by events.
package settings

import (
	"context"
	"encoding/json"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/events"
	"cms/internal/fields"
	"cms/internal/jobs"
	"cms/internal/rbac"
	"cms/internal/site"
)

// Known setting keys.
const (
	FrontPage      = "reading.front_page"      // entry id shown at "/"
	PostsType      = "reading.home_type"       // content type listed on the home page when no front page
	RobotsExtra    = "seo.robots_extra"        // extra robots.txt lines
	DiscourageBots = "seo.discourage_indexing" // noindex everything (staging)
	SiteTitle      = "site.title"              // overrides site.yaml name
	SiteTagline    = "site.tagline"
	TitleFormat    = "seo.title_format" // e.g. "{title} | {site}"
	ThemeSettings  = "theme.settings"
	BundleHash     = "system.bundle_hash" // last bundle hash rendered (rebuild on change)
)

type Service struct {
	DB   *db.DB
	Site func() *site.Bundle
	Jobs jobs.Enqueuer

	mu      sync.RWMutex
	cache   map[string]json.RawMessage
	options map[string]json.RawMessage
}

// Invalidate drops cached values (called on SettingsChanged / OptionsChanged).
func (s *Service) Invalidate() {
	s.mu.Lock()
	s.cache, s.options = nil, nil
	s.mu.Unlock()
}

func (s *Service) load(ctx context.Context) (map[string]json.RawMessage, error) {
	s.mu.RLock()
	c := s.cache
	s.mu.RUnlock()
	if c != nil {
		return c, nil
	}
	rows, err := s.DB.Q.ListSettings(ctx)
	if err != nil {
		return nil, err
	}
	c = make(map[string]json.RawMessage, len(rows))
	for _, r := range rows {
		c[r.Key] = r.Value
	}
	s.mu.Lock()
	s.cache = c
	s.mu.Unlock()
	return c, nil
}

// Raw returns a setting's JSON (nil if unset).
func (s *Service) Raw(ctx context.Context, key string) json.RawMessage {
	c, err := s.load(ctx)
	if err != nil {
		return nil
	}
	return c[key]
}

func (s *Service) String(ctx context.Context, key, def string) string {
	var v string
	if raw := s.Raw(ctx, key); raw != nil && json.Unmarshal(raw, &v) == nil && v != "" {
		return v
	}
	return def
}

func (s *Service) Bool(ctx context.Context, key string) bool {
	var v bool
	if raw := s.Raw(ctx, key); raw != nil {
		_ = json.Unmarshal(raw, &v)
	}
	return v
}

// FrontPageID returns the front page entry id or uuid.Nil.
func (s *Service) FrontPageID(ctx context.Context) uuid.UUID {
	id, err := uuid.Parse(s.String(ctx, FrontPage, ""))
	if err != nil {
		return uuid.Nil
	}
	return id
}

// SiteName is the settings override or the bundle name.
func (s *Service) SiteName(ctx context.Context) string {
	return s.String(ctx, SiteTitle, s.Site().Config.Name)
}

// Set writes a setting. Rendering-affecting keys trigger a full re-render.
func (s *Service) Set(ctx context.Context, key string, value any) error {
	if _, err := actor.Require(ctx, rbac.SettingsManage); err != nil {
		return err
	}
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SetSetting(ctx, dbq.SetSettingParams{Key: key, Value: b}); err != nil {
			return err
		}
		if !strings.HasPrefix(key, "system.") {
			if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderRebuild{Reason: "setting " + key}); err != nil {
				return err
			}
		}
		if err := events.Notify(ctx, tx, events.Event{Topic: events.SettingsChanged}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "settings.set", ResourceType: "setting", ResourceID: key})
	})
	if err == nil {
		s.Invalidate()
	}
	return err
}

// SetSystem writes a system.* setting without auth checks or re-rendering.
func (s *Service) SetSystem(ctx context.Context, key string, value any) error {
	b, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := s.DB.Q.SetSetting(ctx, dbq.SetSettingParams{Key: key, Value: b}); err != nil {
		return err
	}
	s.Invalidate()
	return nil
}

// All returns every setting (admin settings screen).
func (s *Service) All(ctx context.Context) (map[string]json.RawMessage, error) {
	if _, err := actor.Require(ctx, rbac.SettingsManage); err != nil {
		return nil, err
	}
	return s.load(ctx)
}

// ---------------------------------------------------------------------------
// Options pages (spec 4.2): singleton field sets available in every template.
// ---------------------------------------------------------------------------

func (s *Service) loadOptions(ctx context.Context) (map[string]json.RawMessage, error) {
	s.mu.RLock()
	o := s.options
	s.mu.RUnlock()
	if o != nil {
		return o, nil
	}
	rows, err := s.DB.Q.ListOptions(ctx)
	if err != nil {
		return nil, err
	}
	o = map[string]json.RawMessage{}
	for _, r := range rows {
		o[r.Name] = r.Fields
	}
	s.mu.Lock()
	s.options = o
	s.mu.Unlock()
	return o, nil
}

// Options returns an options page's values (empty object if never saved).
func (s *Service) Options(ctx context.Context, name string) json.RawMessage {
	o, err := s.loadOptions(ctx)
	if err != nil || o[name] == nil {
		return json.RawMessage("{}")
	}
	return o[name]
}

// SaveOptions validates and stores an options page.
func (s *Service) SaveOptions(ctx context.Context, name string, values json.RawMessage) error {
	a, err := actor.Require(ctx, rbac.SettingsManage)
	if err != nil {
		return err
	}
	page := s.Site().Options[name]
	if page == nil {
		return fields.Errors{"": "unknown options page"}
	}
	clean, _, err := fields.Validate(page.Fields(), values, fields.Context{CanUse: func(c string) bool { return a.Can(rbac.Cap(c)) }})
	if err != nil {
		return err
	}
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SaveOptions(ctx, dbq.SaveOptionsParams{Name: name, Fields: clean}); err != nil {
			return err
		}
		if err := s.Jobs.EnqueueTx(ctx, tx, jobs.RenderInvalidate{Deps: []jobs.Dep{{Type: "options", ID: name}}}); err != nil {
			return err
		}
		if err := events.Notify(ctx, tx, events.Event{Topic: events.OptionsChanged, ID: name}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "options.save", ResourceType: "options", ResourceID: name})
	})
	if err == nil {
		s.Invalidate()
	}
	return err
}
