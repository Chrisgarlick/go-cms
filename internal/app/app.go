// Package app wires configuration, database pools, services, jobs and HTTP
// handlers together (spec 1.3: explicit constructor injection, no container).
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/riverqueue/river"
	"github.com/riverqueue/river/riverdriver/riverpgxv5"

	"cms/internal/actor"
	"cms/internal/auth"
	"cms/internal/blocks"
	"cms/internal/config"
	"cms/internal/content"
	"cms/internal/db"
	"cms/internal/events"
	"cms/internal/forms"
	"cms/internal/jobs"
	"cms/internal/keys"
	"cms/internal/mail"
	"cms/internal/media"
	"cms/internal/menus"
	"cms/internal/netx"
	"cms/internal/redirects"
	"cms/internal/render"
	"cms/internal/secaudit"
	"cms/internal/settings"
	"cms/internal/site"
	"cms/internal/version"
	"cms/internal/webhooks"
	"cms/themes"
)

type App struct {
	Cfg   *config.Config
	Log   *slog.Logger
	DB    *db.DB
	JobDB *db.DB
	Keys  *keys.Keyring
	Bus   *events.Bus
	River *river.Client[pgx.Tx]
	Jobs  jobs.Enqueuer
	Mail  mail.Sender

	Auth      *auth.Service
	Content   *content.Service
	Media     *media.Service
	Menus     *menus.Service
	Settings  *settings.Service
	Redirects *redirects.Service
	Renderer  *render.Renderer
	Webhooks  *webhooks.Service
	Forms     *forms.Service
	Security  *secaudit.Auditor

	bundle  atomic.Pointer[site.Bundle]
	workers bool
	started time.Time
}

type Options struct {
	// Workers runs River job workers in this process (cms worker, serve --with-worker).
	Workers bool
	// Mail overrides the mail sender (tests, dev).
	Mail mail.Sender
}

// New builds the application. Callers must Close it.
func New(ctx context.Context, cfg *config.Config, log *slog.Logger, opts Options) (*App, error) {
	a := &App{Cfg: cfg, Log: log, workers: opts.Workers, started: time.Now()}
	if err := a.LoadBundle(); err != nil {
		return nil, err
	}
	var err error
	a.DB, err = db.Open(ctx, cfg.Database.URL, db.PoolOptions{MaxConns: cfg.Database.MaxConns, StatementTimeout: 5 * time.Second, AppName: "cms-web"})
	if err != nil {
		return nil, err
	}
	if opts.Workers {
		a.JobDB, err = db.Open(ctx, cfg.Database.URL, db.PoolOptions{MaxConns: cfg.Database.JobMaxConns + 2, StatementTimeout: 60 * time.Second, AppName: "cms-worker"})
		if err != nil {
			a.DB.Close()
			return nil, err
		}
	}
	if cfg.Keys.KEKFile == "" {
		a.Close()
		return nil, errors.New("keys.kek_file (or CMS_KEK_FILE) is required: run `cms install` or `cms keys generate`")
	}
	a.Keys, err = keys.Load(cfg.Keys.KEKFile)
	if err != nil {
		a.Close()
		return nil, err
	}
	a.Bus = events.NewBus(log)
	a.Mail = opts.Mail
	if a.Mail == nil {
		if cfg.Mail.SMTPAddr != "" {
			a.Mail = &mail.SMTP{Addr: cfg.Mail.SMTPAddr, From: cfg.Mail.From, Username: cfg.Mail.Username, Password: cfg.Mail.Password}
		} else {
			a.Mail = &mail.Log{Logger: log}
		}
	}
	if err := a.setupRiver(); err != nil {
		a.Close()
		return nil, err
	}
	a.buildServices()
	return a, nil
}

// LoadBundle (re)loads the site bundle from disk; on error the old one stays.
func (a *App) LoadBundle() error {
	b, err := site.LoadDir(a.Cfg.Paths.Site)
	if err != nil {
		return fmt.Errorf("site bundle: %w", err)
	}
	b.AddBuiltinBlocks(blocks.Builtin())
	if err := b.Finalize(); err != nil {
		return fmt.Errorf("site bundle: %w", err)
	}
	a.bundle.Store(b)
	return nil
}

// Site returns the current bundle.
func (a *App) Site() *site.Bundle { return a.bundle.Load() }

