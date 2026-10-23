package keys

import (
	"bytes"
	"testing"
)

func TestRoundTripAndBinding(t *testing.T) {
	kr, err := Parse([]byte(Generate(1)))
	if err != nil {
		t.Fatal(err)
	}
	ct, err := kr.Encrypt([]byte("secret"), []byte("webhooks:1:secret"))
	if err != nil {
		t.Fatal(err)
	}
	pt, err := kr.Decrypt(ct, []byte("webhooks:1:secret"))
	if err != nil || string(pt) != "secret" {
		t.Fatalf("round trip: %q %v", pt, err)
	}
	if _, err := kr.Decrypt(ct, []byte("webhooks:2:secret")); err == nil {
		t.Fatal("ciphertext moved to another row decrypted")
	}
	ct[len(ct)-1] ^= 1
	if _, err := kr.Decrypt(ct, []byte("webhooks:1:secret")); err == nil {
		t.Fatal("tampered ciphertext decrypted")
	}
}

func TestRotation(t *testing.T) {
	v1 := Generate(1)
	old, _ := Parse([]byte(v1))
	ct, _ := old.Encrypt([]byte("x"), []byte("a"))
	both, err := Parse([]byte(v1 + Generate(2)))
	if err != nil {
		t.Fatal(err)
	}
	if both.Current() != 2 || !both.NeedsRewrap(ct) {
		t.Fatal("expected v2 current and rewrap needed")
	}
	nct, err := both.Rewrap(ct, []byte("a"))
	if err != nil {
		t.Fatal(err)
	}
	pt, _ := both.Decrypt(nct, []byte("a"))
	if !bytes.Equal(pt, []byte("x")) || both.NeedsRewrap(nct) {
		t.Fatal("rewrap failed")
	}
}

func TestParseRejectsBadKeys(t *testing.T) {
	for _, in := range []string{"", "abc", "0:" + Generate(1)[2:], Generate(1) + Generate(1)} {
		if _, err := Parse([]byte(in)); err == nil {
			t.Fatalf("accepted %q", in)
		}
	}
}
