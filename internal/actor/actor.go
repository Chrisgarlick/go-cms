// Package actor carries who is acting through every service call (spec 2.3).
// Services call Require; handlers never check permissions themselves.
package actor

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"os/user"

	"github.com/google/uuid"

	"cms/internal/rbac"
)

type Kind string

const (
	KindUser      Kind = "user"
	KindToken     Kind = "token"
	KindSystem    Kind = "system"
	KindAnonymous Kind = "anonymous"
)

type Actor struct {
	Kind    Kind
	UserID  uuid.UUID // for user and token actors
	TokenID uuid.UUID // for token actors
	Role    rbac.Role // user/token role; system actors use Caps
	Caps    map[rbac.Cap]bool
	Label   string // email, token name, or "cli:chris@host"
	IP      netip.Addr
	// Sudo is true while the session is in re-authenticated "sudo mode".
	Sudo bool
}

// ErrForbidden is returned for any authorisation failure.
type ErrForbidden struct{ Cap rbac.Cap }

func (e *ErrForbidden) Error() string { return fmt.Sprintf("forbidden: requires %s", e.Cap) }

// ErrSudoRequired means the action needs a fresh re-authentication.
var ErrSudoRequired = errors.New("re-authentication required")

var ErrUnauthenticated = errors.New("not signed in")

func (a *Actor) Can(c rbac.Cap) bool {
	if a == nil {
		return false
	}
	switch a.Kind {
	case KindSystem:
		return a.Caps[c]
	case KindUser, KindToken:
		return rbac.Has(a.Role, c)
	}
	return false
}

// ID is a stable string for audit records.
func (a *Actor) ID() string {
	switch a.Kind {
	case KindUser:
		return a.UserID.String()
	case KindToken:
		return a.TokenID.String()
	case KindSystem:
		return a.Label
	}
	return "anonymous"
}

func (a *Actor) IsUser() bool { return a != nil && (a.Kind == KindUser || a.Kind == KindToken) }

type ctxKey struct{}

func With(ctx context.Context, a *Actor) context.Context { return context.WithValue(ctx, ctxKey{}, a) }

// From returns the actor, or an anonymous actor when none is set.
func From(ctx context.Context) *Actor {
	if a, ok := ctx.Value(ctxKey{}).(*Actor); ok && a != nil {
		return a
	}
	return &Actor{Kind: KindAnonymous, Label: "anonymous"}
}

// Require returns the actor if it holds cap, otherwise a typed error.
func Require(ctx context.Context, c rbac.Cap) (*Actor, error) {
	a := From(ctx)
	if a.Kind == KindAnonymous {
		return nil, ErrUnauthenticated
	}
	if !a.Can(c) {
		return nil, &ErrForbidden{Cap: c}
	}
	return a, nil
}

// RequireSudo is Require plus sudo mode for interactive users. System actors
// (CLI on the server) already hold shell access and are exempt.
func RequireSudo(ctx context.Context, c rbac.Cap) (*Actor, error) {
	a, err := Require(ctx, c)
	if err != nil {
		return nil, err
	}
	if a.Kind == KindUser && !a.Sudo {
		return nil, ErrSudoRequired
	}
	return a, nil
}

// System returns a system actor with every capability of the owner role.
// Its label records the OS user and host so CLI actions are attributable.
func System(component string) *Actor {
	return &Actor{Kind: KindSystem, Caps: capSet(rbac.Caps(rbac.Owner)), Label: component + ":" + osIdentity()}
}

// SystemWith returns a system actor limited to the given capabilities.
func SystemWith(component string, caps ...rbac.Cap) *Actor {
	return &Actor{Kind: KindSystem, Caps: capSet(caps), Label: component + ":" + osIdentity()}
}

func capSet(cs []rbac.Cap) map[rbac.Cap]bool {
	m := make(map[rbac.Cap]bool, len(cs))
	for _, c := range cs {
		m[c] = true
	}
	return m
}

func osIdentity() string {
	name := "unknown"
	if u, err := user.Current(); err == nil {
		name = u.Username
	}
	host, _ := os.Hostname()
	return name + "@" + host
}
