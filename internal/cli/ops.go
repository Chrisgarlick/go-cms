package cli

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"go.yaml.in/yaml/v3"

	"cms/internal/app"
	"cms/internal/audit"
	"cms/internal/auth"
	"cms/internal/blocks"
	"cms/internal/config"
	"cms/internal/db"
	"cms/internal/devdb"
	"cms/internal/keys"
	"cms/internal/media"
	"cms/internal/netx"
	"cms/internal/rbac"
	"cms/internal/site"
	"cms/internal/version"
)

type check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail
	Detail string `json:"detail,omitempty"`
}

func doctorCmd() *cobra.Command {
	var fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check configuration, database, files and site health (post-deploy smoke test)",
		RunE: func(cmd *cobra.Command, args []string) error {
			checks := runDoctor(cmd.Context(), fix)
			failed := 0
			for _, c := range checks {
				if c.Status == "fail" {
					failed++
				}
			}
			if err := out(map[string]any{"ok": failed == 0, "checks": checks}, func(w io.Writer) {
				for _, c := range checks {
					mark := map[string]string{"ok": "✓", "warn": "!", "fail": "✗"}[c.Status]
					fmt.Fprintf(w, "%s\t%s\t%s\n", mark, c.Name, c.Detail)
				}
			}); err != nil {
				return err
			}
			if failed > 0 {
				return fmt.Errorf("%d check(s) failed", failed)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&fix, "fix", false, "apply safe, idempotent repairs (directories, key file permissions)")
	return cmd
}

func runDoctor(ctx context.Context, fix bool) []check {
	var cs []check
	add := func(name, status, detail string) { cs = append(cs, check{name, status, detail}) }
	cfg, err := loadConfig()
	if err != nil {
		add("config", "fail", err.Error())
		return cs
	}
	add("config", "ok", fmt.Sprintf("env %s, %s", cfg.Env, cfg.BaseURL))
	if cfg.Env == config.EnvProduction && !strings.HasPrefix(cfg.BaseURL, "https://") {
		add("https", "fail", "base_url must be https in production")
	}
	// Keys.
	if st, err := os.Stat(cfg.Keys.KEKFile); err != nil {
		add("key file", "fail", "missing: "+cfg.Keys.KEKFile)
	} else {
		if _, err := keys.Load(cfg.Keys.KEKFile); err != nil {
			add("key file", "fail", err.Error())
		} else if st.Mode().Perm()&0o077 != 0 {
			if fix {
				_ = os.Chmod(cfg.Keys.KEKFile, 0o400)
				add("key file", "ok", "permissions fixed to 0400")
			} else {
				add("key file", "warn", fmt.Sprintf("mode %o: run with --fix or chmod 0400", st.Mode().Perm()))
			}
		} else {
			add("key file", "ok", "")
		}
	}
	// Directories.
	for _, d := range []string{cfg.PublicDir(), cfg.MediaDir(), cfg.TmpDir()} {
		if fix {
			_ = os.MkdirAll(d, 0o755)
		}
		f, err := os.CreateTemp(d, ".doctor-*")
		if err != nil {
			add("writable "+d, "fail", err.Error())
			continue
		}
		f.Close()
		os.Remove(f.Name())
		add("writable "+d, "ok", "")
	}
	// Site bundle.
	b, err := site.LoadDir(cfg.Paths.Site)
	if err == nil {
		b.AddBuiltinBlocks(blocks.Builtin())
		err = b.Finalize()
	}
	if err != nil {
		add("site bundle", "fail", err.Error())
	} else {
		add("site bundle", "ok", fmt.Sprintf("%s (%d types, hash %s)", b.Config.Name, len(b.Types), b.Hash))
	}
	// Database.
	d, err := db.Open(ctx, cfg.Database.URL, db.PoolOptions{MaxConns: 2, StatementTimeout: 10 * time.Second})
	if err != nil {
		add("database", "fail", err.Error())
		return cs
	}
	defer d.Close()
	var ver string
	_ = d.Pool.QueryRow(ctx, "SHOW server_version").Scan(&ver)
	add("database", "ok", "PostgreSQL "+ver)
	if !strings.HasPrefix(ver, "16") && !strings.HasPrefix(ver, "17") && !strings.HasPrefix(ver, "18") && !strings.HasPrefix(ver, "19") {
		add("postgres version", "warn", "PostgreSQL 16 or newer is supported")
	}
	for _, ext := range []string{"pg_trgm", "unaccent"} {
		var ok bool
		_ = d.Pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = $1)", ext).Scan(&ok)
		if !ok {
			add("extension "+ext, "fail", "not installed: run cms migrate up")
		}
	}
	st, err := db.StatusURL(ctx, cfg.Database.URL)
	if err != nil {
		add("migrations", "fail", err.Error())
	} else {
		pending := 0
		for _, s := range st {
			if !s.Applied {
				pending++
			}
		}
		if pending > 0 {
			add("migrations", "fail", fmt.Sprintf("%d pending: run cms migrate up", pending))
		} else {
			add("migrations", "ok", fmt.Sprintf("%d applied", len(st)))
		}
	}
	if b != nil && err == nil {
		if ch, err := b.Diff(ctx, d.Q); err == nil {
			if len(ch) > 0 {
				add("schema", "warn", fmt.Sprintf("%d difference(s) from the bundle: run cms schema apply", len(ch)))
			} else {
				add("schema", "ok", "matches the bundle")
			}
		}
	}
	var super, createPublic bool
	_ = d.Pool.QueryRow(ctx, "SELECT rolsuper, has_schema_privilege(current_user, 'public', 'CREATE') FROM pg_roles WHERE rolname = current_user").Scan(&super, &createPublic)
	if super || createPublic {
		add("least privilege", "fail", "the application role can change the schema; use cms_app")
	} else {
		add("least privilege", "ok", "")
	}
	if res, err := audit.Verify(ctx, d.Q); err == nil {
		if res.OK {
			add("audit chain", "ok", fmt.Sprintf("%d rows", res.Rows))
		} else {
			add("audit chain", "fail", fmt.Sprintf("broken at row %d", res.BrokenAt))
		}
	}
	if n, err := d.Q.CountOwners(ctx); err == nil && n == 0 {
		add("owner", "warn", "no active owner: cms user create --role owner")
	}
	if n, err := d.Q.MediaMissingAlt(ctx); err == nil && n > 0 {
		add("alt text", "warn", fmt.Sprintf("%d images have no alt text", n))
	}
	p := media.DetectProcessor(cfg.Media.VipsBinary)
	if p.Name() == "vips" {
		add("image processing", "ok", "libvips")
	} else {
		add("image processing", "warn", "libvips not found: using the built-in processor (JPEG/PNG only); apt install libvips-tools")
	}
	if pd, err := exec.LookPath("pg_dump"); err == nil {
		outb, _ := exec.Command(pd, "--version").Output()
		add("pg_dump", "ok", strings.TrimSpace(string(outb)))
	} else {
		add("pg_dump", "warn", "not installed: cms backup needs postgresql-client")
	}
	if bk := latestBackup(cfg); bk != "" {
		if fi, err := os.Stat(bk); err == nil {
			age := time.Since(fi.ModTime())
			status := "ok"
			if age > 36*time.Hour {
				status = "warn"
			}
			add("backups", status, fmt.Sprintf("latest %s (%s ago)", filepath.Base(bk), age.Round(time.Minute)))
		}
	} else if cfg.Env == config.EnvProduction {
		add("backups", "warn", "no backups found in "+backupDir(cfg))
	}
	return cs
}

