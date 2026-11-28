package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
	"github.com/rs/zerolog/log"
)

const inboundSourceRetryDelay = time.Second

// RunInboundReconciliation drains accepted messages independently of other observations.
func RunInboundReconciliation(ctx context.Context, registry *fism.Registry, bus *eventbus.Bus, drain func(context.Context, *fism.Registry, int) (int, error)) error {
	return runReconciliationSource(ctx, registry, bus, "inbound", drain, eventbus.ObservationAccepted)
}

// RunInboundSource durably journals provider messages in input order. A
// message keeps one UID for every retry, so a lost commit acknowledgement is
// resolved by the router's idempotent event journal. Cancellation may leave
// buffered input unjournaled; the provider resumes from the last persisted
// project cursor on the next startup.
func RunInboundSource(ctx context.Context, emitter EventEmitter, registry *fism.Registry, input <-chan channels.InboundEvent) error {
	if ctx == nil || emitter == nil || registry == nil || input == nil {
		return errors.New("inbound source requires context, emitter, registry, and input")
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case incoming, ok := <-input:
			if !ok {
				return nil
			}
			payload, err := json.Marshal(struct {
				ChannelName string `json:"channel_name"`
				RoomID      string `json:"room_id"`
				Message     struct {
					ID         string `json:"id"`
					Content    string `json:"content"`
					AuthorKind string `json:"author_kind"`
				} `json:"message"`
			}{
				ChannelName: incoming.ChannelName,
				RoomID:      incoming.RoomID,
				Message: struct {
					ID         string `json:"id"`
					Content    string `json:"content"`
					AuthorKind string `json:"author_kind"`
				}{ID: incoming.Message.ID, Content: incoming.Message.Content, AuthorKind: incoming.Message.AuthorKind},
			})
			if err != nil {
				return fmt.Errorf("encode inbound channel message: %w", err)
			}
			request := fism.EmitRequest{
				UID: uuid.NewString(), ID: InboundMessageReceivedID, Version: 1,
				Source: "channel:" + incoming.ChannelName, Payload: payload,
			}
			for {
				result, emitErr := emitter(ctx, registry, request)
				if emitErr == nil && (result.Stored || result.Replayed) {
					break
				}
				if ctx.Err() != nil {
					return nil
				}
				if emitErr == nil {
					emitErr = errors.New("router did not durably accept inbound event")
				}
				log.Warn().Ctx(ctx).Str("source", request.Source).Err(emitErr).Msg("inbound channel event persist failed")
				timer := time.NewTimer(inboundSourceRetryDelay)
				select {
				case <-ctx.Done():
					stopSourceTimer(timer)
					return nil
				case <-timer.C:
				}
			}
		}
	}
}
