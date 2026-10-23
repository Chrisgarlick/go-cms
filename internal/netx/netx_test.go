package netx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
	"time"
)

func TestAllowed(t *testing.T) {
	for _, s := range []string{"127.0.0.1", "10.1.2.3", "169.254.169.254", "192.168.1.1", "::1", "fd00:ec2::254", "::ffff:127.0.0.1", "0.0.0.0"} {
		if Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s allowed", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !Allowed(netip.MustParseAddr(s)) {
			t.Errorf("%s blocked", s)
		}
	}
}

func TestClientRefusesLoopback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	_, _, err := Get(context.Background(), Client(2*time.Second), srv.URL, 100)
	if !errors.Is(err, ErrBlockedAddress) {
		t.Fatalf("expected blocked address, got %v", err)
	}
}
