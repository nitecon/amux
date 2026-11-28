package wsock

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nitecon/amux/queues"
)

var ErrOutboundFull = errors.New("websocket outbound queue is full")

// Writer sends JSON frames in order on one connection.
type Writer struct {
	ctx     context.Context
	cancel  context.CancelFunc
	frames  queues.Queue
	pending sync.Map
}

func NewWriter(parent context.Context, manager queues.Manager, topic string, capacity int, timeout time.Duration, write func(context.Context, websocket.MessageType, []byte) error) (*Writer, error) {
	ctx, cancel := context.WithCancel(parent)
	w := &Writer{ctx: ctx, cancel: cancel}
	frames, err := manager.Open(ctx, topic, queues.Options{Capacity: capacity + 1, RejectFull: true}, func(id, payload string) error {
		writeCtx, stop := context.WithTimeout(ctx, timeout)
		defer stop()
		if err := write(writeCtx, websocket.MessageText, []byte(payload)); err != nil {
			cancel()
			return err
		}
		if receipt, ok := w.pending.LoadAndDelete(id); ok {
			close(receipt.(chan struct{}))
		}
		return nil
	})
	if err != nil {
		cancel()
		return nil, err
	}
	w.frames = frames
	return w, nil
}

// EnqueueJSON returns immediately on saturation so callers can reconnect and
// replay durable messages from their last acknowledgment.
func (w *Writer) EnqueueJSON(value any) error {
	return w.enqueueJSON(uuid.NewString(), value)
}

// WriteJSON waits for a frame to be written before its acknowledgment can
// advance a durable cursor.
func (w *Writer) WriteJSON(value any) error {
	id := uuid.NewString()
	sent := make(chan struct{})
	w.pending.Store(id, sent)
	defer w.pending.Delete(id)
	if err := w.enqueueJSON(id, value); err != nil {
		return err
	}
	select {
	case <-sent:
		return nil
	case <-w.Done():
		if err := w.ctx.Err(); err != nil {
			return err
		}
		return errors.New("websocket queue stopped")
	}
}

func (w *Writer) enqueueJSON(id string, value any) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	err = w.frames.Enqueue(w.ctx, id, string(body), 0)
	if errors.Is(err, queues.ErrFull) {
		return ErrOutboundFull
	}
	return err
}

func (w *Writer) Done() <-chan struct{} { return w.frames.Done() }

func (w *Writer) Close() {
	w.cancel()
	<-w.Done()
}
