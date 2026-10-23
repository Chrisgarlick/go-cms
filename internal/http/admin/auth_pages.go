package admin

import (
	"encoding/base64"
	"errors"
	"html/template"
	"net/http"
	"strings"
	"time"

	"rsc.io/qr"

	"cms/internal/auth"
	"cms/internal/web"
)

func (h *Handler) loginPage(w http.ResponseWriter, r *http.Request) {
	if s := sessionFrom(r.Context()); s != nil && s.Stage == auth.StageFull {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	p := h.page(r, "Sign in", "auth", map[string]string{"Next": safeNext(r.URL.Query().Get("next")), "Email": ""})
	h.render(w, r, "login", http.StatusOK, p)
}

func (h *Handler) login(w http.ResponseWriter, r *http.Request) {
	email := r.PostFormValue("email")
	next := safeNext(r.PostFormValue("next"))
	res, err := h.a.Auth.Login(r.Context(), email, r.PostFormValue("password"), web.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		p := h.page(r, "Sign in", "auth", map[string]string{"Next": next, "Email": email})
		p.Error = err.Error()
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrRateLimited) {
			status = http.StatusTooManyRequests
		}
		h.render(w, r, "login", status, p)
		return
	}
	h.setSessionCookie(w, res.Token, 15*time.Minute)
	switch res.Stage {
	case auth.StageMFA:
		http.Redirect(w, r, "/admin/login/2fa?next="+urlEscape(next), http.StatusSeeOther)
	case auth.StageEnroll:
		http.Redirect(w, r, "/admin/enroll", http.StatusSeeOther)
	default:
		h.setSessionCookie(w, res.Token, h.a.Auth.SessionTTL)
		http.Redirect(w, r, next, http.StatusSeeOther)
	}
}

func (h *Handler) mfaPage(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	if s == nil || s.Stage != auth.StageMFA {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	p := h.page(r, "Two-factor authentication", "auth", map[string]any{"Next": safeNext(r.URL.Query().Get("next")), "Recovery": r.URL.Query().Has("recovery"),
		"Passkeys": h.a.Auth.HasPasskeys(r.Context(), s.UserID)})
	h.render(w, r, "mfa", http.StatusOK, p)
}

func (h *Handler) mfa(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	if s == nil || s.Stage != auth.StageMFA {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	next := safeNext(r.PostFormValue("next"))
	var tok string
	var err error
	ip := web.ClientIP(r.Context())
	recovery := r.PostFormValue("recovery") != ""
	if recovery {
		tok, err = h.a.Auth.UseRecoveryCode(r.Context(), s.Session, r.PostFormValue("recovery"), ip, r.UserAgent())
	} else {
		tok, err = h.a.Auth.VerifyTOTP(r.Context(), s.Session, r.PostFormValue("code"), ip, r.UserAgent())
	}
	if err != nil {
		p := h.page(r, "Two-factor authentication", "auth", map[string]any{"Next": next, "Recovery": recovery,
			"Passkeys": h.a.Auth.HasPasskeys(r.Context(), s.UserID)})
		p.Error = err.Error()
		h.render(w, r, "mfa", http.StatusUnauthorized, p)
		return
	}
	h.setSessionCookie(w, tok, h.a.Auth.SessionTTL)
	http.Redirect(w, r, next, http.StatusSeeOther)
}

type enrollData struct {
	Secret   string
	QR       template.URL
	Recovery []string
	Required bool
}

func (h *Handler) enrollPage(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	if s == nil || (s.Stage != auth.StageEnroll && s.Stage != auth.StageFull) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	if s.Stage == auth.StageFull && !s.Sudo() {
		http.Redirect(w, r, "/admin/sudo?next=/admin/enroll", http.StatusSeeOther)
		return
	}
	en, err := h.a.Auth.BeginEnroll(r.Context(), s.UserID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	data := enrollData{Secret: en.Secret, QR: qrDataURL(en.URL), Required: s.Stage == auth.StageEnroll}
	h.render(w, r, "enroll", http.StatusOK, h.page(r, "Set up two-factor authentication", "auth", data))
}

func (h *Handler) enroll(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	if s == nil || (s.Stage != auth.StageEnroll && s.Stage != auth.StageFull) {
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
		return
	}
	tok, codes, err := h.a.Auth.ConfirmEnroll(r.Context(), s.Session, r.PostFormValue("code"), web.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		p := h.page(r, "Set up two-factor authentication", "auth", enrollData{Secret: r.PostFormValue("secret"),
			QR: template.URL(r.PostFormValue("qr")), Required: s.Stage == auth.StageEnroll})
		if !strings.HasPrefix(r.PostFormValue("qr"), "data:image/png;base64,") {
			p.Data = enrollData{Secret: r.PostFormValue("secret"), Required: s.Stage == auth.StageEnroll}
		}
		p.Error = err.Error()
		h.render(w, r, "enroll", http.StatusUnprocessableEntity, p)
		return
	}
	if tok != "" {
		h.setSessionCookie(w, tok, h.a.Auth.SessionTTL)
	}
	// Show recovery codes once.
	p := h.page(r, "Save your recovery codes", "auth", enrollData{Recovery: codes})
	if tok != "" {
		p.CSRF = "" // the new session has its own token; the next page loads it
	}
	h.render(w, r, "recovery", http.StatusOK, p)
}

func qrDataURL(content string) template.URL {
	code, err := qr.Encode(content, qr.M)
	if err != nil {
		return ""
	}
	code.Scale = 6
	return template.URL("data:image/png;base64," + base64.StdEncoding.EncodeToString(code.PNG()))
}

func (h *Handler) logout(w http.ResponseWriter, r *http.Request) {
	if s := sessionFrom(r.Context()); s != nil {
		h.a.Auth.Logout(r.Context(), s.Token)
	}
	h.clearSessionCookie(w)
	http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
}

func (h *Handler) sudoPage(w http.ResponseWriter, r *http.Request) {
	p := h.page(r, "Confirm it's you", "auth", map[string]string{"Next": safeNext(r.URL.Query().Get("next"))})
	h.render(w, r, "sudo", http.StatusOK, p)
}

func (h *Handler) sudo(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	next := safeNext(r.PostFormValue("next"))
	if err := h.a.Auth.Sudo(r.Context(), s.Session, r.PostFormValue("password"), r.PostFormValue("code")); err != nil {
		p := h.page(r, "Confirm it's you", "auth", map[string]string{"Next": next})
		p.Error = "That didn't match. Enter a code from your authenticator app, or your password."
		h.render(w, r, "sudo", http.StatusUnauthorized, p)
		return
	}
	http.Redirect(w, r, next, http.StatusSeeOther)
}

// Password reset: the browser that asks must be the browser that completes it.
const resetCookie = "cms_reset_nonce"

func (h *Handler) resetRequestPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "reset-request", http.StatusOK, h.page(r, "Reset your password", "auth", nil))
}

func (h *Handler) resetRequest(w http.ResponseWriter, r *http.Request) {
	nonce := auth.RandomToken(24)
	http.SetCookie(w, &http.Cookie{Name: resetCookie, Value: nonce, Path: "/admin/reset", HttpOnly: true, Secure: h.secure,
		SameSite: http.SameSiteLaxMode, MaxAge: 1800})
	if err := h.a.Auth.RequestReset(r.Context(), r.PostFormValue("email"), nonce, web.ClientIP(r.Context())); err != nil {
		p := h.page(r, "Reset your password", "auth", nil)
		p.Error = err.Error()
		h.render(w, r, "reset-request", http.StatusTooManyRequests, p)
		return
	}
	http.Redirect(w, r, "/admin/login?ok=reset-sent", http.StatusSeeOther)
}

func (h *Handler) resetPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, "reset", http.StatusOK, h.page(r, "Choose a new password", "auth", map[string]string{"Token": r.PathValue("token")}))
}

