// Package auth implements users, sessions, mandatory two-factor
// authentication, sudo mode, invites, password resets and API tokens (spec 4.7).
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/pquerna/otp"
	"github.com/pquerna/otp/totp"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/keys"
	"cms/internal/mail"
	"cms/internal/rbac"
)

var (
	ErrInvalidCredentials = errors.New("email or password is incorrect")
	ErrRateLimited        = errors.New("too many attempts; wait a few minutes and try again")
	ErrInvalidCode        = errors.New("that code is not valid")
	ErrInvalidToken       = errors.New("this link is invalid or has expired")
	ErrLastOwner          = errors.New("a site must keep at least one active owner")
	ErrUserNotFound       = errors.New("user not found")
	ErrEmailTaken         = errors.New("a user with this email already exists")
)

const (
	StageMFA    = "mfa"
	StageEnroll = "enroll"
	StageFull   = "full"
)

type Service struct {
	DB      *db.DB
	Keys    *keys.Keyring
	Mail    mail.Sender
	Log     *slog.Logger
	BaseURL string // e.g. https://example.com (admin links in emails)
	Issuer  string // shown in authenticator apps
	// HIBP enables the breached-password check when non-nil (hardened client).
	HIBP *http.Client
	// SkipEnroll signs users in without 2FA enrolment (development only; the
	// config refuses it in production).
	SkipEnroll bool

	SessionTTL time.Duration // absolute lifetime
	IdleTTL    time.Duration
	SudoTTL    time.Duration

	ipLimiter      *Limiter
	accountLimiter *Limiter
	codeLimiter    *Limiter

	totpMu   sync.Mutex
	lastTOTP map[uuid.UUID]uint64 // replay protection: last accepted time step
	touchMu  sync.Mutex
	touched  map[string]time.Time
}

