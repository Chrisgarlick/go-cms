// Package events broadcasts cache-invalidation signals between processes
// (blue/green web instances and the worker) using Postgres LISTEN/NOTIFY.
// Notifications sent inside a transaction are delivered only on commit.
package events

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const channel = "cms_events"

// Topics.
const (
	RedirectsChanged = "redirects.changed"
	MenusChanged     = "menus.changed"
	SettingsChanged  = "settings.changed"
	OptionsChanged   = "options.changed"
	UsersChanged     = "users.changed"
)

type Event struct {
	Topic string `json:"t"`
	ID    string `json:"id,omitempty"`
}

type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (any, error)
}

// Notify sends an event; with a tx it is delivered on commit.
func Notify(ctx context.Context, tx pgx.Tx, e Event) error {
	b, _ := json.Marshal(e)
	_, err := tx.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(b))
	return err
}

// NotifyPool sends an event outside a transaction.
func NotifyPool(ctx context.Context, pool *pgxpool.Pool, e Event) error {
	b, _ := json.Marshal(e)
	_, err := pool.Exec(ctx, "SELECT pg_notify($1, $2)", channel, string(b))
	return err
}

// Bus dispatches events to in-process subscribers.
type Bus struct {
	mu   sync.RWMutex
	subs map[string][]func(Event)
	log  *slog.Logger
}

func NewBus(log *slog.Logger) *Bus { return &Bus{subs: map[string][]func(Event){}, log: log} }

func (b *Bus) Subscribe(topic string, fn func(Event)) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs[topic] = append(b.subs[topic], fn)
}

func (b *Bus) dispatch(e Event) {
	b.mu.RLock()
	fns := b.subs[e.Topic]
	b.mu.RUnlock()
	for _, fn := range fns {
		func() {
			defer func() {
				if r := recover(); r != nil {
					b.log.Error("event handler panic", "topic", e.Topic, "panic", r)
				}
			}()
			fn(e)
		}()
	}
}

// Listen holds one connection and dispatches notifications until ctx ends,
// reconnecting with backoff. After a reconnect every topic is dispatched once,
// because notifications may have been missed while disconnected.
func (b *Bus) Listen(ctx context.Context, pool *pgxpool.Pool) {
	backoff := time.Second
	first := true
	for ctx.Err() == nil {
		err := b.listenOnce(ctx, pool, !first)
		first = false
		if ctx.Err() != nil {
			return
		}
		b.log.Warn("event listener disconnected", "err", err, "retry_in", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (b *Bus) listenOnce(ctx context.Context, pool *pgxpool.Pool, resync bool) error {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN "+channel); err != nil {
		return err
	}
	if resync {
		b.mu.RLock()
		topics := make([]string, 0, len(b.subs))
		for t := range b.subs {
			topics = append(topics, t)
		}
		b.mu.RUnlock()
		for _, t := range topics {
			b.dispatch(Event{Topic: t})
		}
	}
	for {
		n, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}
		var e Event
		if json.Unmarshal([]byte(n.Payload), &e) == nil {
			b.dispatch(e)
		}
	}
}

// Local dispatches without Postgres (single-process use and tests).
func (b *Bus) Local(e Event) { b.dispatch(e) }
