package ingest

import (
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/store"
)

// A cached value is a function of the clock, which makes every test of one a
// candidate flake unless the clock is an input. None of these sleep.
func fixedClock(t *testing.T) (*endpointCache, func(time.Duration)) {
	t.Helper()
	var mu sync.Mutex
	now := time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)

	c := newEndpointCache()
	c.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	return c, func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		now = now.Add(d)
	}
}

func TestCacheHitAndExpiry(t *testing.T) {
	c, advance := fixedClock(t)
	ep := &store.Endpoint{ID: "ep-1", Slug: "abc"}

	if _, hit := c.get("abc"); hit {
		t.Fatal("an empty cache reported a hit")
	}

	c.put("abc", ep)
	got, hit := c.get("abc")
	if !hit || got != ep {
		t.Fatalf("hit=%v got=%v, want a hit returning the stored endpoint", hit, got)
	}

	// One nanosecond short of the TTL is still a hit; the boundary itself is
	// not. Pinned because an off-by-one here is invisible in production and
	// changes how long a deleted inbox keeps answering.
	advance(endpointTTL - 1)
	if _, hit := c.get("abc"); !hit {
		t.Error("the entry expired before its TTL elapsed")
	}
	advance(1)
	if _, hit := c.get("abc"); hit {
		t.Error("the entry survived its TTL")
	}
}

// TestNegativeCaching: "no such inbox" is a real answer and gets cached, or a
// scanner walking random subdomains bypasses the cache entirely and puts the
// full lookup rate back on Postgres.
func TestNegativeCaching(t *testing.T) {
	c, advance := fixedClock(t)

	c.put("nosuch", nil)

	ep, hit := c.get("nosuch")
	if !hit {
		t.Fatal("the negative answer was not cached")
	}
	if ep != nil {
		t.Errorf("got %v, want nil for a cached miss", ep)
	}

	// Shorter TTL than a positive entry, because this is the one whose
	// staleness a user could notice.
	advance(negativeTTL)
	if _, hit := c.get("nosuch"); hit {
		t.Error("the negative entry outlived negativeTTL")
	}
}

func TestForgetIsImmediate(t *testing.T) {
	c, _ := fixedClock(t)
	c.put("abc", &store.Endpoint{ID: "ep-1"})
	c.forget("abc")
	if _, hit := c.get("abc"); hit {
		t.Error("forget did not drop the entry")
	}
}

func TestEvictExpiredReachesQuietInboxes(t *testing.T) {
	c, advance := fixedClock(t)
	c.put("busy", &store.Endpoint{ID: "ep-1"})
	c.put("quiet", &store.Endpoint{ID: "ep-2"})

	advance(endpointTTL + time.Second)
	c.put("busy", &store.Endpoint{ID: "ep-1"}) // refreshed by traffic

	// Entries expire on read, so without this sweep "quiet" would sit in the
	// map until something asked for it again -- which may be never.
	if n := c.EvictExpired(); n != 1 {
		t.Errorf("evicted %d, want 1", n)
	}
	if c.len() != 1 {
		t.Errorf("cache holds %d entries, want 1", c.len())
	}
}

// TestFullCacheRefusesAdmissionRatherThanEvicting is the anti-thrash
// assertion, and it is the interesting one.
//
// The realistic way this map fills is somebody enumerating subdomains. Under
// LRU each garbage key would evict a real inbox, so the attacker turns the
// cache off for everyone else while filling it with their own misses.
// Refusing admission when full means established inboxes keep their entries
// and the scan degrades to the pre-cache behaviour.
func TestFullCacheRefusesAdmissionRatherThanEvicting(t *testing.T) {
	c, _ := fixedClock(t)

	real := &store.Endpoint{ID: "ep-real"}
	c.put("real-inbox", real)

	// Fill the rest with a scan.
	for i := range maxEndpointEntries * 2 {
		c.put("scan-"+strconv.Itoa(i), nil)
	}

	if c.len() > maxEndpointEntries {
		t.Errorf("cache grew to %d entries, past the %d cap", c.len(), maxEndpointEntries)
	}
	got, hit := c.get("real-inbox")
	if !hit || got != real {
		t.Error("a scan evicted an established inbox from the cache")
	}
}

// TestExpiredEntriesMakeRoom: refusing admission must not be permanent, or a
// server that once saw a burst of unique slugs is stuck with them until
// restart.
func TestExpiredEntriesMakeRoom(t *testing.T) {
	c, advance := fixedClock(t)

	for i := range maxEndpointEntries {
		c.put("scan-"+strconv.Itoa(i), nil)
	}
	advance(endpointTTL + time.Second)

	c.put("new-inbox", &store.Endpoint{ID: "ep-new"})
	if _, hit := c.get("new-inbox"); !hit {
		t.Error("a full-but-expired cache refused a new entry permanently")
	}
}

func TestCacheIsConcurrencySafe(t *testing.T) {
	c := newEndpointCache()
	var wg sync.WaitGroup

	for i := range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slug := "ep-" + strconv.Itoa(i%5)
			for range 200 {
				c.get(slug)
				c.put(slug, &store.Endpoint{ID: slug})
				c.forget(slug)
				c.EvictExpired()
			}
		}()
	}
	wg.Wait()
}
