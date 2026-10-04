package bevdirect

// Store is a small expiring LRU used by bevdirect-serve for assembled
// viewports, keyed by the hash of the bbox + layers they cover. It is NEVER
// keyed by parcel id, EZ or KG — there is deliberately no per-parcel state on
// the fallback host, so nothing can be retrieved except by asking for a bbox,
// exactly as with a browser cache of the BEV map.

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

type storeEntry struct {
	key  string
	val  any
	exp  time.Time
	elem *list.Element
}

type Store struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	items map[string]*storeEntry
	lru   *list.List
}

func NewStore(ttl time.Duration, max int) *Store {
	return &Store{ttl: ttl, max: max, items: map[string]*storeEntry{}, lru: list.New()}
}

// Hash is sha256 hex of s — the only form in which keys are kept.
func Hash(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func (s *Store) Get(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.items[key]
	if !ok {
		return nil, false
	}
	if time.Now().After(e.exp) {
		s.lru.Remove(e.elem)
		delete(s.items, key)
		return nil, false
	}
	s.lru.MoveToFront(e.elem)
	return e.val, true
}

func (s *Store) Put(key string, val any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.items[key]; ok {
		e.val, e.exp = val, time.Now().Add(s.ttl)
		s.lru.MoveToFront(e.elem)
		return
	}
	e := &storeEntry{key: key, val: val, exp: time.Now().Add(s.ttl)}
	e.elem = s.lru.PushFront(e)
	s.items[key] = e
	for s.lru.Len() > s.max {
		last := s.lru.Back()
		s.lru.Remove(last)
		delete(s.items, last.Value.(*storeEntry).key)
	}
}

func (s *Store) Len() int { s.mu.Lock(); defer s.mu.Unlock(); return s.lru.Len() }
