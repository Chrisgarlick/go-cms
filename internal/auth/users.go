package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"cms/internal/actor"
	"cms/internal/audit"
	"cms/internal/db"
	"cms/internal/db/dbq"
	"cms/internal/rbac"
)

func (s *Service) Users(ctx context.Context) ([]dbq.User, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return nil, err
	}
	return s.DB.Q.ListUsers(ctx)
}

func (s *Service) User(ctx context.Context, id uuid.UUID) (dbq.User, error) {
	if _, err := actor.Require(ctx, rbac.ContentRead); err != nil {
		return dbq.User{}, err
	}
	u, err := s.DB.Q.GetUser(ctx, id)
	if db.IsNotFound(err) {
		return u, ErrUserNotFound
	}
	return u, err
}

// mayManage enforces: users.manage for admins/editors, owners.manage for
// owners, and nobody below Owner can touch an Owner account.
func mayManage(a *actor.Actor, target rbac.Role) error {
	if a.Kind == actor.KindSystem {
		return nil
	}
	if target == rbac.Owner && !a.Can(rbac.OwnersManage) {
		return &actor.ErrForbidden{Cap: rbac.OwnersManage}
	}
	if !a.Can(rbac.UsersManage) {
		return &actor.ErrForbidden{Cap: rbac.UsersManage}
	}
	return nil
}

// SetRole changes a user's role (sudo). The last active owner cannot be demoted.
func (s *Service) SetRole(ctx context.Context, id uuid.UUID, role rbac.Role) error {
	a, err := actor.RequireSudo(ctx, rbac.UsersManage)
	if err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		u, err := q.GetUser(ctx, id)
		if err != nil {
			return ErrUserNotFound
		}
		if err := mayManage(a, rbac.Role(u.Role)); err != nil {
			return err
		}
		if err := mayManage(a, role); err != nil {
			return err
		}
		if u.Role == string(rbac.Owner) && role != rbac.Owner {
			if err := s.keepOwner(ctx, q); err != nil {
				return err
			}
		}
		if err := q.UpdateUserRole(ctx, dbq.UpdateUserRoleParams{ID: id, Role: string(role)}); err != nil {
			return err
		}
		// Role change: end the user's sessions so new permissions apply at once.
		if err := q.DeleteUserSessions(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "user.role", ResourceType: "user", ResourceID: id.String(),
			Diff: map[string]any{"from": u.Role, "to": role}})
	})
}

func (s *Service) keepOwner(ctx context.Context, q *dbq.Queries) error {
	n, err := q.CountOwners(ctx)
	if err != nil {
		return err
	}
	if n <= 1 {
		return ErrLastOwner
	}
	return nil
}

// SetStatus enables or disables a user (sudo).
func (s *Service) SetStatus(ctx context.Context, id uuid.UUID, status string) error {
	a, err := actor.RequireSudo(ctx, rbac.UsersManage)
	if err != nil {
		return err
	}
	if status != "active" && status != "disabled" {
		return errors.New("status must be active or disabled")
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		u, err := q.GetUser(ctx, id)
		if err != nil {
			return ErrUserNotFound
		}
		if err := mayManage(a, rbac.Role(u.Role)); err != nil {
			return err
		}
		if status == "disabled" && u.Role == string(rbac.Owner) && u.Status == "active" {
			if err := s.keepOwner(ctx, q); err != nil {
				return err
			}
		}
		if err := q.UpdateUserStatus(ctx, dbq.UpdateUserStatusParams{ID: id, Status: status}); err != nil {
			return err
		}
		if status == "disabled" {
			if err := q.DeleteUserSessions(ctx, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Event{Action: "user.status", ResourceType: "user", ResourceID: id.String(),
			Diff: map[string]any{"status": status}})
	})
}

// DeleteUser tombstones a user: personal data is removed, the id remains so
// audit rows and authorship still resolve (spec 4.18).
func (s *Service) DeleteUser(ctx context.Context, id uuid.UUID) error {
	a, err := actor.RequireSudo(ctx, rbac.UsersManage)
	if err != nil {
		return err
	}
	if a.IsUser() && a.UserID == id {
		return errors.New("you cannot delete your own account")
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		u, err := q.GetUser(ctx, id)
		if err != nil {
			return ErrUserNotFound
		}
		if err := mayManage(a, rbac.Role(u.Role)); err != nil {
			return err
		}
		if u.Role == string(rbac.Owner) && u.Status == "active" {
			if err := s.keepOwner(ctx, q); err != nil {
				return err
			}
		}
		for _, f := range []func(context.Context, uuid.UUID) error{q.DeleteUserSessions, q.DeleteRecoveryCodes, q.TombstoneUser} {
			if err := f(ctx, id); err != nil {
				return err
			}
		}
		return audit.Record(ctx, q, audit.Event{Action: "user.delete", ResourceType: "user", ResourceID: id.String()})
	})
}

