package auth

import (
	"sync"

	"github.com/google/uuid"
)

type touchSet struct {
	mu  sync.Mutex
	ids map[uuid.UUID]struct{}
}

func newTouchSet() *touchSet { return &touchSet{ids: map[uuid.UUID]struct{}{}} }

func (t *touchSet) add(id uuid.UUID) {
	t.mu.Lock()
	if len(t.ids) < 10000 {
		t.ids[id] = struct{}{}
	}
	t.mu.Unlock()
}

func (t *touchSet) drain() []uuid.UUID {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]uuid.UUID, 0, len(t.ids))
	for id := range t.ids {
		out = append(out, id)
	}
	t.ids = map[uuid.UUID]struct{}{}
	return out
}