func (s *Service) init() {
	if s.ipLimiter == nil {
		s.ipLimiter = NewLimiter(30, 15*time.Minute)
		s.accountLimiter = NewLimiter(10, 15*time.Minute)
		s.codeLimiter = NewLimiter(8, 15*time.Minute)
		s.lastTOTP = map[uuid.UUID]uint64{}
		s.touched = map[string]time.Time{}
	}
	if s.SessionTTL == 0 {
		s.SessionTTL = 14 * 24 * time.Hour
	}
	if s.IdleTTL == 0 {
		s.IdleTTL = 24 * time.Hour
	}
	if s.SudoTTL == 0 {
		s.SudoTTL = 10 * time.Minute
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
}

// New returns an initialised service.
func New(s *Service) *Service {
	s.init()
	return s
}

func sha(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// Session is what the HTTP layer needs from a valid session.
type Session struct {
	IDHash    []byte
	UserID    uuid.UUID
	Role      rbac.Role
	Email     string
	Name      string
	Stage     string
	CSRF      string
	SudoUntil *time.Time
}

func (s *Session) Sudo() bool { return s.SudoUntil != nil && time.Now().Before(*s.SudoUntil) }

// Actor builds the service actor for a full session.
func (s *Session) Actor(ip netip.Addr) *actor.Actor {
	return &actor.Actor{Kind: actor.KindUser, UserID: s.UserID, Role: s.Role, Label: s.Email, IP: ip, Sudo: s.Sudo()}
}

type LoginResult struct {
	Token string
	Stage string
}

// Login checks email and password. The returned session is never "full": it
// still needs a second factor (or 2FA enrolment).
func (s *Service) Login(ctx context.Context, email, password string, ip netip.Addr, ua string) (*LoginResult, error) {
	s.init()
	email = strings.ToLower(strings.TrimSpace(email))
	u, err := s.DB.Q.GetUserByEmail(ctx, email)
	rid := "unknown" // audit rows reference users by id, never by email
	if err == nil {
		rid = u.ID.String()
	}
	if !s.ipLimiter.Allow("ip:"+ip.String()) || s.accountLimiter.Blocked("acct:"+email) {
		s.audit(ctx, "auth.login_throttled", "user", rid, nil)
		return nil, ErrRateLimited
	}
	hash := ""
	if err == nil {
		hash = u.PasswordHash
	}
	ok := VerifyPassword(hash, password) && err == nil && u.Status == "active"
	if !ok {
		s.accountLimiter.Allow("acct:" + email)
		s.audit(ctx, "auth.login_failed", "user", rid, nil)
		return nil, ErrInvalidCredentials
	}
	s.accountLimiter.Reset("acct:" + email)
	stage := StageMFA
	if !u.TotpConfirmed {
		stage = StageEnroll
		if s.SkipEnroll {
			stage = StageFull
		}
	}
	tok, err := s.createSession(ctx, s.DB.Q, u.ID, stage, ip, ua)
	if err != nil {
		return nil, err
	}
	return &LoginResult{Token: tok, Stage: stage}, nil
}

func (s *Service) createSession(ctx context.Context, q *dbq.Queries, userID uuid.UUID, stage string, ip netip.Addr, ua string) (string, error) {
	tok := RandomToken(32)
	var ipp *netip.Addr
	if ip.IsValid() {
		ipp = &ip
	}
	if len(ua) > 300 {
		ua = ua[:300]
	}
	ttl := s.SessionTTL
	if stage != StageFull {
		ttl = 15 * time.Minute
	}
	err := q.CreateSession(ctx, dbq.CreateSessionParams{
		IDSha256: sha(tok), UserID: userID, CsrfToken: RandomToken(24), Stage: stage,
		UserAgent: ua, Ip: ipp, ExpiresAt: time.Now().Add(ttl),
	})
	return tok, err
}

// Authenticate resolves a session cookie. Idle sessions expire; activity is
// written at most every five minutes.
func (s *Service) Authenticate(ctx context.Context, token string) (*Session, error) {
	s.init()
	if token == "" || len(token) > 128 {
		return nil, actor.ErrUnauthenticated
	}
	h := sha(token)
	row, err := s.DB.Q.GetSession(ctx, h)
	if err != nil {
		return nil, actor.ErrUnauthenticated
	}
	if row.UserStatus != "active" || time.Since(row.LastSeenAt) > s.IdleTTL {
		_ = s.DB.Q.DeleteSession(ctx, h)
		return nil, actor.ErrUnauthenticated
	}
	key := string(h)
	s.touchMu.Lock()
	last, ok := s.touched[key]
	if !ok || time.Since(last) > 5*time.Minute {
		s.touched[key] = time.Now()
		if len(s.touched) > 10000 {
			s.touched = map[string]time.Time{}
		}
		s.touchMu.Unlock()
		if time.Since(row.LastSeenAt) > 5*time.Minute {
			_ = s.DB.Q.TouchSession(ctx, h)
		}
	} else {
		s.touchMu.Unlock()
	}
	return &Session{IDHash: h, UserID: row.UserID, Role: rbac.Role(row.Role), Email: row.Email,
		Name: row.DisplayName, Stage: row.Stage, CSRF: row.CsrfToken, SudoUntil: row.SudoUntil}, nil
}

// upgrade replaces a pre-auth session with a full one (new token: prevents fixation).
func (s *Service) upgrade(ctx context.Context, q *dbq.Queries, sess *Session, ip netip.Addr, ua string) (string, error) {
	if err := q.DeleteSession(ctx, sess.IDHash); err != nil {
		return "", err
	}
	if err := q.TouchLogin(ctx, sess.UserID); err != nil {
		return "", err
	}
	return s.createSession(ctx, q, sess.UserID, StageFull, ip, ua)
}

// VerifyTOTP completes login with an authenticator code.
func (s *Service) VerifyTOTP(ctx context.Context, sess *Session, code string, ip netip.Addr, ua string) (string, error) {
	s.init()
	if sess.Stage != StageMFA {
		return "", ErrInvalidCode
	}
	if !s.codeLimiter.Allow("totp:" + sess.UserID.String()) {
		return "", ErrRateLimited
	}
	u, err := s.DB.Q.GetUser(ctx, sess.UserID)
	if err != nil {
		return "", ErrInvalidCode
	}
	if !s.checkTOTP(u, code) {
		s.audit(ctx, "auth.mfa_failed", "user", u.ID.String(), nil)
		return "", ErrInvalidCode
	}
	s.codeLimiter.Reset("totp:" + sess.UserID.String())
	var tok string
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		tok, err = s.upgrade(ctx, q, sess, ip, ua)
		if err != nil {
			return err
		}
		return audit.Record(actor.With(ctx, &actor.Actor{Kind: actor.KindUser, UserID: u.ID, Role: rbac.Role(u.Role), Label: u.Email, IP: ip}),
			q, audit.Event{Action: "auth.login", ResourceType: "user", ResourceID: u.ID.String(), Diff: map[string]any{"method": "totp"}})
	})
	return tok, err
}

