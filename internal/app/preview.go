package app

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"time"

	"github.com/google/uuid"
)

var errBadPreview = errors.New("invalid preview token")

// PreviewToken signs an entry id with an expiry; anyone with the link can view
// the working copy until it expires (spec 4.1).
func (a *App) PreviewToken(id uuid.UUID, ttl time.Duration) string {
	buf := make([]byte, 24, 40)
	copy(buf, id[:])
	binary.BigEndian.PutUint64(buf[16:], uint64(time.Now().Add(ttl).Unix()))
	m := hmac.New(sha256.New, a.Keys.Derive("preview"))
	m.Write(buf)
	buf = append(buf, m.Sum(nil)[:16]...)
	return base64.RawURLEncoding.EncodeToString(buf)
}

func (a *App) verifyPreview(tok string) (uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(b) != 40 {
		return uuid.Nil, errBadPreview
	}
	m := hmac.New(sha256.New, a.Keys.Derive("preview"))
	m.Write(b[:24])
	if !hmac.Equal(m.Sum(nil)[:16], b[24:]) {
		return uuid.Nil, errBadPreview
	}
	if time.Now().Unix() > int64(binary.BigEndian.Uint64(b[16:24])) {
		return uuid.Nil, errBadPreview
	}
	id, _ := uuid.FromBytes(b[:16])
	return id, nil
}
