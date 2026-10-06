package bevdirect

import (
	"container/list"
	"sync"
	"time"
)

// TileCache is a bounded, TTL-expiring, in-memory LRU of raw tile bytes.
//
// Tiles are deliberately never written to disk: the only persistent artefact a
// bevdirect process may leave behind is nothing at all. A tile lives in RAM
// for at most TTL (BEV serves Cache-Control: no-cache; the cadastre changes at
// most monthly) and the whole cache is capped in bytes, oldest-used first.
type TileCache struct {
	mu    sync.Mutex
	max   int64
	ttl   time.Duration
	size  int64
	ll    *list.List
	items map[string]*list.Element
	hits  uint64
	miss  uint64
}

type tileEntry struct {
	key string
	b   []byte
	at  time.Time
}

// NewTileCache returns a cache holding at most maxBytes of tile data; entries
// older than ttl are treated as absent (ttl <= 0 = no expiry). maxBytes <= 0
// returns nil, which every caller treats as "no cache".
func NewTileCache(maxBytes int64, ttl time.Duration) *TileCache {
	if maxBytes <= 0 {
		return nil
	}
	return &TileCache{max: maxBytes, ttl: ttl, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *TileCache) get(key string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.miss++
		return nil, false
	}
	e := el.Value.(*tileEntry)
	if c.ttl > 0 && time.Since(e.at) >= c.ttl {
		c.removeLocked(el)
		c.miss++
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits++
	return e.b, true
}

func (c *TileCache) put(key string, b []byte) {
	if c == nil || int64(len(b)) > c.max {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[key]; ok {
		c.removeLocked(el)
	}
	el := c.ll.PushFront(&tileEntry{key: key, b: b, at: time.Now()})
	c.items[key] = el
	c.size += int64(len(b))
	for c.size > c.max {
		c.removeLocked(c.ll.Back())
	}
}

func (c *TileCache) removeLocked(el *list.Element) {
	e := el.Value.(*tileEntry)
	c.ll.Remove(el)
	delete(c.items, e.key)
	c.size -= int64(len(e.b))
}

// Sweep drops expired entries; returns tiles removed and bytes freed.
func (c *TileCache) Sweep() (int, int64) {
	if c == nil || c.ttl <= 0 {
		return 0, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cutoff := time.Now().Add(-c.ttl)
	n, freed := 0, int64(0)
	for el := c.ll.Back(); el != nil; {
		prev := el.Prev()
		e := el.Value.(*tileEntry)
		if e.at.Before(cutoff) {
			freed += int64(len(e.b))
			n++
			c.removeLocked(el)
		}
		el = prev
	}
	return n, freed
}

// TileCacheStats is what /health reports about the in-memory tile cache.
type TileCacheStats struct {
	Tiles    int    `json:"tiles"`
	Bytes    int64  `json:"bytes"`
	MaxBytes int64  `json:"max_bytes"`
	TTLs     int    `json:"ttl_s"`
	Hits     uint64 `json:"hits"`
	Misses   uint64 `json:"misses"`
}

// Stats returns a snapshot; safe on a nil cache.
func (c *TileCache) Stats() TileCacheStats {
	if c == nil {
		return TileCacheStats{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return TileCacheStats{Tiles: len(c.items), Bytes: c.size, MaxBytes: c.max, TTLs: int(c.ttl.Seconds()), Hits: c.hits, Misses: c.miss}
}
