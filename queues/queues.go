// Package queues opens independent topics using the configured queue provider.
package queues

import (
	"context"
	"errors"
	"fmt"
)

var ErrFull = errors.New("queue is full")

type MessageProcessor func(id string, payload string) error

type Queue interface {
	Enqueue(ctx context.Context, id string, payload string, ttl int64) error
	ConnectResponse()
	Done() <-chan struct{}
}

// Options controls admission for the in-memory provider.
type Options struct {
	Capacity   int
	RejectFull bool
}

// Manager contains connection settings shared by its topics. The zero value
// opens in-memory queues for a single-server deployment.
type Manager struct {
	Provider           string
	PubSubProject      string
	TopicPrefix        string
	SubscriptionPrefix string
	SQSBaseURL         string
}

func (m Manager) Open(ctx context.Context, topic string, options Options, processor MessageProcessor) (Queue, error) {
	if topic == "" || processor == nil {
		return nil, fmt.Errorf("queue topic and processor are required")
	}
	switch m.Provider {
	case "", "memory":
		return newMemoryQueue(options, processor, ctx), nil
	case "pubsub":
		return NewPubSubQueue(m.PubSubProject, m.TopicPrefix+topic, m.SubscriptionPrefix+topic, processor, ctx)
	case "sqs":
		return newSQSQueue(ctx, m.SQSBaseURL+topic, processor)
	case "redis":
		return nil, fmt.Errorf("redis queue is not implemented")
	default:
		return nil, fmt.Errorf("unsupported queue engine %q", m.Provider)
	}
}