func backupDir(cfg *config.Config) string { return filepath.Join(cfg.Paths.Data, "backups") }

func latestBackup(cfg *config.Config) string {
	files, _ := filepath.Glob(filepath.Join(backupDir(cfg), "cms-*.dump*"))
	sort.Strings(files)
	if len(files) == 0 {
		return ""
	}
	return files[len(files)-1]
}

func backupCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "backup", Short: "Database and media backups (pg_dump wrapper)"}
	var withMedia bool
	var ageRecipient string
	var keep int
	create := &cobra.Command{
		Use: "create", Short: "Write a database dump (and optionally a media archive) to <data>/backups",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			dir := backupDir(cfg)
			if err := os.MkdirAll(dir, 0o700); err != nil {
				return err
			}
			stamp := time.Now().UTC().Format("20060102-150405")
			dbURL := cfg.Database.MigratorURL
			if dbURL == "" {
				dbURL = cfg.Database.URL
			}
			dump := filepath.Join(dir, "cms-"+stamp+".dump")
			if err := pipeTo(cmd.Context(), dump, ageRecipient, "pg_dump", "--format=custom", "--no-owner", "--no-privileges", "--dbname", dbURL); err != nil {
				return fmt.Errorf("pg_dump: %w", err)
			}
			files := []string{dump}
			if withMedia {
				mf := filepath.Join(dir, "media-"+stamp+".tar.gz")
				if err := tarDir(cfg.MediaDir(), mf); err != nil {
					return err
				}
				files = append(files, mf)
			}
			pruneBackups(dir, keep)
			return out(map[string]any{"files": files}, func(w io.Writer) {
				for _, f := range files {
					fmt.Fprintln(w, "wrote", f)
				}
			})
		},
	}
	create.Flags().BoolVar(&withMedia, "media", false, "also archive the media directory")
	create.Flags().StringVar(&ageRecipient, "age-recipient", os.Getenv("CMS_BACKUP_AGE_RECIPIENT"), "encrypt with age for this recipient (age1...)")
	create.Flags().IntVar(&keep, "keep", 14, "number of backups to keep")
	restore := &cobra.Command{
		Use: "restore <file.dump>", Short: "Restore a database dump (replaces current data)", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if err := confirm("This replaces the database contents. Continue?"); err != nil {
				return err
			}
			if cfg.Database.MigratorURL == "" {
				return errors.New("database.migrator_url is required to restore")
			}
			c := exec.CommandContext(cmd.Context(), "pg_restore", "--clean", "--if-exists", "--no-owner", "--no-privileges",
				"--single-transaction", "--dbname", cfg.Database.MigratorURL, args[0])
			c.Stdout, c.Stderr = os.Stderr, os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("pg_restore: %w", err)
			}
			if _, err := db.Migrate(cmd.Context(), cfg.Database.MigratorURL, appRole(cfg.Database.URL)); err != nil {
				return err
			}
			info("Restored. Run `cms cache rebuild` to re-render the site.")
			return nil
		},
	}
	restore.Flags().BoolVarP(&g.yes, "yes", "y", false, "do not ask for confirmation")
	verify := &cobra.Command{
		Use: "verify <file.dump>", Short: "Check a dump can be read", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			outb, err := exec.CommandContext(cmd.Context(), "pg_restore", "--list", args[0]).Output()
			if err != nil {
				return fmt.Errorf("unreadable backup: %w", err)
			}
			n := strings.Count(string(outb), "TABLE DATA")
			return out(map[string]any{"ok": true, "tables": n}, func(w io.Writer) { fmt.Fprintf(w, "OK: %d tables with data.\n", n) })
		},
	}
	cmd.AddCommand(create, restore, verify)
	return cmd
}

