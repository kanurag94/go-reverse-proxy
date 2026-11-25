package cache

import (
	"container/list"
	"net/http"
	"sync"
	"time"
)

// entry is one stored response. It is never modified after put, so a hit can
// read it without holding the store's lock.
type entry struct {
	status     int
	header     http.Header
	body       []byte
	vary       http.Header // the request's values of the headers named in Vary
	storedAt   time.Time
	freshFor   time.Duration
	initialAge time.Duration
}

func (e *entry) size(key string) int64 {
	return int64(len(key)+len(e.body)) + headerSize(e.header) + headerSize(e.vary)
}

func headerSize(h http.Header) int64 {
	var n int
	for k, vs := range h {
		n += len(k)
		for _, v := range vs {
			n += len(v)
		}
	}
	return int64(n)
}

// Store is an LRU of responses bounded by total bytes. One Store is shared by
// every cache layer and survives config reloads.
type Store struct {
	mu        sync.Mutex
	max       int64
	maxObject int64
	used      int64
	items     map[string]*list.Element
	lru       *list.List // front is the most recently used
}

type item struct {
	key   string
	entry *entry
	size  int64
}

func NewStore(maxBytes, maxObjectBytes int64) *Store {
	return &Store{
		max:       maxBytes,
		maxObject: min(maxObjectBytes, maxBytes),
		items:     make(map[string]*list.Element),
		lru:       list.New(),
	}
}

func (s *Store) get(key string) *entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	el, ok := s.items[key]
	if !ok {
		return nil
	}
	s.lru.MoveToFront(el)
	return el.Value.(*item).entry
}

func (s *Store) put(key string, e *entry) bool {
	size := e.size(key)
	if size > s.maxObject {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.remove(el)
	}
	s.items[key] = s.lru.PushFront(&item{key: key, entry: e, size: size})
	s.used += size
	for s.used > s.max { // ends before reaching the new item: size <= maxObject <= max
		s.remove(s.lru.Back())
	}
	return true
}

func (s *Store) delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if el, ok := s.items[key]; ok {
		s.remove(el)
	}
}

// remove unlinks el; the caller holds mu.
func (s *Store) remove(el *list.Element) {
	it := s.lru.Remove(el).(*item)
	delete(s.items, it.key)
	s.used -= it.size
}
