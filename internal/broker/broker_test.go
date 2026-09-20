package broker

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestFanOut(t *testing.T) {
	b := New()

	// Three subscribers on one topic must each receive EVERY message. This is
	// the property a single shared channel could not provide -- it would give
	// each of them a different subset.
	subs := []*Subscriber{b.Subscribe("a"), b.Subscribe("a"), b.Subscribe("a")}
	other := b.Subscribe("b")

	b.Publish("a", Message{Event: "capture", Data: "1"})
	b.Publish("a", Message{Event: "capture", Data: "2"})

	for i, s := range subs {
		for _, want := range []string{"1", "2"} {
			select {
			case m := <-s.C():
				if m.Data != want {
					t.Errorf("subscriber %d got %q, want %q", i, m.Data, want)
				}
			default:
				t.Fatalf("subscriber %d did not receive %q", i, want)
			}
		}
	}

	// Topics are isolated.
	select {
	case m := <-other.C():
		t.Errorf("topic b received a message published to a: %+v", m)
	default:
	}
}

// TestPublishNeverBlocks is the property the capture path depends on.
//
// A subscriber that never reads must not be able to stall a publisher, because
// the publisher is a webhook handler with a provider waiting on its response.
func TestPublishNeverBlocks(t *testing.T) {
	b := New()
	b.Subscribe("a") // subscribed, never reads

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than the buffer. With a blocking send this wedges forever.
		for i := range buffer * 10 {
			b.Publish("a", Message{Data: fmt.Sprint(i)})
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading -- " +
			"a backgrounded browser tab would stall webhook capture")
	}
}

// TestSlowSubscriberDropsAndCounts: the consequence of not blocking is that
// somebody loses messages. It must be the slow subscriber, and it must be
// counted rather than silent.
func TestSlowSubscriberDropsAndCounts(t *testing.T) {
	b := New()
	slow := b.Subscribe("a")
	fast := b.Subscribe("a")

	const sent = buffer + 10
	for i := range sent {
		// Drain `fast` as we go; never drain `slow`.
		b.Publish("a", Message{Data: fmt.Sprint(i)})
		select {
		case <-fast.C():
		default:
			t.Fatalf("fast subscriber unexpectedly behind at %d", i)
		}
	}

	if got := b.Dropped(slow); got != sent-buffer {
		t.Errorf("slow subscriber dropped %d, want %d", got, sent-buffer)
	}
	// The fast one must be untouched by its neighbour's problem.
	if got := b.Dropped(fast); got != 0 {
		t.Errorf("fast subscriber dropped %d, want 0 -- one slow reader must not affect another", got)
	}
	// And the slow one still holds a full buffer of the OLDEST messages, not
	// the newest: we drop the arriving message, not the queued one.
	if m := <-slow.C(); m.Data != "0" {
		t.Errorf("slow subscriber head = %q, want %q (drops are of new messages)", m.Data, "0")
	}
}

func TestUnsubscribe(t *testing.T) {
	b := New()
	s := b.Subscribe("a")

	if got := b.Subscribers("a"); got != 1 {
		t.Fatalf("Subscribers = %d, want 1", got)
	}

	b.Unsubscribe("a", s)

	if got := b.Subscribers("a"); got != 0 {
		t.Errorf("Subscribers = %d after unsubscribe, want 0", got)
	}
	// The channel must be closed so a ranging reader terminates rather than
	// blocking forever.
	if _, open := <-s.C(); open {
		t.Error("channel still open after Unsubscribe")
	}
	// Publishing to a topic with no subscribers is a no-op, not a panic on a
	// closed channel.
	b.Publish("a", Message{Data: "x"})

	// Idempotent: a double Unsubscribe must not panic on a second close.
	b.Unsubscribe("a", s)
}

// TestConcurrentSubscribeUnsubscribePublish is the reason this package has a
// mutex, and the reason to run it under -race.
//
// Subscribing, unsubscribing and publishing all mutate the same map from
// different goroutines. Without the lock this is `concurrent map writes` and
// the runtime deliberately crashes; with it, the race detector should find
// nothing.
func TestConcurrentSubscribeUnsubscribePublish(t *testing.T) {
	b := New()
	var wg sync.WaitGroup

	stop := make(chan struct{})

	// Publishers.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					b.Publish("a", Message{Event: "capture", Data: "x"})
				}
			}
		}()
	}

	// Subscribers churning: join, read a bit, leave.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				s := b.Subscribe("a")
				for range 3 {
					select {
					case <-s.C():
					default:
					}
				}
				_ = b.Dropped(s)
				b.Unsubscribe("a", s)
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// Every subscriber unsubscribed, so the topic must be gone entirely --
	// not merely empty. An empty inner map left behind for every topic ever
	// used is a small unbounded leak.
	if got := b.Subscribers("a"); got != 0 {
		t.Errorf("Subscribers = %d after all unsubscribed", got)
	}
	b.mu.Lock()
	remaining := len(b.subs)
	b.mu.Unlock()
	if remaining != 0 {
		t.Errorf("%d empty topic maps left behind", remaining)
	}
}
