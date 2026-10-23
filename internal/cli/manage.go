package cli

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"cms/internal/actor"
	"cms/internal/app"
	"cms/internal/audit"
	"cms/internal/auth"
	"cms/internal/blocks"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/fields"
	"cms/internal/fixtures"
	"cms/internal/jobs"
	"cms/internal/keys"
	"cms/internal/media"
	"cms/internal/rbac"
	"cms/internal/redirects"
	"cms/internal/render"
	"cms/internal/richtext"
	"cms/internal/secaudit"
	"cms/internal/site"
	"cms/themes"
	"context"
)

func userCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "user", Short: "Manage users"}
	var email, name, role, password string
	create := &cobra.Command{
		Use:   "create",
		Short: "Create a user (prints an invite link unless --password is given)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			r, err := rbac.Parse(role)
			if err != nil {
				return err
			}
			if password != "" {
				u, err := a.Auth.CreateUserDirect(ctx, email, name, password, r)
				if err != nil {
					return err
				}
				return out(u, func(w io.Writer) {
					fmt.Fprintf(w, "Created %s (%s). They set up 2FA at first sign-in.\n", u.Email, u.Role)
				})
			}
			u, link, err := a.Auth.Invite(ctx, auth.InviteInput{Email: email, Name: name, Role: r})
			if err != nil {
				return err
			}
			return out(map[string]any{"id": u.ID, "email": u.Email, "role": u.Role, "invite_url": link}, func(w io.Writer) {
				fmt.Fprintf(w, "Invited %s (%s).\nInvite link (valid 7 days):\n  %s\n", u.Email, u.Role, link)
			})
		},
	}
	create.Flags().StringVar(&email, "email", "", "email address (required)")
	create.Flags().StringVar(&name, "name", "", "display name")
	create.Flags().StringVar(&role, "role", "editor", "owner, admin or editor")
	create.Flags().StringVar(&password, "password", "", "set a password directly instead of sending an invite")
	_ = create.MarkFlagRequired("email")

	list := &cobra.Command{
		Use:   "list",
		Short: "List users",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			us, err := a.Auth.Users(ctx)
			if err != nil {
				return err
			}
			type row struct {
				ID     uuid.UUID `json:"id"`
				Email  string    `json:"email"`
				Name   string    `json:"name"`
				Role   string    `json:"role"`
				Status string    `json:"status"`
				TwoFA  bool      `json:"two_factor"`
			}
			rows := make([]row, 0, len(us))
			for _, u := range us {
				rows = append(rows, row{u.ID, u.Email, u.DisplayName, u.Role, u.Status, u.TotpConfirmed})
			}
			return out(rows, func(w io.Writer) {
				fmt.Fprintln(w, "EMAIL\tNAME\tROLE\tSTATUS\t2FA")
				for _, r := range rows {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%v\n", r.Email, r.Name, r.Role, r.Status, r.TwoFA)
				}
			})
		},
	}
	withUser := func(use, short string, fn func(a *appT, ctx ctxT, id uuid.UUID, arg string) error, nargs int) *cobra.Command {
		return &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(nargs),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, ctx, err := openApp(cmd, false)
				if err != nil {
					return err
				}
				defer a.Close()
				id, err := a.FindUserByEmail(ctx, args[0])
				if err != nil {
					return err
				}
				arg := ""
				if len(args) > 1 {
					arg = args[1]
				}
				return fn(a, ctx, id, arg)
			},
		}
	}
	roleCmd := withUser("role <email> <owner|admin|editor>", "Change a user's role", func(a *appT, ctx ctxT, id uuid.UUID, arg string) error {
		r, err := rbac.Parse(arg)
		if err != nil {
			return err
		}
		return a.Auth.SetRole(ctx, id, r)
	}, 2)
	disable := withUser("disable <email>", "Disable a user and end their sessions", func(a *appT, ctx ctxT, id uuid.UUID, _ string) error {
		return a.Auth.SetStatus(ctx, id, "disabled")
	}, 1)
	enable := withUser("enable <email>", "Re-enable a user", func(a *appT, ctx ctxT, id uuid.UUID, _ string) error {
		return a.Auth.SetStatus(ctx, id, "active")
	}, 1)
	reset := withUser("reset-password <email>", "Set a new password (prompted, or $CMS_NEW_PASSWORD)", func(a *appT, ctx ctxT, id uuid.UUID, _ string) error {
		pw := os.Getenv("CMS_NEW_PASSWORD")
		if pw == "" {
			pw = prompt("New password", "")
		}
		if pw == "" {
			return errors.New("no password given")
		}
		return a.Auth.SetPasswordDirect(ctx, id, pw)
	}, 1)
	twofa := withUser("2fa-reset <email>", "Clear a user's authenticator so they enrol again", func(a *appT, ctx ctxT, id uuid.UUID, _ string) error {
		return a.Auth.Reset2FA(ctx, id)
	}, 1)
	del := withUser("delete <email>", "Delete a user (personal data removed, history kept)", func(a *appT, ctx ctxT, id uuid.UUID, _ string) error {
		if err := confirm("Delete this user?"); err != nil {
			return err
		}
		return a.Auth.DeleteUser(ctx, id)
	}, 1)
	cmd.AddCommand(create, list, roleCmd, disable, enable, reset, twofa, del)
	cmd.PersistentFlags().BoolVarP(&g.yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

func tokenCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "token", Short: "Manage API tokens"}
	var as, name, role string
	var days int
	create := &cobra.Command{
		Use:   "create",
		Short: "Create an API token for a user (printed once)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			u, err := a.DB.Q.GetUserByEmail(ctx, strings.ToLower(as))
			if err != nil {
				return errors.New("--as must be an existing user's email")
			}
			r, err := rbac.Parse(role)
			if err != nil {
				return err
			}
			uctx := actor.With(ctx, &actor.Actor{Kind: actor.KindUser, UserID: u.ID, Role: rbac.Role(u.Role), Label: "cli:" + u.Email, Sudo: true})
			plain, t, err := a.Auth.CreateToken(uctx, name, r, time.Duration(days)*24*time.Hour)
			if err != nil {
				return err
			}
			return out(map[string]any{"id": t.ID, "token": plain, "expires_at": t.ExpiresAt}, func(w io.Writer) {
				fmt.Fprintf(w, "%s\n\nStore this token now; it is not shown again. Expires %s.\n", plain, t.ExpiresAt.Format("2 Jan 2006"))
			})
		},
	}
	create.Flags().StringVar(&as, "as", "", "user email (required)")
	create.Flags().StringVar(&name, "name", "cli", "token name")
	create.Flags().StringVar(&role, "role", "editor", "role (capped at the user's role)")
	create.Flags().IntVar(&days, "days", 90, "lifetime in days (max 366)")
	_ = create.MarkFlagRequired("as")
	list := &cobra.Command{
		Use: "list", Short: "List API tokens",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			ts, err := a.Auth.Tokens(ctx)
			if err != nil {
				return err
			}
			return out(ts, func(w io.Writer) {
				fmt.Fprintln(w, "ID\tNAME\tROLE\tEXPIRES\tLAST USED")
				for _, t := range ts {
					last := "never"
					if t.LastUsedAt != nil {
						last = t.LastUsedAt.Format("2006-01-02")
					}
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", t.ID, t.Name, t.Role, t.ExpiresAt.Format("2006-01-02"), last)
				}
			})
		},
	}
	revoke := &cobra.Command{
		Use: "revoke <id>", Short: "Revoke a token", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			id, err := uuid.Parse(args[0])
			if err != nil {
				return err
			}
			sys := actor.With(ctx, &actor.Actor{Kind: actor.KindSystem, Caps: map[rbac.Cap]bool{rbac.TokensOwn: true, rbac.TokensAll: true}, Label: "cli"})
			return a.Auth.RevokeToken(sys, id)
		},
	}
	cmd.AddCommand(create, list, revoke)
	return cmd
}

func schemaCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "schema", Short: "Content model: compare and apply the site bundle"}
	diff := &cobra.Command{
		Use:   "diff",
		Short: "Show differences between the bundle and the database (exit 3 if any)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			ch, err := a.Site().Diff(ctx, a.DB.Q)
			if err != nil {
				return err
			}
			if err := out(ch, func(w io.Writer) {
				if len(ch) == 0 {
					fmt.Fprintln(w, "The database matches the site bundle.")
					return
				}
				for _, c := range ch {
					fmt.Fprintf(w, "%s\t%s\t%s\n", c.Action, c.Kind, c.Name)
				}
			}); err != nil {
				return err
			}
			if len(ch) > 0 {
				return validationErr("schema differs from the bundle; run `cms schema apply`")
			}
			return nil
		},
	}
	apply := &cobra.Command{
		Use:   "apply",
		Short: "Apply the bundle's content model",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			ch, err := a.Site().Apply(ctx, a.DB.Q)
			if err != nil {
				return err
			}
			if len(ch) > 0 {
				_ = a.Jobs.Enqueue(ctx, jobs.RenderRebuild{Reason: "schema applied"})
			}
			return out(ch, func(w io.Writer) { fmt.Fprintf(w, "Applied %d change(s).\n", len(ch)) })
		},
	}
	export := &cobra.Command{
		Use:   "export",
		Short: "Print the resolved content model as JSON (for typed clients)",
		RunE: func(cmd *cobra.Command, args []string) error {
			b, err := loadBundleOnly()
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(map[string]any{"types": b.Types, "taxonomies": b.Taxonomies, "field_groups": b.FieldGroups, "blocks": b.Blocks})
		},
	}
	cmd.AddCommand(diff, apply, export)
	return cmd
}

