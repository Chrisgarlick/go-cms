package admin

import (
	"net/http"

	"cms/internal/rbac"
)

func (h *Handler) routes() {
	m := h.mux
	a := h.authed
	m.Handle("GET /admin/assets/", h.assetsHandler())

	// Sign-in flows (no full session needed).
	m.HandleFunc("GET /admin/login", h.loginPage)
	m.HandleFunc("POST /admin/login", h.login)
	m.HandleFunc("GET /admin/login/2fa", h.mfaPage)
	m.HandleFunc("POST /admin/login/2fa", h.mfa)
	m.HandleFunc("GET /admin/enroll", h.enrollPage)
	m.HandleFunc("POST /admin/enroll", h.enroll)
	m.HandleFunc("POST /admin/logout", h.logout)
	m.HandleFunc("GET /admin/reset", h.resetRequestPage)
	m.HandleFunc("POST /admin/reset", h.resetRequest)
	m.HandleFunc("GET /admin/reset/{token}", h.resetPage)
	m.HandleFunc("POST /admin/reset/{token}", h.reset)
	m.HandleFunc("GET /admin/invite/{token}", h.invitePage)
	m.HandleFunc("POST /admin/invite/{token}", h.invite)
	m.HandleFunc("POST /admin/login/passkey/begin", h.passkeyLoginBegin)
	m.HandleFunc("POST /admin/login/passkey/finish", h.passkeyLoginFinish)
	m.HandleFunc("POST /admin/api/passkeys/begin", a("", h.passkeyRegisterBegin))
	m.HandleFunc("POST /admin/api/passkeys/finish", a("", h.passkeyRegisterFinish))
	m.HandleFunc("POST /admin/account/passkeys/delete", a("", h.passkeyDelete))
	m.HandleFunc("GET /admin/sudo", a("", h.sudoPage))
	m.HandleFunc("POST /admin/sudo", a("", h.sudo))

	m.HandleFunc("GET /admin/{$}", a(rbac.ContentRead, h.dashboard))

	// Content.
	m.HandleFunc("GET /admin/content/{type}/", a(rbac.ContentRead, h.contentList))
	m.HandleFunc("POST /admin/content/{type}/new", a(rbac.ContentEdit, h.contentNew))
	m.HandleFunc("GET /admin/entries/{id}", a(rbac.ContentRead, h.editor))
	m.HandleFunc("POST /admin/entries/{id}/{action}", a(rbac.ContentEdit, h.entryFormAction))
	m.HandleFunc("POST /admin/api/entries/{id}/save", a(rbac.ContentEdit, h.entrySave))
	m.HandleFunc("POST /admin/api/entries/{id}/action/{action}", a(rbac.ContentEdit, h.entryAction))
	m.HandleFunc("GET /admin/api/entries/{id}/revisions", a(rbac.ContentRead, h.revisions))
	m.HandleFunc("POST /admin/api/entries/{id}/revisions/{rid}/restore", a(rbac.ContentEdit, h.revisionRestore))
	m.HandleFunc("GET /admin/api/entries/{id}/preview", a(rbac.ContentRead, h.previewLink))
	m.HandleFunc("GET /admin/api/entries", a(rbac.ContentRead, h.entryPicker))
	m.HandleFunc("POST /admin/api/terms", a(rbac.TermsManage, h.termCreateJSON))

	// Media.
	m.HandleFunc("GET /admin/media/{$}", a(rbac.ContentRead, h.mediaList))
	m.HandleFunc("GET /admin/media/{id}", a(rbac.ContentRead, h.mediaEdit))
	m.HandleFunc("POST /admin/media/{id}", a(rbac.MediaManage, h.mediaUpdate))
	m.HandleFunc("POST /admin/media/{id}/delete", a(rbac.MediaManage, h.mediaDelete))
	m.HandleFunc("GET /admin/api/media", a(rbac.ContentRead, h.mediaJSONList))
	m.HandleFunc("POST /admin/api/media", a(rbac.MediaManage, h.mediaUpload))

	// Organise.
	m.HandleFunc("GET /admin/terms/{tax}/", a(rbac.ContentRead, h.termsPage))
	m.HandleFunc("POST /admin/terms/{tax}/", a(rbac.TermsManage, h.termSave))
	m.HandleFunc("POST /admin/terms/{tax}/{id}/delete", a(rbac.TermsManage, h.termDelete))
	m.HandleFunc("GET /admin/menus/{$}", a(rbac.ContentRead, h.menusPage))
	m.HandleFunc("GET /admin/menus/{name}", a(rbac.MenusManage, h.menuEdit))
	m.HandleFunc("POST /admin/api/menus/{name}", a(rbac.MenusManage, h.menuSave))
	m.HandleFunc("GET /admin/options/{name}", a(rbac.SettingsManage, h.optionsPage))
	m.HandleFunc("POST /admin/api/options/{name}", a(rbac.SettingsManage, h.optionsSave))

	// Form submissions.
	m.HandleFunc("GET /admin/forms/{$}", a(rbac.FormsView, h.formsPage))
	m.HandleFunc("GET /admin/forms/{form}/{$}", a(rbac.FormsView, h.formSubmissions))
	m.HandleFunc("GET /admin/forms/{form}/{id}", a(rbac.FormsView, h.formSubmission))
	m.HandleFunc("POST /admin/forms/{form}/{id}/delete", a(rbac.FormsView, h.formSubmissionDelete))

	// Site.
	m.HandleFunc("GET /admin/redirects/{$}", a(rbac.RedirectManage, h.redirectsPage))
	m.HandleFunc("POST /admin/redirects/{$}", a(rbac.RedirectManage, h.redirectSave))
	m.HandleFunc("POST /admin/redirects/{id}/delete", a(rbac.RedirectManage, h.redirectDelete))
	m.HandleFunc("GET /admin/settings/{$}", a(rbac.SettingsManage, h.settingsPage))
	m.HandleFunc("POST /admin/settings/{$}", a(rbac.SettingsManage, h.settingsSave))
	m.HandleFunc("GET /admin/users/{$}", a(rbac.UsersManage, h.usersPage))
	m.HandleFunc("POST /admin/users/invite", a(rbac.UsersManage, h.userInvite))
	m.HandleFunc("POST /admin/users/{id}/{action}", a(rbac.UsersManage, h.userAction))
	m.HandleFunc("GET /admin/account", a("", h.accountPage))
	m.HandleFunc("POST /admin/account", a("", h.accountSave))
	m.HandleFunc("GET /admin/security/{$}", a(rbac.SecuritySummary, h.securityPage))
	m.HandleFunc("POST /admin/security/run", a(rbac.SecurityManage, h.securityRun))
	m.HandleFunc("POST /admin/security/ack", a(rbac.SecurityManage, h.securityAck))
	m.HandleFunc("GET /admin/system/{$}", a(rbac.SystemView, h.systemPage))
	m.HandleFunc("POST /admin/system/rebuild", a(rbac.SystemManage, h.systemRebuild))

	m.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		h.error(w, r, http.StatusNotFound, "That page doesn't exist.")
	})
}