// UpdateProfile edits display name, bio and locale (own profile, or managed users).
func (s *Service) UpdateProfile(ctx context.Context, id uuid.UUID, name, bio, locale string) (dbq.User, error) {
	a := actor.From(ctx)
	if !a.IsUser() && a.Kind != actor.KindSystem {
		return dbq.User{}, actor.ErrUnauthenticated
	}
	var out dbq.User
	err := s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		u, err := q.GetUser(ctx, id)
		if err != nil {
			return ErrUserNotFound
		}
		if a.UserID != id {
			if err := mayManage(a, rbac.Role(u.Role)); err != nil {
				return err
			}
		}
		name = strings.TrimSpace(name)
		if len([]rune(name)) > 100 || len([]rune(bio)) > 2000 {
			return errors.New("name or bio is too long")
		}
		if locale == "" {
			locale = u.Locale
		}
		out, err = q.UpdateUserProfile(ctx, dbq.UpdateUserProfileParams{ID: id, DisplayName: name, Bio: strings.TrimSpace(bio), Locale: locale})
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "user.profile", ResourceType: "user", ResourceID: id.String()})
	})
	return out, err
}

// Reset2FA clears a user's authenticator so they enrol again on next sign-in.
// Owners only, with sudo.
func (s *Service) Reset2FA(ctx context.Context, id uuid.UUID) error {
	a, err := actor.RequireSudo(ctx, rbac.OwnersManage)
	if err != nil {
		return err
	}
	_ = a
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SetTOTP(ctx, dbq.SetTOTPParams{ID: id, TotpEnc: nil, TotpConfirmed: false}); err != nil {
			return err
		}
		if err := q.DeleteRecoveryCodes(ctx, id); err != nil {
			return err
		}
		if err := q.DeleteUserSessions(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "user.2fa_reset", ResourceType: "user", ResourceID: id.String()})
	})
}

// CreateUserDirect creates an active user with a password (CLI / install only).
func (s *Service) CreateUserDirect(ctx context.Context, email, name, password string, role rbac.Role) (dbq.User, error) {
	a, err := actor.Require(ctx, rbac.UsersManage)
	if err != nil {
		return dbq.User{}, err
	}
	if err := mayManage(a, role); err != nil {
		return dbq.User{}, err
	}
	email = strings.ToLower(strings.TrimSpace(email))
	if err := s.checkNewPassword(ctx, password, email); err != nil {
		return dbq.User{}, err
	}
	h, err := HashPassword(password)
	if err != nil {
		return dbq.User{}, err
	}
	var u dbq.User
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		var err error
		u, err = q.CreateUser(ctx, dbq.CreateUserParams{ID: uuid.Must(uuid.NewV7()), Email: email, PasswordHash: h,
			Role: string(role), DisplayName: strings.TrimSpace(name), Status: "active", Locale: "en"})
		if db.IsUniqueViolation(err, "") {
			return ErrEmailTaken
		}
		if err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "user.create", ResourceType: "user", ResourceID: u.ID.String(),
			Diff: map[string]any{"role": role}})
	})
	return u, err
}

// SetPasswordDirect sets a password from the CLI (system actor on the server).
func (s *Service) SetPasswordDirect(ctx context.Context, id uuid.UUID, password string) error {
	a := actor.From(ctx)
	if a.Kind != actor.KindSystem {
		return &actor.ErrForbidden{Cap: rbac.OwnersManage}
	}
	u, err := s.DB.Q.GetUser(ctx, id)
	if err != nil {
		return ErrUserNotFound
	}
	if err := s.checkNewPassword(ctx, password, u.Email); err != nil {
		return err
	}
	h, err := HashPassword(password)
	if err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.SetPasswordHash(ctx, dbq.SetPasswordHashParams{ID: id, PasswordHash: h}); err != nil {
			return err
		}
		if err := q.DeleteUserSessions(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "auth.password_set", ResourceType: "user", ResourceID: id.String()})
	})
}

// ---------------------------------------------------------------------------
// API tokens: cms_pat_<id>_<secret>
// ---------------------------------------------------------------------------

const tokenPrefix = "cms_pat_"

