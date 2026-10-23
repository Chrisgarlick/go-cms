// Package cli implements the cms command line (spec 5). Every command accepts
// --json; logs go to stderr so stdout stays machine-readable.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"cms/internal/actor"
	"cms/internal/app"
	"cms/internal/auth"
	"cms/internal/config"
	"cms/internal/content"
	"cms/internal/fields"
)

// Exit codes (spec 5.2).
const (
	ExitOK         = 0
	ExitError      = 1
	ExitUsage      = 2
	ExitValidation = 3
	ExitPermission = 4
	ExitNotFound   = 5
	ExitConflict   = 6
)

type globals struct {
	config  string
	json    bool
	quiet   bool
	verbose bool
	yes     bool
}

var g globals

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           "cms",
		Short:         "A fast, secure, CLI-first content management system",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	pf := root.PersistentFlags()
	pf.StringVar(&g.config, "config", os.Getenv("CMS_CONFIG"), "config file (default: $CMS_CONFIG or /etc/cms/config.yaml)")
	pf.BoolVar(&g.json, "json", false, "machine-readable JSON output")
	pf.BoolVarP(&g.quiet, "quiet", "q", false, "only print errors")
	pf.BoolVarP(&g.verbose, "verbose", "v", false, "debug logging")
	root.AddCommand(
		versionCmd(), serveCmd(), workerCmd(), migrateCmd(), devCmd(), newCmd(), makeCmd(),
		userCmd(), schemaCmd(), contentCmd(), themeCmd(), cacheCmd(), buildCmd(), searchCmd(),
		doctorCmd(), securityCmd(), redirectsCmd(), mediaCmd(), keysCmd(), backupCmd(), installCmd(),
		upgradeCmd(), auditCmd(), settingsCmd(), tokenCmd(), mediaProcessCmd(), configCmd(), checkCmd(), caddyCmd(),
	)
	return root
}

// Execute runs the CLI and returns the process exit code.
func Execute() int {
	root := newRoot()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := root.ExecuteContext(ctx)
	if err == nil {
		return ExitOK
	}
	code := exitCode(err)
	if g.json {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"error": err.Error(), "code": code})
	} else {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	return code
}

func exitCode(err error) int {
	var fe fields.Errors
	var forbidden *actor.ErrForbidden
	var conflict *content.ErrConflict
	switch {
	case errors.As(err, &fe):
		return ExitValidation
	case errors.As(err, &forbidden), errors.Is(err, actor.ErrSudoRequired), errors.Is(err, actor.ErrUnauthenticated):
		return ExitPermission
	case errors.As(err, &conflict):
		return ExitConflict
	case errors.Is(err, content.ErrNotFound), errors.Is(err, auth.ErrUserNotFound):
		return ExitNotFound
	case strings.Contains(err.Error(), "unknown command"), strings.Contains(err.Error(), "flag"), strings.Contains(err.Error(), "accepts"):
		return ExitUsage
	}
	return ExitError
}

func logger() *slog.Logger {
	lvl := slog.LevelInfo
	if g.verbose {
		lvl = slog.LevelDebug
	}
	if g.quiet {
		lvl = slog.LevelWarn
	}
	var h slog.Handler
	if os.Getenv("CMS_LOG_FORMAT") == "json" || (os.Getenv("JOURNAL_STREAM") != "") {
		h = slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	} else {
		h = slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl})
	}
	return slog.New(h)
}

func loadConfig() (*config.Config, error) {
	path := g.config
	if path == "" {
		if _, err := os.Stat("/etc/cms/config.yaml"); err == nil {
			path = "/etc/cms/config.yaml"
		}
	}
	return config.Load(path)
}

// openApp loads config and builds the app with a system actor context.
func openApp(cmd *cobra.Command, workers bool) (*app.App, context.Context, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, nil, err
	}
	a, err := app.New(cmd.Context(), cfg, logger(), app.Options{Workers: workers})
	if err != nil {
		return nil, nil, err
	}
	return a, app.SystemCtx(cmd.Context(), "cli"), nil
}

// out prints v as JSON (--json) or via the human formatter.
func out(v any, human func(w io.Writer)) error {
	if g.json {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(v)
	}
	if g.quiet || human == nil {
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	human(tw)
	return tw.Flush()
}

func info(format string, args ...any) {
	if !g.quiet && !g.json {
		fmt.Fprintf(os.Stderr, format+"\n", args...)
	}
}

// confirm requires --yes in non-interactive use for destructive commands.
func confirm(prompt string) error {
	if g.yes {
		return nil
	}
	st, _ := os.Stdin.Stat()
	if st.Mode()&os.ModeCharDevice == 0 {
		return errors.New("refusing a destructive action without --yes in non-interactive mode")
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var ans string
	fmt.Scanln(&ans)
	if strings.ToLower(strings.TrimSpace(ans)) != "y" {
		return errors.New("cancelled")
	}
	return nil
}
