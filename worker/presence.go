package worker

import (
	"context"
	"os"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/projectidentity"
	"github.com/nitecon/amux/storage"
)

func registerAndReportPresence(ctx context.Context, client *gatewayclient.Client, project, directory string, origin storage.AgentOrigin, hostname *string, report storage.PresenceReport) error {
	if err := RegisterTaskTarget(ctx, client, project, directory, origin, hostname); err != nil {
		return err
	}
	_, err := client.ReportPresence(ctx, report)
	return err
}

// RegisterTaskTarget refreshes the target directory supported by both gateways.
func RegisterTaskTarget(ctx context.Context, client *gatewayclient.Client, project, directory string, origin storage.AgentOrigin, hostname *string) error {
	identity, err := projectidentity.Resolve(ctx, directory)
	if err != nil {
		return err
	}
	name := AgentDisplayName(origin)
	_, err = client.RegisterTarget(ctx, project, storage.TargetRegistration{
		Origin: origin, Generation: origin.InstanceID,
		CanonicalRemote: optionalNonEmpty(identity.Upstream), Hostname: hostname, DisplayName: &name,
	})
	return err
}

func workerMachineMetadata() (string, *string) {
	machineOS := runtime.GOOS
	if machineOS == "darwin" {
		machineOS = "macos"
	}
	var hostname *string
	if value, err := os.Hostname(); err == nil {
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 128 && strings.IndexFunc(value, unicode.IsControl) < 0 {
			hostname = &value
		}
	}
	return machineOS, hostname
}

func optionalNonEmpty(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func nextRunPresence(ctx context.Context, store *storage.Store, runID, clientRun string, empty bool) (storage.WorkerRun, storage.PresenceReport, *string, error) {
	current, err := store.NextWorkerRunReport(ctx, runID)
	if err != nil {
		return storage.WorkerRun{}, storage.PresenceReport{}, nil, err
	}
	machineOS, hostname := workerMachineMetadata()
	report := storage.PresenceReport{
		Source: "worker:" + current.ID, Sequence: current.PresenceSequence,
		OS: machineOS, Hostname: hostname, ClientRun: &clientRun,
	}
	if !empty {
		generation := current.Owner.InstanceID
		report.Agents = []storage.PresenceAgent{{
			ProjectIdent: current.Project, Origin: current.Owner, Generation: &generation,
			Hostname: hostname, Goal: current.Goal, Busy: true, AutoAssign: false,
			ReviewContextClear: false,
		}}
	}
	return *current, report, hostname, nil
}

func sendRunPresence(ctx context.Context, client *gatewayclient.Client, run storage.WorkerRun, report storage.PresenceReport, hostname *string) error {
	return registerAndReportPresence(ctx, client, run.Project, run.Directory, run.Owner, hostname, report)
}

func omitRunPresence(client *gatewayclient.Client, store *storage.Store, runID, clientRun string) {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, report, _, err := nextRunPresence(cleanupCtx, store, runID, clientRun, true)
	if err != nil {
		return
	}
	for cleanupCtx.Err() == nil {
		_, err = client.ReportPresence(cleanupCtx, report)
		if err == nil || !gatewayclient.Retryable(err) {
			return
		}
		if pause(cleanupCtx, 100*time.Millisecond) != nil {
			return
		}
	}
}
