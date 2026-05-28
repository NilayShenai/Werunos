package host

import (
	"sync"
	"sync/atomic"
)

type HandleTable[V any] struct {

	mu sync.Map

	next atomic.Uint64
}

func (t *HandleTable[V]) Store(v V) uint64 {

	h := t.next.Add(1)
	t.mu.Store(h, v)
	return h
}

func (t *HandleTable[V]) Replace(h uint64, v V) {
	t.mu.Store(h, v)
}

func (t *HandleTable[V]) Load(h uint64) (V, bool) {
	raw, ok := t.mu.Load(h)
	if !ok {
		var zero V
		return zero, false
	}
	return raw.(V), true
}

func (t *HandleTable[V]) Delete(h uint64) {
	t.mu.Delete(h)
}
