package cli

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"cms/internal/app"
	"cms/internal/db"
	"cms/internal/http/admin"
	"cms/internal/http/api"
	"cms/internal/version"
)

func versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		RunE: func(cmd *cobra.Command, args []string) error {
			v := version.Get()
			return out(v, func(w io.Writer) {
				fmt.Fprintf(w, "cms %s\ncommit\t%s\nbuilt\t%s\ngo\t%s\nplatform\t%s\n", v.Version, v.Commit, v.Date, v.GoVersion, v.Platform)
			})
		},
	}
}

// handlers builds the admin and API handlers for an app.
func handlers(a *app.App) (http.Handler, http.Handler) {
	return admin.New(a), api.New(a)
}

func serveCmd() *cobra.Command {
	var addr string
	var withWorker bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the web server",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, withWorker)
			if err != nil {
				return err
			}
			defer a.Close()
			return runServer(ctx, a, addr, withWorker)
		},
	}
	cmd.Flags().StringVar(&addr, "addr", "", "listen address (default: http.addr from config)")
	cmd.Flags().BoolVar(&withWorker, "with-worker", false, "also run background jobs in this process (small installs)")
	return cmd
}

func runServer(ctx context.Context, a *app.App, addr string, withWorker bool) error {
	if addr == "" {
		addr = a.Cfg.HTTP.Addr
	}
	if err := a.Prepare(ctx); err != nil {
		return err
	}
	a.Background(ctx)
	if withWorker {
		if err := a.River.Start(ctx); err != nil {
			return err
		}
		defer stopRiver(a)
	}
	adm, apiH := handlers(a)
	return a.Serve(ctx, a.Handler(adm, apiH), addr)
}

func stopRiver(a *app.App) {
	sctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := a.River.Stop(sctx); err != nil {
		a.Log.Warn("job runner did not stop cleanly", "err", err)
	}
}

func workerCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "worker",
		Short: "Run background jobs (rendering, media, scheduling, webhooks)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, true)
			if err != nil {
				return err
			}
			defer a.Close()
			if err := a.Prepare(ctx); err != nil {
				return err
			}
			a.Background(ctx)
			if err := a.River.Start(ctx); err != nil {
				return err
			}
			a.Log.Info("worker started", "version", version.Version)
			<-ctx.Done()
			stopRiver(a)
			return nil
		},
	}
}

func migrateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "migrate", Short: "Database migrations (forward-only in production)"}
	up := &cobra.Command{
		Use:   "up",
		Short: "Apply pending migrations as the migrator role, then re-apply grants",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if cfg.Database.MigratorURL == "" {
				return fmt.Errorf("database.migrator_url (or DATABASE_MIGRATOR_URL) is required")
			}
			st, err := db.Migrate(cmd.Context(), cfg.Database.MigratorURL, appRole(cfg.Database.URL))
			if err != nil {
				return err
			}
			return printMigrations(st)
		},
	}
	status := &cobra.Command{
		Use:   "status",
		Short: "Show migration status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			st, err := db.StatusURL(cmd.Context(), cfg.Database.URL)
			if err != nil {
				return err
			}
			return printMigrations(st)
		},
	}
	down := &cobra.Command{
		Use:    "down",
		Short:  "Roll back one migration (development only)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if !cfg.Dev() {
				return fmt.Errorf("migrate down is only available in development; production rolls back by binary (spec 9.1)")
			}
			return db.Down(cmd.Context(), cfg.Database.MigratorURL)
		},
	}
	cmd.AddCommand(up, status, down)
	return cmd
}

func printMigrations(st []db.MigrationStatus) error {
	return out(st, func(w io.Writer) {
		fmt.Fprintln(w, "VERSION\tNAME\tTAG\tAPPLIED")
		for _, s := range st {
			fmt.Fprintf(w, "%d\t%s\t%s\t%v\n", s.Version, s.Name, s.Tag, s.Applied)
		}
	})
}

// appRole extracts the user name from the runtime DATABASE_URL.
func appRole(url string) string {
	rest, ok := strings.CutPrefix(url, "postgres://")
	if !ok {
		rest, _ = strings.CutPrefix(url, "postgresql://")
	}
	userinfo, _, ok := strings.Cut(rest, "@")
	if !ok {
		return ""
	}
	user, _, _ := strings.Cut(userinfo, ":")
	return user
}
