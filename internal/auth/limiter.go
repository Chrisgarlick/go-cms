package auth

import (
	"sync"
	"time"
)

// Limiter is a fixed-window counter per key, held in memory (single node).
// It bounds its own size so a flood of distinct keys cannot grow it forever.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]*bucket
	maxKeys int
}

type bucket struct {
	count int
	reset time.Time
}

func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{limit: limit, window: window, buckets: map[string]*bucket{}, maxKeys: 50000}
}

// Allow records one attempt and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	if b == nil || now.After(b.reset) {
		if len(l.buckets) >= l.maxKeys {
			l.sweep(now)
			if len(l.buckets) >= l.maxKeys {
				return false // fail closed under a key flood
			}
		}
		b = &bucket{reset: now.Add(l.window)}
		l.buckets[key] = b
	}
	b.count++
	return b.count <= l.limit
}

// Blocked reports whether key is over the limit without recording an attempt.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	b := l.buckets[key]
	return b != nil && time.Now().Before(b.reset) && b.count >= l.limit
}

// Reset clears a key (after a successful login).
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	delete(l.buckets, key)
	l.mu.Unlock()
}

func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.After(b.reset) {
			delete(l.buckets, k)
		}
	}
}