func (a *App) setupRiver() error {
	pool := a.DB.Pool
	cfg := &river.Config{Logger: a.Log.WithGroup("river")}
	if a.workers {
		pool = a.JobDB.Pool
		w := river.NewWorkers()
		a.registerWorkers(w)
		cfg.Workers = w
		cfg.Queues = map[string]river.QueueConfig{
			river.QueueDefault: {MaxWorkers: a.Cfg.Jobs.Concurrency},
			jobs.QueueRender:   {MaxWorkers: 2},
			jobs.QueueMedia:    {MaxWorkers: 1},
		}
		cfg.PeriodicJobs = []*river.PeriodicJob{
			river.NewPeriodicJob(river.PeriodicInterval(time.Minute), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.SchedulerSweep{}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(time.Hour), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.Maintenance{}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
			river.NewPeriodicJob(river.PeriodicInterval(24*time.Hour), func() (river.JobArgs, *river.InsertOpts) {
				return jobs.SecurityAudit{}, nil
			}, &river.PeriodicJobOpts{RunOnStart: true}),
		}
		cfg.JobTimeout = 10 * time.Minute
	}
	client, err := river.NewClient(riverpgxv5.New(pool), cfg)
	if err != nil {
		return err
	}
	a.River = client
	a.Jobs = jobs.RiverEnqueuer{Client: client}
	return nil
}

func (a *App) buildServices() {
	cfg := a.Cfg
	bundle := a.Site
	u, _ := url.Parse(cfg.BaseURL)
	a.Settings = &settings.Service{DB: a.DB, Site: bundle, Jobs: a.Jobs}
	a.Content = &content.Service{DB: a.DB, Site: bundle, Jobs: a.Jobs, FrontPage: a.Settings.FrontPageID}
	var hibp *http.Client
	if cfg.Security.VulnLookup {
		hibp = netx.Client(10 * time.Second)
	}
	a.Auth = auth.New(&auth.Service{DB: a.DB, Keys: a.Keys, Mail: a.Mail, Log: a.Log, BaseURL: cfg.BaseURL,
		Issuer: bundle().Config.Name, HIBP: hibp, SkipEnroll: cfg.Security.DevSkip2FA && cfg.Dev()})
	a.Media = &media.Service{DB: a.DB, Store: &media.Local{Root: cfg.MediaDir()}, Site: bundle, Jobs: a.Jobs, Log: a.Log,
		TmpDir: cfg.TmpDir(), BaseURL: cfg.MediaBase(), MaxBytes: int64(cfg.Media.MaxUploadMB) << 20, MaxPixels: cfg.Media.MaxPixels,
		Processor: media.DetectProcessor(cfg.Media.VipsBinary)}
	a.Menus = &menus.Service{DB: a.DB, Site: bundle, Jobs: a.Jobs}
	a.Redirects = &redirects.Service{DB: a.DB, Jobs: a.Jobs, SiteHost: u.Hostname()}
	a.Renderer = &render.Renderer{DB: a.DB, Site: bundle, Content: a.Content, Media: a.Media, Menus: a.Menus, Settings: a.Settings,
		BaseFS: themes.Base(), BaseURL: cfg.BaseURL, Disk: &render.Disk{Root: filepath.Join(cfg.PublicDir(), u.Hostname())},
		Log: a.Log, Version: version.Version}

	a.Webhooks = &webhooks.Service{DB: a.DB, Keys: a.Keys, Jobs: a.Jobs}
	a.Forms = forms.New(a.DB, a.Keys, a.Jobs, a.Site, a.Log)
	var vuln *http.Client
	if cfg.Security.VulnLookup {
		vuln = netx.Client(30 * time.Second)
	}
	a.Security = &secaudit.Auditor{DB: a.DB, Cfg: cfg, Client: vuln}

	a.Bus.Subscribe(events.RedirectsChanged, func(events.Event) { _ = a.Redirects.Reload(context.Background()) })
	a.Bus.Subscribe(events.SettingsChanged, func(events.Event) { a.Settings.Invalidate() })
	a.Bus.Subscribe(events.OptionsChanged, func(events.Event) { a.Settings.Invalidate() })
}

// Close releases pools and clients.
func (a *App) Close() {
	if a.JobDB != nil {
		a.JobDB.Close()
	}
	if a.DB != nil {
		a.DB.Close()
	}
}

// Prepare runs start-up work shared by serve and worker: directories,
// redirect tables, and a rebuild when the site bundle or theme changed.
func (a *App) Prepare(ctx context.Context) error {
	for _, d := range []string{a.Cfg.PublicDir(), a.Cfg.MediaDir(), a.Cfg.TmpDir(), a.Renderer.Disk.Root} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("data directory: %w", err)
		}
	}
	if _, err := a.Renderer.LoadTheme(a.Site()); err != nil {
		return err
	}
	if err := a.Redirects.Reload(ctx); err != nil {
		return err
	}
	want := a.Site().Hash + ":" + a.Renderer.ThemeHash() + ":" + version.Version
	if a.Settings.String(ctx, settings.BundleHash, "") != want {
		if err := a.Jobs.Enqueue(ctx, jobs.RenderRebuild{Reason: "site bundle or theme changed"}); err != nil {
			return err
		}
		if err := a.Settings.SetSystem(ctx, settings.BundleHash, want); err != nil {
			return err
		}
	}
	return nil
}

// Background runs loops every process needs: event listener, counter flushes.
func (a *App) Background(ctx context.Context) {
	go a.Bus.Listen(ctx, a.DB.Pool)
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = a.Redirects.Flush(fctx)
				_ = a.Auth.FlushTokenUse(fctx)
				cancel()
				return
			case <-t.C:
				if err := a.Redirects.Flush(ctx); err != nil {
					a.Log.Warn("flush redirect counters", "err", err)
				}
				if err := a.Auth.FlushTokenUse(ctx); err != nil {
					a.Log.Warn("flush token use", "err", err)
				}
			}
		}
	}()
}

// SystemCtx returns a context with a system actor (CLI, jobs).
func SystemCtx(ctx context.Context, component string) context.Context {
	return actor.With(ctx, actor.System(component))
}

// Uptime is how long this process has run.
func (a *App) Uptime() time.Duration { return time.Since(a.started) }

// FindUserByEmail is a CLI helper.
func (a *App) FindUserByEmail(ctx context.Context, email string) (uuid.UUID, error) {
	u, err := a.DB.Q.GetUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
	if err != nil {
		return uuid.Nil, auth.ErrUserNotFound
	}
	return u.ID, nil
}
