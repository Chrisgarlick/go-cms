package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"cms/internal/actor"
	"cms/internal/app"
	"cms/internal/auth"
	"cms/internal/blocks"
	"cms/internal/config"
	"cms/internal/db"
	"cms/internal/devdb"
	"cms/internal/fixtures"
	"cms/internal/jobs"
	"cms/internal/keys"
	"cms/internal/rbac"
	"cms/internal/richtext"
	"cms/internal/site"
	"cms/internal/version"
	"cms/starters"
)

func newCmd() *cobra.Command {
	var starter, name string
	var gitInit, github bool
	cmd := &cobra.Command{
		Use:   "new <directory>",
		Short: "Create a new site repository from a starter",
		Long: "Create a new site repository. Starters: " + strings.Join(starters.Names(), ", ") +
			".\nThe starter is built into this binary, so the project matches the installed cms version.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := args[0]
			if starter == "" {
				starter = prompt("Starter ("+strings.Join(starters.Names(), ", ")+")", "blog")
			}
			fsys, ok := starters.Get(starter)
			if !ok {
				for _, s := range starters.List() {
					if s.Name == starter && s.Planned {
						return fmt.Errorf("the %q starter is planned but not available yet", starter)
					}
				}
				return fmt.Errorf("unknown starter %q (choose from %s)", starter, strings.Join(starters.Names(), ", "))
			}
			if name == "" {
				name = titleFromDir(dir)
			}
			if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
				return fmt.Errorf("%s already exists and is not empty", dir)
			}
			if err := scaffold(fsys, dir, name); err != nil {
				return err
			}
			if gitInit || github {
				if err := run(dir, "git", "init", "-q", "-b", "main"); err != nil {
					info("git init failed: %v", err)
				} else {
					_ = run(dir, "git", "add", "-A")
					_ = run(dir, "git", "commit", "-q", "-m", "Create site from the "+starter+" starter")
				}
			}
			if github {
				if _, err := exec.LookPath("gh"); err != nil {
					info("gh is not installed; skipping GitHub repo creation (brew install gh)")
				} else if err := run(dir, "gh", "repo", "create", filepath.Base(dir), "--private", "--source", ".", "--push"); err != nil {
					info("gh repo create failed: %v", err)
				}
			}
			res := map[string]string{"directory": dir, "starter": starter, "name": name, "cms_version": version.Version}
			return out(res, func(w io.Writer) {
				fmt.Fprintf(w, "Created %s from the %s starter (cms %s).\n\nNext:\n  cd %s\n  cms dev\n", dir, starter, version.Version, dir)
			})
		},
	}
	cmd.Flags().StringVar(&starter, "starter", "", "starter: "+strings.Join(starters.Names(), ", "))
	cmd.Flags().StringVar(&name, "name", "", "site name (default: from the directory name)")
	cmd.Flags().BoolVar(&gitInit, "git", true, "initialise a git repository")
	cmd.Flags().BoolVar(&github, "github", false, "create a private GitHub repository with gh and push")
	return cmd
}

