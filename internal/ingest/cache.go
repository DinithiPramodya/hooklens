package ingest

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/store"
)

// Cache tuning. Constants rather than configuration: these are not numbers an
// operator has any basis for choosing, and every knob in a config file is a
// knob someone will set wrong.
const (
	// endpointTTL bounds how stale a cached inbox may be.
	//
	// Five seconds is chosen against the one thing that actually goes wrong:
	// an inbox is deleted and captures keep landing. Five seconds of that is
	// invisible to a human and a rounding error against a provider's retry
	// schedule. Longer would save nothing measurable -- at any interesting
	// rate the cache already serves ~99.99% of lookups at 5s, and the
	// remaining 0.01% is not worth extending the window in which a deleted
	// inbox still answers.
	endpointTTL = 5 * time.Second

	// negativeTTL bounds a cached "no such inbox".
	//
	// Shorter than endpointTTL, because this is the entry whose staleness a
	// user could notice: a slug that does not exist yet and then does. In
	// practice that cannot happen -- slugs are random, so nobody POSTs to one
	// before it is created -- but one second costs nothing and removes the
	// need to be sure.
	negativeTTL = time.Second

	// maxEndpointEntries caps the map.
	//
	// A cache with no limit is a memory leak that grows exactly as fast as
	// traffic diversity, and the capture path takes its key from the
	// hostname, so anyone scanning subdomains supplies one distinct key per
	// guess.
	maxEndpointEntries = 10_000
)

// endpointCache holds resolved inboxes for a few seconds.
//
// Unit 33 measured the capture path at three database round trips: resolve
// the inbox, insert the request, record the forward outcome. The first one
// asks the same question about the same immutable row a thousand times a
// second. This is the standard shape a cache fits -- high read rate, near-zero
// write rate, and tolerance for a slightly stale answer -- and the tolerance
// is the part that has to be argued rather than assumed. See
// docs/learn/34-caching-and-invalidation.md.
//
// What is cached is deliberately NOT an authorisation decision. A capture URL
// is public by design (unit 8), so resolving a slug grants nothing; the token
// checks in AuthenticateEndpoint and AuthenticateRequest go to the database
// every time and are not touched by any of this. Caching an authorisation
// answer would mean a revoked token stays valid for one TTL, which is a
// different and much worse trade.
type endpointCache struct {
	mu      sync.Mutex
	entries map[string]cacheEntry

	// now is injectable so tests do not sleep. A cached value is a function
	// of the clock, which makes every test of it a candidate flake unless
	// the clock is an input -- the same reasoning as internal/ratelimit.
	now func() time.Time
}

type cacheEntry struct {
	// ep is nil for a cached "no such inbox". A negative entry is a real
	// answer, not an absence: without it, a scanner walking random subdomains
	// bypasses the cache completely and puts the full lookup rate back on the
	// database, which is precisely the traffic you least want to serve from
	// Postgres.
	ep      *store.Endpoint
	expires time.Time
}

func newEndpointCache() *endpointCache {
	return &endpointCache{
		entries: map[string]cacheEntry{},
		now:     time.Now,
	}
}

// get returns the cached answer for slug.
//
// Three outcomes, which is one more than a cache usually has: (nil, false) is
// a miss, (ep, true) is a hit, and (nil, true) is a hit on a cached "no such
// inbox". The caller has to distinguish the first and third, so they cannot
// collapse into one signal.
func (c *endpointCache) get(slug string) (*store.Endpoint, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e, ok := c.entries[slug]
	if !ok {
		return nil, false
	}
	if !c.now().Before(e.expires) {
		// Expired. Delete on read rather than leaving it: this is the cheapest
		// eviction there is, it happens exactly where the entry is proven
		// stale, and it keeps the map from filling with entries for inboxes
		// that went quiet.
		delete(c.entries, slug)
		return nil, false
	}
	return e.ep, true
}

