package eventbus

import (
	"errors"
	"sync"
	"testing"
)

func TestFanoutAndSlowConsumer(t *testing.T) {
	b := New()
	fast, slow := b.Subscribe(2), b.Subscribe(1)
	if err := b.Publish(Event{Topic: TaskChanged}); err != nil {
		t.Fatal(err)
	}
	if got := <-fast.C; got.Topic != TaskChanged || got.At.IsZero() {
		t.Fatalf("event = %+v", got)
	}
	if err := b.Publish(Event{Topic: Tick}); err != nil {
		t.Fatal(err)
	}
	if err := b.Publish(Event{Topic: ConversationChanged}); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(slow.Err(), ErrSlowConsumer) {
		t.Fatalf("slow subscriber error = %v", slow.Err())
	}
	for range slow.C {
	}
	fast.Unsubscribe()
	for range fast.C {
	}
}

func TestConcurrentPublishUnsubscribe(t *testing.T) {
	b := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		s := b.Subscribe(4)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range s.C {
			}
		}()
		for j := 0; j < 8; j++ {
			if err := b.Publish(Event{Topic: Tick}); err != nil {
				t.Fatal(err)
			}
		}
		s.Unsubscribe()
	}
	wg.Wait()
}

func TestCloseAndSubscribeAfterClose(t *testing.T) {
	b := New()
	s := b.Subscribe(1)
	b.Close()
	if !errors.Is(s.Err(), ErrBusClosed) {
		t.Fatalf("close error = %v", s.Err())
	}
	late := b.Subscribe(1)
	if !errors.Is(late.Err(), ErrBusClosed) {
		t.Fatalf("late subscription error = %v", late.Err())
	}
	if err := b.Publish(Event{Topic: Tick}); !errors.Is(err, ErrBusClosed) {
		t.Fatalf("publish error = %v", err)
	}
}