func titleFromDir(dir string) string {
	base := filepath.Base(dir)
	words := strings.FieldsFunc(base, func(r rune) bool { return r == '-' || r == '_' || r == ' ' })
	for i, w := range words {
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}

func prompt(q, def string) string {
	st, _ := os.Stdin.Stat()
	if st.Mode()&os.ModeCharDevice == 0 {
		return def
	}
	fmt.Fprintf(os.Stderr, "%s [%s]: ", q, def)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	if line = strings.TrimSpace(line); line != "" {
		return line
	}
	return def
}

func run(dir, name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Stdout, c.Stderr = os.Stderr, os.Stderr
	return c.Run()
}

// scaffold copies a starter, filling placeholders, and adds the shared files.
func scaffold(fsys fs.FS, dir, name string) error {
	slug := richtext.Slugify(name)
	repl := strings.NewReplacer("__SITE_NAME__", name, "__SITE_SLUG__", slug, "__CMS_VERSION__", strings.TrimPrefix(version.Version, "v"))
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(p))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		return os.WriteFile(target, []byte(repl.Replace(string(data))), 0o644)
	})
	if err != nil {
		return err
	}
	extra := map[string]string{
		".gitignore": ".cms/\n.DS_Store\n/dist/\n",
		".github/workflows/site.yml": `# Validates the theme and schema against the pinned cms version, then builds
# and signs the site bundle. The pipeline itself lives in the cms repository.
name: site
on:
  push: { branches: [main] }
  pull_request:
permissions:
  contents: write
  id-token: write
jobs:
  site:
    uses: ` + version.Repo + `/.github/workflows/site.yml@v1
    secrets: inherit
`,
		"README.md": "# " + name + `

A site built with cms.

## Run it locally

    cms dev

Open http://localhost:8080 for the site and http://localhost:8080/admin/ for the admin.
The first run creates a local database in ` + "`.cms/`" + ` (git-ignored) and loads sample content from ` + "`fixtures/`" + `.

## Layout

- ` + "`site.yaml`" + `: name, locale and the cms version this site needs
- ` + "`content-types/`, `taxonomies/`, `field-groups/`" + `: the content model
- ` + "`blocks/`" + `: custom blocks (` + "`cms make block <name>`" + `)
- ` + "`theme/`" + `: templates and assets; extends the built-in base theme
- ` + "`fixtures/`" + `: sample content for local development

## Deploy

Push to main. CI validates the bundle and publishes a signed release; the server
installs it automatically (` + "`cms upgrade`" + ` runs on a timer).
`,
	}
	for p, content := range extra {
		target := filepath.Join(dir, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func makeCmd() *cobra.Command {
	var dry bool
	cmd := &cobra.Command{Use: "make", Short: "Generate content types, blocks and field groups in a site repo"}
	gen := func(kind string, files func(name string) map[string]string) *cobra.Command {
		return &cobra.Command{
			Use:   kind + " <name>",
			Short: "Generate a " + kind,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if _, err := os.Stat("site.yaml"); err != nil {
					return errors.New("run this inside a site repository (no site.yaml here)")
				}
				name := strings.ReplaceAll(richtext.Slugify(args[0]), "-", "_")
				if kind == "block" {
					name = richtext.Slugify(args[0])
				}
				var created []string
				for p, c := range files(name) {
					if _, err := os.Stat(p); err == nil {
						return fmt.Errorf("%s already exists", p)
					}
					created = append(created, p)
					if dry {
						continue
					}
					if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
						return err
					}
					if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
						return err
					}
				}
				return out(map[string]any{"created": created, "dry_run": dry}, func(w io.Writer) {
					for _, c := range created {
						fmt.Fprintln(w, "created", c)
					}
				})
			},
		}
	}
	cmd.AddCommand(
		gen("type", func(n string) map[string]string {
			label := fieldsHumanize(n)
			return map[string]string{
				"content-types/" + n + ".yaml":             fmt.Sprintf("name: %s\nlabel: { singular: %s, plural: %ss }\nurl: /%s/{slug}\narchive: /%s\nblocks:\n  default: [rich-text]\nfields:\n  - { name: summary, type: textarea }\n", n, label, label, richtext.Slugify(n)+"s", richtext.Slugify(n)+"s"),
				"theme/templates/single-" + n + ".gohtml":  "{{define \"main\"}}\n<article class=\"entry wrap\">\n  <h1>{{.Entry.Title}}</h1>\n  {{with .Entry.Field \"summary\"}}<p class=\"lede\">{{.}}</p>{{end}}\n  <div class=\"entry-content\">{{.Content}}</div>\n</article>\n{{end}}\n",
				"theme/templates/archive-" + n + ".gohtml": "{{define \"main\"}}\n<div class=\"wrap\">\n  <h1>{{.Title}}</h1>\n  <div class=\"cards\">{{range .Entries}}{{template \"partials/entry-card\" .}}{{end}}</div>\n  {{template \"partials/pagination\" .}}\n</div>\n{{end}}\n",
			}
		}),
		gen("block", func(n string) map[string]string {
			return map[string]string{
				"blocks/" + n + "/" + n + ".yaml":   fmt.Sprintf("name: %s\nlabel: %s\nicon: square\ncategory: layout\nfields:\n  - { name: heading, type: text, required: true }\n  - { name: text, type: textarea }\n", n, fieldsHumanize(n)),
				"blocks/" + n + "/" + n + ".gohtml": "<section class=\"" + n + "\">\n  <h2>{{.Data.heading}}</h2>\n  {{with .Data.text}}<p>{{.}}</p>{{end}}\n</section>\n",
			}
		}),
		gen("field-group", func(n string) map[string]string {
			return map[string]string{
				"field-groups/" + n + ".yaml": fmt.Sprintf("name: %s\nlabel: %s\nposition: sidebar\nlocation:\n  - { type: page }\nfields:\n  - { name: example, type: text, help: Describe what this field is for }\n", strings.ReplaceAll(n, "_", "-"), fieldsHumanize(n)),
			}
		}),
	)
	cmd.PersistentFlags().BoolVar(&dry, "dry-run", false, "show what would be created")
	return cmd
}

