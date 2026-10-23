package testutil

import (
	"context"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"cms/internal/app"
	"cms/internal/config"
	"cms/internal/keys"
	"cms/internal/mail"
	"cms/starters"
)

// NewApp builds a full App on a fresh database with a starter site bundle.
// Jobs are enqueued but no workers run unless the test starts them.
func NewApp(t testing.TB, starter string) (*app.App, *mail.Log) {
	t.Helper()
	d := NewDB(t)
	dir := t.TempDir()
	siteDir := filepath.Join(dir, "site")
	fsys, ok := starters.Get(starter)
	if !ok {
		t.Fatalf("no starter %s", starter)
	}
	err := fs.WalkDir(fsys, ".", func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(siteDir, p)
		if de.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		s := strings.NewReplacer("__SITE_NAME__", "Test Site", "__SITE_SLUG__", "test-site", "__CMS_VERSION__", "0.0.0").Replace(string(b))
		return os.WriteFile(target, []byte(s), 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	kek := filepath.Join(dir, "kek")
	if err := os.WriteFile(kek, []byte(keys.Generate(1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.Env = config.EnvDevelopment
	cfg.BaseURL = "http://localhost:8080"
	cfg.Database.URL = d.AppURL
	cfg.Database.MigratorURL = d.MigratorURL
	cfg.Paths.Data = filepath.Join(dir, "data")
	cfg.Paths.Site = siteDir
	cfg.Keys.KEKFile = kek
	cfg.HTTP.ServeStatic = true
	cfg.Security.VulnLookup = false
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	ml := &mail.Log{}
	a, err := app.New(context.Background(), &cfg, slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn})), app.Options{Mail: ml})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := a.Site().Apply(ctx, a.DB.Q); err != nil {
		t.Fatal(err)
	}
	if err := a.Prepare(ctx); err != nil {
		t.Fatal(err)
	}
	return a, ml
}
