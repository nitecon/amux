package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func startLocalGateway(ctx context.Context, database, address, key string) (string, func(), error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return "", nil, fmt.Errorf("start the local gateway: %w", err)
	}
	lock, err := server.LockProfile(database)
	if err != nil {
		listener.Close()
		return "", nil, err
	}
	store, err := storage.Open(ctx, database)
	if err != nil {
		listener.Close()
		lock.Close()
		return "", nil, err
	}
	serveCtx, cancel := context.WithCancel(ctx)
	closeGateway := func() {
		cancel()
		listener.Close()
		store.Close()
		lock.Close()
	}
	if err := store.RecoverTaskConsumers(serveCtx); err != nil {
		closeGateway()
		return "", nil, err
	}
	builtins, err := server.NewGatewayRouter(store, 30, 7)
	if err != nil {
		closeGateway()
		return "", nil, err
	}
	store.UseRouterReviews()
	ready := make(chan struct{})
	routerDone := make(chan error, 1)
	go func() {
		routerDone <- builtins.Run(serveCtx, ready)
		cancel()
	}()
	select {
	case <-ready:
	case err := <-routerDone:
		closeGateway()
		if err == nil {
			err = serveCtx.Err()
		}
		return "", nil, fmt.Errorf("start local gateway router: %w", err)
	case <-serveCtx.Done():
		<-routerDone
		closeGateway()
		return "", nil, serveCtx.Err()
	}
	handler, err := server.NewHandlerWithRemote(store, key, server.RemoteConfig{
		Context: serveCtx, Registry: builtins.Registry(), Forwarded: plugins.RemoteForwardedEvents(),
	})
	if err != nil {
		cancel()
		<-routerDone
		closeGateway()
		return "", nil, err
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := server.Serve(serveCtx, listener, handler, 5*time.Second); err != nil {
			fmt.Fprintf(os.Stderr, "amux local gateway: %v\n", err)
		}
		cancel()
		if err := <-routerDone; err != nil {
			fmt.Fprintf(os.Stderr, "amux local gateway router: %v\n", err)
		}
		closeGateway()
	}()
	return "http://" + listener.Addr().String(), func() {
		cancel()
		<-done
	}, nil
}