// UseRecoveryCode completes login with a one-time recovery code.
func (s *Service) UseRecoveryCode(ctx context.Context, sess *Session, code string, ip netip.Addr, ua string) (string, error) {
	s.init()
	if sess.Stage != StageMFA {
		return "", ErrInvalidCode
	}
	if !s.codeLimiter.Allow("rc:" + sess.UserID.String()) {
		return "", ErrRateLimited
	}
	code = normaliseRecovery(code)
	var tok string
	err := s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		n, err := q.UseRecoveryCode(ctx, dbq.UseRecoveryCodeParams{UserID: sess.UserID, CodeSha256: sha(code)})
		if err != nil {
			return err
		}
		if n != 1 {
			return ErrInvalidCode
		}
		tok, err = s.upgrade(ctx, q, sess, ip, ua)
		if err != nil {
			return err
		}
		return audit.Record(actor.With(ctx, &actor.Actor{Kind: actor.KindUser, UserID: sess.UserID, Role: sess.Role, Label: sess.Email, IP: ip}),
			q, audit.Event{Action: "auth.login", ResourceType: "user", ResourceID: sess.UserID.String(), Diff: map[string]any{"method": "recovery_code"}})
	})
	return tok, err
}

func (s *Service) checkTOTP(u dbq.User, code string) bool {
	if len(u.TotpEnc) == 0 {
		return false
	}
	secret, err := s.Keys.Decrypt(u.TotpEnc, totpAAD(u.ID))
	if err != nil {
		s.Log.Error("totp secret decrypt failed", "user", u.ID, "err", err)
		return false
	}
	return s.validateCode(u.ID, string(secret), code)
}

func (s *Service) validateCode(userID uuid.UUID, secret, code string) bool {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != 6 {
		return false
	}
	now := time.Now()
	for _, skew := range []int{0, -1, 1} {
		t := now.Add(time.Duration(skew) * 30 * time.Second)
		ok, err := totp.ValidateCustom(code, secret, t, totp.ValidateOpts{Period: 30, Digits: otp.DigitsSix, Algorithm: otp.AlgorithmSHA1})
		if err == nil && ok {
			step := uint64(t.Unix() / 30)
			s.totpMu.Lock()
			defer s.totpMu.Unlock()
			if s.lastTOTP[userID] >= step {
				return false // replayed code
			}
			s.lastTOTP[userID] = step
			return true
		}
	}
	return false
}

func totpAAD(id uuid.UUID) []byte { return []byte("users:" + id.String() + ":totp") }

// Enrolment is shown while setting up an authenticator app.
type Enrolment struct {
	Secret string
	URL    string // otpauth:// for the QR code
}

