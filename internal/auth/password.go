package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
)

// argon2id parameters: OWASP's minimum-memory profile (19 MiB, t=2). Hashing
// is limited to two at a time so a burst of logins cannot exhaust memory on a
// 1 GB server (spec 8.2).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
	saltLen      = 16
)

var hashSem = make(chan struct{}, 2)

var ErrWeakPassword = errors.New("password is too weak")

// HashPassword returns a PHC-format argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, saltLen)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	hashSem <- struct{}{}
	key := argon2.IDKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	<-hashSem
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// dummyHash is verified against when the user does not exist, so response
// time does not reveal which emails have accounts.
var dummyHash, _ = HashPassword("timing-equaliser")

// VerifyPassword checks pw against an encoded hash in constant time.
func VerifyPassword(encoded, pw string) bool {
	if encoded == "" {
		encoded = dummyHash
		pw += "\x00never"
	}
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return false
	}
	if m > 256*1024 || t > 10 || p > 8 { // refuse absurd stored parameters
		return false
	}
	hashSem <- struct{}{}
	got := argon2.IDKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	<-hashSem
	return subtle.ConstantTimeCompare(got, want) == 1
}

// CheckPasswordStrength enforces length and blocks very common passwords.
// A breached-password check (HIBP k-anonymity) runs separately when enabled.
func CheckPasswordStrength(pw, email string) error {
	n := utf8.RuneCountInString(pw)
	if n < 12 {
		return fmt.Errorf("%w: use at least 12 characters", ErrWeakPassword)
	}
	if n > 256 {
		return fmt.Errorf("%w: use at most 256 characters", ErrWeakPassword)
	}
	low := strings.ToLower(pw)
	if local, _, ok := strings.Cut(strings.ToLower(email), "@"); ok && len(local) >= 4 && strings.Contains(low, local) {
		return fmt.Errorf("%w: do not include your email address", ErrWeakPassword)
	}
	for _, c := range commonPasswords {
		if low == c {
			return fmt.Errorf("%w: this password is too common", ErrWeakPassword)
		}
	}
	distinct := map[rune]bool{}
	for _, r := range pw {
		distinct[r] = true
	}
	if len(distinct) < 5 {
		return fmt.Errorf("%w: use more varied characters", ErrWeakPassword)
	}
	return nil
}

var commonPasswords = []string{
	"password1234", "passwordpassword", "123456789012", "qwertyuiopas", "letmeinletmein",
	"iloveyou1234", "welcome12345", "administrator", "changeme1234", "correcthorsebatterystaple",
}

// Breached reports whether a password appears in the Have I Been Pwned corpus
// using k-anonymity: only the first 5 hex characters of its SHA-1 leave the
// server. client must be the hardened outbound client.
func Breached(ctx context.Context, client *http.Client, pw string) (bool, error) {
	sum := sha1.Sum([]byte(pw))
	h := strings.ToUpper(hex.EncodeToString(sum[:]))
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.pwnedpasswords.com/range/"+h[:5], nil)
	req.Header.Set("Add-Padding", "true")
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("hibp: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(string(body), "\n") {
		suffix, count, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && suffix == h[5:] && count != "0" {
			return true, nil
		}
	}
	return false, nil
}

// RandomToken returns n random bytes, base64url-encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
