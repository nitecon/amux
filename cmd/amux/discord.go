package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/storage"
)

const discordInboundQueueSize = 256

type discordRoomRegistrar interface {
	RegisterRoom(roomID string, afterID *string) error
}

type discordInboundProvider interface {
	discordRoomRegistrar
	Run(context.Context, chan<- channels.InboundEvent) error
}

// registerDiscordRooms restores the provider's per-room message cursors before
// its inbound loop begins. AllProjects intentionally includes __global__.
func registerDiscordRooms(ctx context.Context, store *storage.Store, discord discordRoomRegistrar) error {
	if ctx == nil || store == nil || discord == nil {
		return fmt.Errorf("Discord room registration requires context, store, and provider")
	}
	projects, err := store.AllProjects(ctx)
	if err != nil {
		return fmt.Errorf("list projects for Discord ingress: %w", err)
	}
	for _, project := range projects {
		if project.ChannelName != "discord" || project.RoomID == "" {
			continue
		}
		if err := discord.RegisterRoom(project.RoomID, project.LastMsgID); err != nil {
			slog.WarnContext(ctx, "skip invalid Discord project room", "project", project.Ident, "error", err)
		}
	}
	return nil
}

type discordIngress struct {
	providerDone chan error
	sourceDone   chan error
}

func startDiscordIngress(ctx context.Context, discord discordInboundProvider, emitter plugins.EventEmitter, registry *router.Registry) *discordIngress {
	events := make(chan channels.InboundEvent, discordInboundQueueSize)
	run := &discordIngress{providerDone: make(chan error, 1), sourceDone: make(chan error, 1)}
	go func() {
		run.sourceDone <- plugins.RunInboundSource(ctx, emitter, registry, events)
	}()
	go func() {
		err := discord.Run(ctx, events)
		close(events)
		if err != nil && !(ctx.Err() != nil && errors.Is(err, context.Canceled)) {
			slog.ErrorContext(ctx, "Discord inbound source stopped", "error", err)
		} else if err == nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "Discord inbound source stopped unexpectedly")
		}
		run.providerDone <- err
	}()
	return run
}
