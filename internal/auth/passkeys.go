package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/netip"
	"net/url"
	"strings"

	"github.com/go-webauthn/webauthn/protocol"
	"github.com/go-webauthn/webauthn/webauthn"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db/dbq"
	"cms/internal/rbac"
)

// Passkeys (WebAuthn) work as a second factor after the password, and as a
// faster alternative to authenticator codes (spec 4.7). TOTP stays the
// baseline every account enrols in, so losing a device never locks anyone out.

var ErrNoPasskeys = errors.New("this account has no passkeys")

type waUser struct {
	u     dbq.User
	creds []webauthn.Credential
}

func (w *waUser) WebAuthnID() []byte                         { return w.u.ID[:] }
func (w *waUser) WebAuthnName() string                       { return w.u.Email }
func (w *waUser) WebAuthnDisplayName() string                { return firstNonEmpty(w.u.DisplayName, w.u.Email) }
func (w *waUser) WebAuthnCredentials() []webauthn.Credential { return w.creds }

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

func (s *Service) webauthn() (*webauthn.WebAuthn, error) {
	u, err := url.Parse(s.BaseURL)
	if err != nil {
		return nil, err
	}
	origin := u.Scheme + "://" + u.Host
	return webauthn.New(&webauthn.Config{RPID: u.Hostname(), RPDisplayName: s.Issuer, RPOrigins: []string{origin}})
}

func (s *Service) loadWAUser(ctx context.Context, id uuid.UUID) (*waUser, error) {
	u, err := s.DB.Q.GetUser(ctx, id)
	if err != nil {
		return nil, ErrUserNotFound
	}
	rows, err := s.DB.Q.ListWebAuthnCredentials(ctx, id)
	if err != nil {
		return nil, err
	}
	w := &waUser{u: u}
	for _, r := range rows {
		var c webauthn.Credential
		if json.Unmarshal(r.Credential, &c) == nil {
			w.creds = append(w.creds, c)
		}
	}
	return w, nil
}

// HasPasskeys reports whether a user can sign in with a passkey.
func (s *Service) HasPasskeys(ctx context.Context, id uuid.UUID) bool {
	n, err := s.DB.Q.CountWebAuthnCredentials(ctx, id)
	return err == nil && n > 0
}

// BeginPasskeyRegistration starts adding a passkey (full session + sudo).
func (s *Service) BeginPasskeyRegistration(ctx context.Context, sess *Session) (*protocol.CredentialCreation, error) {
	if sess.Stage != StageFull {
		return nil, actor.ErrUnauthenticated
	}
	if !sess.Sudo() {
		return nil, actor.ErrSudoRequired
	}
	wa, err := s.webauthn()
	if err != nil {
		return nil, err
	}
	user, err := s.loadWAUser(ctx, sess.UserID)
	if err != nil {
		return nil, err
	}
	var exclude []protocol.CredentialDescriptor
	for _, c := range user.creds {
		exclude = append(exclude, c.Descriptor())
	}
	creation, data, err := wa.BeginRegistration(user,
		webauthn.WithResidentKeyRequirement(protocol.ResidentKeyRequirementPreferred),
		webauthn.WithExclusions(exclude),
		webauthn.WithAuthenticatorSelection(protocol.AuthenticatorSelection{UserVerification: protocol.VerificationPreferred,
			ResidentKey: protocol.ResidentKeyRequirementPreferred}))
	if err != nil {
		return nil, err
	}
	return creation, s.saveState(ctx, sess, "register", data)
}

// FinishPasskeyRegistration verifies the browser's response and stores the passkey.
func (s *Service) FinishPasskeyRegistration(ctx context.Context, sess *Session, name string, r *http.Request) error {
	if !sess.Sudo() {
		return actor.ErrSudoRequired
	}
	wa, err := s.webauthn()
	if err != nil {
		return err
	}
	data, err := s.loadState(ctx, sess, "register")
	if err != nil {
		return err
	}
	user, err := s.loadWAUser(ctx, sess.UserID)
	if err != nil {
		return err
	}
	cred, err := wa.FinishRegistration(user, *data, r)
	if err != nil {
		return errors.New("the passkey could not be verified; try again")
	}
	cj, _ := json.Marshal(cred)
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Passkey"
	}
	if len(name) > 60 {
		name = name[:60]
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.InsertWebAuthnCredential(ctx, dbq.InsertWebAuthnCredentialParams{ID: cred.ID, UserID: sess.UserID, Name: name, Credential: cj}); err != nil {
			return err
		}
		if err := q.SetWebAuthnState(ctx, dbq.SetWebAuthnStateParams{IDSha256: sess.IDHash, WebauthnState: nil}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "auth.passkey_added", ResourceType: "user", ResourceID: sess.UserID.String()})
	})
}

