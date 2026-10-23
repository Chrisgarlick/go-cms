-- name: CreateUser :one
INSERT INTO users (id, email, password_hash, role, display_name, status, locale)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetUser :one
SELECT * FROM users WHERE id = $1 AND status <> 'deleted';

-- name: GetUserByEmail :one
SELECT * FROM users WHERE lower(email) = lower($1) AND status <> 'deleted';

-- name: ListUsers :many
SELECT * FROM users WHERE status <> 'deleted' ORDER BY role, lower(display_name), lower(email);

-- name: CountOwners :one
SELECT count(*) FROM users WHERE role = 'owner' AND status = 'active';

-- name: UpdateUserProfile :one
UPDATE users SET display_name = $2, bio = $3, locale = $4, updated_at = now()
WHERE id = $1 RETURNING *;

-- name: UpdateUserRole :exec
UPDATE users SET role = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserStatus :exec
UPDATE users SET status = $2, updated_at = now() WHERE id = $1;

-- name: UpdateUserEmail :exec
UPDATE users SET email = $2, updated_at = now() WHERE id = $1;

-- name: SetPasswordHash :exec
UPDATE users SET password_hash = $2, updated_at = now() WHERE id = $1;

-- name: SetTOTP :exec
UPDATE users SET totp_enc = $2, totp_confirmed = $3, totp_pending_enc = NULL, updated_at = now() WHERE id = $1;

-- name: SetPendingTOTP :exec
UPDATE users SET totp_pending_enc = $2, updated_at = now() WHERE id = $1;

-- name: TouchLogin :exec
UPDATE users SET last_login_at = now() WHERE id = $1;

-- name: TombstoneUser :exec
UPDATE users SET status = 'deleted',
    email = 'deleted-' || id::text || '@invalid',
    display_name = 'Deleted user', bio = '', password_hash = '',
    totp_enc = NULL, totp_pending_enc = NULL, totp_confirmed = false, updated_at = now()
WHERE id = $1;

-- name: DeleteRecoveryCodes :exec
DELETE FROM user_recovery_codes WHERE user_id = $1;

-- name: InsertRecoveryCode :exec
INSERT INTO user_recovery_codes (user_id, code_sha256) VALUES ($1, $2);

-- name: UseRecoveryCode :execrows
UPDATE user_recovery_codes SET used_at = now()
WHERE user_id = $1 AND code_sha256 = $2 AND used_at IS NULL;

-- name: CountRecoveryCodes :one
SELECT count(*) FROM user_recovery_codes WHERE user_id = $1 AND used_at IS NULL;

-- name: InsertWebAuthnCredential :exec
INSERT INTO webauthn_credentials (id, user_id, name, credential) VALUES ($1, $2, $3, $4);

-- name: ListWebAuthnCredentials :many
SELECT * FROM webauthn_credentials WHERE user_id = $1 ORDER BY created_at;

-- name: UpdateWebAuthnCredential :exec
UPDATE webauthn_credentials SET credential = $2, last_used_at = now() WHERE id = $1;

-- name: DeleteWebAuthnCredential :exec
DELETE FROM webauthn_credentials WHERE id = $1 AND user_id = $2;

-- name: CountWebAuthnCredentials :one
SELECT count(*) FROM webauthn_credentials WHERE user_id = $1;

-- name: CreateAuthToken :exec
INSERT INTO auth_tokens (id, user_id, purpose, token_sha256, browser_nonce_sha256, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ConsumeAuthToken :one
UPDATE auth_tokens SET used_at = now()
WHERE token_sha256 = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > now()
RETURNING *;

-- name: DeleteExpiredAuthTokens :execrows
DELETE FROM auth_tokens WHERE expires_at < now() - interval '7 days';

-- name: CreateAPIToken :exec
INSERT INTO api_tokens (id, user_id, name, secret_sha256, role, expires_at)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: GetAPIToken :one
SELECT t.*, u.status AS user_status, u.role AS user_role
FROM api_tokens t JOIN users u ON u.id = t.user_id
WHERE t.id = $1;

-- name: ListAPITokens :many
SELECT * FROM api_tokens WHERE revoked_at IS NULL ORDER BY created_at DESC;

-- name: ListAPITokensForUser :many
SELECT * FROM api_tokens WHERE user_id = $1 AND revoked_at IS NULL ORDER BY created_at DESC;

-- name: RevokeAPIToken :exec
UPDATE api_tokens SET revoked_at = now() WHERE id = $1;

-- name: TouchAPITokens :exec
UPDATE api_tokens SET last_used_at = now() WHERE id = ANY(@ids::uuid[]);

-- name: CreateSession :exec
INSERT INTO sessions (id_sha256, user_id, csrf_token, stage, user_agent, ip, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: GetSession :one
SELECT s.*, u.role, u.status AS user_status, u.email, u.display_name, u.totp_confirmed
FROM sessions s JOIN users u ON u.id = s.user_id
WHERE s.id_sha256 = $1 AND s.expires_at > now();

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = now() WHERE id_sha256 = $1;

-- name: SetSessionSudo :exec
UPDATE sessions SET sudo_until = $2 WHERE id_sha256 = $1;

-- name: DeleteSession :exec
DELETE FROM sessions WHERE id_sha256 = $1;

-- name: DeleteSessionByUser :exec
DELETE FROM sessions WHERE id_sha256 = $1 AND user_id = $2;

-- name: ListSessionsForUser :many
SELECT * FROM sessions WHERE user_id = $1 AND expires_at > now() ORDER BY last_seen_at DESC;

-- name: DeleteUserSessions :exec
DELETE FROM sessions WHERE user_id = $1;

-- name: DeleteOtherUserSessions :exec
DELETE FROM sessions WHERE user_id = $1 AND id_sha256 <> $2;

-- name: DeleteExpiredSessions :execrows
DELETE FROM sessions WHERE expires_at < now() OR last_seen_at < now() - (@idle::text)::interval;

-- name: SetWebAuthnState :exec
UPDATE sessions SET webauthn_state = $2 WHERE id_sha256 = $1;

-- name: GetWebAuthnState :one
SELECT webauthn_state FROM sessions WHERE id_sha256 = $1;
