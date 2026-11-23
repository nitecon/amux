package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

// Source-case evidence map for crates/gateway-backend/src/plugins/presence.rs:
//
//   legacy_directory_and_event_state_merge_without_granting_observations_routing_authority
//     -> TestPresenceObservationFullRustProfileKeepsArchivesAndSeparateCursors,
//        TestPresenceObservationMigrationImportsRetainedAndLegacyRows,
//        TestPresenceObservationSummaryMatchesLegacyInventoryContract,
//        TestRawPresenceContractsKeepObservationsOutOfNativeCapabilities (below).
//   event_and_advertisement_leases_share_identity_but_preserve_independent_contracts
//     -> TestPresenceObservationRuntimePreservesCausalityAndIndependentLeases,
//        TestRawPresenceContractsKeepObservationsOutOfNativeCapabilities (below).
//   delayed_rest_ad_cannot_erase_busy_state_or_release_event_source_ownership
//     -> TestTargetRegistrationAndNativePresenceHaveIndependentLeases,
//        TestPresenceContractMovedIdentityKeepsGlobalForeignSourceOwnership (below),
//        TestRawPresenceContractsKeepObservationsOutOfNativeCapabilities (below).
//   registration_host_survives_routing_invalidation_rebinding_and_departure
//     -> TestTargetRegistrationRebindingRevokesRoutingButKeepsDisplayHistory,
//        TestPresenceContractRegisteredHostSurvivesRawDepartureAndRebinding (below).
//   invalid_native_repository_reports_do_not_block_valid_v1_or_native_siblings
//     -> TestRawHeartbeatAllowsEmptyLegacyIdentityAndSkipsBadBindingSibling,
//        TestPresenceBindingFilterKeepsEmptySnapshotClock.
//   heartbeat_sequence_survives_omission_and_rejects_reverse_order_and_replay
//     -> TestReconcilePresenceEventPreservesRawAndFencesActivity,
//        TestRawHeartbeatCursorAndNativePrecedence.
//   activity_sequence_fences_drafts_identity_changes_and_backwards_evaluation_time
//     -> TestPresenceContractIdentityResetAndReverseOutputOrder (below),
//        TestPresenceObservationRuntimePreservesCausalityAndIndependentLeases.
//   clock_expiry_rejects_expired_replay_but_accepts_a_delayed_live_heartbeat
//     -> TestPresenceContractDelayedLiveHeartbeatRecoversAfterExpiry (below),
//        TestPresenceObservationRuntimePreservesCausalityAndIndependentLeases.
//   failed_presence_transition_does_not_advance_the_report_cursor
//     -> TestPresenceObservationFanoutFailureRollsBackCursorClockAndRows,
//        TestPresenceTransitionFailureRollsBackStateClockAndReportCursor.
//   summary_counts_machines_oses_agents_and_busy_agents
//     -> TestPresenceObservationSummaryMatchesLegacyInventoryContract,
//        TestPresenceSummaryCountsCurrentMachinesAgentsAndOS.
//   summary_counts_machines_by_registered_hostname
//     -> TestPresenceSummarySourceFallbackPreservesCase,
//        TestPresenceSummaryCountsCurrentMachinesAgentsAndOS.
//   summary_counts_empty_machine_inventories_and_unresolved_agents_without_os_deduplication
//     -> TestPresenceObservationSummaryMatchesLegacyInventoryContract.
//   controller_snapshot_excludes_expired_reports_and_checks_idle_eligibility
//     -> TestPresenceTransitionsDurabilityAndMonotonicClock,
//        TestPresenceUnknownDraftGoneOmissionAndReplay,
//        TestPresenceContractDelayedLiveHeartbeatRecoversAfterExpiry (below).
//   heartbeats_and_activity_drive_online_idle_busy_and_gone
//     -> TestPresenceObservationRuntimePreservesCausalityAndIndependentLeases,
//        TestPresenceContractIdentityResetAndReverseOutputOrder (below).
//
// Raw event presence intentionally cannot carry native auto-assign/review-clear
// authority, and raw project/session identifiers are not constrained to UUIDs.

