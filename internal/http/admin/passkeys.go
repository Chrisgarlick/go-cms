package admin

import (
	"encoding/base64"
	"errors"
	"net/http"

	"cms/internal/actor"
	"cms/internal/auth"
	"cms/internal/web"
)

func (h *Handler) passkeyLoginBegin(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	if s == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Sign in with your password first."})
		return
	}
	opts, err := h.a.Auth.BeginPasskeyLogin(r.Context(), s.Session)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (h *Handler) passkeyLoginFinish(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	if s == nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "Sign in with your password first."})
		return
	}
	tok, err := h.a.Auth.FinishPasskeyLogin(r.Context(), s.Session, r, web.ClientIP(r.Context()), r.UserAgent())
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, auth.ErrRateLimited) {
			status = http.StatusTooManyRequests
		}
		writeJSON(w, status, map[string]string{"error": err.Error()})
		return
	}
	h.setSessionCookie(w, tok, h.a.Auth.SessionTTL)
	writeJSON(w, http.StatusOK, map[string]string{"redirect": safeNext(r.URL.Query().Get("next"))})
}

func (h *Handler) passkeyRegisterBegin(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	opts, err := h.a.Auth.BeginPasskeyRegistration(r.Context(), s.Session)
	if err != nil {
		if errors.Is(err, actor.ErrSudoRequired) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Confirm it's you first.", "sudo": true})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, opts)
}

func (h *Handler) passkeyRegisterFinish(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	if err := h.a.Auth.FinishPasskeyRegistration(r.Context(), s.Session, r.URL.Query().Get("name"), r); err != nil {
		if errors.Is(err, actor.ErrSudoRequired) {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "Confirm it's you first.", "sudo": true})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (h *Handler) passkeyDelete(w http.ResponseWriter, r *http.Request) {
	s := sessionFrom(r.Context())
	id, err := base64.RawURLEncoding.DecodeString(r.PostFormValue("id"))
	if err != nil {
		h.error(w, r, http.StatusBadRequest, "Unknown passkey.")
		return
	}
	if err := h.a.Auth.DeletePasskey(r.Context(), s.UserID, id); err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/admin/account?ok=deleted", http.StatusSeeOther)
}
