// Package secaudit is the self-audit (spec 4.20). It reports only; it never
// changes code or configuration. CI's govulncheck stays the authority.
package secaudit

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/config"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/netx"
	"cms/internal/rbac"
)

type Severity string

const (
	Info     Severity = "info"
	Low      Severity = "low"
	Medium   Severity = "medium"
	High     Severity = "high"
	Critical Severity = "critical"
)

var rank = map[Severity]int{Info: 0, Low: 1, Medium: 2, High: 3, Critical: 4}

// AtLeast reports whether s is at or above min.
func AtLeast(s, min Severity) bool { return rank[s] >= rank[min] }

type Finding struct {
	ID          string   `json:"id"`
	Category    string   `json:"category"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	Remediation string   `json:"remediation"`
}

type Auditor struct {
	DB     *db.DB
	Cfg    *config.Config
	Client *http.Client // hardened; nil disables the online vulnerability lookup
	// Offline is a downloaded OSV snapshot (cms security db import), JSON list of vulns.
	OfflineDB string
}

// Run executes every check, stores findings, resolves those no longer seen,
// and returns the current list plus ids of findings that are new.
func (a *Auditor) Run(ctx context.Context) ([]Finding, []string, error) {
	var all []Finding
	cats := []string{"config", "access", "database"}
	all = append(all, a.configChecks()...)
	acc, err := a.accessChecks(ctx)
	if err != nil {
		return nil, nil, err
	}
	all = append(all, acc...)
	dbf, err := a.databaseChecks(ctx)
	if err != nil {
		return nil, nil, err
	}
	all = append(all, dbf...)
	if a.Client != nil || a.OfflineDB != "" {
		v, err := a.vulnChecks(ctx)
		if err == nil {
			cats = append(cats, "dependency")
			all = append(all, v...)
		} else {
			all = append(all, Finding{ID: "vuln:lookup-failed", Category: "config", Severity: Low,
				Title: "Dependency vulnerability lookup failed", Detail: err.Error(),
				Remediation: "Check outbound HTTPS to api.osv.dev, or import an offline snapshot with `cms security db import`."})
		}
	}
	var fresh []string
	ids := make([]string, 0, len(all))
	err = a.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		for _, f := range all {
			ids = append(ids, f.ID)
			inserted, err := q.UpsertFinding(ctx, dbq.UpsertFindingParams{ID: f.ID, Category: f.Category, Severity: string(f.Severity),
				Title: f.Title, Detail: f.Detail, Remediation: f.Remediation})
			if err != nil {
				return err
			}
			if inserted {
				fresh = append(fresh, f.ID)
			}
		}
		if _, err := q.ResolveFindingsNotIn(ctx, dbq.ResolveFindingsNotInParams{Categories: cats, Ids: ids}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "security.audit", ResourceType: "security", ResourceID: "run",
			Diff: map[string]any{"findings": len(all), "new": len(fresh)}})
	})
	return all, fresh, err
}

func (a *Auditor) configChecks() []Finding {
	var out []Finding
	c := a.Cfg
	if c.Env == config.EnvProduction && !strings.HasPrefix(c.BaseURL, "https://") {
		out = append(out, Finding{"config:https", "config", Critical, "Site is not served over HTTPS", c.BaseURL,
			"Set base_url to an https:// address; Caddy obtains certificates automatically."})
	}
	if c.Env != config.EnvProduction {
		out = append(out, Finding{"config:env", "config", Medium, "Not running in production mode", "env = " + string(c.Env),
			"Set env: production on live servers."})
	}
	if c.Keys.KEKFile != "" {
		if st, err := os.Stat(c.Keys.KEKFile); err == nil && st.Mode().Perm()&0o077 != 0 {
			out = append(out, Finding{"config:kek-perms", "config", High, "Key file is readable by other users",
				fmt.Sprintf("%s has mode %o", c.Keys.KEKFile, st.Mode().Perm()), "chmod 0400 the key file, or load it with systemd LoadCredential."})
		}
	}
	for _, p := range c.HTTP.TrustedProxies {
		if p == "0.0.0.0/0" || p == "::/0" {
			out = append(out, Finding{"config:trusted-proxies", "config", High, "Every address is trusted as a proxy",
				"Clients can spoof their IP, defeating rate limits and audit IPs.", "Limit http.trusted_proxies to Caddy's address (127.0.0.1)."})
		}
	}
	if c.MediaURL == "" && c.Env == config.EnvProduction {
		out = append(out, Finding{"config:media-origin", "config", Low, "Media is served from the admin origin",
			"Uploaded files share the admin's origin.", "Set media_url to a separate host (e.g. https://media.example.com)."})
	}
	if c.Mail.SMTPAddr == "" && c.Env == config.EnvProduction {
		out = append(out, Finding{"config:mail", "config", Medium, "Email is not configured",
			"Password resets, invites and security alerts cannot be delivered.", "Set mail.smtp_addr and mail.from."})
	}
	if st, err := os.Stat(c.Paths.Data); err == nil && st.Mode().Perm()&0o002 != 0 {
		out = append(out, Finding{"config:data-perms", "config", High, "Data directory is world-writable", c.Paths.Data, "chmod o-w the data directory."})
	}
	return out
}

func (a *Auditor) accessChecks(ctx context.Context) ([]Finding, error) {
	var out []Finding
	users, err := a.DB.Q.UsersWithout2FA(ctx)
	if err != nil {
		return nil, err
	}
	if a.Cfg.Security.DevSkip2FA && a.Cfg.Dev() {
		users = nil // `cms dev` deliberately signs in without 2FA
	}
	for _, u := range users {
		sev := Medium
		if u.Role == string(rbac.Owner) || u.Role == string(rbac.Admin) {
			sev = High
		}
		out = append(out, Finding{"access:no-2fa:" + u.ID.String(), "access", sev, "An active account has not set up two-factor authentication",
			"User " + u.ID.String() + " (" + u.Role + ")", "They are asked to enrol at next sign-in; or reset their 2FA from Users."})
	}
	stale, err := a.DB.Q.StaleTokens(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range stale {
		out = append(out, Finding{"access:stale-token:" + t.ID.String(), "access", Low, "An API token has not been used for 90 days",
			"Token " + t.Name, "Revoke tokens that are no longer needed."})
	}
	n, err := a.DB.Q.CountOwners(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		out = append(out, Finding{"access:no-owner", "access", Critical, "The site has no active owner", "", "Create one with `cms user create --role owner`."})
	}
	return out, nil
}

func (a *Auditor) databaseChecks(ctx context.Context) ([]Finding, error) {
	var out []Finding
	var super, createPublic, auditUpdate, auditDelete bool
	err := a.DB.Pool.QueryRow(ctx, `SELECT r.rolsuper,
		has_schema_privilege(current_user, 'public', 'CREATE'),
		has_table_privilege(current_user, 'audit_log', 'UPDATE'),
		has_table_privilege(current_user, 'audit_log', 'DELETE')
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&super, &createPublic, &auditUpdate, &auditDelete)
	if err != nil {
		return nil, err
	}
	if super {
		out = append(out, Finding{"database:superuser", "database", Critical, "The application connects as a database superuser", "",
			"Use the least-privileged cms_app role (cms install creates it)."})
	}
	if createPublic {
		out = append(out, Finding{"database:create-public", "database", High, "The application role can create tables", "",
			"REVOKE CREATE ON SCHEMA public FROM the app role; migrations run as cms_migrator."})
	}
	if auditUpdate || auditDelete {
		out = append(out, Finding{"database:audit-writable", "database", High, "The audit log can be modified by the application", "",
			"Run `cms migrate up` as the migrator to re-apply grants."})
	}
	res, err := audit.Verify(ctx, a.DB.Q)
	if err != nil {
		return nil, err
	}
	if !res.OK {
		out = append(out, Finding{"database:audit-chain", "database", Critical, "The audit log hash chain is broken",
			fmt.Sprintf("Row %d: %s", res.BrokenAt, res.Reason), "Investigate who had database access; restore from backup if needed."})
	}
	return out, nil
}