// BeginEnroll creates (or replaces) an unconfirmed TOTP secret.
func (s *Service) BeginEnroll(ctx context.Context, userID uuid.UUID) (*Enrolment, error) {
	u, err := s.DB.Q.GetUser(ctx, userID)
	if err != nil {
		return nil, ErrUserNotFound
	}
	key, err := totp.Generate(totp.GenerateOpts{Issuer: s.Issuer, AccountName: u.Email, Period: 30, SecretSize: 20})
	if err != nil {
		return nil, err
	}
	enc, err := s.Keys.Encrypt([]byte(key.Secret()), totpAAD(u.ID))
	if err != nil {
		return nil, err
	}
	if u.TotpConfirmed {
		// Re-enrolment keeps the old secret active until the new one is confirmed.
		return &Enrolment{Secret: key.Secret(), URL: key.URL()},
			s.DB.Q.SetPendingTOTP(ctx, dbq.SetPendingTOTPParams{ID: u.ID, TotpPendingEnc: enc})
	}
	if err := s.DB.Q.SetTOTP(ctx, dbq.SetTOTPParams{ID: u.ID, TotpEnc: enc, TotpConfirmed: false}); err != nil {
		return nil, err
	}
	return &Enrolment{Secret: key.Secret(), URL: key.URL()}, nil
}

// ConfirmEnroll checks the first code, activates 2FA, issues recovery codes and
// (for an enrolment-stage session) completes sign-in.
func (s *Service) ConfirmEnroll(ctx context.Context, sess *Session, code string, ip netip.Addr, ua string) (token string, recovery []string, err error) {
	s.init()
	if !s.codeLimiter.Allow("enroll:" + sess.UserID.String()) {
		return "", nil, ErrRateLimited
	}
	u, err := s.DB.Q.GetUser(ctx, sess.UserID)
	if err != nil {
		return "", nil, ErrUserNotFound
	}
	enc := u.TotpEnc
	if len(u.TotpPendingEnc) > 0 {
		enc = u.TotpPendingEnc
	}
	secret, err := s.Keys.Decrypt(enc, totpAAD(u.ID))
	if err != nil || !s.validateCode(u.ID, string(secret), code) {
		return "", nil, ErrInvalidCode
	}
	codes := make([]string, 10)
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SetTOTP(ctx, dbq.SetTOTPParams{ID: u.ID, TotpEnc: enc, TotpConfirmed: true}); err != nil {
			return err
		}
		if err := q.DeleteRecoveryCodes(ctx, u.ID); err != nil {
			return err
		}
		for i := range codes {
			codes[i] = newRecoveryCode()
			if err := q.InsertRecoveryCode(ctx, dbq.InsertRecoveryCodeParams{UserID: u.ID, CodeSha256: sha(normaliseRecovery(codes[i]))}); err != nil {
				return err
			}
		}
		if sess.Stage == StageEnroll {
			token, err = s.upgrade(ctx, q, sess, ip, ua)
			if err != nil {
				return err
			}
		}
		a := &actor.Actor{Kind: actor.KindUser, UserID: u.ID, Role: rbac.Role(u.Role), Label: u.Email, IP: ip}
		return audit.Record(actor.With(ctx, a), q, audit.Event{Action: "auth.2fa_enrolled", ResourceType: "user", ResourceID: u.ID.String()})
	})
	return token, codes, err
}

func newRecoveryCode() string {
	b := make([]byte, 7)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	raw := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	return raw[:5] + "-" + raw[5:10]
}

func normaliseRecovery(c string) string {
	return strings.ToLower(strings.NewReplacer("-", "", " ", "").Replace(strings.TrimSpace(c)))
}

// Logout deletes one session.
func (s *Service) Logout(ctx context.Context, token string) {
	_ = s.DB.Q.DeleteSession(ctx, sha(token))
}