func fieldsHumanize(s string) string {
	s = strings.NewReplacer("_", " ", "-", " ").Replace(s)
	return strings.ToUpper(s[:1]) + s[1:]
}

func devCmd() *cobra.Command {
	var port int
	var siteDir string
	var reset bool
	cmd := &cobra.Command{
		Use:   "dev",
		Short: "Run a site locally: embedded Postgres, sample content, live reload",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDev(cmd.Context(), siteDir, port, reset)
		},
	}
	cmd.Flags().IntVar(&port, "port", 8080, "HTTP port")
	cmd.Flags().StringVar(&siteDir, "site", ".", "site repository directory")
	cmd.Flags().BoolVar(&reset, "reset", false, "delete the local database and start again")
	return cmd
}

func runDev(ctx context.Context, siteDir string, port int, reset bool) error {
	siteDir, _ = filepath.Abs(siteDir)
	if _, err := os.Stat(filepath.Join(siteDir, "site.yaml")); err != nil {
		return errors.New("no site.yaml here: run `cms new my-site` first, or pass --site")
	}
	state := filepath.Join(siteDir, ".cms", "dev")
	if reset {
		if err := os.RemoveAll(state); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return err
	}
	if ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port)); err != nil {
		return fmt.Errorf("port %d is in use (try --port)", port)
	} else {
		ln.Close()
	}
	log := logger()
	info("Starting Postgres (first run downloads it once, ~30 MB)…")
	cluster, err := devdb.Start(filepath.Join(state, "pg"), 0, nil)
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
	kek := filepath.Join(state, "kek")
	if _, err := os.Stat(kek); err != nil {
		if err := os.WriteFile(kek, []byte(keys.Generate(1)), 0o600); err != nil {
			return err
		}
	}
	cfg := config.Defaults()
	cfg.Env = config.EnvDevelopment
	cfg.BaseURL = fmt.Sprintf("http://localhost:%d", port)
	cfg.HTTP.Addr = fmt.Sprintf("127.0.0.1:%d", port)
	cfg.HTTP.ServeStatic = true
	cfg.Database.URL = cluster.AppURL("cms")
	cfg.Database.MigratorURL = cluster.MigratorURL("cms")
	cfg.Paths.Data = filepath.Join(state, "data")
	cfg.Paths.Site = siteDir
	cfg.Keys.KEKFile = kek
	cfg.Security.DevSkip2FA = true
	cfg.Security.VulnLookup = false
	if err := cfg.Validate(); err != nil {
		return err
	}
	a, err := app.New(ctx, &cfg, log, app.Options{Workers: true})
	if err != nil {
		return err
	}
	defer a.Close()
	sys := app.SystemCtx(ctx, "dev")
	if _, err := a.Site().Apply(ctx, a.DB.Q); err != nil {
		return err
	}
	if err := a.River.Start(ctx); err != nil {
		return err
	}
	defer stopRiver(a)
	if err := ensureDevOwner(sys, a); err != nil {
		return err
	}
	if n, err := a.DB.Q.CountEntriesByStatus(ctx); err == nil && len(n) == 0 {
		// Load as the dev owner so sample entries have an author.
		fctx := sys
		if u, err := a.DB.Q.GetUserByEmail(ctx, "dev@localhost"); err == nil {
			fctx = actor.With(ctx, &actor.Actor{Kind: actor.KindUser, UserID: u.ID, Role: rbac.Owner, Label: u.Email})
		}
		count, err := fixtures.Load(fctx, a.Site().FS, fixtures.Services{Content: a.Content, Menus: a.Menus, Settings: a.Settings, Q: a.DB.Q, Redirects: a.Redirects})
		if err != nil {
			log.Warn("sample content failed to load", "err", err)
		} else if count > 0 {
			info("Loaded %d sample entries from fixtures/", count)
		}
	}
	if err := a.Prepare(ctx); err != nil {
		return err
	}
	if err := a.Jobs.Enqueue(ctx, jobs.RenderRebuild{Reason: "cms dev start"}); err != nil {
		return err
	}
	a.Background(ctx)
	go watchBundle(ctx, a)
	info("\n  Site   %s\n  Admin  %s/admin/   (dev@localhost / see .cms/dev/credentials)\n", cfg.BaseURL, cfg.BaseURL)
	adm, apiH := handlers(a)
	return a.Serve(ctx, a.Handler(adm, apiH), cfg.HTTP.Addr)
}

