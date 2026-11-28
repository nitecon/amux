// Package eventbus provides bounded, in-process notifications. Durable state
// remains the responsibility of the publisher; consumers reconcile from it.
package eventbus

import (
	"errors"
	"sync"
	"time"
)

type Topic string

const (
	ProjectChanged           Topic = "project_changed"
	TaskChanged              Topic = "task_changed"
	ConversationChanged      Topic = "conversation_changed"
	FleetConversationChanged Topic = "fleet_conversation_changed"
	PresenceChanged          Topic = "presence_changed"
	ObservationAccepted      Topic = "observation_accepted"
	RouterChanged            Topic = "router_changed"
	Tick                     Topic = "tick"
)

// Event is intentionally small and extensible. Current consumers use Topic as
// a wake hint and reload authoritative state.
type Event struct {
	Topic Topic
	At    time.Time
}

var (
	ErrSlowConsumer = errors.New("eventbus subscription closed: consumer is too slow; reconcile durable state")
	ErrBusClosed    = errors.New("eventbus is closed")
)

type Bus struct {
	mu     sync.Mutex
	nextID uint64
	subs   map[uint64]*Subscription
	closed bool
}

// Subscription owns one receive channel. Call Unsubscribe when finished. If
// its bounded queue fills, the bus closes it with ErrSlowConsumer; consumers
// must resubscribe and reconcile from durable state.
type Subscription struct {
	C    <-chan Event
	bus  *Bus
	id   uint64
	ch   chan Event
	err  error
	done bool
}

func New() *Bus { return &Bus{subs: make(map[uint64]*Subscription)} }

func (b *Bus) Subscribe(buffer int) *Subscription {
	if buffer < 1 {
		buffer = 1
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	ch := make(chan Event, buffer)
	s := &Subscription{C: ch, bus: b, id: b.nextID, ch: ch}
	if b.closed {
		s.done = true
		s.err = ErrBusClosed
		close(ch)
	} else {
		b.subs[s.id] = s
	}
	return s
}

// Publish fans out without blocking. Overflow terminates only that subscriber
// and is observable through Err; the event remains recoverable from storage.
func (b *Bus) Publish(event Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return ErrBusClosed
	}
	if event.At.IsZero() {
		event.At = time.Now().UTC()
	}
	for id, s := range b.subs {
		select {
		case s.ch <- event:
		default:
			s.err = ErrSlowConsumer
			s.done = true
			close(s.ch)
			delete(b.subs, id)
		}
	}
	return nil
}

func (s *Subscription) Err() error {
	s.bus.mu.Lock()
	defer s.bus.mu.Unlock()
	return s.err
}

func (s *Subscription) Unsubscribe() {
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	if s.done {
		return
	}
	s.done = true
	delete(b.subs, s.id)
	close(s.ch)
}

func (b *Bus) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return
	}
	b.closed = true
	for id, s := range b.subs {
		s.err = ErrBusClosed
		s.done = true
		close(s.ch)
		delete(b.subs, id)
	}
}