// Modules lists the modules compiled into this binary, plus the Go stdlib.
func Modules() map[string]string {
	mods := map[string]string{"stdlib": strings.TrimPrefix(runtime.Version(), "go")}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, m := range bi.Deps {
			if m.Replace != nil {
				m = m.Replace
			}
			if m.Version != "" && m.Version != "(devel)" {
				mods[m.Path] = m.Version
			}
		}
	}
	return mods
}

type osvQuery struct {
	Package struct {
		Name      string `json:"name"`
		Ecosystem string `json:"ecosystem"`
	} `json:"package"`
	Version string `json:"version"`
}

type osvVuln struct {
	ID       string   `json:"id"`
	Summary  string   `json:"summary"`
	Aliases  []string `json:"aliases"`
	Severity []struct {
		Type  string `json:"type"`
		Score string `json:"score"`
	} `json:"severity"`
	Affected []struct {
		Package struct {
			Name string `json:"name"`
		} `json:"package"`
		Ranges []struct {
			Events []map[string]string `json:"events"`
		} `json:"ranges"`
	} `json:"affected"`
	DatabaseSpecific map[string]any `json:"database_specific"`
}

// vulnChecks queries OSV for every module (one batch request).
func (a *Auditor) vulnChecks(ctx context.Context) ([]Finding, error) {
	mods := Modules()
	names := make([]string, 0, len(mods))
	var queries []osvQuery
	for name, ver := range mods {
		var q osvQuery
		q.Package.Name, q.Package.Ecosystem, q.Version = name, "Go", strings.TrimPrefix(ver, "v")
		if name != "stdlib" {
			q.Version = ver
		}
		queries = append(queries, q)
		names = append(names, name)
	}
	if a.Client == nil {
		return nil, fmt.Errorf("offline database import is not loaded")
	}
	body, _ := json.Marshal(map[string]any{"queries": queries})
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.osv.dev/v1/querybatch", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := a.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := netx.ReadLimited(resp.Body, 8<<20)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("osv: status %d", resp.StatusCode)
	}
	var br struct {
		Results []struct {
			Vulns []struct {
				ID string `json:"id"`
			} `json:"vulns"`
		} `json:"results"`
	}
	if err := json.Unmarshal(data, &br); err != nil {
		return nil, err
	}
	var out []Finding
	for i, r := range br.Results {
		for _, v := range r.Vulns {
			mod := names[i]
			f := Finding{ID: "vuln:" + v.ID + ":" + mod, Category: "dependency", Severity: High,
				Title:       fmt.Sprintf("%s affects %s %s", v.ID, mod, mods[mod]),
				Remediation: "Upgrade to a release built with the fixed version (Renovate opens the PR; `cms upgrade` installs it)."}
			if detail, err := a.vulnDetail(ctx, v.ID); err == nil {
				f.Detail = detail.Summary
				if len(detail.Aliases) > 0 {
					f.Detail += " (" + strings.Join(detail.Aliases, ", ") + ")"
				}
				if fixed := fixedVersion(detail, mod); fixed != "" {
					f.Remediation = "Fixed in " + mod + " " + fixed + ". " + f.Remediation
				}
			}
			out = append(out, f)
		}
	}
	return out, nil
}

