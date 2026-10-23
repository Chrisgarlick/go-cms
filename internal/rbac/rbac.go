// Package rbac defines the three fixed roles and their capabilities (spec 4.7).
// Roles are code, not data: there is no custom role editor.
package rbac

import "fmt"

type Role string

const (
	Owner  Role = "owner"
	Admin  Role = "admin"
	Editor Role = "editor"
)

type Cap string

const (
	ContentRead     Cap = "content.read"
	ContentEdit     Cap = "content.edit"
	ContentPublish  Cap = "content.publish"
	ContentDelete   Cap = "content.delete"
	MediaManage     Cap = "media.manage"
	MenusManage     Cap = "menus.manage"
	TermsManage     Cap = "terms.manage"
	RedirectManage  Cap = "redirects.manage"
	SettingsManage  Cap = "settings.manage"
	UsersManage     Cap = "users.manage"  // admins and editors
	OwnersManage    Cap = "owners.manage" // owner accounts
	TokensOwn       Cap = "tokens.own"    // create/revoke own API tokens
	TokensAll       Cap = "tokens.all"
	WebhooksManage  Cap = "webhooks.manage"
	RawHTML         Cap = "content.raw_html"
	SecurityView    Cap = "security.view" // full security page
	SecuritySummary Cap = "security.summary"
	SecurityManage  Cap = "security.manage" // ack findings
	AuditView       Cap = "audit.view"
	BackupManage    Cap = "backup.manage"
	KeysManage      Cap = "keys.manage"
	SystemView      Cap = "system.view" // cache, jobs, health (read)
	SystemManage    Cap = "system.manage"
	SchemaManage    Cap = "schema.manage" // apply schema (CLI/system only in production)
	FormsView       Cap = "forms.view"    // read and delete form submissions (personal data)
)

var editorCaps = []Cap{ContentRead, ContentEdit, ContentPublish, ContentDelete, MediaManage, MenusManage, TermsManage}

var adminCaps = append(append([]Cap{}, editorCaps...),
	RedirectManage, SettingsManage, UsersManage, TokensOwn, WebhooksManage, SecuritySummary, SystemView, FormsView)

var ownerCaps = append(append([]Cap{}, adminCaps...),
	OwnersManage, TokensAll, RawHTML, SecurityView, SecurityManage, AuditView, BackupManage, KeysManage, SystemManage, SchemaManage)

var table = map[Role]map[Cap]bool{
	Editor: set(editorCaps),
	Admin:  set(adminCaps),
	Owner:  set(ownerCaps),
}

func set(cs []Cap) map[Cap]bool {
	m := make(map[Cap]bool, len(cs))
	for _, c := range cs {
		m[c] = true
	}
	return m
}

// Has reports whether role grants cap. Unknown roles grant nothing.
func Has(r Role, c Cap) bool { return table[r][c] }

// Caps returns every capability of a role.
func Caps(r Role) []Cap {
	switch r {
	case Owner:
		return ownerCaps
	case Admin:
		return adminCaps
	case Editor:
		return editorCaps
	}
	return nil
}

// AllCaps lists every capability, for the authorisation matrix tests.
func AllCaps() []Cap { return ownerCaps }

func Parse(s string) (Role, error) {
	switch Role(s) {
	case Owner, Admin, Editor:
		return Role(s), nil
	}
	return "", fmt.Errorf("unknown role %q (owner, admin, editor)", s)
}

// Rank orders roles for "may manage" checks.
func Rank(r Role) int {
	switch r {
	case Owner:
		return 3
	case Admin:
		return 2
	case Editor:
		return 1
	}
	return 0
}

// CanAssign reports whether a user with role actor may give role target to someone.
func CanAssign(actor, target Role) bool {
	if target == Owner {
		return Has(actor, OwnersManage)
	}
	return Has(actor, UsersManage)
}