func TestPresenceContractIdentityResetAndReverseOutputOrder(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-contract-identity.db"))
	registry := presenceContractJournalRegistry(t, s)
	base := time.Now().Add(-time.Minute).UnixMilli()
	emit := func(id, source, payload string, at int64) {
		t.Helper()
		event := emitObservationFixtureEvent(t, s, registry, id, source, payload, at)
		if err := s.ReconcilePresenceEvent(ctx, registry, event); err != nil {
			t.Fatalf("reconcile %s: %v", id, err)
		}
	}
	identity := `"project":"presence","instance_id":"legacy-instance","session_id":"legacy-session","provider":"codex","os":"linux"`
	emit(plugins.ClientHeartbeatID, "legacy:identity", `{"agents":[{`+identity+`,"generation":"first"}]}`, base)
	emit(plugins.ClientActivityID, "legacy:identity", `{"session_id":"legacy-session","instance_id":"legacy-instance","output_at":`+strconv.FormatInt(base-60_000, 10)+`,"draft":true}`, base+1)
	if live, err := s.LivePresence(ctx, time.UnixMilli(base+2)); err != nil || len(live) != 1 || live[0].State != "busy" || !live[0].Draft {
		t.Fatalf("draft activity was not busy: %+v %v", live, err)
	}
	// A changed identity for the same project/instance/session starts a fresh
	// execution and clears activity and draft state.
	emit(plugins.ClientHeartbeatID, "legacy:identity", `{"agents":[{`+identity+`,"generation":"second"}]}`, base+2)
	if live, err := s.LivePresence(ctx, time.UnixMilli(base+3)); err != nil || len(live) != 1 || live[0].State != "unknown" || live[0].Draft || live[0].LastActivityAt != nil {
		t.Fatalf("changed identity retained prior activity: %+v %v", live, err)
	}
	// A later event sequence with an older output timestamp updates the draft,
	// while the last observed output time remains monotonic.
	emit(plugins.ClientActivityID, "legacy:identity", `{"session_id":"legacy-session","instance_id":"legacy-instance","output_at":`+strconv.FormatInt(base-20_000, 10)+`,"draft":false}`, base+3)
	emit(plugins.ClientActivityID, "legacy:identity", `{"session_id":"legacy-session","instance_id":"legacy-instance","output_at":`+strconv.FormatInt(base-70_000, 10)+`,"draft":true}`, base+4)
	live, err := s.LivePresence(ctx, time.UnixMilli(base+5))
	if err != nil || len(live) != 1 || live[0].State != "busy" || !live[0].Draft || live[0].LastActivityAt == nil || *live[0].LastActivityAt != base-20_000 {
		t.Fatalf("reverse output order changed current activity contract: %+v %v", live, err)
	}
}

func TestPresenceContractMovedIdentityKeepsGlobalForeignSourceOwnership(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-contract-ownership.db"))
	registry := rawPresenceRegistry(t)
	base := time.Now().Add(-time.Minute).UnixMilli()
	first := `{"agents":[{"project":"presence","instance_id":"old-instance","session_id":"shared-session","provider":"legacy","os":"linux"}]}`
	moved := `{"agents":[{"project":"presence-moved","instance_id":"new-instance","session_id":"shared-session","provider":"legacy","os":"linux"}]}`
	foreign := `{"agents":[{"project":"presence-foreign","instance_id":"foreign-instance","session_id":"shared-session","provider":"legacy","os":"linux"}]}`
	for _, event := range []struct {
		source string
		seq    int64
		at     int64
		body   string
	}{{"legacy:one", 1, base, first}, {"legacy:two", 2, base + 1, foreign}, {"legacy:one", 3, base + 2, moved}} {
		raw := rawPresenceEvent(plugins.ClientHeartbeatID, event.source, event.seq, event.at, event.body)
		if err := s.ReconcilePresenceEvent(ctx, registry, raw); err != nil {
			t.Fatal(err)
		}
	}
	live, err := s.LivePresence(ctx, time.UnixMilli(base+3))
	if err != nil || len(live) != 1 || live[0].ProjectIdent != "presence-moved" || live[0].Origin.InstanceID != "new-instance" || live[0].Source != "legacy:one" {
		t.Fatalf("moved session lost its source owner or admitted foreign source: %+v %v", live, err)
	}
	// Raw legacy aliases remain valid identities, but never produce manual
	// registration entries or native review/assignment authority.
	if targets, err := s.Targets(ctx, "presence", time.UnixMilli(base+3)); err != nil || len(targets) != 0 {
		t.Fatalf("raw identity created a native target: %+v %v", targets, err)
	}
}

func TestPresenceContractDelayedLiveHeartbeatRecoversAfterExpiry(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-contract-delayed.db"))
	registry := presenceContractJournalRegistry(t, s)
	base := time.Now().Add(-4 * time.Minute).UnixMilli()
	body := `{"agents":[{"project":"presence","instance_id":"legacy-instance","session_id":"legacy-session","provider":"codex","os":"linux"}]}`
	reconcile := func(at int64, payload string) {
		t.Helper()
		event := emitObservationFixtureEvent(t, s, registry, plugins.ClientHeartbeatID, "legacy:delayed", payload, at)
		if err := s.ReconcilePresenceEvent(ctx, registry, event); err != nil {
			t.Fatal(err)
		}
	}
	reconcile(base, body)
	// An expired replay cannot resurrect, while a delayed heartbeat still inside
	// the 90-second activity window can correct the early expiry.
	reconcile(base+presenceGoneAfter, `{"agents":[]}`)
	reconcile(base, body)
	if live, err := s.LivePresence(ctx, time.UnixMilli(base+presenceGoneAfter)); err != nil || len(live) != 0 {
		t.Fatalf("expired heartbeat replay resurrected presence: %+v %v", live, err)
	}
	reconcile(base+presenceGoneAfter/2, body)
	live, err := s.LivePresence(ctx, time.UnixMilli(base+presenceGoneAfter))
	if err != nil || len(live) != 1 || live[0].Origin.SessionID != "legacy-session" {
		t.Fatalf("delayed but live heartbeat did not restore presence: %+v %v", live, err)
	}
}

