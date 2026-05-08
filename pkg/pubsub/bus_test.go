package pubsub

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPublishSubscribe_Single covers the basic round-trip. [SRE-116.D]
func TestPublishSubscribe_Single(t *testing.T) {
	bus := NewBus()
	ch, unsub := bus.Subscribe()
	defer unsub()

	bus.Publish(Event{Type: EventHeartbeat, Payload: "hello"})

	select {
	case e := <-ch:
		if e.Type != EventHeartbeat {
			t.Errorf("type mismatch: got %s want %s", e.Type, EventHeartbeat)
		}
		if e.Payload.(string) != "hello" {
			t.Errorf("payload mismatch: got %v", e.Payload)
		}
		if e.At.IsZero() {
			t.Errorf("Publish should populate At when zero")
		}
	case <-time.After(time.Second):
		t.Fatal("subscriber didn't receive event within 1s")
	}
}

// TestPublishMulti_FanOut verifies each subscriber gets its own copy.
func TestPublishMulti_FanOut(t *testing.T) {
	bus := NewBus()
	const n = 5
	chans := make([]<-chan Event, n)
	unsubs := make([]func(), n)
	for i := range n {
		chans[i], unsubs[i] = bus.Subscribe()
	}
	defer func() {
		for _, u := range unsubs {
			u()
		}
	}()

	bus.Publish(Event{Type: EventBouncer, Payload: 42})

	for i, c := range chans {
		select {
		case e := <-c:
			if e.Type != EventBouncer {
				t.Errorf("subscriber %d: bad type %s", i, e.Type)
			}
		case <-time.After(time.Second):
			t.Errorf("subscriber %d: timeout waiting for event", i)
		}
	}
}

// TestUnsubscribe_RemovesAndCloses verifies unsub removes the channel from the
// pool and closes it (so range loops exit).
func TestUnsubscribe_RemovesAndCloses(t *testing.T) {
	bus := NewBus()
	ch, unsub := bus.Subscribe()

	unsub()

	// Channel must be closed.
	if _, ok := <-ch; ok {
		t.Errorf("channel still open after unsubscribe")
	}
	// Bus must drop the subscriber so the next publish doesn't try to send to it.
	bus.Publish(Event{Type: EventChaos}) // would panic on closed-channel send if not removed
}

// TestPublish_DropsSlowSubscriber verifies publishers never block when a
// subscriber's buffer is full — full channel triggers select-default drop.
func TestPublish_DropsSlowSubscriber(t *testing.T) {
	bus := NewBus()
	_, unsub := bus.Subscribe() // never drained
	defer unsub()

	// Fire 200 events; channel buffer is 64. Publisher must not block beyond
	// the trivial loop time even with a wedged subscriber.
	done := make(chan struct{})
	go func() {
		for i := range 200 {
			bus.Publish(Event{Type: EventHeartbeat, Payload: i})
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("publisher blocked on slow subscriber — should drop instead")
	}
}

// TestConcurrentPublishers — race detector smoke test.
func TestConcurrentPublishers(t *testing.T) {
	bus := NewBus()
	ch, unsub := bus.Subscribe()
	defer unsub()

	var received atomic.Int64
	go func() {
		for range ch {
			received.Add(1)
		}
	}()

	const writers = 8
	const perWriter = 100
	var wg sync.WaitGroup
	wg.Add(writers)
	for i := range writers {
		go func(id int) {
			defer wg.Done()
			for j := range perWriter {
				bus.Publish(Event{Type: EventMCTS, Payload: id*1000 + j})
			}
		}(i)
	}
	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	// Reception count is best-effort because slow-subscriber drops happen,
	// but at least a handful must have arrived (test the channel survived).
	if received.Load() == 0 {
		t.Fatal("subscriber received zero events from 800 concurrent publishes")
	}
}

// TestPublish_NoSubscribers exercises the no-op path — must not panic.
func TestPublish_NoSubscribers(t *testing.T) {
	bus := NewBus()
	bus.Publish(Event{Type: EventHeartbeat}) // should be a silent no-op
}