// CreateToken issues a token for the calling user with at most their role.
// The plaintext is returned once and never stored.
func (s *Service) CreateToken(ctx context.Context, name string, role rbac.Role, ttl time.Duration) (string, dbq.ApiToken, error) {
	a, err := actor.RequireSudo(ctx, rbac.TokensOwn)
	if err != nil {
		return "", dbq.ApiToken{}, err
	}
	if !a.IsUser() {
		return "", dbq.ApiToken{}, errors.New("tokens belong to a user; use --as")
	}
	if rbac.Rank(role) > rbac.Rank(a.Role) {
		role = a.Role
	}
	if ttl <= 0 || ttl > 366*24*time.Hour {
		ttl = 90 * 24 * time.Hour
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return "", dbq.ApiToken{}, errors.New("give the token a name (up to 100 characters)")
	}
	id := uuid.Must(uuid.NewV7())
	secret := RandomToken(32)
	plain := tokenPrefix + strings.ReplaceAll(id.String(), "-", "") + "_" + secret
	t := dbq.ApiToken{ID: id, UserID: a.UserID, Name: name, Role: string(role), ExpiresAt: time.Now().Add(ttl)}
	err = s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		if err := q.CreateAPIToken(ctx, dbq.CreateAPITokenParams{ID: id, UserID: a.UserID, Name: name,
			SecretSha256: sha(secret), Role: string(role), ExpiresAt: t.ExpiresAt}); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "token.create", ResourceType: "api_token", ResourceID: id.String(),
			Diff: map[string]any{"role": role, "expires_at": t.ExpiresAt}})
	})
	return plain, t, err
}

// AuthenticateToken resolves a bearer token to an actor.
func (s *Service) AuthenticateToken(ctx context.Context, raw string) (*actor.Actor, error) {
	rest, ok := strings.CutPrefix(raw, tokenPrefix)
	if !ok || len(rest) < 34 {
		return nil, actor.ErrUnauthenticated
	}
	idHex, secret, ok := strings.Cut(rest, "_")
	if !ok {
		return nil, actor.ErrUnauthenticated
	}
	id, err := uuid.Parse(idHex)
	if err != nil {
		return nil, actor.ErrUnauthenticated
	}
	t, err := s.DB.Q.GetAPIToken(ctx, id)
	if err != nil || t.RevokedAt != nil || time.Now().After(t.ExpiresAt) || t.UserStatus != "active" {
		return nil, actor.ErrUnauthenticated
	}
	if subtle.ConstantTimeCompare(t.SecretSha256, sha(secret)) != 1 {
		return nil, actor.ErrUnauthenticated
	}
	role := rbac.Role(t.Role)
	if rbac.Rank(rbac.Role(t.UserRole)) < rbac.Rank(role) {
		role = rbac.Role(t.UserRole) // the user was demoted after issuing the token
	}
	s.touchToken(id)
	return &actor.Actor{Kind: actor.KindToken, UserID: t.UserID, TokenID: id, Role: role, Label: "token:" + t.Name}, nil
}

var tokenTouches = newTouchSet()

func (s *Service) touchToken(id uuid.UUID) { tokenTouches.add(id) }

// FlushTokenUse writes batched last_used_at updates (spec 8.3).
func (s *Service) FlushTokenUse(ctx context.Context) error {
	ids := tokenTouches.drain()
	if len(ids) == 0 {
		return nil
	}
	return s.DB.Q.TouchAPITokens(ctx, ids)
}

func (s *Service) Tokens(ctx context.Context) ([]dbq.ApiToken, error) {
	a := actor.From(ctx)
	if a.Can(rbac.TokensAll) {
		return s.DB.Q.ListAPITokens(ctx)
	}
	if _, err := actor.Require(ctx, rbac.TokensOwn); err != nil {
		return nil, err
	}
	return s.DB.Q.ListAPITokensForUser(ctx, a.UserID)
}

func (s *Service) RevokeToken(ctx context.Context, id uuid.UUID) error {
	a, err := actor.Require(ctx, rbac.TokensOwn)
	if err != nil {
		return err
	}
	return s.DB.Tx(ctx, func(tx pgx.Tx, q *dbq.Queries) error {
		t, err := q.GetAPIToken(ctx, id)
		if err != nil {
			return errors.New("token not found")
		}
		if t.UserID != a.UserID && !a.Can(rbac.TokensAll) {
			return &actor.ErrForbidden{Cap: rbac.TokensAll}
		}
		if err := q.RevokeAPIToken(ctx, id); err != nil {
			return err
		}
		return audit.Record(ctx, q, audit.Event{Action: "token.revoke", ResourceType: "api_token", ResourceID: id.String()})
	})
}
