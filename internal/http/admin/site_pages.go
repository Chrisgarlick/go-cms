package admin

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"cms/internal/audit"
	"cms/internal/auth"
	"cms/internal/content"
	"cms/internal/db/dbq"
	"cms/internal/jobs"
	"cms/internal/menus"
	"cms/internal/rbac"
	"cms/internal/redirects"
	"cms/internal/secaudit"
	"cms/internal/settings"
	"cms/internal/site"
	"cms/internal/version"
)

func versionString() string { return version.Version }

// ---------------------------------------------------------------- terms

type termsData struct {
	Taxonomy *site.Taxonomy
	Terms    []dbq.ListTermsRow
	Edit     *dbq.Term
}

func (h *Handler) termsPage(w http.ResponseWriter, r *http.Request) {
	tx := h.a.Site().Taxonomies[r.PathValue("tax")]
	if tx == nil {
		h.error(w, r, http.StatusNotFound, "Unknown taxonomy.")
		return
	}
	terms, err := h.a.Content.Terms(r.Context(), tx.Name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d := termsData{Taxonomy: tx, Terms: terms}
	if id, err := uuid.Parse(r.URL.Query().Get("edit")); err == nil {
		if t, err := h.a.Content.Term(r.Context(), id); err == nil {
			d.Edit = &t
		}
	}
	h.render(w, r, "terms", http.StatusOK, h.page(r, tx.Label.Plural, "terms", d))
}

func (h *Handler) termSave(w http.ResponseWriter, r *http.Request) {
	tax := r.PathValue("tax")
	in := content.TermInput{Taxonomy: tax, Name: r.PostFormValue("name"), Slug: r.PostFormValue("slug"), Description: r.PostFormValue("description")}
	if pid, err := uuid.Parse(r.PostFormValue("parent_id")); err == nil {
		in.ParentID = &pid
	}
	var err error
	if id, perr := uuid.Parse(r.PostFormValue("id")); perr == nil {
		_, err = h.a.Content.UpdateTerm(r.Context(), id, in)
	} else {
		_, err = h.a.Content.CreateTerm(r.Context(), in)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/terms/"+tax+"/?ok=saved", http.StatusSeeOther)
}

func (h *Handler) termDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	var into *uuid.UUID
	if m, err := uuid.Parse(r.PostFormValue("merge_into")); err == nil {
		into = &m
	}
	if err := h.a.Content.DeleteTerm(r.Context(), id, into); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/terms/"+r.PathValue("tax")+"/?ok=deleted", http.StatusSeeOther)
}

// ---------------------------------------------------------------- menus

type menusData struct {
	Locations []menuLoc
}

type menuLoc struct {
	Name, Label string
	Count       int
}

func (h *Handler) menusPage(w http.ResponseWriter, r *http.Request) {
	var d menusData
	for _, name := range sortedKeys(h.a.Menus.Locations()) {
		m, _ := h.a.Menus.Get(r.Context(), name)
		n := 0
		if m != nil {
			n = len(m.Items)
		}
		d.Locations = append(d.Locations, menuLoc{Name: name, Label: h.a.Menus.Locations()[name], Count: n})
	}
	h.render(w, r, "menus", http.StatusOK, h.page(r, "Menus", "menus", d))
}

type menuEditData struct {
	Name, Label string
	Items       []menus.Item
	Entries     []editorOption
}

func (h *Handler) menuEdit(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	m, err := h.a.Menus.Get(r.Context(), name)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d := menuEditData{Name: name, Label: firstNonEmpty(h.a.Menus.Locations()[name], m.Label), Items: m.Items}
	if d.Items == nil {
		d.Items = []menus.Item{}
	}
	for _, t := range h.a.Site().TypeOrder {
		rows, _, _ := h.a.Content.List(r.Context(), content.ListFilter{Type: t, Status: "published", Limit: 200, Sort: "title"})
		for _, e := range rows {
			d.Entries = append(d.Entries, editorOption{ID: e.ID.String(), Label: h.a.Site().Types[t].Label.Singular + ": " + e.DraftTitle})
		}
	}
	h.render(w, r, "menu-edit", http.StatusOK, h.page(r, d.Label, "menus", d))
}

func (h *Handler) menuSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Label string       `json:"label"`
		Items []menus.Item `json:"items"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request."})
		return
	}
	if err := h.a.Menus.Save(r.Context(), r.PathValue("name"), body.Label, body.Items); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- redirects

type redirectsData struct {
	Rules    []dbq.Redirect
	NotFound []dbq.NotFoundLog
	Problems []string
	Prefill  string
}

func (h *Handler) redirectsPage(w http.ResponseWriter, r *http.Request) {
	rules, err := h.a.Redirects.List(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	nf, _ := h.a.Redirects.NotFound(r.Context(), 50)
	probs, _ := h.a.Redirects.Problems(r.Context())
	h.render(w, r, "redirects", http.StatusOK, h.page(r, "Redirects", "redirects",
		redirectsData{Rules: rules, NotFound: nf, Problems: probs, Prefill: r.URL.Query().Get("source")}))
}

func (h *Handler) redirectSave(w http.ResponseWriter, r *http.Request) {
	status, _ := strconv.Atoi(r.PostFormValue("status"))
	in := redirects.Input{Source: r.PostFormValue("source"), Target: r.PostFormValue("target"), Status: status,
		MatchType: r.PostFormValue("match_type"), PreserveQuery: r.PostFormValue("preserve_query") != "", Note: r.PostFormValue("note")}
	var err error
	if id, perr := uuid.Parse(r.PostFormValue("id")); perr == nil {
		_, err = h.a.Redirects.Update(r.Context(), id, in)
	} else {
		_, err = h.a.Redirects.Create(r.Context(), in)
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/redirects/?ok=saved", http.StatusSeeOther)
}

func (h *Handler) redirectDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	if err := h.a.Redirects.Delete(r.Context(), id); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/redirects/?ok=deleted", http.StatusSeeOther)
}

// ---------------------------------------------------------------- settings

type settingsData struct {
	Name, Tagline, TitleFormat, RobotsExtra string
	FrontPage, HomeType                     string
	Discourage                              bool
	Pages                                   []editorOption
	Types                                   []editorOption
}

func (h *Handler) settingsPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := h.a.Settings
	d := settingsData{Name: s.SiteName(ctx), Tagline: s.String(ctx, settings.SiteTagline, ""),
		TitleFormat: s.String(ctx, settings.TitleFormat, "{title} | {site}"), RobotsExtra: s.String(ctx, settings.RobotsExtra, ""),
		FrontPage: s.String(ctx, settings.FrontPage, ""), HomeType: s.String(ctx, settings.PostsType, ""), Discourage: s.Bool(ctx, settings.DiscourageBots)}
	for _, t := range h.a.Site().TypeOrder {
		ct := h.a.Site().Types[t]
		if ct.Archive != "" {
			d.Types = append(d.Types, editorOption{ID: t, Label: ct.Label.Plural})
		}
		if ct.Hierarchical {
			rows, _, _ := h.a.Content.List(ctx, content.ListFilter{Type: t, Status: "published", Limit: 200, Sort: "title"})
			for _, e := range rows {
				d.Pages = append(d.Pages, editorOption{ID: e.ID.String(), Label: e.DraftTitle})
			}
		}
	}
	h.render(w, r, "settings", http.StatusOK, h.page(r, "Settings", "settings", d))
}

func (h *Handler) settingsSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	set := func(k string, v any) error { return h.a.Settings.Set(ctx, k, v) }
	for _, kv := range []struct {
		k string
		v any
	}{
		{settings.SiteTitle, strings.TrimSpace(r.PostFormValue("name"))},
		{settings.SiteTagline, strings.TrimSpace(r.PostFormValue("tagline"))},
		{settings.TitleFormat, strings.TrimSpace(r.PostFormValue("title_format"))},
		{settings.RobotsExtra, strings.TrimSpace(r.PostFormValue("robots_extra"))},
		{settings.FrontPage, r.PostFormValue("front_page")},
		{settings.PostsType, r.PostFormValue("home_type")},
		{settings.DiscourageBots, r.PostFormValue("discourage") != ""},
	} {
		if err := set(kv.k, kv.v); err != nil {
			h.fail(w, r, err)
			return
		}
	}
	http.Redirect(w, r, "/admin/settings/?ok=saved", http.StatusSeeOther)
}

type optionsData struct {
	Page   *site.OptionsPage
	Groups []editorGroup
	Values json.RawMessage
	Media  map[string]mediaJSON
}

func (h *Handler) optionsPage(w http.ResponseWriter, r *http.Request) {
	op := h.a.Site().Options[r.PathValue("name")]
	if op == nil {
		h.error(w, r, http.StatusNotFound, "Unknown options page.")
		return
	}
	d := optionsData{Page: op, Values: h.a.Settings.Options(r.Context(), op.Name), Media: map[string]mediaJSON{}}
	for _, g := range op.Groups {
		d.Groups = append(d.Groups, editorGroup{Name: g.Name, Label: g.Label, Position: "main", Fields: g.Fields})
	}
	h.render(w, r, "options", http.StatusOK, h.page(r, op.Label, "options", d))
}

func (h *Handler) optionsSave(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Values json.RawMessage `json:"values"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "Invalid request."})
		return
	}
	if err := h.a.Settings.SaveOptions(r.Context(), r.PathValue("name"), body.Values); err != nil {
		h.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---------------------------------------------------------------- users

type usersData struct {
	Users     []dbq.User
	InviteURL string
	Me        uuid.UUID
}

func (h *Handler) usersPage(w http.ResponseWriter, r *http.Request) {
	us, err := h.a.Auth.Users(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, "users", http.StatusOK, h.page(r, "Users", "users", usersData{Users: us, Me: sessionFrom(r.Context()).UserID}))
}

func (h *Handler) userInvite(w http.ResponseWriter, r *http.Request) {
	role, err := rbac.Parse(r.PostFormValue("role"))
	if err != nil {
		h.error(w, r, http.StatusBadRequest, err.Error())
		return
	}
	_, link, err := h.a.Auth.Invite(r.Context(), auth.InviteInput{Email: r.PostFormValue("email"), Name: r.PostFormValue("name"), Role: role})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	us, _ := h.a.Auth.Users(r.Context())
	p := h.page(r, "Users", "users", usersData{Users: us, InviteURL: link, Me: sessionFrom(r.Context()).UserID})
	p.Flash = "Invitation created. It has been emailed; you can also copy the link below."
	h.render(w, r, "users", http.StatusOK, p)
}

func (h *Handler) userAction(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		h.error(w, r, http.StatusNotFound, "Not found.")
		return
	}
	ctx := r.Context()
	switch r.PathValue("action") {
	case "role":
		role, perr := rbac.Parse(r.PostFormValue("role"))
		if perr != nil {
			h.error(w, r, http.StatusBadRequest, perr.Error())
			return
		}
		err = h.a.Auth.SetRole(ctx, id, role)
	case "disable":
		err = h.a.Auth.SetStatus(ctx, id, "disabled")
	case "enable":
		err = h.a.Auth.SetStatus(ctx, id, "active")
	case "reset-2fa":
		err = h.a.Auth.Reset2FA(ctx, id)
	case "delete":
		err = h.a.Auth.DeleteUser(ctx, id)
	default:
		h.error(w, r, http.StatusNotFound, "Unknown action.")
		return
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/users/?ok=saved", http.StatusSeeOther)
}

// ---------------------------------------------------------------- account

type accountData struct {
	User      dbq.User
	Sessions  []dbq.Session
	Current   []byte
	Tokens    []dbq.ApiToken
	Recovery  int64
	Passkeys  []passkeyRow
	NewToken  string
	CanTokens bool
}

func (h *Handler) accountPage(w http.ResponseWriter, r *http.Request) {
	h.renderAccount(w, r, "")
}

func (h *Handler) renderAccount(w http.ResponseWriter, r *http.Request, newToken string) {
	ctx := r.Context()
	s := sessionFrom(ctx)
	u, err := h.a.Auth.User(ctx, s.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	d := accountData{User: u, Current: s.IDHash, NewToken: newToken, CanTokens: rbac.Has(s.Role, rbac.TokensOwn)}
	d.Sessions, _ = h.a.Auth.Sessions(ctx, s.UserID)
	d.Recovery, _ = h.a.Auth.RecoveryCodesLeft(ctx, s.UserID)
	if pks, err := h.a.Auth.Passkeys(ctx, s.UserID); err == nil {
		for _, pk := range pks {
			d.Passkeys = append(d.Passkeys, passkeyRow{ID: base64.RawURLEncoding.EncodeToString(pk.ID), Name: pk.Name, Created: pk.CreatedAt, LastUsed: pk.LastUsedAt})
		}
	}
	if d.CanTokens {
		all, _ := h.a.DB.Q.ListAPITokensForUser(ctx, s.UserID)
		d.Tokens = all
	}
	h.render(w, r, "account", http.StatusOK, h.page(r, "Your account", "account", d))
}

func (h *Handler) accountSave(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	s := sessionFrom(ctx)
	switch r.PostFormValue("action") {
	case "profile":
		if _, err := h.a.Auth.UpdateProfile(ctx, s.UserID, r.PostFormValue("name"), r.PostFormValue("bio"), ""); err != nil {
			h.fail(w, r, err)
			return
		}
	case "password":
		if err := h.a.Auth.ChangePassword(ctx, s.Session, r.PostFormValue("password")); err != nil {
			h.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/admin/account?ok=password", http.StatusSeeOther)
		return
	case "signout-others":
		if err := h.a.Auth.SignOutEverywhere(ctx, s.Session); err != nil {
			h.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/admin/account?ok=signedout", http.StatusSeeOther)
		return
	case "token":
		days, _ := strconv.Atoi(r.PostFormValue("days"))
		plain, _, err := h.a.Auth.CreateToken(ctx, r.PostFormValue("name"), s.Role, time.Duration(days)*24*time.Hour)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		h.renderAccount(w, r, plain)
		return
	case "revoke-token":
		id, err := uuid.Parse(r.PostFormValue("id"))
		if err == nil {
			err = h.a.Auth.RevokeToken(ctx, id)
		}
		if err != nil {
			h.fail(w, r, err)
			return
		}
		http.Redirect(w, r, "/admin/account?ok=revoked", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/admin/account?ok=saved", http.StatusSeeOther)
}

type passkeyRow struct {
	ID, Name string
	Created  time.Time
	LastUsed *time.Time
}

// ---------------------------------------------------------------- security

type securityData struct {
	Findings []dbq.SecurityFinding
	Full     bool
}

func (h *Handler) securityPage(w http.ResponseWriter, r *http.Request) {
	fs, err := secaudit.List(r.Context(), h.a.DB.Q, false)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, r, "security", http.StatusOK, h.page(r, "Security", "security",
		securityData{Findings: fs, Full: rbac.Has(sessionFrom(r.Context()).Role, rbac.SecurityView)}))
}

func (h *Handler) securityRun(w http.ResponseWriter, r *http.Request) {
	if _, _, err := h.a.Security.Run(r.Context()); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/security/?ok=audit", http.StatusSeeOther)
}

func (h *Handler) securityAck(w http.ResponseWriter, r *http.Request) {
	days, _ := strconv.Atoi(r.PostFormValue("days"))
	var until *time.Time
	if days > 0 {
		t := time.Now().Add(time.Duration(days) * 24 * time.Hour)
		until = &t
	}
	if err := secaudit.Ack(r.Context(), h.a.DB, r.PostFormValue("id"), r.PostFormValue("reason"), until); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/security/?ok=saved", http.StatusSeeOther)
}

// ---------------------------------------------------------------- system

type systemData struct {
	Version version.Info
	Audit   []dbq.AuditLog
	Chain   audit.VerifyResult
	Uptime  string
	Jobs    []jobRow
	Theme   string
	Bundle  string
}

type jobRow struct {
	Kind, State string
	Count       int64
}

func (h *Handler) systemPage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	d := systemData{Version: version.Get(), Uptime: h.a.Uptime().Round(time.Second).String(), Theme: h.a.Renderer.ThemeHash(), Bundle: h.a.Site().Hash}
	if rbac.Has(sessionFrom(ctx).Role, rbac.AuditView) {
		d.Audit, _ = h.a.DB.Q.ListAudit(ctx, dbq.ListAuditParams{Lim: 50})
		d.Chain, _ = audit.Verify(ctx, h.a.DB.Q)
	}
	rows, err := h.a.DB.Pool.Query(ctx, `SELECT kind, state::text, count(*) FROM river_job
		WHERE created_at > now() - interval '7 days' GROUP BY kind, state ORDER BY kind, state`)
	if err == nil {
		for rows.Next() {
			var j jobRow
			if rows.Scan(&j.Kind, &j.State, &j.Count) == nil {
				d.Jobs = append(d.Jobs, j)
			}
		}
		rows.Close()
	}
	h.render(w, r, "system", http.StatusOK, h.page(r, "System", "system", d))
}

func (h *Handler) systemRebuild(w http.ResponseWriter, r *http.Request) {
	if err := h.a.Jobs.Enqueue(r.Context(), jobs.RenderRebuild{Reason: "requested from the admin"}); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/system/?ok=rebuild", http.StatusSeeOther)
}