func TestPresenceContractRegisteredHostSurvivesRawDepartureAndRebinding(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-contract-host.db"))
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc',canonical_remote=NULL WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(-time.Minute)
	origin := AgentOrigin{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", InstanceID: "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb", Provider: "codex", OS: "linux"}
	registry := rawPresenceRegistry(t)
	base := now.UnixMilli()
	heartbeat := rawPresenceEvent(plugins.ClientHeartbeatID, "legacy:registered", 1, base, `{"agents":[{"project":"presence","instance_id":"`+origin.InstanceID+`","session_id":"`+origin.SessionID+`","provider":"codex","os":"linux"}]}`)
	if err := s.ReconcilePresenceEvent(ctx, registry, heartbeat); err != nil {
		t.Fatal(err)
	}
	activity := rawPresenceEvent(plugins.ClientActivityID, "legacy:registered", 2, base+1, `{"session_id":"`+origin.SessionID+`","instance_id":"`+origin.InstanceID+`","output_at":`+strconv.FormatInt(base, 10)+`,"draft":true}`)
	if err := s.ReconcilePresenceEvent(ctx, registry, activity); err != nil {
		t.Fatal(err)
	}
	live, err := s.LivePresence(ctx, time.UnixMilli(base+2))
	if err != nil || len(live) != 1 || !live[0].Draft || live[0].State != "busy" {
		t.Fatalf("raw activity did not produce busy/draft state before registration: %+v %v", live, err)
	}
	if _, err := s.RegisterTarget(ctx, "presence", TargetRegistration{Origin: origin, Generation: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", Hostname: ptr("RegisteredHost")}, time.UnixMilli(base+2)); err != nil {
		t.Fatal(err)
	}
	live, err = s.LivePresence(ctx, time.UnixMilli(base+3))
	if err != nil || len(live) != 1 || !live[0].Draft || live[0].State != "busy" {
		t.Fatalf("delayed manual advertisement changed raw busy/draft state: %+v %v", live, err)
	}
	if targets, err := s.Targets(ctx, "presence", time.UnixMilli(base+2)); err != nil || len(targets) != 1 {
		t.Fatalf("registration disappeared during observation: %+v %v", targets, err)
	}
	departure := rawPresenceEvent(plugins.ClientHeartbeatID, "legacy:registered", 3, base+3, `{"agents":[]}`)
	if err := s.ReconcilePresenceEvent(ctx, registry, departure); err != nil {
		t.Fatal(err)
	}
	originJSON, err := json.Marshal(origin)
	if err != nil {
		t.Fatal(err)
	}
	if host, err := registeredNotificationHost(ctx, s.db, "presence", originJSON); err != nil || host == nil || *host != "RegisteredHost" {
		t.Fatalf("departure erased registered host history: %v %v", host, err)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='repo',canonical_remote='github.com/example/rebound' WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	if targets, err := s.Targets(ctx, "presence", time.UnixMilli(base+4)); err != nil || len(targets) != 0 {
		t.Fatalf("rebound project retained routing authority: %+v %v", targets, err)
	}
	if host, err := registeredNotificationHost(ctx, s.db, "presence", originJSON); err != nil || host == nil || *host != "RegisteredHost" {
		t.Fatalf("rebind erased registered host history: %v %v", host, err)
	}
}

func TestRawPresenceContractsKeepObservationsOutOfNativeCapabilities(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-contract-capabilities.db"))
	if _, err := s.db.ExecContext(ctx, "UPDATE projects SET kind='adhoc' WHERE ident='presence'"); err != nil {
		t.Fatal(err)
	}
	registry := rawPresenceRegistry(t)
	base := time.Now().Add(-time.Minute).UnixMilli()
	body := `{"agents":[{"project":"presence","instance_id":"bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb","session_id":"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa","provider":"codex","os":"linux","auto_assign":true,"review_context_clear":true}]}`
	event := rawPresenceEvent(plugins.ClientHeartbeatID, "legacy:capability", 1, base, body)
	if err := s.ReconcilePresenceEvent(ctx, registry, event); err != nil {
		t.Fatal(err)
	}
	live, err := s.LivePresence(ctx, time.UnixMilli(base+1))
	if err != nil || len(live) != 1 || live[0].AutoAssign || live[0].ReviewContextClear {
		t.Fatalf("raw payload gained native execution capability: %+v %v", live, err)
	}
	if targets, err := s.Targets(ctx, "presence", time.UnixMilli(base+1)); err != nil || len(targets) != 0 {
		t.Fatalf("raw payload granted manual routing: %+v %v", targets, err)
	}
}

func presenceContractJournalRegistry(t *testing.T, s *Store) *router.Registry {
	t.Helper()
	runtime, err := router.NewRuntime(s, s.EventBus(), nativePresenceDefinitions(), []router.Plugin{plugins.PresencePlugin(s), nativePresenceObserver()}, router.RunConfig{Capacity: 4})
	if err != nil {
		t.Fatal(err)
	}
	return runtime.Registry()
}