// Sudo re-authenticates for sensitive actions with a TOTP code or password.
func (s *Service) Sudo(ctx context.Context, sess *Session, password, code string) error {
	s.init()
	if !s.codeLimiter.Allow("sudo:" + sess.UserID.String()) {
		return ErrRateLimited
	}
	u, err := s.DB.Q.GetUser(ctx, sess.UserID)
	if err != nil {
		return ErrUserNotFound
	}
	ok := false
	if code != "" {
		ok = s.checkTOTP(u, code)
	} else if password != "" {
		ok = VerifyPassword(u.PasswordHash, password)
	}
	if !ok {
		return ErrInvalidCredentials
	}
	until := time.Now().Add(s.SudoTTL)
	return s.DB.Q.SetSessionSudo(ctx, dbq.SetSessionSudoParams{IDSha256: sess.IDHash, SudoUntil: &until})
}

// Sessions lists a user's sessions (own, or any for user managers).
func (s *Service) Sessions(ctx context.Context, userID uuid.UUID) ([]dbq.Session, error) {
	a := actor.From(ctx)
	if a.UserID != userID {
		if _, err := actor.Require(ctx, rbac.UsersManage); err != nil {
			return nil, err
		}
	}
	return s.DB.Q.ListSessionsForUser(ctx, userID)
}

// RevokeSession ends one of the caller's own sessions by its hash.
func (s *Service) RevokeSession(ctx context.Context, userID uuid.UUID, idHash []byte) error {
	a := actor.From(ctx)
	if !a.IsUser() || a.UserID != userID {
		if _, err := actor.RequireSudo(ctx, rbac.UsersManage); err != nil {
			return err
		}
	}
	return s.DB.Q.DeleteSessionByUser(ctx, dbq.DeleteSessionByUserParams{IDSha256: idHash, UserID: userID})
}

// SignOutEverywhere ends every other session of the caller.
func (s *Service) SignOutEverywhere(ctx context.Context, sess *Session) error {
	if err := s.DB.Q.DeleteOtherUserSessions(ctx, dbq.DeleteOtherUserSessionsParams{UserID: sess.UserID, IDSha256: sess.IDHash}); err != nil {
		return err
	}
	s.audit(ctx, "auth.signout_everywhere", "user", sess.UserID.String(), nil)
	return nil
}

func (s *Service) audit(ctx context.Context, action, rtype, rid string, diff any) {
	if err := audit.RecordTx(ctx, s.DB, audit.Event{Action: action, ResourceType: rtype, ResourceID: rid, Diff: diff}); err != nil {
		s.Log.Error("audit write failed", "action", action, "err", err)
	}
}

// ---------------------------------------------------------------------------
// Password reset (browser-bound) and invites
// ---------------------------------------------------------------------------