func (h *Handler) reset(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	nonce := ""
	if c, err := r.Cookie(resetCookie); err == nil {
		nonce = c.Value
	}
	if err := h.a.Auth.CompleteReset(r.Context(), tok, nonce, r.PostFormValue("password")); err != nil {
		p := h.page(r, "Choose a new password", "auth", map[string]string{"Token": tok})
		p.Error = err.Error()
		h.render(w, r, "reset", http.StatusUnprocessableEntity, p)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: resetCookie, Value: "", Path: "/admin/reset", MaxAge: -1})
	http.Redirect(w, r, "/admin/login?ok=reset-done", http.StatusSeeOther)
}

func (h *Handler) invitePage(w http.ResponseWriter, r *http.Request) {
	u, err := h.a.Auth.InviteUser(r.Context(), r.PathValue("token"))
	if err != nil {
		h.error(w, r, http.StatusGone, "This invitation is invalid or has expired. Ask for a new one.")
		return
	}
	h.render(w, r, "invite", http.StatusOK, h.page(r, "Accept your invitation", "auth", map[string]string{"Token": r.PathValue("token"),
		"Email": u.Email, "Name": u.DisplayName}))
}

func (h *Handler) invite(w http.ResponseWriter, r *http.Request) {
	tok := r.PathValue("token")
	sess, err := h.a.Auth.AcceptInvite(r.Context(), tok, r.PostFormValue("password"), r.PostFormValue("name"),
		web.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		u, _ := h.a.Auth.InviteUser(r.Context(), tok)
		p := h.page(r, "Accept your invitation", "auth", map[string]string{"Token": tok, "Email": u.Email, "Name": r.PostFormValue("name")})
		p.Error = err.Error()
		h.render(w, r, "invite", http.StatusUnprocessableEntity, p)
		return
	}
	h.setSessionCookie(w, sess, 15*time.Minute)
	http.Redirect(w, r, "/admin/enroll", http.StatusSeeOther)
}

func urlEscape(s string) string {
	return strings.NewReplacer("%", "%25", "&", "%26", "#", "%23", "?", "%3F", " ", "%20").Replace(s)
}
