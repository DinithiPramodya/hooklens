package ingest

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/broker"
	"github.com/DinithiPramodya/hooklens/internal/store"
)

// receive reads one message or fails; never blocks the test forever.
func receive(t *testing.T, sub *broker.Subscriber) broker.Message {
	t.Helper()
	select {
	case m := <-sub.C():
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no event published")
		return broker.Message{}
	}
}

// TestDeliveryEventReportsSuccess is the regression test for the stale live
// view: a capture delivered to the local app stayed "not attempted" in an open
// browser, because the capture event goes out before the forward and nothing
// sent the outcome afterwards.
func TestDeliveryEventReportsSuccess(t *testing.T) {
	br := broker.New()
	h := &Handler{broker: br}
	sub := br.Subscribe("ep-1")
	defer br.Unsubscribe("ep-1", sub)

	h.publishDelivery("ep-1", "req-1", store.ForwardOutcome{Status: 200, Elapsed: 90})

	m := receive(t, sub)
	if m.Event != "delivery" {
		t.Fatalf("event = %q, want delivery", m.Event)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(m.Data), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["id"] != "req-1" || got["forward_status"] != float64(200) || got["forward_ms"] != float64(90) {
		t.Errorf("event = %v", got)
	}
	// Absent, not null: the frontend reads absence as "no failure", and a
	// present-but-null forward_error would be a fourth state it does not know.
	if _, ok := got["forward_error"]; ok {
		t.Errorf("forward_error present on a success: %v", got)
	}
}

func TestDeliveryEventReportsFailure(t *testing.T) {
	br := broker.New()
	h := &Handler{broker: br}
	sub := br.Subscribe("ep-1")
	defer br.Unsubscribe("ep-1", sub)

	h.publishDelivery("ep-1", "req-1", store.ForwardOutcome{Error: "unreachable", Elapsed: 3})

	var got map[string]any
	if err := json.Unmarshal([]byte(receive(t, sub).Data), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["forward_error"] != "unreachable" {
		t.Errorf("forward_error = %v, want unreachable", got["forward_error"])
	}
	if _, ok := got["forward_status"]; ok {
		t.Errorf("forward_status present on a failure: %v", got)
	}
}

// TestDeliveryEventSkippedWithNoSubscribers: most inboxes have no browser
// open, and the marshal is skipped entirely then. Also covers a nil broker,
// which the handler supports.
func TestDeliveryEventSkippedWithNoSubscribers(t *testing.T) {
	(&Handler{}).publishDelivery("ep-1", "req-1", store.ForwardOutcome{Status: 200})
	(&Handler{broker: broker.New()}).publishDelivery("ep-1", "req-1", store.ForwardOutcome{Status: 200})
}