// RequestReset emails a reset link. browserNonce is a random value the HTTP
// layer also sets as a cookie; the link only works in the same browser.
// It always succeeds from the caller's point of view (no account enumeration).
func (s *Service) RequestReset(ctx context.Context, email, browserNonce string, ip netip.Addr) error {
	s.init()
	if !s.ipLimiter.Allow("reset:" + ip.String()) {
		return ErrRateLimited
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if !s.accountLimiter.Allow("reset:" + email) {
		return nil
	}
	u, err := s.DB.Q.GetUserByEmail(ctx, email)
	if err != nil || u.Status != "active" {
		return nil
	}
	tok := RandomToken(32)
	err = s.DB.Q.CreateAuthToken(ctx, dbq.CreateAuthTokenParams{
		ID: uuid.Must(uuid.NewV7()), UserID: u.ID, Purpose: "reset", TokenSha256: sha(tok),
		BrowserNonceSha256: sha(browserNonce), ExpiresAt: time.Now().Add(30 * time.Minute),
	})
	if err != nil {
		return err
	}
	link := strings.TrimRight(s.BaseURL, "/") + "/admin/reset/" + tok
	s.audit(ctx, "auth.reset_requested", "user", u.ID.String(), nil)
	return s.Mail.Send(ctx, mail.Message{To: u.Email, Subject: "Reset your " + s.Issuer + " password",
		Text: "Someone (hopefully you) asked to reset your password.\n\nOpen this link in the same browser within 30 minutes:\n" + link +
			"\n\nIf you did not ask for this, you can ignore this email."})
}

// CompleteReset sets a new password and signs the user out everywhere.
func (s *Service) CompleteReset(ctx context.Context, token, browserNonce, password string) error {
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		t, err := q.ConsumeAuthToken(ctx, dbq.ConsumeAuthTokenParams{TokenSha256: sha(token), Purpose: "reset"})
		if err != nil {
			return ErrInvalidToken
		}
		if subtle.ConstantTimeCompare(t.BrowserNonceSha256, sha(browserNonce)) != 1 {
			return fmt.Errorf("%w: open the link in the browser you requested it from", ErrInvalidToken)
		}
		u, err := q.GetUser(ctx, t.UserID)
		if err != nil {
			return ErrInvalidToken
		}
		if err := s.checkNewPassword(ctx, password, u.Email); err != nil {
			return err
		}
		h, err := HashPassword(password)
		if err != nil {
			return err
		}
		if err := q.SetPasswordHash(ctx, dbq.SetPasswordHashParams{ID: u.ID, PasswordHash: h}); err != nil {
			return err
		}
		if err := q.DeleteUserSessions(ctx, u.ID); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "auth.password_reset", ResourceType: "user", ResourceID: u.ID.String()})
	})
}

func (s *Service) checkNewPassword(ctx context.Context, pw, email string) error {
	if err := CheckPasswordStrength(pw, email); err != nil {
		return err
	}
	if s.HIBP != nil {
		if bad, err := Breached(ctx, s.HIBP, pw); err == nil && bad {
			return fmt.Errorf("%w: this password appears in known data breaches", ErrWeakPassword)
		}
	}
	return nil
}

// InviteInput creates a user who sets their own password via an emailed link.
type InviteInput struct {
	Email string
	Name  string
	Role  rbac.Role
}

// Invite creates an invited user and returns the invite link (also emailed).
func (s *Service) Invite(ctx context.Context, in InviteInput) (dbq.User, string, error) {
	a, err := actor.Require(ctx, rbac.UsersManage)
	if err != nil {
		return dbq.User{}, "", err
	}
	if !rbac.CanAssign(a.Role, in.Role) && a.Kind != actor.KindSystem {
		return dbq.User{}, "", &actor.ErrForbidden{Cap: rbac.OwnersManage}
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !strings.Contains(email, "@") || len(email) > 254 || strings.ContainsAny(email, " \r\n") {
		return dbq.User{}, "", errors.New("enter a valid email address")
	}
	tok := RandomToken(32)
	var u dbq.User
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var err error
		u, err = q.CreateUser(ctx, dbq.CreateUserParams{ID: uuid.Must(uuid.NewV7()), Email: email, Role: string(in.Role),
			DisplayName: strings.TrimSpace(in.Name), Status: "invited", Locale: "en"})
		if db.IsUniqueViolation(err, "") {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		if err := q.CreateAuthToken(ctx, dbq.CreateAuthTokenParams{ID: uuid.Must(uuid.NewV7()), UserID: u.ID, Purpose: "invite",
			TokenSha256: sha(tok), ExpiresAt: time.Now().Add(7 * 24 * time.Hour)}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "user.invite", ResourceType: "user", ResourceID: u.ID.String(),
			Diff: map[string]any{"role": in.Role}})
	})
	if err != nil {
		return u, "", err
	}
	link := strings.TrimRight(s.BaseURL, "/") + "/admin/invite/" + tok
	if err := s.Mail.Send(ctx, mail.Message{To: email, Subject: "You're invited to " + s.Issuer,
		Text: "You have been invited to manage " + s.Issuer + ".\n\nSet your password and two-factor authentication here (valid for 7 days):\n" + link}); err != nil {
		s.Log.Warn("invite email failed; share the link manually", "err", err)
	}
	return u, link, nil
}

