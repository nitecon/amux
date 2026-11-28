package queues

import (
	"context"
	"fmt"
	"sync"

	"cloud.google.com/go/pubsub"
	"github.com/rs/zerolog/log"
)

type PubSubQueue struct {
	AppCtx      context.Context
	done        chan struct{}
	client      *pubsub.Client
	topicID     string
	subID       string
	sub         *pubsub.Subscription
	messageList []*pubsub.Message
	Processor   MessageProcessor
	messages    map[string]*pubsub.Message // Cache of unacknowledged messages keyed by their ID
	msgMutex    sync.RWMutex               // Mutex to control access to the messages map
}

// NewPubSubQueue initializes a new PubSubQueue with Pub/Sub client, topicID, and subscriptionID.
// The provided processor function will handle messages as they are received.
func NewPubSubQueue(projectID, topicID, subID string, processor MessageProcessor, ctx context.Context) (*PubSubQueue, error) {
	client, err := pubsub.NewClient(ctx, projectID)
	if err != nil {
		return nil, err
	}

	if processor == nil {
		return nil, fmt.Errorf("processor cannot be nil")
	}

	psq := &PubSubQueue{
		AppCtx:    ctx,
		done:      make(chan struct{}),
		client:    client,
		topicID:   topicID,
		subID:     subID,
		Processor: processor,
		messages:  make(map[string]*pubsub.Message), // Cache of PubSub messages keyed by their ID
	}

	log.Info().Msgf("Created new PubSubQueue with topicID: %s, subID: %s, project ID: %s", topicID, subID, projectID)

	// Start receiving messages in the background
	go psq.receiveMessages()

	return psq, nil
}

// receiveMessages runs in a background goroutine and continuously receives messages
// and processes them using the provided processor function.
func (q *PubSubQueue) receiveMessages() {
	defer close(q.done)
	defer q.client.Close()
	log.Info().Msgf("Started receiving messages from PubSub topic: %s, subID: %s", q.topicID, q.subID)
	q.sub = q.client.Subscription(q.subID)

	err := q.sub.Receive(q.AppCtx, func(ctx context.Context, msg *pubsub.Message) {
		// Process the message using the provided processor function
		err := q.Processor(pubSubMessageID(msg), string(msg.Data))
		if err != nil {
			log.Error().Err(err).Msgf("Failed to process message: %s", msg.ID)

			// Store unacknowledged message in memory
			q.msgMutex.Lock()
			q.messages[msg.ID] = msg
			q.msgMutex.Unlock()
			return // Don't acknowledge the message if processing fails
		}

		// Acknowledge the message if processing is successful
		msg.Ack()
		log.Debug().Msgf("Acknowledged message: %s", msg.ID)
	})
	if err != nil && q.AppCtx.Err() == nil {
		log.Error().Err(err).Msg("Pub/Sub queue receiver stopped")
	}

	log.Info().Msgf("Stopped receiving messages from PubSub topic: %s, subID: %s", q.topicID, q.subID)
}

// Enqueue adds a new message to the Pub/Sub topic
func (q *PubSubQueue) Enqueue(ctx context.Context, id string, payload string, ttl int64) error {
	topic := q.client.Topic(q.topicID)
	result := topic.Publish(ctx, &pubsub.Message{
		Data: []byte(payload),
		Attributes: map[string]string{
			"id": id,
		},
	})
	_, err := result.Get(ctx)
	return err
}

// ConnectResponse processes unacknowledged messages stored in memory
func (q *PubSubQueue) ConnectResponse() {
	q.msgMutex.Lock()
	defer q.msgMutex.Unlock()

	for id, msg := range q.messages {
		log.Info().Msgf("Processing unacknowledged message: %s", id)

		// Process the message using the provided processor function
		err := q.Processor(pubSubMessageID(msg), string(msg.Data))
		if err != nil {
			log.Error().Err(err).Msgf("Failed to process message: %s, keeping in memory", id)
			continue // Keep the message in memory if processing fails
		}

		// If processing is successful, acknowledge and remove the message from memory
		msg.Ack()
		log.Debug().Msgf("Acknowledged and removed message: %s", id)
		delete(q.messages, id) // Remove from memory
	}
}

func (q *PubSubQueue) Done() <-chan struct{} { return q.done }

func pubSubMessageID(msg *pubsub.Message) string {
	if id := msg.Attributes["id"]; id != "" {
		return id
	}
	return msg.ID
}
