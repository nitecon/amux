package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/nitecon/amux/fism"
)

const InboundMessageReceivedID = "channel.message_received"

// InboundPayload is the validated channel message envelope.
type InboundPayload struct {
	ChannelName string
	RoomID      string
	Message     InboundMessage
}

// InboundMessage contains the provider identity and original message fields.
type InboundMessage struct {
	ID         string
	Content    string
	AuthorKind string
}

// InboundStore is the durable callback implemented by storage.Store.
type InboundStore interface {
	IngestRouterInbound(context.Context, fism.Event) (stop bool, err error)
}

// InboundDefinition declares locally emitted inbound channel messages.
func InboundDefinition() fism.EventDefinition {
	return fism.EventDefinition{
		ID: InboundMessageReceivedID, Version: 1, RemoteEmit: false,
		Validate: func(raw json.RawMessage) error {
			_, err := DecodeInboundPayload(raw)
			return err
		},
	}
}

type inboundPayloadJSON struct {
	ChannelName *string `json:"channel_name"`
	RoomID      *string `json:"room_id"`
	Message     *struct {
		ID         *string `json:"id"`
		Content    *string `json:"content"`
		AuthorKind *string `json:"author_kind"`
	} `json:"message"`
}

// DecodeInboundPayload validates and decodes one inbound envelope. Unknown
// fields are ignored; all required fields must be JSON strings, including when
// their values are empty strings.
func DecodeInboundPayload(raw json.RawMessage) (InboundPayload, error) {
	var body inboundPayloadJSON
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := decoder.Decode(&body); err != nil {
		return InboundPayload{}, errors.New("payload must be one JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return InboundPayload{}, errors.New("payload must be one JSON object")
	}
	if body.ChannelName == nil || body.RoomID == nil || body.Message == nil ||
		body.Message.ID == nil || body.Message.Content == nil || body.Message.AuthorKind == nil {
		return InboundPayload{}, errors.New("channel_name, room_id and message.id/content/author_kind must be strings")
	}
	return InboundPayload{
		ChannelName: *body.ChannelName,
		RoomID:      *body.RoomID,
		Message: InboundMessage{
			ID: *body.Message.ID, Content: *body.Message.Content, AuthorKind: *body.Message.AuthorKind,
		},
	}, nil
}

// InboundController adapts durable channel message ingestion to the router.
func InboundController(store InboundStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: "inbound",
			Subscriptions: []fism.Subscription{{
				Event: InboundMessageReceivedID, MinVersion: 1, MaxVersion: 1,
			}},
			FIFO: true,
		},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "inbound plugin store is nil"}
			}
			stop, err := store.IngestRouterInbound(ctx, event)
			if err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			if stop {
				return fism.Outcome{Kind: fism.Stop}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