// InviteUser looks up an invite token without consuming it (to show the form).
func (s *Service) InviteUser(ctx context.Context, token string) (dbq.User, error) {
	var u dbq.User
	err := s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		// Peek: consume then roll back.
		t, err := q.ConsumeAuthToken(ctx, dbq.ConsumeAuthTokenParams{TokenSha256: sha(token), Purpose: "invite"})
		if err != nil {
			return ErrInvalidToken
		}
		u, err = q.GetUser(ctx, t.UserID)
		if err != nil {
			return ErrInvalidToken
		}
		return errPeek
	})
	if errors.Is(err, errPeek) {
		return u, nil
	}
	return u, err
}

var errPeek = errors.New("peek")

// AcceptInvite sets the password and starts an enrolment-stage session.
func (s *Service) AcceptInvite(ctx context.Context, token, password, name string, ip netip.Addr, ua string) (string, error) {
	var sessTok string
	err := s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		t, err := q.ConsumeAuthToken(ctx, dbq.ConsumeAuthTokenParams{TokenSha256: sha(token), Purpose: "invite"})
		if err != nil {
			return ErrInvalidToken
		}
		u, err := q.GetUser(ctx, t.UserID)
		if err != nil || u.Status != "invited" {
			return ErrInvalidToken
		}
		if err := s.checkNewPassword(ctx, password, u.Email); err != nil {
			return err
		}
		h, err := HashPassword(password)
		if err != nil {
			return err
		}
		if err := q.SetPasswordHash(ctx, dbq.SetPasswordHashParams{ID: u.ID, PasswordHash: h}); err != nil {
			return err
		}
		if strings.TrimSpace(name) != "" {
			if _, err := q.UpdateUserProfile(ctx, dbq.UpdateUserProfileParams{ID: u.ID, DisplayName: strings.TrimSpace(name), Bio: u.Bio, Locale: u.Locale}); err != nil {
				return err
			}
		}
		if err := q.UpdateUserStatus(ctx, dbq.UpdateUserStatusParams{ID: u.ID, Status: "active"}); err != nil {
			return err
		}
		sessTok, err = s.createSession(ctx, q, u.ID, StageEnroll, ip, ua)
		if err != nil {
			return err
		}
		a := &actor.Actor{Kind: actor.KindUser, UserID: u.ID, Role: rbac.Role(u.Role), Label: u.Email, IP: ip}
		return audit.Record(actor.With(ctx, a), q, audit.Event{Action: "user.invite_accepted", ResourceType: "user", ResourceID: u.ID.String()})
	})
	return sessTok, err
}

// ChangePassword for the signed-in user (requires sudo).
func (s *Service) ChangePassword(ctx context.Context, sess *Session, newPassword string) error {
	a := actor.From(ctx)
	if !a.Sudo {
		return actor.ErrSudoRequired
	}
	if err := s.checkNewPassword(ctx, newPassword, sess.Email); err != nil {
		return err
	}
	h, err := HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SetPasswordHash(ctx, dbq.SetPasswordHashParams{ID: sess.UserID, PasswordHash: h}); err != nil {
			return err
		}
		if err := q.DeleteOtherUserSessions(ctx, dbq.DeleteOtherUserSessionsParams{UserID: sess.UserID, IDSha256: sess.IDHash}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "auth.password_changed", ResourceType: "user", ResourceID: sess.UserID.String()})
	})
}

// RecoveryCodesLeft reports unused recovery codes.
func (s *Service) RecoveryCodesLeft(ctx context.Context, userID uuid.UUID) (int64, error) {
	return s.DB.Q.CountRecoveryCodes(ctx, userID)
}

// AcceptInviteCheck reports whether an invite token is valid (tests, forms).
func (s *Service) AcceptInviteCheck(token string) error {
	_, err := s.InviteUser(context.Background(), token)
	return err
}