// BeginPasskeyLogin starts the second-factor ceremony for an MFA-stage session.
func (s *Service) BeginPasskeyLogin(ctx context.Context, sess *Session) (*protocol.CredentialAssertion, error) {
	s.init()
	if sess.Stage != StageMFA {
		return nil, ErrInvalidCode
	}
	wa, err := s.webauthn()
	if err != nil {
		return nil, err
	}
	user, err := s.loadWAUser(ctx, sess.UserID)
	if err != nil {
		return nil, err
	}
	if len(user.creds) == 0 {
		return nil, ErrNoPasskeys
	}
	assertion, data, err := wa.BeginLogin(user, webauthn.WithUserVerification(protocol.VerificationPreferred))
	if err != nil {
		return nil, err
	}
	return assertion, s.saveState(ctx, sess, "login", data)
}

// FinishPasskeyLogin verifies the assertion and upgrades the session.
func (s *Service) FinishPasskeyLogin(ctx context.Context, sess *Session, r *http.Request, ip netip.Addr, ua string) (string, error) {
	s.init()
	if sess.Stage != StageMFA {
		return "", ErrInvalidCode
	}
	if !s.codeLimiter.Allow("passkey:" + sess.UserID.String()) {
		return "", ErrRateLimited
	}
	wa, err := s.webauthn()
	if err != nil {
		return "", err
	}
	data, err := s.loadState(ctx, sess, "login")
	if err != nil {
		return "", err
	}
	user, err := s.loadWAUser(ctx, sess.UserID)
	if err != nil {
		return "", err
	}
	cred, err := wa.FinishLogin(user, *data, r)
	if err != nil {
		s.audit(ctx, "auth.mfa_failed", "user", sess.UserID.String(), map[string]any{"method": "passkey"})
		return "", ErrInvalidCode
	}
	if cred.Authenticator.CloneWarning {
		s.audit(ctx, "auth.passkey_clone_warning", "user", sess.UserID.String(), nil)
		return "", errors.New("this passkey looks like it was copied; sign in with your authenticator code and remove it")
	}
	cj, _ := json.Marshal(cred)
	var tok string
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.UpdateWebAuthnCredential(ctx, dbq.UpdateWebAuthnCredentialParams{ID: cred.ID, Credential: cj}); err != nil {
			return err
		}
		tok, err = s.upgrade(ctx, q, sess, ip, ua)
		if err != nil {
			return err
		}
		a := &actor.Actor{Kind: actor.KindUser, UserID: sess.UserID, Role: sess.Role, Label: sess.Email, IP: ip}
		return audit.Record(actor.With(ctx, a), q, audit.Event{Action: "auth.login", ResourceType: "user", ResourceID: sess.UserID.String(),
			Diff: map[string]any{"method": "passkey"}})
	})
	return tok, err
}

// Passkeys lists the caller's passkeys.
func (s *Service) Passkeys(ctx context.Context, userID uuid.UUID) ([]dbq.WebauthnCredential, error) {
	return s.DB.Q.ListWebAuthnCredentials(ctx, userID)
}

// DeletePasskey removes one of the caller's passkeys (sudo).
func (s *Service) DeletePasskey(ctx context.Context, userID uuid.UUID, id []byte) error {
	if _, err := actor.RequireSudo(ctx, rbac.ContentRead); err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.DeleteWebAuthnCredential(ctx, dbq.DeleteWebAuthnCredentialParams{ID: id, UserID: userID}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "auth.passkey_removed", ResourceType: "user", ResourceID: userID.String()})
	})
}

type waState struct {
	Purpose string               `json:"purpose"`
	Data    webauthn.SessionData `json:"data"`
}

func (s *Service) saveState(ctx context.Context, sess *Session, purpose string, data *webauthn.SessionData) error {
	b, err := json.Marshal(waState{Purpose: purpose, Data: *data})
	if err != nil {
		return err
	}
	return s.DB.Q.SetWebAuthnState(ctx, dbq.SetWebAuthnStateParams{IDSha256: sess.IDHash, WebauthnState: b})
}

func (s *Service) loadState(ctx context.Context, sess *Session, purpose string) (*webauthn.SessionData, error) {
	raw, err := s.DB.Q.GetWebAuthnState(ctx, sess.IDHash)
	if err != nil || len(raw) == 0 {
		return nil, errors.New("no passkey request in progress; try again")
	}
	var st waState
	if err := json.Unmarshal(raw, &st); err != nil || st.Purpose != purpose {
		return nil, errors.New("no passkey request in progress; try again")
	}
	return &st.Data, nil
}
