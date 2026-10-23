// Package counters batches hit counts in memory and flushes them periodically,
// so hot paths never write to the database per request (spec 8.3).
package counters

import (
	"context"
	"sync"
)

// Counter counts by key with a hard cap on distinct keys per window.
type Counter[K comparable] struct {
	mu      sync.Mutex
	counts  map[K]int64
	maxKeys int
	dropped int64
}

func New[K comparable](maxKeys int) *Counter[K] {
	return &Counter[K]{counts: map[K]int64{}, maxKeys: maxKeys}
}

// Add increments key; new keys beyond the cap are dropped (sampled away).
func (c *Counter[K]) Add(k K, n int64) {
	c.mu.Lock()
	if _, ok := c.counts[k]; ok || len(c.counts) < c.maxKeys {
		c.counts[k] += n
	} else {
		c.dropped += n
	}
	c.mu.Unlock()
}

// Drain returns and resets the counts.
func (c *Counter[K]) Drain() (map[K]int64, int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, dropped := c.counts, c.dropped
	c.counts, c.dropped = make(map[K]int64, len(out)), 0
	return out, dropped
}

// Flush drains and hands each count to fn; failed keys are re-added.
func (c *Counter[K]) Flush(ctx context.Context, fn func(context.Context, K, int64) error) error {
	counts, _ := c.Drain()
	var firstErr error
	for k, n := range counts {
		if err := fn(ctx, k, n); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			c.Add(k, n)
		}
	}
	return firstErr
}
