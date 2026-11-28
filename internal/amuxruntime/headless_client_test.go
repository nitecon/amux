package amuxruntime

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/gatewayclient"
	projectid "github.com/nitecon/amux/identity/project"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
	"github.com/urfave/cli/v3"
)

func runHeadlessClient(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	flags := commandFlags("amux client run", stderr)
	configPath := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "config", Value: "", Usage: "client configuration YAML (default ~/.amux/config.yaml)", Destination: configPath})
	state := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "state", Value: "", Usage: "durable worker journal", Destination: state})
	profile := new(string)
	flags.Flags = append(flags.Flags, &cli.StringFlag{Name: "enrolled-profile", Value: "", Usage: "enrolled gateway manifest", Destination: profile})
	if err := parseCommand(ctx, flags, args); err != nil {
		if errors.Is(err, errCommandHelp) {
			return nil
		}
		return err
	}
	if flags.Args().Len() != 0 {
		return fmt.Errorf("unexpected headless client arguments")
	}
	return startTestClient(ctx, *configPath, *state, *profile, stdout, stderr)
}

func startTestClient(ctx context.Context, configPath, state, profile string, stdout, stderr io.Writer) error {
	if profile == "" {
		path := configPath
		if path == "" {
			var err error
			path, err = config.DefaultClientPath()
			if err != nil {
				return err
			}
		}
		return runConfiguredClient(ctx, path, state, stdout, stderr)
	}
	if state == "" || configPath != "" {
		return fmt.Errorf("usage: amux headless --state PATH --enrolled-profile PATH")
	}
	manifest, err := absoluteEnrolledManifest(profile)
	if err != nil {
		return err
	}
	state, err = filepath.Abs(state)
	if err != nil {
		return err
	}
	settings := config.Client{EnrolledProfile: manifest, WorkerJournal: state}
	if err := settings.Validate(); err != nil {
		return err
	}
	client, _, err := gatewayclient.NewForProfile("", settings.EnrolledProfile, "")
	if err != nil {
		return err
	}
	lock, err := server.LockProfile(settings.WorkerJournal)
	if err != nil {
		return err
	}
	defer lock.Close()
	store, err := storage.Open(ctx, settings.WorkerJournal)
	if err != nil {
		return err
	}
	defer store.Close()
	pools, err := store.WorkerPools(ctx, client.Endpoint())
	if err != nil {
		return err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	open := worker.OpenWithNativeTools(worker.NativeToolsConfig{Executable: executable, Server: client.Endpoint(),
		EnrolledProfile: settings.EnrolledProfile})
	if len(pools) == 0 {
		return fmt.Errorf("no checkouts registered for the enrolled gateway")
	}
	type runningPool struct {
		cancel context.CancelFunc
		done   chan error
	}
	running := make(map[string]runningPool)
	defer func() {
		for _, run := range running {
			run.cancel()
		}
		for _, run := range running {
			<-run.done
		}
	}()
	unavailable := make(map[string]bool)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	reportFailed := false
	for ctx.Err() == nil {
		report := make([]storage.CheckoutReportItem, 0, len(pools))
		reported := make(map[string]bool)
		unavailableProjects := make(map[string]bool)
		for _, pool := range pools {
			checkout, err := projectid.ResolveCheckout(ctx, pool.Directory)
			if ctx.Err() != nil {
				return nil
			}
			run, started := running[pool.ID]
			if err != nil || checkout.Directory != pool.Directory {
				unavailableProjects[pool.Project] = true
				if started {
					run.cancel()
					<-run.done
					delete(running, pool.ID)
				}
				if !unavailable[pool.ID] {
					fmt.Fprintf(stderr, "project %s: checkout %q is unavailable or no longer resolves to its registered path\n", pool.Project, pool.Directory)
					unavailable[pool.ID] = true
				}
				continue
			}
			delete(unavailable, pool.ID)
			if started {
				select {
				case err := <-run.done:
					run.cancel()
					delete(running, pool.ID)
					fmt.Fprintf(stderr, "project %s: worker pool stopped (%v); retrying\n", pool.Project, err)
					started = false
				default:
				}
			}
			if !started {
				poolCtx, cancel := context.WithCancel(ctx)
				done := make(chan error, 1)
				running[pool.ID] = runningPool{cancel: cancel, done: done}
				fmt.Fprintf(stdout, "starting worker pool %s for %s\n", pool.ID, pool.Project)
				go func() {
					done <- worker.RunPool(poolCtx, worker.PoolConfig{Store: store, Client: client, PoolID: pool.ID, Open: open})
				}()
			}
			if !reported[pool.Project] {
				var origin *string
				if checkout.Upstream != "" {
					origin = &checkout.Upstream
				}
				report = append(report, storage.CheckoutReportItem{Project: pool.Project, Origin: origin})
				reported[pool.Project] = true
			}
		}
		for project := range unavailableProjects {
			if !reported[project] {
				report = append(report, storage.CheckoutReportItem{Project: project, Unavailable: true})
			}
		}
		_, err := client.SyncCheckouts(ctx, hostname, report)
		if err != nil && !reportFailed && ctx.Err() == nil {
			fmt.Fprintf(stderr, "report checkouts: %v\n", err)
		}
		reportFailed = err != nil
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return nil
}
