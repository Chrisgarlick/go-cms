package auth_test

import (
	"context"
	"errors"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/pquerna/otp/totp"

	"cms/internal/actor"
	"cms/internal/auth"
	"cms/internal/keys"
	"cms/internal/mail"
	"cms/internal/rbac"
	"cms/internal/testutil"
)

func TestMain(m *testing.M) { testutil.Main(m) }

var ip = netip.MustParseAddr("192.0.2.1")

func newSvc(t *testing.T) (*auth.Service, *mail.Log) {
	d := testutil.NewDB(t)
	kr, _ := keys.Parse([]byte(keys.Generate(1)))
	ml := &mail.Log{}
	return auth.New(&auth.Service{DB: d.DB, Keys: kr, Mail: ml, BaseURL: "https://example.test", Issuer: "Test"}), ml
}

func TestInviteEnrolLoginFlow(t *testing.T) {
	s, ml := newSvc(t)
	sys := actor.With(context.Background(), actor.System("test"))
	_, link, err := s.Invite(sys, auth.InviteInput{Email: "Owner@Example.test", Name: "Owner", Role: rbac.Owner})
	if err != nil {
		t.Fatal(err)
	}
	if m, ok := ml.Last(); !ok || !strings.Contains(m.Text, link) {
		t.Fatal("invite email not sent")
	}
	tok := link[strings.LastIndex(link, "/")+1:]
	if err := s.AcceptInviteCheck(tok); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcceptInvite(context.Background(), tok, "short", "", ip, "ua"); !errors.Is(err, auth.ErrWeakPassword) {
		t.Fatalf("weak password accepted: %v", err)
	}
	enrollTok, err := s.AcceptInvite(context.Background(), tok, "a much longer passphrase!", "", ip, "ua")
	if err != nil {
		t.Fatal(err)
	}
	// Token is single-use.
	if _, err := s.AcceptInvite(context.Background(), tok, "a much longer passphrase!", "", ip, "ua"); err == nil {
		t.Fatal("invite reused")
	}
	sess, err := s.Authenticate(context.Background(), enrollTok)
	if err != nil || sess.Stage != auth.StageEnroll {
		t.Fatalf("enroll session: %+v %v", sess, err)
	}
	en, err := s.BeginEnroll(context.Background(), sess.UserID)
	if err != nil {
		t.Fatal(err)
	}
	code, _ := totp.GenerateCode(en.Secret, time.Now())
	full, codes, err := s.ConfirmEnroll(context.Background(), sess, code, ip, "ua")
	if err != nil || full == "" || len(codes) != 10 {
		t.Fatalf("confirm: %v %d", err, len(codes))
	}
	if _, err := s.Authenticate(context.Background(), enrollTok); err == nil {
		t.Fatal("pre-auth token still valid after upgrade")
	}
	// Fresh login needs the second factor.
	res, err := s.Login(context.Background(), "owner@example.test", "a much longer passphrase!", ip, "ua")
	if err != nil || res.Stage != auth.StageMFA {
		t.Fatalf("login: %+v %v", res, err)
	}
	mfa, _ := s.Authenticate(context.Background(), res.Token)
	// The enrolment code was just used in this time step: replay is rejected.
	if _, err := s.VerifyTOTP(context.Background(), mfa, code, ip, "ua"); err == nil {
		t.Fatal("TOTP replay accepted")
	}
	if _, err := s.UseRecoveryCode(context.Background(), mfa, codes[0], ip, "ua"); err != nil {
		t.Fatalf("recovery code: %v", err)
	}
	// Recovery codes are single-use.
	res, _ = s.Login(context.Background(), "owner@example.test", "a much longer passphrase!", ip, "ua")
	mfa, _ = s.Authenticate(context.Background(), res.Token)
	if _, err := s.UseRecoveryCode(context.Background(), mfa, codes[0], ip, "ua"); err == nil {
		t.Fatal("recovery code reused")
	}
	// Wrong password.
	if _, err := s.Login(context.Background(), "owner@example.test", "wrong password here", ip, "ua"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Fatalf("wrong password: %v", err)
	}
	// Last owner cannot be demoted.
	u, _ := s.User(sys, sess.UserID)
	if err := s.SetRole(sys, u.ID, rbac.Admin); !errors.Is(err, auth.ErrLastOwner) {
		t.Fatalf("demoted last owner: %v", err)
	}
}

func TestResetIsBrowserBound(t *testing.T) {
	s, ml := newSvc(t)
	sys := actor.With(context.Background(), actor.System("test"))
	if _, err := s.CreateUserDirect(sys, "ed@example.test", "Ed", "correct horse battery staple 9", rbac.Editor); err != nil {
		t.Fatal(err)
	}
	if err := s.RequestReset(context.Background(), "ed@example.test", "nonce-a", ip); err != nil {
		t.Fatal(err)
	}
	m, _ := ml.Last()
	link := strings.Fields(m.Text[strings.Index(m.Text, "https://"):])[0]
	tok := link[strings.LastIndex(link, "/")+1:]
	if err := s.CompleteReset(context.Background(), tok, "nonce-b", "another long passphrase 1"); err == nil {
		t.Fatal("reset accepted from another browser")
	}
	// Unknown email: no error, no email (no enumeration).
	before := len(ml.Sent)
	if err := s.RequestReset(context.Background(), "nobody@example.test", "n", ip); err != nil || len(ml.Sent) != before {
		t.Fatal("reset leaked account existence")
	}
}

func TestAPITokens(t *testing.T) {
	s, _ := newSvc(t)
	sys := actor.With(context.Background(), actor.System("test"))
	u, err := s.CreateUserDirect(sys, "ad@example.test", "Ad", "correct horse battery staple 9", rbac.Admin)
	if err != nil {
		t.Fatal(err)
	}
	userCtx := actor.With(context.Background(), &actor.Actor{Kind: actor.KindUser, UserID: u.ID, Role: rbac.Admin, Sudo: true})
	plain, _, err := s.CreateToken(userCtx, "ci", rbac.Owner, 0)
	if err != nil {
		t.Fatal(err)
	}
	a, err := s.AuthenticateToken(context.Background(), plain)
	if err != nil || a.Role != rbac.Admin {
		t.Fatalf("token auth: %+v %v (role must be capped at admin)", a, err)
	}
	if _, err := s.AuthenticateToken(context.Background(), plain+"x"); err == nil {
		t.Fatal("tampered token accepted")
	}
	noSudo := actor.With(context.Background(), &actor.Actor{Kind: actor.KindUser, UserID: u.ID, Role: rbac.Admin})
	if _, _, err := s.CreateToken(noSudo, "x", rbac.Admin, 0); !errors.Is(err, actor.ErrSudoRequired) {
		t.Fatalf("token without sudo: %v", err)
	}
}
