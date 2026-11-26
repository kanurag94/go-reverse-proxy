package cache

import (
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func newEntry(body string) *entry {
	return &entry{status: http.StatusOK, body: []byte(body)}
}

func TestEntrySize(t *testing.T) {
	e := &entry{header: http.Header{"Content-Type": {"text/plain"}}, body: []byte("hello")}
	if got, want := e.size("key"), int64(len("key")+len("hello")+len("Content-Type")+len("text/plain")); got != want {
		t.Errorf("size = %d, want %d", got, want)
	}
}

func TestStoreGetPut(t *testing.T) {
	s := NewStore(1000, 1000)
	e := newEntry("hello")
	if !s.put("a", e) {
		t.Fatal("put rejected a small entry")
	}
	if got := s.get("a"); got != e {
		t.Errorf("get(a) = %v, want the stored entry", got)
	}
	if got := s.get("missing"); got != nil {
		t.Errorf("get(missing) = %v, want nil", got)
	}
}

func TestStoreEvictsLeastRecentlyUsed(t *testing.T) {
	s := NewStore(303, 303) // room for three 101-byte entries (1-byte key + 100-byte body)
	body := strings.Repeat("x", 100)
	for _, k := range []string{"a", "b", "c"} {
		s.put(k, newEntry(body))
	}
	s.get("a")
	s.put("d", newEntry(body))

	for k, want := range map[string]bool{"a": true, "b": false, "c": true, "d": true} {
		if got := s.get(k) != nil; got != want {
			t.Errorf("%s present = %v, want %v", k, got, want)
		}
	}
	if s.used != 303 {
		t.Errorf("used = %d, want 303", s.used)
	}
}

func TestStoreReplaceUpdatesSize(t *testing.T) {
	s := NewStore(1000, 1000)
	s.put("a", newEntry(strings.Repeat("x", 100)))
	s.put("a", newEntry(strings.Repeat("x", 50)))
	if s.used != 51 || s.lru.Len() != 1 {
		t.Errorf("used = %d, entries = %d; want 51, 1", s.used, s.lru.Len())
	}
}

func TestStoreRejectsOversized(t *testing.T) {
	s := NewStore(1000, 50)
	s.put("small", newEntry("x"))
	if s.put("big", newEntry(strings.Repeat("x", 100))) {
		t.Error("put accepted an entry over the object limit")
	}
	if s.get("big") != nil || s.get("small") == nil {
		t.Error("an oversized put must store nothing and evict nothing")
	}
}

func TestStoreObjectLimitCappedByTotal(t *testing.T) {
	s := NewStore(100, 1000)
	if s.put("a", newEntry(strings.Repeat("x", 200))) {
		t.Error("put accepted an entry larger than the whole store")
	}
}

func TestStoreDelete(t *testing.T) {
	s := NewStore(1000, 1000)
	s.put("a", newEntry("hello"))
	s.delete("a")
	s.delete("missing")
	if s.get("a") != nil || s.used != 0 {
		t.Errorf("after delete: present = %v, used = %d", s.get("a") != nil, s.used)
	}
}

func TestStoreConcurrentUse(t *testing.T) {
	s := NewStore(2000, 500)
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Go(func() {
			for i := range 1000 {
				key := strconv.Itoa((g*7 + i) % 20)
				switch i % 3 {
				case 0:
					s.put(key, newEntry(strings.Repeat("x", i%300)))
				case 1:
					if e := s.get(key); e != nil {
						_ = len(e.body)
					}
				case 2:
					s.delete(key)
				}
			}
		})
	}
	wg.Wait()

	var sum int64
	for el := s.lru.Front(); el != nil; el = el.Next() {
		sum += el.Value.(*item).size
	}
	if sum != s.used || s.used > s.max || len(s.items) != s.lru.Len() {
		t.Errorf("inconsistent store: sum %d, used %d, max %d, map %d, list %d",
			sum, s.used, s.max, len(s.items), s.lru.Len())
	}
}