// put stores an answer. A nil ep stores "no such inbox".
func (c *endpointCache) put(slug string, ep *store.Endpoint) {
	ttl := endpointTTL
	if ep == nil {
		ttl = negativeTTL
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if len(c.entries) >= maxEndpointEntries {
		// Full. Clear what has expired, and if that frees nothing, decline to
		// admit this entry rather than evicting a live one.
		//
		// That is the opposite of LRU, and deliberately. The realistic way
		// this map fills is someone enumerating subdomains, and under LRU each
		// garbage key would evict a real inbox -- the attacker would turn the
		// cache off for everybody else while filling it with their own misses.
		// Refusing admission means established inboxes keep their entries and
		// the scan degrades to what it was before the cache existed. Entries
		// expire on their own within seconds, so the population still turns
		// over; this is not a permanent freeze.
		if c.evictExpiredLocked() == 0 {
			return
		}
	}

	c.entries[slug] = cacheEntry{ep: ep, expires: c.now().Add(ttl)}
}

// forget drops an entry immediately.
//
// Explicit invalidation, for the one writer in this process that can make a
// cached answer wrong on purpose. TTL alone would be correct and slower; this
// makes a delete take effect now rather than within five seconds, and it
// costs one map delete.
func (c *endpointCache) forget(slug string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, slug)
}

// EvictExpired drops expired entries and returns how many, for the sweeper.
func (c *endpointCache) EvictExpired() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictExpiredLocked()
}

func (c *endpointCache) evictExpiredLocked() int {
	now := c.now()
	n := 0
	for slug, e := range c.entries {
		if !now.Before(e.expires) {
			delete(c.entries, slug)
			n++
		}
	}
	return n
}

func (c *endpointCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// resolve turns a slug into an inbox, through the cache.
//
// The cache is consulted first and populated after, including with the
// negative answer. Note what is NOT here: no lock is held across the database
// call. Holding one would serialise every capture behind a single query and
// reproduce, in a new place, exactly the row-lock contention unit 33 removed.
//
// The cost of not holding it is a small stampede: when a hot entry expires
// while requests are in flight, several of them miss and several identical
// queries run. With one busy inbox that is a handful of duplicate lookups
// every five seconds -- cheaper than the coordination needed to prevent it,
// and singleflight can be added later if a real workload disagrees.
func (h *Handler) resolve(ctx context.Context, slug string) (*store.Endpoint, error) {
	if ep, hit := h.endpoints.get(slug); hit {
		if ep == nil {
			// A cached "no such inbox". Returning the sentinel rather than a
			// nil endpoint keeps the caller's error handling identical
			// whether the answer came from memory or from Postgres.
			return nil, store.ErrNotFound
		}
		return ep, nil
	}

	ep, err := h.store.EndpointBySlug(ctx, slug)
	switch {
	case errors.Is(err, store.ErrNotFound):
		h.endpoints.put(slug, nil)
		return nil, err
	case err != nil:
		// A real failure -- the database is unreachable, the query timed out.
		// Deliberately NOT cached: caching a transient error would turn a
		// blip into seconds of guaranteed failure, and the retry that would
		// have succeeded never reaches the database to find out.
		return nil, err
	}

	h.endpoints.put(slug, ep)
	return ep, nil
}

// ForgetEndpoint drops an inbox from the resolution cache.
//
// Exported for whoever deletes an inbox, so the delete takes effect at once
// instead of within endpointTTL. Nothing calls it yet -- there is no delete
// endpoint -- and it exists now because the invalidation hook is much easier
// to add while the cache is being written than to remember during the feature
// that needs it.
func (h *Handler) ForgetEndpoint(slug string) { h.endpoints.forget(slug) }

// EvictEndpointCache drops expired entries, for the sweeper's tick.
//
// Entries already expire on read, so this only matters for inboxes that went
// quiet: without it their entries sit in the map until something asks for
// them again, which may be never.
func (h *Handler) EvictEndpointCache() int { return h.endpoints.EvictExpired() }