// validationErr exits with code 3 (spec 5.2).
func validationErr(s string) error { return fields.Errors{"": s} }

func loadBundleOnly() (*site.Bundle, error) {
	dir := "."
	if cfg, err := loadConfig(); err == nil && cfg.Paths.Site != "" {
		if _, err := os.Stat(filepath.Join(cfg.Paths.Site, "site.yaml")); err == nil {
			dir = cfg.Paths.Site
		}
	}
	b, err := site.LoadDir(dir)
	if err != nil {
		return nil, err
	}
	b.AddBuiltinBlocks(blocks.Builtin())
	return b, b.Finalize()
}

func themeCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "theme", Short: "Theme tools"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate [site-dir]",
		Short: "Load the bundle and parse every template (no database needed)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := "."
			if len(args) == 1 {
				dir = args[0]
			}
			b, err := site.LoadDir(dir)
			if err != nil {
				return err
			}
			b.AddBuiltinBlocks(blocks.Builtin())
			if err := b.Finalize(); err != nil {
				return err
			}
			var siteTheme fs.FS
			if _, err := fs.Stat(b.FS, "theme"); err == nil {
				siteTheme, _ = fs.Sub(b.FS, "theme")
			}
			t, err := render.LoadTheme(themes.Base(), siteTheme, b.FS, render.Funcs())
			if err != nil {
				return err
			}
			var problems []string
			for _, e := range t.Validate() {
				problems = append(problems, e.Error())
			}
			for name, d := range b.Blocks {
				if !t.Has("blocks/" + name) {
					problems = append(problems, fmt.Sprintf("block %s has no template (blocks/%s.gohtml)", name, name))
				}
				_ = d
			}
			res := map[string]any{"theme_hash": t.Hash, "types": len(b.Types), "blocks": len(b.Blocks), "problems": problems}
			if err := out(res, func(w io.Writer) {
				if len(problems) == 0 {
					fmt.Fprintf(w, "OK: %d content types, %d blocks, theme %s\n", len(b.Types), len(b.Blocks), t.Hash)
				}
				for _, p := range problems {
					fmt.Fprintln(w, "problem:", p)
				}
			}); err != nil {
				return err
			}
			if len(problems) > 0 {
				return validationErr(fmt.Sprintf("%d theme problem(s)", len(problems)))
			}
			return nil
		},
	})
	return cmd
}

func cacheCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Rendered page cache"}
	cmd.AddCommand(&cobra.Command{
		Use:   "rebuild",
		Short: "Re-render every page now",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			if err := a.Prepare(ctx); err != nil {
				return err
			}
			n, err := a.Renderer.Rebuild(ctx)
			if err != nil {
				return err
			}
			return out(map[string]int{"pages": n}, func(w io.Writer) { fmt.Fprintf(w, "Rendered %d pages.\n", n) })
		},
	}, &cobra.Command{
		Use:   "clear",
		Short: "Delete every rendered file (pages render again on first request)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			if err := os.RemoveAll(a.Renderer.Disk.Root); err != nil {
				return err
			}
			if err := a.DB.Q.ClearAllRenderDeps(ctx); err != nil {
				return err
			}
			info("Cleared %s", a.Renderer.Disk.Root)
			return nil
		},
	})
	return cmd
}

func buildCmd() *cobra.Command {
	var outDir string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Static export: render every page into a folder for any static host",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			if _, err := a.Renderer.LoadTheme(a.Site()); err != nil {
				return err
			}
			paths, err := a.Renderer.AllPaths(ctx)
			if err != nil {
				return err
			}
			disk := &render.Disk{Root: outDir}
			var skipped []string
			for _, p := range paths {
				res, err := a.Renderer.Render(ctx, p)
				if err != nil {
					return fmt.Errorf("%s: %w", p, err)
				}
				if res.Status != 200 || !res.Cacheable {
					skipped = append(skipped, p)
					continue
				}
				if err := disk.Write(p, res.Body); err != nil {
					return err
				}
			}
			th := a.Renderer.Theme()
			for _, p := range th.Assets() {
				u, _ := th.AssetPath(p)
				data, _, _ := th.OpenAsset(u)
				if err := disk.Write(u, data); err != nil {
					return err
				}
			}
			res, _ := a.Renderer.Render(ctx, "/__404__/")
			if res != nil {
				_ = disk.Write("/404.html", res.Body)
			}
			return out(map[string]any{"pages": len(paths) - len(skipped), "skipped": skipped, "out": outDir}, func(w io.Writer) {
				fmt.Fprintf(w, "Exported %d pages to %s.\n", len(paths)-len(skipped), outDir)
				for _, s := range skipped {
					fmt.Fprintf(w, "skipped (dynamic or not cacheable): %s\n", s)
				}
				fmt.Fprintln(w, "Not available in a static export: search, previews, private pages. Media lives in the data directory.")
			})
		},
	}
	cmd.Flags().StringVar(&outDir, "out", "./dist", "output directory")
	return cmd
}

func searchCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "search", Short: "Search index"}
	cmd.AddCommand(&cobra.Command{
		Use: "reindex", Short: "Rebuild the search index for every published entry",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			n, err := a.Content.Reindex(ctx)
			if err != nil {
				return err
			}
			return out(map[string]int{"entries": n}, func(w io.Writer) { fmt.Fprintf(w, "Reindexed %d entries.\n", n) })
		},
	})
	return cmd
}

func contentCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "content", Short: "Entries"}
	var typ, status string
	var limit int
	list := &cobra.Command{
		Use: "list", Short: "List entries of a type",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			rows, total, err := a.Content.List(ctx, content.ListFilter{Type: typ, Status: status, Limit: int32(limit)})
			if err != nil {
				return err
			}
			return out(map[string]any{"total": total, "entries": rows}, func(w io.Writer) {
				fmt.Fprintln(w, "ID\tSTATUS\tTITLE\tUPDATED")
				for _, r := range rows {
					fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", r.ID, r.Status, r.DraftTitle, r.UpdatedAt.Format("2006-01-02 15:04"))
				}
			})
		},
	}
	list.Flags().StringVar(&typ, "type", "post", "content type")
	list.Flags().StringVar(&status, "status", "", "filter by status")
	list.Flags().IntVar(&limit, "limit", 50, "maximum rows")
	var title, body string
	var publish bool
	create := &cobra.Command{
		Use: "create", Short: "Create an entry (body as Markdown)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			e, err := a.Content.Create(ctx, content.CreateInput{Type: typ, Title: title})
			if err != nil {
				return err
			}
			if body != "" {
				if strings.HasPrefix(body, "@") {
					b, err := os.ReadFile(body[1:])
					if err != nil {
						return err
					}
					body = string(b)
				}
				tree, _ := json.Marshal([]map[string]any{{"id": blocks.NewID(), "type": "rich-text", "data": map[string]any{"body": richtextFromMarkdown(body)}}})
				e, err = a.Content.Save(ctx, content.SaveInput{ID: e.ID, ExpectedVersion: e.Version, Title: title, Slug: e.DraftSlug, Blocks: tree})
				if err != nil {
					return err
				}
			}
			if publish {
				if e, err = a.Content.Publish(ctx, e.ID, e.Version); err != nil {
					return err
				}
			}
			return out(e, func(w io.Writer) { fmt.Fprintf(w, "Created %s (%s)\n", e.ID, e.Status) })
		},
	}
	create.Flags().StringVar(&typ, "type", "post", "content type")
	create.Flags().StringVar(&title, "title", "", "title (required)")
	create.Flags().StringVar(&body, "body", "", "Markdown body, or @file.md")
	create.Flags().BoolVar(&publish, "publish", false, "publish immediately")
	_ = create.MarkFlagRequired("title")
	byID := func(use, short string, fn func(a *appT, ctx ctxT, id uuid.UUID) error) *cobra.Command {
		return &cobra.Command{Use: use + " <id>", Short: short, Args: cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				a, ctx, err := openApp(cmd, false)
				if err != nil {
					return err
				}
				defer a.Close()
				id, err := uuid.Parse(args[0])
				if err != nil {
					return err
				}
				return fn(a, ctx, id)
			}}
	}
	var skipExisting bool
	imp := &cobra.Command{
		Use:   "import <file.yaml>",
		Short: "Import entries, menus, options and redirects from a content file (fixtures format)",
		Long: "Loads a content file in the fixtures/content.yaml format into this site's database:\n" +
			"entries (Markdown or ProseMirror bodies, exact publish dates, SEO), terms, menus,\n" +
			"options and redirects. Use it to move a site onto cms. With --skip-existing,\n" +
			"entries whose type and slug already exist are left alone, so it can be re-run.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			n, err := fixtures.Import(ctx, data, fixtures.Services{Content: a.Content, Menus: a.Menus, Settings: a.Settings,
				Q: a.DB.Q, Redirects: a.Redirects}, skipExisting)
			if err != nil {
				return err
			}
			return out(map[string]any{"entries": n}, func(w io.Writer) { fmt.Fprintf(w, "Imported %d entries.\n", n) })
		},
	}
	imp.Flags().BoolVar(&skipExisting, "skip-existing", false, "leave entries whose type and slug already exist")
	cmd.AddCommand(imp)
	cmd.AddCommand(list, create,
		byID("publish", "Publish the working copy", func(a *appT, ctx ctxT, id uuid.UUID) error {
			_, err := a.Content.Publish(ctx, id, 0)
			return err
		}),
		byID("unpublish", "Take an entry off the site", func(a *appT, ctx ctxT, id uuid.UUID) error { return a.Content.Unpublish(ctx, id) }),
		byID("trash", "Move an entry to the trash", func(a *appT, ctx ctxT, id uuid.UUID) error { return a.Content.Trash(ctx, id) }),
		byID("restore", "Restore an entry from the trash", func(a *appT, ctx ctxT, id uuid.UUID) error { return a.Content.Restore(ctx, id) }),
		byID("clone", "Duplicate an entry as a new draft", func(a *appT, ctx ctxT, id uuid.UUID) error {
			e, err := a.Content.Clone(ctx, id)
			if err == nil {
				info("Created %s", e.ID)
			}
			return err
		}),
		byID("revisions", "List revisions", func(a *appT, ctx ctxT, id uuid.UUID) error {
			rs, err := a.Content.Revisions(ctx, id, 100)
			if err != nil {
				return err
			}
			return out(rs, func(w io.Writer) {
				fmt.Fprintln(w, "NUMBER\tKIND\tTITLE\tAUTHOR\tWHEN")
				for _, r := range rs {
					fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\n", r.Number, r.Kind, r.Title, r.AuthorName, r.CreatedAt.Format("2006-01-02 15:04"))
				}
			})
		}),
	)
	return cmd
}

func settingsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "settings", Short: "Site settings"}
	cmd.AddCommand(&cobra.Command{
		Use: "list", Short: "List settings",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			all, err := a.Settings.All(ctx)
			if err != nil {
				return err
			}
			return out(all, func(w io.Writer) {
				for k, v := range all {
					fmt.Fprintf(w, "%s\t%s\n", k, v)
				}
			})
		},
	}, &cobra.Command{
		Use: "set <key> <json-or-text>", Short: "Set a setting", Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			var v any
			if json.Unmarshal([]byte(args[1]), &v) != nil {
				v = args[1]
			}
			return a.Settings.Set(ctx, args[0], v)
		},
	})
	return cmd
}

func redirectsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "redirects", Short: "Redirect rules"}
	list := &cobra.Command{Use: "list", Short: "List rules", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		rs, err := a.Redirects.List(ctx)
		if err != nil {
			return err
		}
		return out(rs, func(w io.Writer) {
			fmt.Fprintln(w, "ID\tSTATUS\tTYPE\tSOURCE\tTARGET\tHITS")
			for _, r := range rs {
				fmt.Fprintf(w, "%s\t%d\t%s\t%s\t%s\t%d\n", r.ID, r.StatusCode, r.MatchType, r.Source, r.Target, r.Hits)
			}
		})
	}}
	var status int
	var match string
	add := &cobra.Command{Use: "add <source> <target>", Short: "Add a rule", Args: cobra.RangeArgs(1, 2), RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		in := redirects.Input{Source: args[0], Status: status, MatchType: match, PreserveQuery: true}
		if len(args) == 2 {
			in.Target = args[1]
		}
		r, err := a.Redirects.Create(ctx, in)
		if err != nil {
			return err
		}
		return out(r, func(w io.Writer) { fmt.Fprintf(w, "Added %s -> %s (%d)\n", r.Source, r.Target, r.StatusCode) })
	}}
	add.Flags().IntVar(&status, "status", 301, "301, 302, 307, 308, 410 or 451")
	add.Flags().StringVar(&match, "match", "exact", "exact, prefix or regex")
	del := &cobra.Command{Use: "delete <id>", Short: "Delete a rule", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		id, err := uuid.Parse(args[0])
		if err != nil {
			return err
		}
		return a.Redirects.Delete(ctx, id)
	}}
	test := &cobra.Command{Use: "test <path>", Short: "Show where a path redirects (following chains)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		if err := a.Redirects.Reload(ctx); err != nil {
			return err
		}
		chain := a.Redirects.Test(args[0])
		return out(chain, func(w io.Writer) {
			if len(chain) == 0 {
				fmt.Fprintln(w, "No redirect matches.")
			}
			for _, m := range chain {
				fmt.Fprintf(w, "%d -> %s\n", m.Status, m.Target)
			}
		})
	}}
	check := &cobra.Command{Use: "check", Short: "Find loops and chains", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		if err := a.Redirects.Reload(ctx); err != nil {
			return err
		}
		probs, err := a.Redirects.Problems(ctx)
		if err != nil {
			return err
		}
		return out(probs, func(w io.Writer) {
			if len(probs) == 0 {
				fmt.Fprintln(w, "No loops or chains.")
			}
			for _, p := range probs {
				fmt.Fprintln(w, p)
			}
		})
	}}
	var dry bool
	imp := &cobra.Command{Use: "import <file.csv>", Short: "Import rules from CSV: source,target,status[,match]", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		f, err := os.Open(args[0])
		if err != nil {
			return err
		}
		defer f.Close()
		rows, err := csv.NewReader(f).ReadAll()
		if err != nil {
			return err
		}
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		ok, failed := 0, 0
		for i, r := range rows {
			if len(r) < 2 || (i == 0 && strings.EqualFold(r[0], "source")) {
				continue
			}
			in := redirects.Input{Source: r[0], Target: r[1], Status: 301, MatchType: "exact", PreserveQuery: true}
			if len(r) > 2 {
				_, _ = fmt.Sscan(r[2], &in.Status)
			}
			if len(r) > 3 {
				in.MatchType = r[3]
			}
			if dry {
				ok++
				continue
			}
			if _, err := a.Redirects.Create(ctx, in); err != nil {
				failed++
				info("line %d: %v", i+1, err)
				continue
			}
			ok++
		}
		return out(map[string]any{"imported": ok, "failed": failed, "dry_run": dry}, func(w io.Writer) {
			fmt.Fprintf(w, "Imported %d, failed %d.\n", ok, failed)
		})
	}}
	imp.Flags().BoolVar(&dry, "dry-run", false, "validate only")
	cmd.AddCommand(list, add, del, test, check, imp)
	return cmd
}

func mediaCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "media", Short: "Media library"}
	upload := &cobra.Command{Use: "upload <files...>", Short: "Upload files", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		var res []map[string]any
		for _, p := range args {
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			m, dup, err := a.Media.Upload(ctx, media.UploadInput{Filename: filepath.Base(p), Body: f})
			f.Close()
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			res = append(res, map[string]any{"file": p, "id": m.ID, "duplicate": dup})
		}
		return out(res, func(w io.Writer) {
			for _, r := range res {
				fmt.Fprintf(w, "%s\t%s\n", r["id"], r["file"])
			}
		})
	}}
	list := &cobra.Command{Use: "list", Short: "List media", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		rows, _, err := a.Media.List(ctx, media.ListFilter{Limit: 200})
		if err != nil {
			return err
		}
		return out(rows, func(w io.Writer) {
			fmt.Fprintln(w, "ID\tFILE\tTYPE\tSIZE\tUSED")
			for _, m := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%d KB\t%d\n", m.ID, m.Filename, m.Mime, m.Size/1024, m.UsageCount)
			}
		})
	}}
	regen := &cobra.Command{Use: "regenerate", Short: "Re-create every image variant", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		rows, _, err := a.Media.List(ctx, media.ListFilter{Kind: "image/", Limit: 200})
		if err != nil {
			return err
		}
		for _, m := range rows {
			if err := a.Jobs.Enqueue(ctx, jobs.MediaProcess{MediaID: m.ID.String(), Force: true}); err != nil {
				return err
			}
		}
		info("Queued %d images.", len(rows))
		return nil
	}}
	cmd.AddCommand(upload, list, regen)
	return cmd
}

func mediaProcessCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "media-process",
		Short:  "Internal: process one image in an isolated subprocess",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return media.RunChild(os.Stdin, os.Stdout)
		},
	}
}

func auditCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "audit", Short: "Audit log"}
	var limit int
	list := &cobra.Command{Use: "list", Short: "Recent audit events", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		rows, err := a.DB.Q.ListAudit(ctx, auditParams(int32(limit)))
		if err != nil {
			return err
		}
		return out(rows, func(w io.Writer) {
			fmt.Fprintln(w, "WHEN\tACTOR\tACTION\tRESOURCE")
			for _, r := range rows {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s:%s\n", r.CreatedAt.Format("2006-01-02 15:04:05"), r.ActorLabel, r.Action, r.ResourceType, r.ResourceID)
			}
		})
	}}
	list.Flags().IntVar(&limit, "limit", 50, "rows")
	verify := &cobra.Command{Use: "verify", Short: "Check the audit log hash chain", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		res, err := audit.Verify(ctx, a.DB.Q)
		if err != nil {
			return err
		}
		if err := out(res, func(w io.Writer) {
			if res.OK {
				fmt.Fprintf(w, "OK: %d rows, chain intact.\n", res.Rows)
			} else {
				fmt.Fprintf(w, "BROKEN at row %d: %s\n", res.BrokenAt, res.Reason)
			}
		}); err != nil {
			return err
		}
		if !res.OK {
			return errors.New("audit chain broken")
		}
		return nil
	}}
	cmd.AddCommand(list, verify)
	return cmd
}

func securityCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "security", Short: "Security self-audit (reports only)"}
	var failOn string
	var offline bool
	run := &cobra.Command{Use: "audit", Short: "Run every check now", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		if offline {
			a.Security.Client = nil
		}
		fs, _, err := a.Security.Run(ctx)
		if err != nil {
			return err
		}
		if err := out(fs, func(w io.Writer) {
			if len(fs) == 0 {
				fmt.Fprintln(w, "No findings.")
			}
			fmt.Fprintln(w, "SEVERITY\tTITLE\tFIX")
			for _, f := range fs {
				fmt.Fprintf(w, "%s\t%s\t%s\n", f.Severity, f.Title, f.Remediation)
			}
		}); err != nil {
			return err
		}
		for _, f := range fs {
			if failOn != "" && secaudit.AtLeast(f.Severity, secaudit.Severity(failOn)) {
				return fmt.Errorf("findings at or above %s", failOn)
			}
		}
		return nil
	}}
	run.Flags().StringVar(&failOn, "fail-on", "", "exit non-zero for findings at or above: low, medium, high, critical")
	run.Flags().BoolVar(&offline, "offline", false, "skip the online vulnerability lookup")
	status := &cobra.Command{Use: "status", Short: "Show open findings", RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		fs, err := secaudit.List(ctx, a.DB.Q, false)
		if err != nil {
			return err
		}
		return out(fs, func(w io.Writer) {
			fmt.Fprintln(w, "ID\tSEVERITY\tTITLE\tSINCE")
			for _, f := range fs {
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", f.ID, f.Severity, f.Title, f.FirstSeen.Format("2006-01-02"))
			}
		})
	}}
	var reason string
	var days int
	ack := &cobra.Command{Use: "ack <id>", Short: "Acknowledge a finding with a reason", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		a, ctx, err := openApp(cmd, false)
		if err != nil {
			return err
		}
		defer a.Close()
		var until *time.Time
		if days > 0 {
			t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
			until = &t
		}
		return secaudit.Ack(ctx, a.DB, args[0], reason, until)
	}}
	ack.Flags().StringVar(&reason, "reason", "", "why this is acceptable (required)")
	ack.Flags().IntVar(&days, "days", 30, "re-raise after this many days (0 = never)")
	cmd.AddCommand(run, status, ack)
	return cmd
}

func keysCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "keys", Short: "Encryption keys"}
	cmd.AddCommand(&cobra.Command{
		Use: "generate", Short: "Print a new key-encryption key (store it as a systemd credential)",
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Print(keys.Generate(1))
			return nil
		},
	}, &cobra.Command{
		Use: "status", Short: "Show loaded key versions",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			kr, err := keys.Load(cfg.Keys.KEKFile)
			if err != nil {
				return err
			}
			return out(map[string]any{"versions": kr.Versions(), "current": kr.Current()}, func(w io.Writer) {
				fmt.Fprintf(w, "Current key version %d (loaded: %v)\n", kr.Current(), kr.Versions())
			})
		},
	}, &cobra.Command{
		Use: "rotate", Short: "Re-encrypt every secret with the newest key version",
		Long: "Add a new line \"N:<hex>\" to the key file (cms keys generate), then run rotate. Keep old versions until rotate finishes.",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, ctx, err := openApp(cmd, false)
			if err != nil {
				return err
			}
			defer a.Close()
			n, err := a.RotateKeys(ctx)
			if err != nil {
				return err
			}
			return out(map[string]int{"rewrapped": n}, func(w io.Writer) { fmt.Fprintf(w, "Re-encrypted %d values.\n", n) })
		},
	})
	return cmd
}

func configCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Configuration"}
	cmd.AddCommand(&cobra.Command{
		Use: "validate", Short: "Validate the config file",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			info("Config is valid (env %s, %s).", cfg.Env, cfg.BaseURL)
			return nil
		},
	}, &cobra.Command{
		Use: "show", Short: "Print the effective config (secrets redacted)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig()
			if err != nil {
				return err
			}
			c := *cfg
			c.Database.URL = redactURL(c.Database.URL)
			c.Database.MigratorURL = redactURL(c.Database.MigratorURL)
			c.Mail.Password = ""
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(c)
		},
	})
	return cmd
}

func redactURL(u string) string {
	if i := strings.Index(u, "://"); i >= 0 {
		if j := strings.Index(u[i+3:], "@"); j >= 0 {
			cred := u[i+3 : i+3+j]
			if k := strings.Index(cred, ":"); k >= 0 {
				return u[:i+3] + cred[:k] + ":***" + u[i+3+j:]
			}
		}
	}
	return u
}

type (
	appT = app.App
	ctxT = context.Context
)

func richtextFromMarkdown(md string) any { return richtext.FromMarkdown(md) }

func auditParams(limit int32) dbq.ListAuditParams { return dbq.ListAuditParams{Lim: limit} }
