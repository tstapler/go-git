package filesystem

import (
	"io"
	"sync"
	"sync/atomic"
)

// defaultMaxIdlePackHandles bounds the open packfile handles kept idle between reads when
// neither KeepDescriptors nor MaxOpenDescriptors is set.
const defaultMaxIdlePackHandles = 32

// packHandlePool keeps a bounded number of idle, open packfile handles so repeated object reads
// do not reopen the pack file each time. A handle is borrowed exclusively (a Packfile carries
// seek state, so it is never shared between concurrent reads) and returned afterwards; handles
// beyond the idle limit are closed immediately, so open descriptors never exceed the limit plus
// the reads in flight. Close releases every idle handle.
type packHandlePool[K comparable, T io.Closer] struct {
	mu      sync.Mutex
	idle    map[K][]T
	order   []K // packs with idle handles, least recently returned first
	nIdle   int
	shut    bool // after Close, returned handles are closed instead of kept
	max     int
	open    atomic.Int64
	onCount func(open int64)
}

func newPackHandlePool[K comparable, T io.Closer](max int, onCount func(open int64)) *packHandlePool[K, T] {
	return &packHandlePool[K, T]{idle: make(map[K][]T), max: max, onCount: onCount}
}

// get returns an idle handle for pack, or nil when none is idle.
func (p *packHandlePool[K, T]) get(pack K) (h T, ok bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	list := p.idle[pack]
	if len(list) == 0 {
		return h, false
	}
	h = list[len(list)-1]
	p.idle[pack] = list[:len(list)-1]
	if len(p.idle[pack]) == 0 {
		delete(p.idle, pack)
		p.dropFromOrder(pack)
	}
	p.nIdle--
	return h, true
}

func (p *packHandlePool[K, T]) dropFromOrder(pack K) {
	for i, h := range p.order {
		if h == pack {
			p.order = append(p.order[:i], p.order[i+1:]...)
			return
		}
	}
}

// opened and closed track the open-handle gauge.
func (p *packHandlePool[K, T]) opened() { p.report(p.open.Add(1)) }
func (p *packHandlePool[K, T]) closed() { p.report(p.open.Add(-1)) }

func (p *packHandlePool[K, T]) report(n int64) {
	if p.onCount != nil {
		p.onCount(n)
	}
}

// put returns h to the pool. When the pool is full it makes room by closing an idle handle of the
// least recently used pack (a repository can have many packs; a per-pack-only cache would starve
// all but the first few), and closes h itself only when it is the only candidate.
func (p *packHandlePool[K, T]) put(pack K, h T) error {
	var evict T
	var evicting bool
	p.mu.Lock()
	if p.shut {
		p.mu.Unlock()
		p.closed()
		return h.Close()
	}
	if p.nIdle >= p.max {
		victimPack, ok := p.lruOther(pack)
		if !ok {
			p.mu.Unlock()
			p.closed()
			return h.Close()
		}
		list := p.idle[victimPack]
		evict, evicting = list[0], true
		p.idle[victimPack] = list[1:]
		if len(p.idle[victimPack]) == 0 {
			delete(p.idle, victimPack)
			p.dropFromOrder(victimPack)
		}
		p.nIdle--
	}
	if len(p.idle[pack]) == 0 {
		p.order = append(p.order, pack)
	}
	p.idle[pack] = append(p.idle[pack], h)
	p.nIdle++
	p.mu.Unlock()
	if evicting {
		p.closed()
		return evict.Close()
	}
	return nil
}

// lruOther returns the least recently used pack with idle handles other than pack.
func (p *packHandlePool[K, T]) lruOther(pack K) (k K, ok bool) {
	for _, h := range p.order {
		if h != pack {
			return h, true
		}
	}
	return k, false
}

// closeAll closes every idle handle (the pack set changed, or the storage is closing).
func (p *packHandlePool[K, T]) closeAll() error {
	p.mu.Lock()
	idle := p.idle
	p.idle = make(map[K][]T)
	p.order = nil
	p.nIdle = 0
	p.mu.Unlock()

	var first error
	for _, list := range idle {
		for _, h := range list {
			p.closed()
			if err := h.Close(); first == nil && err != nil {
				first = err
			}
		}
	}
	return first
}

// shutdown closes every idle handle and makes later returns close theirs.
func (p *packHandlePool[K, T]) shutdown() error {
	p.mu.Lock()
	p.shut = true
	p.mu.Unlock()
	return p.closeAll()
}