func pipeTo(ctx context.Context, path, ageRecipient string, name string, args ...string) error {
	if ageRecipient != "" {
		path += ".age"
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	c := exec.CommandContext(ctx, name, args...)
	c.Stderr = os.Stderr
	if ageRecipient == "" {
		c.Stdout = f
		return c.Run()
	}
	enc := exec.CommandContext(ctx, "age", "-r", ageRecipient)
	enc.Stdout, enc.Stderr = f, os.Stderr
	pipe, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	enc.Stdin = pipe
	if err := enc.Start(); err != nil {
		return fmt.Errorf("age: %w", err)
	}
	if err := c.Run(); err != nil {
		return err
	}
	return enc.Wait()
}

func tarDir(src, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	err = filepath.Walk(src, func(p string, fi os.FileInfo, err error) error {
		if err != nil || !fi.Mode().IsRegular() {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		hdr, _ := tar.FileInfoHeader(fi, "")
		hdr.Name = filepath.ToSlash(rel)
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(tw, in)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func pruneBackups(dir string, keep int) {
	if keep <= 0 {
		return
	}
	for _, pat := range []string{"cms-*.dump*", "media-*.tar.gz"} {
		files, _ := filepath.Glob(filepath.Join(dir, pat))
		sort.Strings(files)
		for i := 0; i < len(files)-keep; i++ {
			os.Remove(files[i])
		}
	}
}

func installCmd() *cobra.Command {
	var siteDir, domain, superURL, dataDir, configOut, kekOut, ownerEmail, dbName, dbHost string
	var feed, siteFeed, tokenFile, pubKey, channel, sitePrefixFlag, siteKey string
	cmd := &cobra.Command{
		Use:   "install",
		Short: "Set up a server for a site: keys, database roles, config, migrations, first owner",
		Long: `Set up a fresh server. Needs a Postgres superuser URL once (never stored).
Creates the cms_migrator and cms_app roles with random passwords, writes
/etc/cms/config.yaml and the key file, migrates, applies the site's schema and
invites the first owner.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if domain == "" || superURL == "" || ownerEmail == "" {
				return errors.New("--domain, --db-super-url and --owner-email are required")
			}
			if _, err := os.Stat(filepath.Join(siteDir, "site.yaml")); err != nil {
				return fmt.Errorf("no site bundle at %s", siteDir)
			}
			migPass, appPass := auth.RandomToken(24), auth.RandomToken(24)
			if err := devdb.EnsureRolesAndDatabase(ctx, superURL, dbName, "cms_migrator", "cms_app", migPass, appPass); err != nil {
				return fmt.Errorf("database setup: %w", err)
			}
			su, err := url.Parse(superURL)
			if err != nil {
				return err
			}
			host := dbHost
			if host == "" {
				host = su.Host
			}
			mk := func(user, pass string) string {
				u := url.URL{Scheme: "postgres", User: url.UserPassword(user, pass), Host: host, Path: "/" + dbName}
				return u.String()
			}
			if _, err := os.Stat(kekOut); err != nil {
				if err := os.MkdirAll(filepath.Dir(kekOut), 0o750); err != nil {
					return err
				}
				if err := os.WriteFile(kekOut, []byte(keys.Generate(1)), 0o400); err != nil {
					return err
				}
				info("Wrote %s. Back this key up somewhere safe: without it encrypted data cannot be recovered.", kekOut)
			}
			cfgFile := map[string]any{
				"env": "production", "base_url": "https://" + domain, "hosts": []string{domain},
				"http":     map[string]any{"addr": "127.0.0.1:8081", "trusted_proxies": []string{"127.0.0.1/32", "::1/128"}},
				"database": map[string]any{"url": mk("cms_app", appPass), "migrator_url": mk("cms_migrator", migPass)},
				"paths":    map[string]any{"data": dataDir, "site": siteDir},
				"keys":     map[string]any{"kek_file": kekOut},
				"upgrade": map[string]any{"channel": channel, "feed_url": feed, "site_feed_url": siteFeed, "site_tag_prefix": sitePrefixFlag,
					"token_file": tokenFile, "public_key": pubKey, "site_public_key": siteKey},
			}
			yb, _ := yaml.Marshal(cfgFile)
			if err := os.MkdirAll(filepath.Dir(configOut), 0o750); err != nil {
				return err
			}
			if err := os.WriteFile(configOut, yb, 0o600); err != nil {
				return err
			}
			g.config = configOut
			cfg, err := config.Load(configOut)
			if err != nil {
				return err
			}
			if _, err := db.Migrate(ctx, cfg.Database.MigratorURL, "cms_app"); err != nil {
				return err
			}
			a, err := app.New(ctx, cfg, logger(), app.Options{})
			if err != nil {
				return err
			}
			defer a.Close()
			sys := app.SystemCtx(ctx, "install")
			if _, err := a.Site().Apply(ctx, a.DB.Q); err != nil {
				return err
			}
			_, link, err := a.Auth.Invite(sys, auth.InviteInput{Email: ownerEmail, Name: "", Role: rbac.Owner})
			if err != nil && !errors.Is(err, auth.ErrEmailTaken) {
				return err
			}
			return out(map[string]any{"config": configOut, "invite_url": link}, func(w io.Writer) {
				fmt.Fprintf(w, "Installed.\n\nConfig: %s\nOwner invite (valid 7 days): %s\n\nNext:\n  systemctl enable --now cms@blue cms-worker\n  cms doctor\n", configOut, link)
			})
		},
	}
	cmd.Flags().StringVar(&siteDir, "site", "/opt/cms/site/current", "site bundle directory")
	cmd.Flags().StringVar(&domain, "domain", "", "public hostname, e.g. example.com")
	cmd.Flags().StringVar(&superURL, "db-super-url", "", "Postgres superuser URL (used once, not stored)")
	cmd.Flags().StringVar(&dbName, "db-name", "cms", "database name")
	cmd.Flags().StringVar(&dataDir, "data-dir", "/var/lib/cms", "data directory")
	cmd.Flags().StringVar(&configOut, "config-out", "/etc/cms/config.yaml", "where to write the config")
	cmd.Flags().StringVar(&kekOut, "kek-out", "/etc/cms/kek", "where to write the key file")
	cmd.Flags().StringVar(&ownerEmail, "owner-email", "", "email of the first owner (gets an invite)")
	cmd.Flags().StringVar(&dbHost, "db-host", "127.0.0.1:5432", "host:port the app connects to (written to the config)")
	cmd.Flags().StringVar(&feed, "upgrade-feed", "", "release feed URL, e.g. https://api.github.com/repos/Chrisgarlick/go-cms/releases")
	cmd.Flags().StringVar(&siteFeed, "site-feed", "", "site bundle feed URL, e.g. https://api.github.com/repos/OWNER/site-acme/releases")
	cmd.Flags().StringVar(&tokenFile, "token-file", "/etc/cms/releases-token", "read-only token for the release feeds")
	cmd.Flags().StringVar(&pubKey, "public-key", "/opt/cms/cosign.pub", "cosign public key used to verify releases")
	cmd.Flags().StringVar(&channel, "channel", "stable", "release channel: edge, canary or stable")
	cmd.Flags().StringVar(&sitePrefixFlag, "site-tag-prefix", "site", "tag prefix of this site's bundles in the site feed (e.g. docs for docs-20260101-…)")
	cmd.Flags().StringVar(&siteKey, "site-public-key", "", "cosign public key for site bundles, if they are signed with a different key than core releases")
	return cmd
}

func upgradeCmd() *cobra.Command {
	var checkOnly, siteMode bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Install the newest signed release for this server's channel (blue/green, auto-rollback)",
		Long: `Checks the private release feed for the configured channel, downloads the
release for this platform, verifies its cosign signature and checksum, unpacks
it to /opt/cms/releases/<version>/ and runs the blue/green deploy script
(/opt/cms/deploy.sh). It only installs signed releases and never changes code
in place (spec 9.6).`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			if siteMode {
				return upgradeSite(cmd.Context(), cfg, checkOnly)
			}
			if cfg.Upgrade.FeedURL == "" {
				return errors.New("upgrade.feed_url is not configured")
			}
			rel, err := latestRelease(cmd.Context(), cfg, cfg.Upgrade.FeedURL, feedFilter{Channel: cfg.Upgrade.Channel})
			if err != nil {
				return err
			}
			current := version.Version
			if rel.TagName == current || rel.TagName == "v"+current {
				return out(map[string]any{"current": current, "latest": rel.TagName, "update": false}, func(w io.Writer) {
					fmt.Fprintf(w, "Up to date (%s).\n", current)
				})
			}
			if checkOnly {
				return out(map[string]any{"current": current, "latest": rel.TagName, "update": true}, func(w io.Writer) {
					fmt.Fprintf(w, "Update available: %s -> %s\n", current, rel.TagName)
				})
			}
			dir, err := fetchRelease(cmd.Context(), cfg, rel)
			if err != nil {
				return err
			}
			script := "/opt/cms/deploy.sh"
			c := exec.CommandContext(cmd.Context(), script, dir)
			c.Stdout, c.Stderr = os.Stderr, os.Stderr
			if err := c.Run(); err != nil {
				return fmt.Errorf("deploy failed (the previous version is still serving): %w", err)
			}
			info("Upgraded to %s.", rel.TagName)
			return nil
		},
	}
	cmd.Flags().BoolVar(&checkOnly, "check", false, "only report whether an update is available")
	cmd.Flags().BoolVar(&siteMode, "site", false, "upgrade the site bundle instead of the core")
	return cmd
}

// upgradeSite installs the newest signed site bundle (site repos publish every
// main build; bundles skip the release rings, spec 9.4).
func upgradeSite(ctx context.Context, cfg *config.Config, checkOnly bool) error {
	if cfg.Upgrade.SiteFeedURL == "" {
		return nil // no site feed configured: nothing to do
	}
	prefix := sitePrefix(cfg)
	rel, err := latestRelease(ctx, cfg, cfg.Upgrade.SiteFeedURL, feedFilter{SitePrefix: prefix})
	if err != nil {
		return err
	}
	current := ""
	if t, err := os.Readlink("/opt/cms/site/current"); err == nil {
		current = filepath.Base(t)
	}
	if current == rel.TagName {
		return out(map[string]any{"site": current, "update": false}, func(w io.Writer) { fmt.Fprintf(w, "Site bundle up to date (%s).\n", current) })
	}
	if checkOnly {
		return out(map[string]any{"site": current, "latest": rel.TagName, "update": true}, func(w io.Writer) {
			fmt.Fprintf(w, "Site update available: %s -> %s\n", current, rel.TagName)
		})
	}
	dir, err := fetchAsset(ctx, cfg, rel, "site.tar.gz", filepath.Join("/opt/cms/site/releases", rel.TagName), cfg.Upgrade.SiteKey())
	if err != nil {
		return err
	}
	currentDir := ""
	if current != "" {
		currentDir = "/opt/cms/site/current"
	}
	if err := checkBundleIdentity(dir, currentDir, prefix); err != nil {
		_ = os.RemoveAll(dir)
		return fmt.Errorf("refusing site bundle %s (the current bundle is still serving): %w", rel.TagName, err)
	}
	c := exec.CommandContext(ctx, "/opt/cms/deploy.sh", "--site", dir)
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	if err := c.Run(); err != nil {
		return fmt.Errorf("site deploy failed (the previous bundle is still serving): %w", err)
	}
	info("Site bundle %s is live.", rel.TagName)
	return nil
}

// httpClient is the outbound client for release feeds (tests swap it).
var httpClient = func() *http.Client { return netx.Client(5 * time.Minute) }

func feedClient(cfg *config.Config) (*http.Client, string) {
	tok := ""
	if cfg.Upgrade.TokenFile != "" {
		if b, err := os.ReadFile(cfg.Upgrade.TokenFile); err == nil {
			tok = strings.TrimSpace(string(b))
		}
	}
	return httpClient(), tok
}

func fetchRelease(ctx context.Context, cfg *config.Config, rel *release) (string, error) {
	want := fmt.Sprintf("cms_%s_%s_%s.tar.gz", strings.TrimPrefix(rel.TagName, "v"), runtime.GOOS, runtime.GOARCH)
	return fetchAsset(ctx, cfg, rel, want, filepath.Join("/opt/cms/releases", strings.TrimPrefix(rel.TagName, "v")), cfg.Upgrade.PublicKey)
}

// fetchAsset downloads an archive plus checksums.txt and its signature,
// verifies both, and unpacks the archive into dest.
func fetchAsset(ctx context.Context, cfg *config.Config, rel *release, want, dest, publicKey string) (string, error) {
	c, tok := feedClient(cfg)
	var archiveURL, sumsURL, sigURL string
	for _, a := range rel.Assets {
		switch a.Name {
		case want:
			archiveURL = a.URL
		case "checksums.txt":
			sumsURL = a.URL
		case "checksums.txt.sig":
			sigURL = a.URL
		}
	}
	if archiveURL == "" || sumsURL == "" || sigURL == "" {
		return "", fmt.Errorf("release %s is missing %s, checksums.txt or its signature", rel.TagName, want)
	}
	tmp, err := os.MkdirTemp("", "cms-upgrade-")
	if err != nil {
		return "", err
	}
	get := func(u, dst string) error {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		req.Header.Set("Accept", "application/octet-stream")
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := c.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("download %s: %d", u, resp.StatusCode)
		}
		f, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, io.LimitReader(resp.Body, 512<<20))
		return err
	}
	archive, sums, sig := filepath.Join(tmp, want), filepath.Join(tmp, "checksums.txt"), filepath.Join(tmp, "checksums.txt.sig")
	for _, p := range [][2]string{{archiveURL, archive}, {sumsURL, sums}, {sigURL, sig}} {
		if err := get(p[0], p[1]); err != nil {
			return "", err
		}
	}
	if publicKey == "" {
		return "", errors.New("no cosign public key configured (upgrade.public_key / upgrade.site_public_key): refusing to install an unverified release")
	}
	v := exec.CommandContext(ctx, "cosign", "verify-blob", "--key", publicKey, "--signature", sig,
		"--insecure-ignore-tlog=true", sums)
	if outb, err := v.CombinedOutput(); err != nil {
		return "", fmt.Errorf("signature verification failed: %s", strings.TrimSpace(string(outb)))
	}
	sumData, _ := os.ReadFile(sums)
	f, err := os.Open(archive)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, _ = io.Copy(h, f)
	f.Close()
	got := hex.EncodeToString(h.Sum(nil))
	if !strings.Contains(string(sumData), got+"  "+want) {
		return "", errors.New("checksum mismatch: refusing to install")
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", err
	}
	x := exec.CommandContext(ctx, "tar", "-xzf", archive, "-C", dest)
	if outb, err := x.CombinedOutput(); err != nil {
		return "", fmt.Errorf("unpack: %s", outb)
	}
	return dest, nil
}