func (a *Auditor) vulnDetail(ctx context.Context, id string) (*osvVuln, error) {
	b, code, err := netx.Get(ctx, a.Client, "https://api.osv.dev/v1/vulns/"+id, 2<<20)
	if err != nil || code != 200 {
		return nil, fmt.Errorf("osv detail: %v %d", err, code)
	}
	var v osvVuln
	return &v, json.Unmarshal(b, &v)
}

func fixedVersion(v *osvVuln, mod string) string {
	for _, af := range v.Affected {
		if af.Package.Name != mod {
			continue
		}
		for _, r := range af.Ranges {
			for _, ev := range r.Events {
				if f := ev["fixed"]; f != "" {
					return f
				}
			}
		}
	}
	return ""
}

// List returns stored findings (security page / CLI).
func List(ctx context.Context, q *dbq.Queries, includeResolved bool) ([]dbq.SecurityFinding, error) {
	if _, err := actor.Require(ctx, rbac.SecuritySummary); err != nil {
		return nil, err
	}
	return q.ListFindings(ctx, includeResolved)
}

// Ack acknowledges a finding with a reason and optional expiry.
func Ack(ctx context.Context, d *db.DB, id, reason string, until *time.Time) error {
	a, err := actor.Require(ctx, rbac.SecurityManage)
	if err != nil {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("give a reason for acknowledging the finding")
	}
	return d.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var by *uuid.UUID
		if a.IsUser() {
			u := a.UserID
			by = &u
		}
		if err := q.AckFinding(ctx, dbq.AckFindingParams{ID: id, AckBy: by, AckReason: reason, AckUntil: until}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "security.ack", ResourceType: "finding", ResourceID: id,
			Diff: map[string]any{"reason": reason}})
	})
}
