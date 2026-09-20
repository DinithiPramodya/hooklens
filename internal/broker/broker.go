// Package broker is an in-process publish/subscribe hub.
//
// It exists to keep the capture path from knowing anything about HTTP
// streaming. Ingest publishes; SSE handlers subscribe. See
// docs/learn/14-pubsub.md.
package broker

import "sync"

// buffer is how many messages a subscriber may fall behind before it starts
// losing them.
//
// Large enough to absorb a burst -- a provider retrying a backlog can deliver
// dozens of webhooks in a second, and a browser should not miss those merely
// for being mid-render. Small enough that a dead tab holds a bounded amount of
// memory until it is cleaned up. There is no principled value here; the
// principled part is that it is finite.
const buffer = 32

// Message is one published event, already serialised.
//
// The broker deliberately does not know what is inside Data. Passing a
// pre-rendered string rather than an `any` keeps every subscriber from
// re-serialising the same payload, and keeps this package free of the
// application's types.
type Message struct {
	Event string
	Data  string
}

// Subscriber is one listener's queue.
type Subscriber struct {
	ch chan Message

	// dropped is guarded by the OWNING BROKER's mutex, not by a lock of its
	// own. One lock for the whole structure means there is no lock ordering to
	// get wrong -- two mutexes here would be the beginning of a deadlock story
	// for no benefit, since every write to this field already happens inside
	// Publish, which holds that lock anyway.
	dropped int
}

// C is the receive side of the subscriber's queue.
//
// Returned as a receive-only channel so a caller cannot send on it or close
// it. Closing is the broker's job and doing it twice panics.
func (s *Subscriber) C() <-chan Message { return s.ch }

type Broker struct {
	mu sync.Mutex
	// topic -> set of subscribers. A set rather than a slice because removal is
	// the frequent operation (every closed tab) and a slice would need a linear
	// scan plus a splice.
	subs map[string]map[*Subscriber]struct{}
}

func New() *Broker {
	return &Broker{subs: make(map[string]map[*Subscriber]struct{})}
}

// Subscribe registers a new listener on a topic.
//
// The caller must Unsubscribe, on every exit path including panics, or the
// subscriber stays in the map forever and Publish keeps writing into a queue
// nobody reads.
func (b *Broker) Subscribe(topic string) *Subscriber {
	s := &Subscriber{ch: make(chan Message, buffer)}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.subs[topic] == nil {
		b.subs[topic] = make(map[*Subscriber]struct{})
	}
	b.subs[topic][s] = struct{}{}
	return s
}

// Unsubscribe removes a listener and closes its channel.
//
// Closing happens HERE, under the same mutex Publish holds, and that is the
// invariant the whole design rests on: a send can never race a close, because
// both require the lock. Closing from the subscriber's own goroutine instead
// would be a panic waiting for the first concurrent publish.
//
// Safe to call more than once: the map delete makes the second call a no-op
// before it can reach the close.
func (b *Broker) Unsubscribe(topic string, s *Subscriber) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if _, ok := b.subs[topic][s]; !ok {
		return
	}
	delete(b.subs[topic], s)
	if len(b.subs[topic]) == 0 {
		// Drop the empty inner map too. Without this, every inbox that has
		// ever been watched keeps an empty map alive for the life of the
		// process -- small, but unbounded in the number of inboxes.
		delete(b.subs, topic)
	}
	close(s.ch)
}

// Publish delivers a message to every subscriber on a topic.
//
// It never blocks. That is the single most important property in this package:
// the caller is the webhook capture path, with a provider waiting on the HTTP
// response, and it must not be slowed by a browser tab that has stopped
// reading.
//
// The non-blocking send is also what makes it safe to send while holding the
// mutex. A blocking send under the lock would deadlock against Unsubscribe the
// moment one subscriber's buffer filled: the publisher would wait for a reader
// that is trying to take the lock the publisher holds.
func (b *Broker) Publish(topic string, m Message) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for s := range b.subs[topic] {
		select {
		case s.ch <- m:
		default:
			// The subscriber is behind by `buffer` messages. Drop this one and
			// record it. Dropping degrades the live feed for that one viewer;
			// blocking would degrade webhook capture for everybody.
			s.dropped++
		}
	}
}

// Dropped reports how many messages this subscriber has missed.
//
// A method on Broker rather than on Subscriber because the field is guarded by
// the broker's mutex. Callers use it to tell the user their view is incomplete
// and should be refetched -- silently showing a partial list is the failure
// this counter exists to prevent.
func (b *Broker) Dropped(s *Subscriber) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return s.dropped
}

// Subscribers reports how many listeners a topic has. Used by tests and by the
// capture path, which can skip serialising a payload nobody is watching.
func (b *Broker) Subscribers(topic string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[topic])
}
