// Package keys implements envelope encryption (spec 7.3). A key-encryption key
// (KEK) loaded from a file wraps a fresh data-encryption key (DEK) per value.
// Ciphertexts bind the caller's associated data (usually "table:id:column"),
// so they cannot be moved between rows.
package keys

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
)

const (
	formatV1 = 1
	keyLen   = 32
)

var ErrDecrypt = errors.New("keys: decryption failed")

// Keyring holds one or more KEK versions. New values use the highest version.
type Keyring struct {
	keks    map[uint32][]byte
	current uint32
}

// Load reads a KEK file. Format: either one 64-char hex key (version 1), or
// lines "version:hex" for rotation. Blank lines and # comments are ignored.
func Load(path string) (*Keyring, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("keys: %w", err)
	}
	return Parse(b)
}

func Parse(b []byte) (*Keyring, error) {
	kr := &Keyring{keks: map[uint32][]byte{}}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		ver := uint32(1)
		if v, k, ok := strings.Cut(line, ":"); ok {
			n, err := strconv.ParseUint(v, 10, 32)
			if err != nil || n == 0 {
				return nil, fmt.Errorf("keys: bad version %q", v)
			}
			ver, line = uint32(n), k
		}
		k, err := hex.DecodeString(line)
		if err != nil || len(k) != keyLen {
			return nil, errors.New("keys: each KEK must be 64 hex characters (32 bytes)")
		}
		if _, dup := kr.keks[ver]; dup {
			return nil, fmt.Errorf("keys: duplicate version %d", ver)
		}
		kr.keks[ver] = k
		if ver > kr.current {
			kr.current = ver
		}
	}
	if kr.current == 0 {
		return nil, errors.New("keys: no key found")
	}
	return kr, nil
}

// Generate returns a new random KEK in file format.
func Generate(version uint32) string {
	k := make([]byte, keyLen)
	if _, err := rand.Read(k); err != nil {
		panic(err)
	}
	return fmt.Sprintf("%d:%s\n", version, hex.EncodeToString(k))
}

// Versions lists the loaded KEK versions, newest last.
func (kr *Keyring) Versions() []uint32 {
	var vs []uint32
	for v := range kr.keks {
		vs = append(vs, v)
	}
	sort.Slice(vs, func(i, j int) bool { return vs[i] < vs[j] })
	return vs
}

func (kr *Keyring) Current() uint32 { return kr.current }

// Encrypt seals plaintext. Layout:
// v1 | kekVersion(4) | wrapNonce(12) | wrappedDEK(48) | nonce(12) | ciphertext
func (kr *Keyring) Encrypt(plaintext, aad []byte) ([]byte, error) {
	dek := make([]byte, keyLen)
	if _, err := rand.Read(dek); err != nil {
		return nil, err
	}
	kek := kr.keks[kr.current]
	wrapNonce, wrapped, err := seal(kek, dek, wrapAAD(kr.current, aad))
	if err != nil {
		return nil, err
	}
	nonce, ct, err := seal(dek, plaintext, aad)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteByte(formatV1)
	_ = binary.Write(&buf, binary.BigEndian, kr.current)
	buf.Write(wrapNonce)
	buf.Write(wrapped)
	buf.Write(nonce)
	buf.Write(ct)
	return buf.Bytes(), nil
}

// Decrypt opens a value sealed by Encrypt with the same aad.
func (kr *Keyring) Decrypt(blob, aad []byte) ([]byte, error) {
	const hdr = 1 + 4 + 12 + 48 + 12
	if len(blob) < hdr || blob[0] != formatV1 {
		return nil, ErrDecrypt
	}
	ver := binary.BigEndian.Uint32(blob[1:5])
	kek, ok := kr.keks[ver]
	if !ok {
		return nil, fmt.Errorf("keys: KEK version %d not loaded", ver)
	}
	dek, err := open(kek, blob[5:17], blob[17:65], wrapAAD(ver, aad))
	if err != nil {
		return nil, ErrDecrypt
	}
	pt, err := open(dek, blob[65:77], blob[77:], aad)
	if err != nil {
		return nil, ErrDecrypt
	}
	return pt, nil
}

// NeedsRewrap reports whether blob was sealed with an older KEK.
func (kr *Keyring) NeedsRewrap(blob []byte) bool {
	return len(blob) > 5 && binary.BigEndian.Uint32(blob[1:5]) != kr.current
}

// Rewrap re-seals blob under the current KEK (used by `cms keys rotate`).
func (kr *Keyring) Rewrap(blob, aad []byte) ([]byte, error) {
	pt, err := kr.Decrypt(blob, aad)
	if err != nil {
		return nil, err
	}
	return kr.Encrypt(pt, aad)
}

func wrapAAD(ver uint32, aad []byte) []byte {
	return append([]byte(fmt.Sprintf("cms-kek:%d:", ver)), aad...)
}

func seal(key, pt, aad []byte) (nonce, ct []byte, err error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, err
	}
	nonce = make([]byte, g.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return nonce, g.Seal(nil, nonce, pt, aad), nil
}

func open(key, nonce, ct, aad []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	g, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return g.Open(nil, nonce, ct, aad)
}

// Derive returns a purpose-specific 32-byte key from the current KEK
// (HMAC-SHA256(KEK, "cms-derive:"+label)). Used for signing preview links.
func (kr *Keyring) Derive(label string) []byte {
	m := hmac.New(sha256.New, kr.keks[kr.current])
	m.Write([]byte("cms-derive:" + label))
	return m.Sum(nil)
}
