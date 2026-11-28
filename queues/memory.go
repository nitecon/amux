package queues

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/rs/zerolog/log"
)

type memoryMessage struct {
	id        string
	payload   string
	expiresAt time.Time
}

// MemoryQueue processes messages in FIFO order, including retries.
type MemoryQueue struct {
	ctx        context.Context
	processor  MessageProcessor
	queue      chan memoryMessage
	slots      chan struct{}
	retry      chan struct{}
	done       chan struct{}
	rejectFull bool
	mu         sync.Mutex
	stopped    bool
}

func NewMemoryQueue(bufferSize int, processor MessageProcessor, ctx context.Context) (*MemoryQueue, error) {
	if processor == nil {
		return nil, fmt.Errorf("processor cannot be nil")
	}
	return newMemoryQueue(Options{Capacity: bufferSize}, processor, ctx), nil
}

func newMemoryQueue(options Options, processor MessageProcessor, ctx context.Context) *MemoryQueue {
	capacity := options.Capacity
	if capacity <= 0 {
		capacity = 1000
	}
	q := &MemoryQueue{
		ctx: ctx, processor: processor, rejectFull: options.RejectFull,
		queue: make(chan memoryMessage, capacity),
		slots: make(chan struct{}, capacity),
		retry: make(chan struct{}, 1),
		done:  make(chan struct{}),
	}
	go q.receive()
	return q
}

func (q *MemoryQueue) receive() {
	defer close(q.done)
	defer func() {
		q.mu.Lock()
		q.stopped = true
		close(q.queue)
		q.mu.Unlock()
		for range q.queue {
			<-q.slots
		}
	}()
	for q.ctx.Err() == nil {
		select {
		case <-q.ctx.Done():
			return
		case msg := <-q.queue:
			q.process(msg)
			<-q.slots
		}
	}
}

func (q *MemoryQueue) process(msg memoryMessage) {
	var expires <-chan time.Time
	if !msg.expiresAt.IsZero() {
		timer := time.NewTimer(time.Until(msg.expiresAt))
		defer timer.Stop()
		expires = timer.C
	}
	for q.ctx.Err() == nil && (msg.expiresAt.IsZero() || time.Now().Before(msg.expiresAt)) {
		err := q.processor(msg.id, msg.payload)
		if err == nil || q.ctx.Err() != nil {
			return
		}
		log.Debug().Err(err).Str("message_id", msg.id).Msg("queued message awaiting retry")
		select {
		case <-q.ctx.Done():
			return
		case <-expires:
			return
		case <-q.retry:
		}
	}
}

// Enqueue reserves capacity across waiting, in-flight and retrying messages.
func (q *MemoryQueue) Enqueue(ctx context.Context, id string, payload string, ttl int64) error {
	if ttl > math.MaxInt64/int64(time.Second) {
		return fmt.Errorf("message TTL exceeds supported duration")
	}
	if q.rejectFull {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-q.ctx.Done():
			return q.ctx.Err()
		case q.slots <- struct{}{}:
		default:
			return ErrFull
		}
	} else {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-q.ctx.Done():
			return q.ctx.Err()
		case q.slots <- struct{}{}:
		}
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if err := ctx.Err(); err != nil {
		<-q.slots
		return err
	}
	if q.stopped || q.ctx.Err() != nil {
		<-q.slots
		return q.ctx.Err()
	}

	msg := memoryMessage{id: id, payload: payload}
	if ttl > 0 {
		msg.expiresAt = time.Now().Add(time.Duration(ttl) * time.Second)
	}
	q.queue <- msg
	return nil
}

func (q *MemoryQueue) ConnectResponse() {
	select {
	case q.retry <- struct{}{}:
	default:
	}
}

func (q *MemoryQueue) Done() <-chan struct{} { return q.done }