// ensureDevOwner creates dev@localhost with a random password stored in .cms/dev/credentials.
func ensureDevOwner(ctx context.Context, a *app.App) error {
	if _, err := a.DB.Q.GetUserByEmail(ctx, "dev@localhost"); err == nil {
		return nil
	}
	pw := "dev-" + auth.RandomToken(12)
	u, err := a.Auth.CreateUserDirect(ctx, "dev@localhost", "Developer", pw, rbac.Owner)
	if err != nil {
		return err
	}
	_ = u
	cred := filepath.Join(filepath.Dir(a.Cfg.Keys.KEKFile), "credentials")
	return os.WriteFile(cred, []byte("email: dev@localhost\npassword: "+pw+"\n"), 0o600)
}

// watchBundle reloads the site bundle when files change (templates, schema, assets).
func watchBundle(ctx context.Context, a *app.App) {
	last := a.Site().Hash
	t := time.NewTicker(700 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		b, err := site.LoadDir(a.Cfg.Paths.Site)
		if err != nil || b.Hash == last {
			continue
		}
		b.AddBuiltinBlocks(blocks.Builtin())
		if err := b.Finalize(); err != nil {
			a.Log.Error("site bundle has errors (keeping the previous version)", "err", err)
			last = b.Hash
			continue
		}
		last = b.Hash
		if err := a.LoadBundle(); err != nil {
			a.Log.Error("reload failed", "err", err)
			continue
		}
		if _, err := a.Site().Apply(ctx, a.DB.Q); err != nil {
			a.Log.Error("schema apply failed", "err", err)
			continue
		}
		if errs := a.Renderer.Theme().Validate(); len(errs) > 0 {
			for _, e := range errs {
				a.Log.Error("template error", "err", e)
			}
		}
		_ = a.Jobs.Enqueue(ctx, jobs.RenderRebuild{Reason: "files changed"})
		a.Log.Info("site reloaded", "hash", last)
	}
}
