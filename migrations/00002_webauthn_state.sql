-- +cms expand
-- Passkeys: holds the in-flight WebAuthn ceremony (challenge) per session.
-- Nullable column with no default: no table rewrite, safe on a live database.
-- +goose Up
ALTER TABLE sessions ADD COLUMN webauthn_state jsonb;

-- +goose Down
ALTER TABLE sessions DROP COLUMN webauthn_state;
