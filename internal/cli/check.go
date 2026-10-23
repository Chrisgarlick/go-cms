package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"cms/internal/app"
	"cms/internal/config"
	"cms/internal/db"
	"cms/internal/devdb"
	"cms/internal/fixtures"
	"cms/internal/keys"
	"cms/internal/render"
)

// checkCmd validates a site repository end to end without a server: schema,
// templates, fixtures, and a full render of every page. Site CI runs it.
func checkCmd() *cobra.Command {
	var siteDir string
	cmd := &cobra.Command{
		Use:   "check [site-dir]",
		Short: "Validate a site repo: schema, templates, fixtures and a full render (CI)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				siteDir = args[0]
			}
			return runCheck(cmd.Context(), siteDir)
		},
	}
	cmd.Flags().StringVar(&siteDir, "site", ".", "site repository directory")
	return cmd
}

type checkResult struct {
	Pages    int      `json:"pages"`
	Failures []string `json:"failures"`
	Took     string   `json:"took"`
}

func runCheck(ctx context.Context, siteDir string) error {
	start := time.Now()
	siteDir, _ = filepath.Abs(siteDir)
	tmp, err := os.MkdirTemp("", "cms-check-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	info("Starting a temporary database…")
	cluster, err := devdb.Start(filepath.Join(tmp, "pg"), 0, nil)
	if err != nil {
		return err
	}
	defer cluster.Stop()
	if err := cluster.EnsureDatabase(ctx, "cms"); err != nil {
		return err
	}
	if _, err := db.Migrate(ctx, cluster.MigratorURL("cms"), devdb.AppRole); err != nil {
		return err
	}
	kek := filepath.Join(tmp, "kek")
	if err := os.WriteFile(kek, []byte(keys.Generate(1)), 0o600); err != nil {
		return err
	}
	cfg := config.Defaults()
	cfg.Env = config.EnvTest
	cfg.BaseURL = "https://example.test"
	cfg.Database.URL = cluster.AppURL("cms")
	cfg.Database.MigratorURL = cluster.MigratorURL("cms")
	cfg.Paths.Data = filepath.Join(tmp, "data")
	cfg.Paths.Site = siteDir
	cfg.Keys.KEKFile = kek
	cfg.Security.VulnLookup = false
	if err := cfg.Validate(); err != nil {
		return err
	}
	a, err := app.New(ctx, &cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), app.Options{})
	if err != nil {
		return err
	}
	defer a.Close()
	sys := app.SystemCtx(ctx, "check")
	if _, err := a.Site().Apply(ctx, a.DB.Q); err != nil {
		return err
	}
	if errs := a.Renderer.Theme().Validate(); len(errs) > 0 {
		return errors.Join(errs...)
	}
	n, err := fixtures.Load(sys, a.Site().FS, fixtures.Services{Content: a.Content, Menus: a.Menus, Settings: a.Settings, Q: a.DB.Q, Redirects: a.Redirects})
	if err != nil {
		return fmt.Errorf("fixtures: %w", err)
	}
	info("Loaded %d fixture entries; rendering every page…", n)
	if _, err := a.Renderer.LoadTheme(a.Site()); err != nil {
		return err
	}
	paths, err := a.Renderer.AllPaths(ctx)
	if err != nil {
		return err
	}
	res := checkResult{}
	disk := &render.Disk{Root: filepath.Join(tmp, "out")}
	for _, p := range append(paths, "/__check_404__/", "/search/") {
		var r *render.Result
		var err error
		if p == "/search/" {
			r, err = a.Renderer.Search(ctx, "the", 1)
		} else {
			r, err = a.Renderer.Render(ctx, p)
		}
		switch {
		case err != nil:
			res.Failures = append(res.Failures, fmt.Sprintf("%s: %v", p, err))
		case p == "/__check_404__/" && r.Status != 404:
			res.Failures = append(res.Failures, "404 page returned "+fmt.Sprint(r.Status))
		case p != "/__check_404__/" && r.Status != 200:
			res.Failures = append(res.Failures, fmt.Sprintf("%s: status %d", p, r.Status))
		default:
			res.Pages++
			_ = disk.Write(p, r.Body)
		}
	}
	res.Took = time.Since(start).Round(time.Millisecond).String()
	if err := out(res, func(w io.Writer) {
		fmt.Fprintf(w, "Rendered %d pages in %s.\n", res.Pages, res.Took)
		for _, f := range res.Failures {
			fmt.Fprintln(w, "FAIL", f)
		}
	}); err != nil {
		return err
	}
	if len(res.Failures) > 0 {
		return validationErr(fmt.Sprintf("%d page(s) failed to render", len(res.Failures)))
	}
	return nil
}
