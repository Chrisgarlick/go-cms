package app

import (
	"context"

	"cms/internal/actor"
	"cms/internal/rbac"
)

// RotateKeys re-encrypts every envelope-encrypted value under the newest KEK.
func (a *App) RotateKeys(ctx context.Context) (int, error) {
	if _, err := actor.Require(ctx, rbac.KeysManage); err != nil {
		return 0, err
	}
	n := 0
	rows, err := a.DB.Pool.Query(ctx, `SELECT id, totp_enc, totp_pending_enc FROM users WHERE totp_enc IS NOT NULL OR totp_pending_enc IS NOT NULL`)
	if err != nil {
		return 0, err
	}
	type u struct {
		id        string
		enc, pend []byte
	}
	var us []u
	for rows.Next() {
		var x u
		if err := rows.Scan(&x.id, &x.enc, &x.pend); err != nil {
			rows.Close()
			return n, err
		}
		us = append(us, x)
	}
	rows.Close()
	for _, x := range us {
		aad := []byte("users:" + x.id + ":totp")
		for col, val := range map[string][]byte{"totp_enc": x.enc, "totp_pending_enc": x.pend} {
			if len(val) == 0 || !a.Keys.NeedsRewrap(val) {
				continue
			}
			nv, err := a.Keys.Rewrap(val, aad)
			if err != nil {
				return n, err
			}
			if _, err := a.DB.Pool.Exec(ctx, "UPDATE users SET "+col+" = $1 WHERE id = $2", nv, x.id); err != nil {
				return n, err
			}
			n++
		}
	}
	hooks, err := a.DB.Q.ListWebhooks(ctx)
	if err != nil {
		return n, err
	}
	for _, h := range hooks {
		if !a.Keys.NeedsRewrap(h.SecretEnc) {
			continue
		}
		nv, err := a.Keys.Rewrap(h.SecretEnc, []byte("webhooks:"+h.ID.String()+":secret"))
		if err != nil {
			return n, err
		}
		if _, err := a.DB.Pool.Exec(ctx, "UPDATE webhooks SET secret_enc = $1 WHERE id = $2", nv, h.ID); err != nil {
			return n, err
		}
		n++
	}
	secs, err := a.DB.Q.ListSecrets(ctx)
	if err != nil {
		return n, err
	}
	for _, s := range secs {
		if !a.Keys.NeedsRewrap(s.ValueEnc) {
			continue
		}
		nv, err := a.Keys.Rewrap(s.ValueEnc, []byte("secrets:"+s.Name))
		if err != nil {
			return n, err
		}
		if _, err := a.DB.Pool.Exec(ctx, "UPDATE secrets SET value_enc = $1, rotated_at = now() WHERE name = $2", nv, s.Name); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
