package containerruntime

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
)

func TestManagerCheckPersistsCASAndFirstActivationRecordsStartedContainer(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	checker := &managerCheckerFake{candidate: candidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)

	checked, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if checked.StateDigest == "" || checked.Candidate == nil {
		t.Fatalf("check receipt = %#v", checked)
	}
	if _, found, err := manager.store.load(); err != nil || !found {
		t.Fatalf("first check did not persist a CAS state: found=%t err=%v", found, err)
	}
	staged, err := manager.Stage(context.Background(), StageRequest{
		ExpectedManifestSHA256: candidate.ManifestSHA256, ExpectedStateDigest: checked.StateDigest,
		ExpectedRoutingGeneration: checked.RoutingGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease := fixture.secretServer.lease
	lease.onWait = func() error {
		journal, found, err := manager.store.loadJournal()
		if err != nil || !found || journal.Phase != phaseNewStarted || journal.NewContainerID != ContainerName {
			t.Fatalf("container start was not journaled before bootstrap wait: %#v %t %v", journal, found, err)
		}
		events = append(events, "secret-wait")
		return nil
	}
	activated, err := manager.Activate(context.Background(), ActivateRequest{
		ExpectedStateDigest: staged.StateDigest, ExpectedRoutingGeneration: staged.RoutingGeneration,
		ConfirmDesktopExited: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if activated.State != StateHealthy || activated.Active == nil || activated.Staged != nil || activated.Active.ManifestSHA256 != candidate.ManifestSHA256 {
		t.Fatalf("activation receipt = %#v", activated)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("successful activation retained journal: found=%t err=%v", found, err)
	}
	assertEventOrder(t, events, "runtime-start", "secret-wait", "runtime-verify", "http-probe", "route-activate")
}

func TestManagerCheckWithoutStableManifestProjectsUnavailableWithoutMutation(t *testing.T) {
	fixture := newManagerFixture(t)
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{unavailable: true}, runtimeFake, routingFake, &events)

	checked, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if checked.State != StateUnavailable || checked.Status != runtimemanifest.CheckStatusUnavailable ||
		checked.Reason != "stable_runtime_manifest_unavailable" || checked.Compatible || checked.Candidate != nil ||
		checked.Staged != nil || checked.Active != nil {
		t.Fatalf("check receipt = %#v", checked)
	}
	state, found, err := manager.store.load()
	if err != nil || !found || state.Status != StateStopped || state.Staged != nil || state.Active != nil {
		t.Fatalf("durable state was reclassified or mutated: found=%t state=%#v err=%v", found, state, err)
	}
	if runtimeFake.pullCount != 0 || len(events) != 0 {
		t.Fatalf("unavailable check reached runtime mutation: pulls=%d events=%#v", runtimeFake.pullCount, events)
	}
	if _, err := manager.Stage(context.Background(), StageRequest{
		ExpectedManifestSHA256:    strings.Repeat("a", 64),
		ExpectedStateDigest:       checked.StateDigest,
		ExpectedRoutingGeneration: checked.RoutingGeneration,
	}); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("manual candidate digest stage error = %v", err)
	}
	if runtimeFake.pullCount != 0 || len(events) != 0 {
		t.Fatalf("manual candidate digest reached runtime mutation: pulls=%d events=%#v", runtimeFake.pullCount, events)
	}
}

func TestManagerStopAndRecoverRequireFreshDesktopExitBeforeStateMutation(t *testing.T) {
	for _, operation := range []string{"stop", "recover"} {
		t.Run(operation, func(t *testing.T) {
			fixture := newManagerFixture(t)
			events := []string{}
			runtimeFake := &managerRuntimeFake{events: &events}
			routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
			manager := fixture.manager(t, &managerCheckerFake{}, runtimeFake, routingFake, &events)

			var err error
			switch operation {
			case "stop":
				_, err = manager.Stop(context.Background(), StopRequest{})
			case "recover":
				_, err = manager.Recover(context.Background(), RecoverRequest{})
			}
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("error=%v, want invalid request", err)
			}
			if _, statErr := os.Lstat(filepath.Join(fixture.root, stateFileName)); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("state mutated before Desktop exit confirmation: %v", statErr)
			}
			if len(events) != 0 || routingFake.lastRequest != nil || runtimeFake.exists {
				t.Fatalf("mutation escaped confirmation gate: events=%#v route=%#v runtime=%t", events, routingFake.lastRequest, runtimeFake.exists)
			}
		})
	}
}

func TestManagerStageRejectsStaleStateAndRoutingCAS(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	checker := &managerCheckerFake{candidate: candidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 7}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	request := StageRequest{ExpectedManifestSHA256: candidate.ManifestSHA256, ExpectedStateDigest: strings.Repeat("0", 64), ExpectedRoutingGeneration: 7}
	if _, err := manager.Stage(context.Background(), request); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("state CAS error = %v", err)
	}
	request.ExpectedStateDigest = checked.StateDigest
	request.ExpectedRoutingGeneration = 8
	if _, err := manager.Stage(context.Background(), request); !errors.Is(err, ErrRoutingChanged) {
		t.Fatalf("routing CAS error = %v", err)
	}
	if runtimeFake.pullCount != 0 {
		t.Fatalf("stale CAS reached image pull %d times", runtimeFake.pullCount)
	}
}

func TestManagerUpdateDrainsBeforeCloneAndRollsBackPinnedOlderSequence(t *testing.T) {
	fixture := newManagerFixture(t)
	oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
	newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
	checker := &managerCheckerFake{candidate: oldCandidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)

	checked, _ := manager.Check(context.Background())
	staged, err := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true})
	if err != nil {
		t.Fatal(err)
	}

	checker.candidate = newCandidate
	checked, err = manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged, err = manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	runtimeFake.failManifest = newCandidate.ManifestSHA256
	events = events[:0]
	_, err = manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("update error = %v", err)
	}
	assertEventOrder(t, events, "route-prepare", "clone", "runtime-stop", "runtime-delete", "runtime-start", "runtime-verify", "route-rollback")

	state, found, loadErr := manager.store.load()
	if loadErr != nil || !found {
		t.Fatal(loadErr)
	}
	if state.Status != StateHealthy || state.Active == nil || state.Active.ManifestSHA256 != oldCandidate.ManifestSHA256 ||
		state.Staged == nil || state.Staged.ManifestSHA256 != newCandidate.ManifestSHA256 || state.HighestSeenSequence != 2 {
		t.Fatalf("rolled-back state = %#v (prior active %#v)", state, active.Active)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("rollback retained journal: found=%t err=%v", found, err)
	}
}

func TestCommitNewIsIdempotentAfterStateWriteBeforeJournalRemoval(t *testing.T) {
	fixture := newManagerFixture(t)
	oldRecord, err := recordFromCandidate(fixture.candidate(t, "2.40.0", 1, 1))
	if err != nil {
		t.Fatal(err)
	}
	newRecord, err := recordFromCandidate(fixture.candidate(t, "2.41.0", 2, 2))
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 9, AppleActive: true, RuntimeRoutingPending: true}}
	manager := fixture.manager(t, &managerCheckerFake{}, &managerRuntimeFake{events: &events}, routingFake, &events)
	if err := manager.store.prepareRoot(); err != nil {
		t.Fatal(err)
	}
	state := durableState{
		Schema: SchemaVersion, InstallationID: strings.Repeat("1", 64), Status: StateHealthy,
		HighestSeenSequence: 2, Active: &newRecord, Previous: &oldRecord,
		ActiveGeneration: 2, PreviousGeneration: 1, NextGeneration: 3,
		ContainerID: ContainerName, ActiveOperationID: strings.Repeat("4", 64), RoutingGeneration: 9,
	}
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: strings.Repeat("2", 64),
		Phase: phaseVerified, ExpectedStateDigest: strings.Repeat("3", 64), ExpectedRoutingGeneration: 7,
		OldArtifact: &oldRecord, NewArtifact: newRecord, OldGeneration: 1, NewGeneration: 2,
		OldContainerID: ContainerName, OldOperationID: strings.Repeat("5", 64), NewContainerID: ContainerName,
	}
	journal.OperationID = state.ActiveOperationID
	if err := manager.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.commitNew(context.Background(), state, journal, ContainerName, 9); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := manager.store.load()
	if err != nil || loaded.Previous == nil || *loaded.Previous != oldRecord || loaded.PreviousGeneration != 1 {
		t.Fatalf("idempotent commit rotated previous again: %#v err=%v", loaded, err)
	}
}

func TestManagerCommitRetainsExactObsoleteGenerationCleanupUntilSafeRemoval(t *testing.T) {
	for _, test := range []struct {
		name             string
		unsafe           bool
		replacementCrash string
	}{
		{name: "transient removal retries after restart"},
		{name: "crash after cleared container witness", replacementCrash: "cleared"},
		{name: "crash after replacement start before witness", replacementCrash: "started"},
		{name: "unsafe symlink parks without deleting retained generations", unsafe: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			candidates := []runtimemanifest.Candidate{
				fixture.candidate(t, "2.40.0", 1, 1),
				fixture.candidate(t, "2.41.0", 2, 2),
				fixture.candidate(t, "2.42.0", 3, 3),
			}
			checker := &managerCheckerFake{}
			events := []string{}
			runtimeFake := &managerRuntimeFake{events: &events}
			routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
			manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)

			var active Inspection
			for index := 0; index < 2; index++ {
				checker.candidate = candidates[index]
				checked, err := manager.Check(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				staged, err := manager.Stage(context.Background(), StageRequest{
					ExpectedManifestSHA256: candidates[index].ManifestSHA256,
					ExpectedStateDigest:    checked.StateDigest, ExpectedRoutingGeneration: checked.RoutingGeneration,
				})
				if err != nil {
					t.Fatal(err)
				}
				active, err = manager.Activate(context.Background(), ActivateRequest{
					ExpectedStateDigest: staged.StateDigest, ExpectedRoutingGeneration: staged.RoutingGeneration,
					ConfirmDesktopExited: true,
				})
				if err != nil {
					t.Fatal(err)
				}
			}

			obsoletePath, err := manager.store.generationPath(1)
			if err != nil {
				t.Fatal(err)
			}
			if test.unsafe {
				backup := filepath.Join(fixture.root, "obsolete-generation-backup")
				if err := os.Rename(obsoletePath, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), obsoletePath); err != nil {
					t.Fatal(err)
				}
			} else {
				manager.removeGeneration = func(generation uint64) error {
					if generation != 1 {
						t.Fatalf("cleanup generation=%d, want exact obsolete generation 1", generation)
					}
					return io.ErrUnexpectedEOF
				}
			}

			checker.candidate = candidates[2]
			checked, err := manager.Check(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			staged, err := manager.Stage(context.Background(), StageRequest{
				ExpectedManifestSHA256: candidates[2].ManifestSHA256,
				ExpectedStateDigest:    checked.StateDigest, ExpectedRoutingGeneration: checked.RoutingGeneration,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Activate(context.Background(), ActivateRequest{
				ExpectedStateDigest: staged.StateDigest, ExpectedRoutingGeneration: staged.RoutingGeneration,
				ConfirmDesktopExited: true,
			}); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("cleanup failure error=%v", err)
			}
			state, found, stateErr := manager.store.load()
			journal, journalFound, journalErr := manager.store.loadJournal()
			if stateErr != nil || !found || journalErr != nil || !journalFound ||
				state.Status != StateRecoveryRequired || state.ActiveGeneration != 3 || state.PreviousGeneration != 2 ||
				journal.ObsoleteGeneration != 1 || journal.Phase != phaseRecoveryRequired {
				t.Fatalf("cleanup witness state=%#v found=%t err=%v journal=%#v found=%t err=%v", state, found, stateErr, journal, journalFound, journalErr)
			}
			for _, generation := range []uint64{state.ActiveGeneration, state.PreviousGeneration} {
				path, _ := manager.store.generationPath(generation)
				if info, err := os.Lstat(path); err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
					t.Fatalf("retained generation %d was mutated: info=%#v err=%v", generation, info, err)
				}
			}
			if test.replacementCrash != "" {
				manager.removeGeneration = manager.store.removeGeneration
				runtimeFake.failManifest = candidates[2].ManifestSHA256
				working := journal
				if err := manager.cleanupJournalNew(context.Background(), &working); err != nil {
					t.Fatal(err)
				}
				cleared, found, err := manager.store.loadJournal()
				if err != nil || !found || cleared.NewContainerID != "" || cleared.Phase != phaseRecoveryRequired || cleared.ObsoleteGeneration != 1 {
					t.Fatalf("cleared replacement witness=%#v found=%t err=%v", cleared, found, err)
				}
				runtimeFake.failManifest = ""
				if test.replacementCrash == "started" {
					path, _ := manager.store.generationPath(journal.NewGeneration)
					runtimeFake.current = startSpec(journal.InstallationID, journal.OperationID, journal.NewArtifact, journal.NewGeneration, path, "")
					runtimeFake.exists = true
				}
			}

			inspection, err := manager.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if test.unsafe {
				if _, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
					t.Fatalf("unsafe cleanup recovery error=%v", err)
				}
				if info, err := os.Lstat(obsoletePath); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Fatalf("unsafe obsolete path was mutated: info=%#v err=%v", info, err)
				}
				return
			}

			manager.removeGeneration = manager.store.removeGeneration
			recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
			if err != nil {
				t.Fatal(err)
			}
			if recovered.State != StateHealthy || recovered.Active == nil || recovered.Active.ManifestSHA256 != candidates[2].ManifestSHA256 {
				t.Fatalf("cleanup retry receipt=%#v (previous active=%#v)", recovered, active)
			}
			if _, err := os.Lstat(obsoletePath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("obsolete generation remains after retry: %v", err)
			}
			if _, found, err := manager.store.loadJournal(); err != nil || found {
				t.Fatalf("successful cleanup retained journal: found=%t err=%v", found, err)
			}
		})
	}
}

func TestManagerStopFailurePersistsWitnessAndRecoverCompletesForward(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	checker := &managerCheckerFake{candidate: candidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	staged, err := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true})
	if err != nil {
		t.Fatal(err)
	}
	runtimeFake.deleteFailure = true
	if _, err := manager.Stop(context.Background(), StopRequest{ExpectedStateDigest: active.StateDigest, ExpectedRoutingGeneration: active.RoutingGeneration, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("stop failure error = %v", err)
	}
	journal, found, err := manager.store.loadStopJournal()
	if err != nil || !found || journal.Phase != stopPhaseRecoveryRequired || journal.Artifact.ManifestSHA256 != candidate.ManifestSHA256 || journal.FinalRoutingGeneration == 0 {
		t.Fatalf("stop journal = %#v found=%t err=%v", journal, found, err)
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil || inspection.State != StateRecoveryRequired {
		t.Fatalf("recovery inspection = %#v err=%v", inspection, err)
	}
	runtimeFake.deleteFailure = false
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateStopped || recovered.Active == nil || recovered.Active.ManifestSHA256 != candidate.ManifestSHA256 {
		t.Fatalf("recovered stop = %#v", recovered)
	}
	if _, found, err := manager.store.loadStopJournal(); err != nil || found {
		t.Fatalf("stop recovery retained journal: found=%t err=%v", found, err)
	}
}

func TestManagerParkPersistsFailClosedStopWitnessWithoutRuntimeMutation(t *testing.T) {
	manager, runtimeFake, routingFake, active := activateManagerFixture(t)
	beforeEvents := len(*runtimeFake.events)
	parked, err := manager.Park(context.Background(), ParkRequest{
		ExpectedStateDigest:       active.StateDigest,
		ExpectedRoutingGeneration: active.RoutingGeneration,
	})
	if err != nil {
		t.Fatal(err)
	}
	if parked.State != StateRecoveryRequired || !parked.RecoveryRequired || parked.Active == nil {
		t.Fatalf("park receipt = %#v", parked)
	}
	if len(*runtimeFake.events) != beforeEvents || !routingFake.snapshot.AppleActive {
		t.Fatalf("park mutated runtime/route: events=%v route=%#v", *runtimeFake.events, routingFake.snapshot)
	}
	state, found, stateErr := manager.store.load()
	journal, journalFound, journalErr := manager.store.loadStopJournal()
	if stateErr != nil || !found || journalErr != nil || !journalFound ||
		state.Status != StateRecoveryRequired || journal.Phase != stopPhaseRecoveryRequired ||
		journal.ExpectedStateDigest != active.StateDigest ||
		journal.ExpectedRoutingGeneration != active.RoutingGeneration {
		t.Fatalf("park witness state=%#v found=%t err=%v journal=%#v found=%t err=%v", state, found, stateErr, journal, journalFound, journalErr)
	}

	recovered, err := manager.Recover(context.Background(), RecoverRequest{
		ExpectedStateDigest:  parked.StateDigest,
		ConfirmDesktopExited: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateStopped || routingFake.snapshot.AppleActive {
		t.Fatalf("park recovery = %#v route=%#v", recovered, routingFake.snapshot)
	}
}

func TestManagerRecoversExactOrdinaryRoutingRecoveryWitnessForActivationAndStop(t *testing.T) {
	t.Run("first activation", func(t *testing.T) {
		fixture := newManagerFixture(t)
		candidate := fixture.candidate(t, "2.40.0", 1, 1)
		events := []string{}
		runtimeFake := &managerRuntimeFake{events: &events}
		routingFake := &managerRoutingFake{
			events: &events, snapshot: RoutingSnapshot{Generation: 1},
			activateError: ErrUnavailable, leaveRouteRecovery: true, reconcileAdvance: 2,
		}
		manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
		checked, err := manager.Check(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		staged, err := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("activation routing failure=%v", err)
		}
		journal, found, err := manager.store.loadJournal()
		if err != nil || !found || routingFake.lastRequest == nil ||
			routingFake.lastRequest.Intent.OperationID != journal.OperationID ||
			routingFake.lastRequest.Intent.InstallationID != journal.InstallationID ||
			routingFake.lastRequest.Intent.NewManifestSHA256 != journal.NewArtifact.ManifestSHA256 ||
			routingFake.lastRequest.ExpectedOriginRoutingGeneration != journal.ExpectedRoutingGeneration {
			t.Fatalf("activation routing binding request=%#v journal=%#v found=%t err=%v", routingFake.lastRequest, journal, found, err)
		}
		inspection, err := manager.Inspect(context.Background())
		if err != nil || inspection.State != StateRecoveryRequired {
			t.Fatalf("activation recovery inspection=%#v err=%v", inspection, err)
		}
		recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
		if err != nil {
			t.Fatal(err)
		}
		if recovered.State != StateHealthy || recovered.RoutingGeneration != staged.RoutingGeneration+5 ||
			routingFake.reconcileCount != 1 || routingFake.snapshot.RuntimeRoutingPending {
			t.Fatalf("activation recovery receipt=%#v route=%#v reconciles=%d", recovered, routingFake.snapshot, routingFake.reconcileCount)
		}
	})

	t.Run("stop", func(t *testing.T) {
		manager, _, routingFake, active := activateManagerFixture(t)
		routingFake.stopError = ErrUnavailable
		routingFake.leaveRouteRecovery = true
		routingFake.reconcileAdvance = 2
		if _, err := manager.Stop(context.Background(), StopRequest{ExpectedStateDigest: active.StateDigest, ExpectedRoutingGeneration: active.RoutingGeneration, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("stop routing failure=%v", err)
		}
		journal, found, err := manager.store.loadStopJournal()
		if err != nil || !found || journal.FinalRoutingGeneration != 0 || routingFake.lastRequest == nil ||
			routingFake.lastRequest.Intent.OperationID != journal.OperationID ||
			routingFake.lastRequest.Intent.InstallationID != journal.InstallationID ||
			routingFake.lastRequest.Intent.OldManifestSHA256 != journal.Artifact.ManifestSHA256 {
			t.Fatalf("stop routing binding request=%#v journal=%#v found=%t err=%v", routingFake.lastRequest, journal, found, err)
		}
		inspection, err := manager.Inspect(context.Background())
		if err != nil || inspection.State != StateRecoveryRequired {
			t.Fatalf("stop recovery inspection=%#v err=%v", inspection, err)
		}
		recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
		if err != nil {
			t.Fatal(err)
		}
		if recovered.State != StateStopped || recovered.RoutingGeneration != active.RoutingGeneration+5 ||
			routingFake.reconcileCount != 1 || routingFake.snapshot.RuntimeRoutingPending {
			t.Fatalf("stop recovery receipt=%#v route=%#v reconciles=%d", recovered, routingFake.snapshot, routingFake.reconcileCount)
		}
		journal, found, err = manager.store.loadStopJournal()
		if err != nil || found {
			t.Fatalf("stop E+5 journal retained=%#v found=%t err=%v", journal, found, err)
		}
	})

	t.Run("restore first activation origin", func(t *testing.T) {
		fixture := newManagerFixture(t)
		candidate := fixture.candidate(t, "2.40.0", 1, 1)
		events := []string{}
		runtimeFake := &managerRuntimeFake{events: &events}
		routingFake := &managerRoutingFake{
			events: &events, snapshot: RoutingSnapshot{Generation: 1},
			activateError: ErrUnavailable, leaveRouteRecovery: true, reconcileAdvance: 2,
		}
		manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
		checked, _ := manager.Check(context.Background())
		staged, err := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("activation routing failure=%v", err)
		}
		// Make the forward candidate unverifiable. Recovery must consume the same
		// pending operation witness in restore_origin direction and return to the
		// exact pre-activation stopped state.
		runtimeFake.failManifest = candidate.ManifestSHA256
		inspection, err := manager.Inspect(context.Background())
		if err != nil || inspection.State != StateRecoveryRequired {
			t.Fatalf("restore recovery inspection=%#v err=%v", inspection, err)
		}
		routingFake.acknowledgeResponseError = ErrUnavailable
		if _, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("lost restore ACK error=%v", err)
		}
		inspection, err = manager.Inspect(context.Background())
		if err != nil || inspection.State != StateRecoveryRequired || routingFake.snapshot.RuntimeRoutingPending {
			t.Fatalf("lost restore ACK inspection=%#v route=%#v err=%v", inspection, routingFake.snapshot, err)
		}
		recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
		if err != nil {
			t.Fatal(err)
		}
		if recovered.State != StateStopped || recovered.Active != nil || recovered.RoutingGeneration != staged.RoutingGeneration+5 ||
			routingFake.lastRequest == nil || routingFake.lastRequest.Direction != RoutingRestoreOrigin ||
			routingFake.snapshot.RuntimeRoutingPending || routingFake.reconcileCount != 1 || routingFake.acknowledgeCount != 1 {
			t.Fatalf("restore-origin recovery=%#v route=%#v request=%#v reconciles=%d acks=%d", recovered, routingFake.snapshot, routingFake.lastRequest, routingFake.reconcileCount, routingFake.acknowledgeCount)
		}
	})
}

func TestManagerRecoversCrashAfterRoutingAcknowledgementWithoutSecondMutation(t *testing.T) {
	t.Run("first activation", func(t *testing.T) {
		fixture := newManagerFixture(t)
		candidate := fixture.candidate(t, "2.40.0", 1, 1)
		events := []string{}
		runtimeFake := &managerRuntimeFake{events: &events}
		routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}, acknowledgeResponseError: ErrUnavailable}
		manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
		routingFake.acknowledgeHook = func() {
			committed, found, loadErr := manager.store.load()
			journal, journalFound, journalErr := manager.store.loadJournal()
			if loadErr != nil || !found || journalErr != nil || !journalFound ||
				committed.Status != StateHealthy || committed.Active == nil ||
				committed.ActiveOperationID != journal.OperationID ||
				committed.RoutingGeneration != routingFake.snapshot.Generation {
				t.Fatalf("routing ACK preceded durable lifecycle commit: state=%#v found=%t journal=%#v journalFound=%t errors=%v/%v", committed, found, journal, journalFound, loadErr, journalErr)
			}
		}
		checked, _ := manager.Check(context.Background())
		staged, err := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("lost activation ACK error=%v", err)
		}
		inspection, err := manager.Inspect(context.Background())
		if err != nil || inspection.State != StateRecoveryRequired || routingFake.snapshot.RuntimeRoutingPending {
			t.Fatalf("lost activation ACK inspection=%#v route=%#v err=%v", inspection, routingFake.snapshot, err)
		}
		recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
		if err != nil {
			t.Fatal(err)
		}
		if recovered.State != StateHealthy || routingFake.reconcileCount != 0 || routingFake.acknowledgeCount != 1 {
			t.Fatalf("activation ACK recovery=%#v reconciles=%d acks=%d", recovered, routingFake.reconcileCount, routingFake.acknowledgeCount)
		}
	})

	t.Run("stopped restart", func(t *testing.T) {
		manager, _, routingFake, active := activateManagerFixture(t)
		stopped, err := manager.Stop(context.Background(), StopRequest{ExpectedStateDigest: active.StateDigest, ExpectedRoutingGeneration: active.RoutingGeneration, ConfirmDesktopExited: true})
		if err != nil {
			t.Fatal(err)
		}
		acknowledgements := routingFake.acknowledgeCount
		routingFake.acknowledgeResponseError = ErrUnavailable
		if _, err := manager.Activate(context.Background(), ActivateRequest{stopped.StateDigest, stopped.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("lost restart ACK error=%v", err)
		}
		inspection, err := manager.Inspect(context.Background())
		if err != nil || inspection.State != StateRecoveryRequired || routingFake.snapshot.RuntimeRoutingPending {
			t.Fatalf("lost restart ACK inspection=%#v route=%#v err=%v", inspection, routingFake.snapshot, err)
		}
		recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
		if err != nil {
			t.Fatal(err)
		}
		if recovered.State != StateHealthy || routingFake.reconcileCount != 0 || routingFake.acknowledgeCount != acknowledgements+1 {
			t.Fatalf("restart ACK recovery=%#v reconciles=%d acks=%d", recovered, routingFake.reconcileCount, routingFake.acknowledgeCount)
		}
	})

	t.Run("stop", func(t *testing.T) {
		manager, _, routingFake, active := activateManagerFixture(t)
		acknowledgements := routingFake.acknowledgeCount
		routingFake.acknowledgeResponseError = ErrUnavailable
		if _, err := manager.Stop(context.Background(), StopRequest{ExpectedStateDigest: active.StateDigest, ExpectedRoutingGeneration: active.RoutingGeneration, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("lost stop ACK error=%v", err)
		}
		inspection, err := manager.Inspect(context.Background())
		if err != nil || inspection.State != StateRecoveryRequired || routingFake.snapshot.RuntimeRoutingPending {
			t.Fatalf("lost stop ACK inspection=%#v route=%#v err=%v", inspection, routingFake.snapshot, err)
		}
		recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
		if err != nil {
			t.Fatal(err)
		}
		if recovered.State != StateStopped || routingFake.reconcileCount != 0 || routingFake.acknowledgeCount != acknowledgements+1 {
			t.Fatalf("stop ACK recovery=%#v reconciles=%d acks=%d", recovered, routingFake.reconcileCount, routingFake.acknowledgeCount)
		}
	})
}

func TestManagerRecoverForwardCompletesVerifiedFirstActivation(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	checker := &managerCheckerFake{candidate: candidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	stagedReceipt, err := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	state, _, err := manager.store.load()
	if err != nil {
		t.Fatal(err)
	}
	record := *state.Staged
	expectedDigest, _ := manager.store.digest(state)
	operationID := strings.Repeat("7", 64)
	statePath, err := manager.store.createEmptyGeneration(1)
	if err != nil {
		t.Fatal(err)
	}
	spec := startSpec(state.InstallationID, operationID, record, 1, statePath, "")
	runtimeFake.current, runtimeFake.exists = spec, true
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: operationID,
		Phase: phaseVerified, ExpectedStateDigest: expectedDigest, ExpectedRoutingGeneration: 1,
		NewArtifact: record, NewGeneration: 1, NewContainerID: ContainerName,
	}
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	state.Status = StateRecoveryRequired
	if err := manager.store.save(state); err != nil {
		t.Fatal(err)
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil || inspection.StateDigest == stagedReceipt.StateDigest {
		t.Fatalf("crash state was not visible: %#v err=%v", inspection, err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateHealthy || recovered.Active == nil || recovered.Active.ManifestSHA256 != candidate.ManifestSHA256 {
		t.Fatalf("forward recovery = %#v", recovered)
	}
	for _, event := range events {
		if event == "runtime-start" {
			t.Fatalf("verified running container was unnecessarily recreated: %#v", events)
		}
	}
	assertEventOrder(t, events, "runtime-verify", "http-probe", "route-activate")
}

func TestManagerRecoverForwardRejectsUnrelatedOrdinaryRoutingGeneration(t *testing.T) {
	for _, test := range []struct {
		name        string
		generation  uint64
		appleActive bool
	}{
		{name: "stale inactive newer generation", generation: 2},
		{name: "Apple active wrong generation", generation: 5, appleActive: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			candidate := fixture.candidate(t, "2.40.0", 1, 1)
			events := []string{}
			runtimeFake := &managerRuntimeFake{events: &events}
			routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
			manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
			checked, _ := manager.Check(context.Background())
			staged, err := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
			if err != nil {
				t.Fatal(err)
			}
			state, found, err := manager.store.load()
			if err != nil || !found || state.Staged == nil {
				t.Fatalf("staged state = %#v found=%t err=%v", state, found, err)
			}
			expectedDigest, err := manager.store.digest(state)
			if err != nil {
				t.Fatal(err)
			}
			operationID := strings.Repeat("7", 64)
			statePath, err := manager.store.createEmptyGeneration(state.NextGeneration)
			if err != nil {
				t.Fatal(err)
			}
			spec := startSpec(state.InstallationID, operationID, *state.Staged, state.NextGeneration, statePath, "")
			runtimeFake.current, runtimeFake.exists = spec, true
			journal := transactionJournal{
				Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: operationID,
				Phase: phaseVerified, ExpectedStateDigest: expectedDigest, ExpectedRoutingGeneration: staged.RoutingGeneration,
				NewArtifact: *state.Staged, NewGeneration: state.NextGeneration, NewContainerID: ContainerName,
			}
			if err := manager.store.saveJournal(journal); err != nil {
				t.Fatal(err)
			}
			state.Status = StateRecoveryRequired
			if err := manager.store.save(state); err != nil {
				t.Fatal(err)
			}
			routingFake.snapshot = RoutingSnapshot{Generation: test.generation, AppleActive: test.appleActive}
			events = events[:0]
			inspection, err := manager.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("unrelated route recovery error = %v", err)
			}
			current, journalFound, err := manager.store.loadJournal()
			if err != nil || !journalFound || current.Phase != phaseRecoveryRequired || !runtimeFake.exists {
				t.Fatalf("unrelated route was not parked: journal=%#v found=%t err=%v runtime=%#v", current, journalFound, err, runtimeFake)
			}
			if _, err := os.Lstat(statePath); err != nil {
				t.Fatalf("live generation was removed: %v", err)
			}
			for _, event := range events {
				if event == "route-activate" || event == "runtime-stop" || event == "runtime-delete" {
					t.Fatalf("unrelated routing state authorized mutation: %#v", events)
				}
			}
		})
	}
}

func TestManagerAPFSCloneFailureRollsBackPreparedRouteBeforeRuntimeMutation(t *testing.T) {
	fixture := newManagerFixture(t)
	oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
	newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
	checker := &managerCheckerFake{candidate: oldCandidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	active, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true})
	if err != nil {
		t.Fatal(err)
	}
	checker.candidate = newCandidate
	checked, _ = manager.Check(context.Background())
	staged, err = manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	events = events[:0]
	manager.cloner = &managerClonerFake{events: &events, err: ErrUnavailable}
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("clone failure error = %v", err)
	}
	assertEventOrder(t, events, "route-prepare", "clone", "route-rollback")
	for _, event := range events {
		if event == "runtime-stop" || event == "runtime-delete" || event == "runtime-start" {
			t.Fatalf("clone failure mutated runtime: %#v", events)
		}
	}
	state, _, _ := manager.store.load()
	if state.Status != StateHealthy || state.Active == nil || active.Active == nil || state.Active.ManifestSHA256 != active.Active.ManifestSHA256 || state.Staged == nil {
		t.Fatalf("clone rollback state = %#v", state)
	}
}

func TestManagerRecoverAcceptsLostMaintenanceCommitResponseWithoutSecondFinish(t *testing.T) {
	fixture := newManagerFixture(t)
	oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
	newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
	checker := &managerCheckerFake{candidate: oldCandidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	active, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true})
	if err != nil {
		t.Fatal(err)
	}
	checker.candidate = newCandidate
	checked, _ = manager.Check(context.Background())
	staged, _ = manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	routingFake.commitError = ErrUnavailable
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("lost commit response error = %v", err)
	}
	if routingFake.commitCount != 1 || routingFake.snapshot.MaintenancePending ||
		routingFake.snapshot.Generation != active.RoutingGeneration+2 {
		t.Fatalf("routing did not durably finish before response loss: count=%d snapshot=%#v", routingFake.commitCount, routingFake.snapshot)
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if routingFake.commitCount != 1 || recovered.State != StateHealthy || recovered.Active == nil ||
		recovered.Active.ManifestSHA256 != newCandidate.ManifestSHA256 {
		t.Fatalf("lost commit response was not reconciled exactly: count=%d receipt=%#v", routingFake.commitCount, recovered)
	}
}

func TestManagerRecoverAcceptsLostMaintenanceRollbackResponseWithoutSecondFinish(t *testing.T) {
	fixture := newManagerFixture(t)
	oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
	newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
	checker := &managerCheckerFake{candidate: oldCandidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); err != nil {
		t.Fatal(err)
	}
	checker.candidate = newCandidate
	checked, _ = manager.Check(context.Background())
	staged, _ = manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	runtimeFake.failManifest = newCandidate.ManifestSHA256
	routingFake.rollbackError = ErrUnavailable
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("lost rollback response error = %v", err)
	}
	if routingFake.rollbackCount != 1 || routingFake.snapshot.MaintenancePending {
		t.Fatalf("routing did not durably roll back before response loss: count=%d snapshot=%#v", routingFake.rollbackCount, routingFake.snapshot)
	}
	runtimeFake.failManifest = ""
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if routingFake.rollbackCount != 1 || recovered.State != StateHealthy || recovered.Active == nil ||
		recovered.Active.ManifestSHA256 != oldCandidate.ManifestSHA256 {
		t.Fatalf("lost rollback response was not reconciled exactly: count=%d receipt=%#v", routingFake.rollbackCount, recovered)
	}
}

func TestManagerAbortPreservesRecoveryJournalWhenUnsafeGenerationCannotBeRemoved(t *testing.T) {
	for _, test := range []struct {
		name    string
		corrupt func(*testing.T, string, string)
	}{
		{name: "symlink", corrupt: func(t *testing.T, root, destination string) {
			t.Helper()
			if err := os.Symlink(filepath.Join(root, "outside"), filepath.Join(destination, "unsafe-link")); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "hardlink", corrupt: func(t *testing.T, root, destination string) {
			t.Helper()
			outside := filepath.Join(root, "outside-hardlink")
			if err := os.WriteFile(outside, []byte("fixture"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Link(outside, filepath.Join(destination, "unsafe-hardlink")); err != nil {
				t.Skipf("hard links unavailable: %v", err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
			newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
			checker := &managerCheckerFake{candidate: oldCandidate}
			events := []string{}
			runtimeFake := &managerRuntimeFake{events: &events}
			routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
			manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
			checked, _ := manager.Check(context.Background())
			staged, _ := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
			if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); err != nil {
				t.Fatal(err)
			}
			checker.candidate = newCandidate
			checked, _ = manager.Check(context.Background())
			staged, _ = manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
			manager.cloner = managerClonerFunc(func(_ context.Context, _, destination string) error {
				if err := os.Mkdir(destination, 0o700); err != nil {
					return err
				}
				test.corrupt(t, fixture.root, destination)
				return ErrUnavailable
			})
			events = events[:0]
			if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("unsafe generation cleanup error = %v", err)
			}
			state, found, stateErr := manager.store.load()
			journal, journalFound, journalErr := manager.store.loadJournal()
			if stateErr != nil || !found || journalErr != nil || !journalFound || state.Status != StateRecoveryRequired ||
				journal.Phase != phaseRecoveryRequired || !journal.CleanupNewGeneration {
				t.Fatalf("recovery witness was not retained: state=%#v found=%t err=%v journal=%#v found=%t err=%v", state, found, stateErr, journal, journalFound, journalErr)
			}
			if _, err := os.Lstat(filepath.Join(fixture.root, "homes", "generation-0002")); err != nil {
				t.Fatalf("unsafe generation was unexpectedly removed: %v", err)
			}
			for _, event := range events {
				if event == "route-rollback" || event == "runtime-stop" || event == "runtime-delete" || event == "runtime-start" {
					t.Fatalf("cleanup failure crossed a later transaction boundary: %#v", events)
				}
			}
		})
	}
}

func TestManagerGenerationRemovalIOFailureRequiresRecoveryBeforeJournalRelease(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events, failManifest: candidate.ManifestSHA256}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	manager.removeGeneration = func(uint64) error { return io.ErrUnexpectedEOF }
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("injected remove error = %v", err)
	}
	state, _, _ := manager.store.load()
	journal, found, err := manager.store.loadJournal()
	if err != nil || !found || state.Status != StateRecoveryRequired || journal.Phase != phaseRecoveryRequired || !journal.CleanupNewGeneration {
		t.Fatalf("remove failure did not preserve recovery witness: state=%#v journal=%#v found=%t err=%v", state, journal, found, err)
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events = events[:0]
	if _, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("recovery unexpectedly bypassed remove failure: %v", err)
	}
	for _, event := range events {
		if event == "runtime-start" || event == "route-activate" {
			t.Fatalf("cleanup-required journal was completed forward: %#v", events)
		}
	}
	if _, found, err := manager.store.loadJournal(); err != nil || !found {
		t.Fatalf("failed recovery released journal: found=%t err=%v", found, err)
	}
	manager.removeGeneration = manager.store.removeGeneration
	inspection, err = manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateStopped || recovered.Active != nil || recovered.RecoveryRequired {
		t.Fatalf("recovery after cleanup became available = %#v", recovered)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("successful recovery retained journal: found=%t err=%v", found, err)
	}
}

func TestManagerPostStartCleanupIsBoundedAndRetainsExactRecoveryWitness(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{
		events: &events, failManifest: candidate.ManifestSHA256, cleanupWait: true,
	}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
	manager.postStartCleanupTimeout = 25 * time.Millisecond
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	started := time.Now()
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("post-start cleanup error = %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("post-start cleanup was not bounded: %s", elapsed)
	}
	state, _, _ := manager.store.load()
	journal, found, err := manager.store.loadJournal()
	if err != nil || !found || state.Status != StateRecoveryRequired || journal.Phase != phaseRecoveryRequired ||
		journal.NewContainerID != ContainerName || !runtimeFake.cleanupDeadlineObserved || !runtimeFake.exists {
		t.Fatalf("bounded cleanup did not preserve exact witness: state=%#v journal=%#v found=%t err=%v runtime=%#v", state, journal, found, err, runtimeFake)
	}
	runtimeFake.cleanupWait = false
	runtimeFake.failManifest = ""
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateHealthy || recovered.Active == nil || recovered.Active.ManifestSHA256 != candidate.ManifestSHA256 {
		t.Fatalf("exact surviving container could not complete forward recovery: %#v", recovered)
	}
}

func TestManagerRecoveryAdoptsExactFixedNameContainerAfterPreJournalCrash(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events, failManifest: candidate.ManifestSHA256, cleanupWait: true}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
	manager.postStartCleanupTimeout = 25 * time.Millisecond
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("prepare pre-journal crash: %v", err)
	}
	runtimeFake.cleanupWait = false
	runtimeFake.failManifest = ""
	journal, found, err := manager.store.loadJournal()
	if err != nil || !found {
		t.Fatalf("load recovery journal: found=%t err=%v", found, err)
	}
	journal.NewContainerID = ""
	journal.Phase = phasePrepared
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	events = events[:0]
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateHealthy || recovered.Active == nil || recovered.Active.ManifestSHA256 != candidate.ManifestSHA256 || !runtimeFake.exists {
		t.Fatalf("exact crash survivor was not committed: receipt=%#v runtime=%#v", recovered, runtimeFake)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("successful exact adoption retained journal: found=%t err=%v", found, err)
	}
	for _, event := range events {
		if event == "runtime-start" || event == "runtime-stop" || event == "runtime-delete" {
			t.Fatalf("exact survivor was mutated instead of adopted: %#v", events)
		}
	}
}

func TestManagerRecoveryRetainsExactPreJournalContainerWhenPinnedManifestIsUnavailable(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events, failManifest: candidate.ManifestSHA256, cleanupWait: true}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, &managerRoutingFake{
		events: &events, snapshot: RoutingSnapshot{Generation: 1},
	}, &events)
	manager.postStartCleanupTimeout = 25 * time.Millisecond
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("prepare pre-journal crash: %v", err)
	}
	runtimeFake.cleanupWait = false
	runtimeFake.failManifest = ""
	journal, found, err := manager.store.loadJournal()
	if err != nil || !found {
		t.Fatalf("load recovery journal: found=%t err=%v", found, err)
	}
	journal.NewContainerID = ""
	journal.Phase = phasePrepared
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	generationPath, err := manager.store.generationPath(journal.NewGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(fixture.root, "manifests", candidate.ManifestSHA256+".json")); err != nil {
		t.Fatal(err)
	}
	events = events[:0]
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("unavailable manifest recovery error = %v", err)
	}
	current, found, err := manager.store.loadJournal()
	if err != nil || !found || current.NewContainerID != ContainerName || current.Phase != phaseRecoveryRequired {
		t.Fatalf("exact runtime witness was not retained: journal=%#v found=%t err=%v", current, found, err)
	}
	if !runtimeFake.exists {
		t.Fatal("exact running container was removed")
	}
	if _, err := os.Lstat(generationPath); err != nil {
		t.Fatalf("mounted generation was removed: %v", err)
	}
	for _, event := range events {
		if event == "runtime-stop" || event == "runtime-delete" || event == "runtime-start" || event == "route-activate" {
			t.Fatalf("unavailable manifest crossed a mutation boundary: %#v", events)
		}
	}
}

func TestManagerRecoveryParksUnknownFixedNameContainerWithoutDeletingGeneration(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*managerRuntimeFake)
	}{
		{name: "mismatched", mutate: func(runtimeFake *managerRuntimeFake) {
			runtimeFake.current.OperationID = strings.Repeat("f", 64)
		}},
		{name: "foreign", mutate: func(runtimeFake *managerRuntimeFake) {
			runtimeFake.foreignExisting = true
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			candidate := fixture.candidate(t, "2.40.0", 1, 1)
			events := []string{}
			runtimeFake := &managerRuntimeFake{events: &events, failManifest: candidate.ManifestSHA256, cleanupWait: true}
			manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, &managerRoutingFake{
				events: &events, snapshot: RoutingSnapshot{Generation: 1},
			}, &events)
			manager.postStartCleanupTimeout = 25 * time.Millisecond
			checked, _ := manager.Check(context.Background())
			staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
			if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("prepare pre-journal crash: %v", err)
			}
			runtimeFake.cleanupWait = false
			runtimeFake.failManifest = ""
			journal, found, err := manager.store.loadJournal()
			if err != nil || !found {
				t.Fatalf("load recovery journal: found=%t err=%v", found, err)
			}
			journal.NewContainerID = ""
			journal.Phase = phasePrepared
			if err := manager.store.saveJournal(journal); err != nil {
				t.Fatal(err)
			}
			generationPath, err := manager.store.generationPath(journal.NewGeneration)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(runtimeFake)
			before := runtimeFake.current
			events = events[:0]
			inspection, err := manager.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("unknown fixed-name recovery error = %v", err)
			}
			current, found, err := manager.store.loadJournal()
			if err != nil || !found || current.Phase != phaseRecoveryRequired || current.NewContainerID != "" {
				t.Fatalf("unknown fixed-name witness was not parked: journal=%#v found=%t err=%v", current, found, err)
			}
			if !runtimeFake.exists || runtimeFake.current != before {
				t.Fatalf("unknown fixed-name resource was mutated: before=%#v after=%#v", before, runtimeFake.current)
			}
			if _, err := os.Lstat(generationPath); err != nil {
				t.Fatalf("possibly mounted generation was removed: %v", err)
			}
			for _, event := range events {
				if event == "runtime-start" || event == "runtime-stop" || event == "runtime-delete" || event == "route-activate" || event == "route-rollback" {
					t.Fatalf("unknown fixed-name resource crossed a mutation boundary: %#v", events)
				}
			}
		})
	}
}

func TestManagerCleansExactRuntimeCreatedByIndeterminateStartFailure(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events, startError: ErrUnavailable}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("indeterminate start error = %v", err)
	}
	state, found, err := manager.store.load()
	if err != nil || !found || state.Status != StateStopped || state.Active != nil || runtimeFake.exists {
		t.Fatalf("partial start was not rolled back: state=%#v found=%t err=%v runtime=%#v", state, found, err, runtimeFake)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("clean partial start retained journal: found=%t err=%v", found, err)
	}
	assertEventOrder(t, events, "runtime-start", "runtime-verify", "runtime-stop", "runtime-delete")
}

func TestManagerParksExactRuntimeWhenIndeterminateStartCleanupFails(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{
		events: &events, startError: ErrUnavailable, cleanupWait: true,
	}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
	manager.postStartCleanupTimeout = 25 * time.Millisecond
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("indeterminate start cleanup error = %v", err)
	}
	state, _, _ := manager.store.load()
	journal, found, err := manager.store.loadJournal()
	if err != nil || !found || state.Status != StateRecoveryRequired || journal.Phase != phaseRecoveryRequired ||
		journal.NewContainerID != ContainerName || !runtimeFake.exists || !runtimeFake.cleanupDeadlineObserved {
		t.Fatalf("partial start witness was lost: state=%#v journal=%#v found=%t err=%v runtime=%#v", state, journal, found, err, runtimeFake)
	}
	runtimeFake.cleanupWait = false
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateHealthy || recovered.Active == nil || recovered.Active.ManifestSHA256 != candidate.ManifestSHA256 {
		t.Fatalf("partial start could not complete forward recovery: %#v", recovered)
	}
}

func TestManagerRecoveryPersistsClearedInvalidContainerWitnessBeforeRestart(t *testing.T) {
	for _, test := range []struct {
		name           string
		breakFirstSave bool
	}{
		{name: "clear is durable before restart"},
		{name: "clear save failure prohibits restart and retries", breakFirstSave: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newManagerFixture(t)
			candidate := fixture.candidate(t, "2.40.0", 1, 1)
			events := []string{}
			runtimeFake := &managerRuntimeFake{
				events: &events, failManifest: candidate.ManifestSHA256, cleanupWait: true,
			}
			manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, &managerRoutingFake{
				events: &events, snapshot: RoutingSnapshot{Generation: 1},
			}, &events)
			manager.postStartCleanupTimeout = 25 * time.Millisecond
			checked, _ := manager.Check(context.Background())
			staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
			if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("prepare invalid exact container: %v", err)
			}
			runtimeFake.cleanupWait = false
			startedBeforeClear := false
			runtimeFake.startHook = func() {
				journal, found, err := manager.store.loadJournal()
				if err != nil || !found || journal.NewContainerID != "" || journal.Phase != phasePrepared {
					startedBeforeClear = true
				}
			}

			transactions := filepath.Join(fixture.root, "transactions")
			backup := filepath.Join(fixture.root, "transactions-backup")
			decoy := t.TempDir()
			broken := false
			runtimeFake.stopHook = func() {
				runtimeFake.failManifest = ""
				if !test.breakFirstSave {
					return
				}
				if err := os.Rename(transactions, backup); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(decoy, transactions); err != nil {
					t.Fatal(err)
				}
				broken = true
			}
			restoreTransactions := func() {
				if !broken {
					return
				}
				if err := os.Remove(transactions); err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(backup, transactions); err != nil {
					t.Fatal(err)
				}
				broken = false
			}
			t.Cleanup(restoreTransactions)

			inspection, err := manager.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			recovered, recoverErr := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
			if test.breakFirstSave {
				if !errors.Is(recoverErr, ErrRecoveryRequired) || runtimeFake.exists || startedBeforeClear {
					t.Fatalf("failed clear save crossed restart boundary: receipt=%#v err=%v runtime=%t startedEarly=%t events=%#v", recovered, recoverErr, runtimeFake.exists, startedBeforeClear, events)
				}
				restoreTransactions()
				inspection, err = manager.Inspect(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				recovered, recoverErr = manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
			}
			if recoverErr != nil {
				t.Fatal(recoverErr)
			}
			if startedBeforeClear || recovered.State != StateHealthy || !runtimeFake.exists {
				t.Fatalf("clear-before-restart recovery=%#v runtime=%t startedEarly=%t", recovered, runtimeFake.exists, startedBeforeClear)
			}
		})
	}
}

func TestManagerRecoveryPreservesInMemoryWitnessWhenFirstJournalWriteFails(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{
		events: &events, failManifest: candidate.ManifestSHA256, cleanupWait: true,
	}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, &managerRoutingFake{
		events: &events, snapshot: RoutingSnapshot{Generation: 1},
	}, &events)
	manager.postStartCleanupTimeout = 25 * time.Millisecond
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("prepare recovery entry: %v", err)
	}

	// Model a crash-era journal that has no running-container witness. Recover
	// must start the pinned candidate and retain the in-memory witness even if
	// its first attempted journal write fails.
	runtimeFake.exists = false
	runtimeFake.current = StartSpec{}
	runtimeFake.cleanupWait = true
	runtimeFake.cleanupDeadlineObserved = false
	journal, found, err := manager.store.loadJournal()
	if err != nil || !found {
		t.Fatalf("load recovery journal: found=%t err=%v", found, err)
	}
	journal.NewContainerID = ""
	journal.Phase = phasePrepared
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}

	transactions := filepath.Join(fixture.root, "transactions")
	backup := filepath.Join(fixture.root, "transactions-backup")
	decoy := t.TempDir()
	restored := false
	t.Cleanup(func() {
		if restored {
			return
		}
		_ = os.Remove(transactions)
		_ = os.Rename(backup, transactions)
	})
	runtimeFake.startHook = func() {
		if err := os.Rename(transactions, backup); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(decoy, transactions); err != nil {
			t.Fatal(err)
		}
	}
	runtimeFake.stopHook = func() {
		if err := os.Remove(transactions); err != nil {
			t.Fatal(err)
		}
		// Leave the canonical journal directory absent. The recovery path must
		// distinguish this from a completed commit and recreate the exact
		// in-memory witness; the renamed entry journal is intentionally stale.
		restored = true
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true}); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("bounded recovery cleanup error = %v", err)
	}
	current, found, err := manager.store.loadJournal()
	if err != nil || !found || current.Phase != phaseRecoveryRequired || current.NewContainerID != ContainerName ||
		current.OperationID != runtimeFake.current.OperationID || !runtimeFake.exists || !runtimeFake.cleanupDeadlineObserved {
		t.Fatalf("recovery lost exact in-memory witness: journal=%#v found=%t err=%v runtime=%#v", current, found, err, runtimeFake)
	}
}

func TestRestoreOrRecreateRecognizesJournaledReplacementAfterCleanupFailure(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	record, err := recordFromCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	runtimeFake := &managerRuntimeFake{
		events: &events, failManifest: candidate.ManifestSHA256, cleanupWait: true,
	}
	manager := fixture.manager(t, &managerCheckerFake{}, runtimeFake, &managerRoutingFake{
		events: &events, snapshot: RoutingSnapshot{Generation: 1},
	}, &events)
	manager.postStartCleanupTimeout = 25 * time.Millisecond
	if err := manager.store.prepareRoot(); err != nil {
		t.Fatal(err)
	}
	statePath, err := manager.store.createEmptyGeneration(1)
	if err != nil {
		t.Fatal(err)
	}
	existing := startSpec(strings.Repeat("1", 64), strings.Repeat("2", 64), record, 1, statePath, "")
	replacement := startSpec(existing.InstallationID, strings.Repeat("3", 64), record, 1, statePath, "")
	journaled := ""
	callback := func(containerID string) error {
		journaled = containerID
		return nil
	}
	if _, _, err := manager.restoreOrRecreate(context.Background(), existing, replacement, candidate.Manifest, testSecrets(), callback); !errors.Is(err, errPostStartCleanupIncomplete) {
		t.Fatalf("replacement cleanup error = %v", err)
	}
	if journaled != ContainerName || !runtimeFake.exists || !runtimeFake.cleanupDeadlineObserved {
		t.Fatalf("replacement witness was not retained: journaled=%q runtime=%#v", journaled, runtimeFake)
	}
	runtimeFake.cleanupWait = false
	runtimeFake.failManifest = ""
	journaled = ""
	containerID, operationID, err := manager.restoreOrRecreate(context.Background(), existing, replacement, candidate.Manifest, testSecrets(), callback)
	if err != nil {
		t.Fatal(err)
	}
	if containerID != ContainerName || operationID != replacement.OperationID || journaled != ContainerName {
		t.Fatalf("replacement recovery = container %q operation %q journaled %q", containerID, operationID, journaled)
	}
}

func TestManagerRecoverBindsUnhealthyCrashEraReplacementBeforeRecreateAndRollback(t *testing.T) {
	fixture := newManagerFixture(t)
	oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
	newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
	checker := &managerCheckerFake{candidate: oldCandidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, _ := manager.Check(context.Background())
	staged, _ := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); err != nil {
		t.Fatal(err)
	}
	checker.candidate = newCandidate
	checked, _ = manager.Check(context.Background())
	if _, err := manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration}); err != nil {
		t.Fatal(err)
	}
	state, found, err := manager.store.load()
	if err != nil || !found || state.Active == nil || state.Staged == nil {
		t.Fatalf("staged update state = %#v found=%t err=%v", state, found, err)
	}
	expectedDigest, err := manager.store.digest(state)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.store.createEmptyGeneration(state.NextGeneration); err != nil {
		t.Fatal(err)
	}
	operationID := strings.Repeat("a", 64)
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: operationID,
		Phase: phaseRecoveryRequired, ExpectedStateDigest: expectedDigest,
		ExpectedRoutingGeneration: routingFake.snapshot.Generation,
		OldArtifact:               cloneRecord(state.Active), NewArtifact: *state.Staged,
		OldGeneration: state.ActiveGeneration, NewGeneration: state.NextGeneration,
		OldContainerID: state.ContainerID, OldOperationID: state.ActiveOperationID,
		CleanupNewGeneration: true,
	}
	witness, err := routingFake.Prepare(context.Background(), maintenanceRequest(journal))
	if err != nil {
		t.Fatal(err)
	}
	journal.Maintenance = &witness
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	state.Status = StateRecoveryRequired
	if err := manager.store.save(state); err != nil {
		t.Fatal(err)
	}
	oldPath, err := manager.store.generationPath(state.ActiveGeneration)
	if err != nil {
		t.Fatal(err)
	}
	// Model a crash after Start created the replacement-operation container but
	// before onRecreated persisted its identity into the runtime journal.
	runtimeFake.current = startSpec(state.InstallationID, operationID, *state.Active, state.ActiveGeneration, oldPath, "")
	runtimeFake.exists = true
	manager.prober = &managerProberFake{events: &events, verifyErrors: []error{ErrUnavailable, nil}}
	events = events[:0]
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateHealthy || recovered.Active == nil ||
		recovered.Active.ManifestSHA256 != oldCandidate.ManifestSHA256 ||
		runtimeFake.current.OperationID != operationID || routingFake.rollbackCount != 1 {
		t.Fatalf("crash-era replacement rollback = receipt=%#v runtime=%#v rollbacks=%d", recovered, runtimeFake.current, routingFake.rollbackCount)
	}
	assertEventOrder(t, events, "runtime-verify", "http-probe", "runtime-stop", "runtime-delete", "runtime-start", "runtime-verify", "http-probe", "route-rollback")
}

func TestRestoreOrRecreateLeavesUnknownFixedNameContainerUntouched(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "mismatched", true: "foreign"}[foreign], func(t *testing.T) {
			fixture := newManagerFixture(t)
			candidate := fixture.candidate(t, "2.40.0", 1, 1)
			record, err := recordFromCandidate(candidate)
			if err != nil {
				t.Fatal(err)
			}
			events := []string{}
			runtimeFake := &managerRuntimeFake{events: &events, exists: true, foreignExisting: foreign}
			manager := fixture.manager(t, &managerCheckerFake{}, runtimeFake, &managerRoutingFake{
				events: &events, snapshot: RoutingSnapshot{Generation: 1},
			}, &events)
			if err := manager.store.prepareRoot(); err != nil {
				t.Fatal(err)
			}
			statePath, err := manager.store.createEmptyGeneration(1)
			if err != nil {
				t.Fatal(err)
			}
			existing := startSpec(strings.Repeat("1", 64), strings.Repeat("2", 64), record, 1, statePath, "")
			replacement := startSpec(existing.InstallationID, strings.Repeat("3", 64), record, 1, statePath, "")
			runtimeFake.current = replacement
			if !foreign {
				runtimeFake.current.OperationID = strings.Repeat("4", 64)
			}
			before := runtimeFake.current
			if _, _, err := manager.restoreOrRecreate(context.Background(), existing, replacement, candidate.Manifest, testSecrets(), nil); err == nil {
				t.Fatal("unknown fixed-name container was accepted")
			}
			if !runtimeFake.exists || runtimeFake.current != before {
				t.Fatalf("unknown container mutated: before=%#v after=%#v", before, runtimeFake.current)
			}
			for _, event := range events {
				if event == "runtime-stop" || event == "runtime-delete" || event == "runtime-start" {
					t.Fatalf("unknown container crossed mutation boundary: %#v", events)
				}
			}
		})
	}
}

func TestRestoreOrRecreatePreservesExactReplacementWhenJournalCallbackFails(t *testing.T) {
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	record, err := recordFromCandidate(candidate)
	if err != nil {
		t.Fatal(err)
	}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	manager := fixture.manager(t, &managerCheckerFake{}, runtimeFake, &managerRoutingFake{
		events: &events, snapshot: RoutingSnapshot{Generation: 1},
	}, &events)
	if err := manager.store.prepareRoot(); err != nil {
		t.Fatal(err)
	}
	statePath, err := manager.store.createEmptyGeneration(1)
	if err != nil {
		t.Fatal(err)
	}
	existing := startSpec(strings.Repeat("1", 64), strings.Repeat("2", 64), record, 1, statePath, "")
	replacement := startSpec(existing.InstallationID, strings.Repeat("3", 64), record, 1, statePath, "")
	runtimeFake.current, runtimeFake.exists = replacement, true
	bound := ""
	containerID, operationID, err := manager.restoreOrRecreate(
		context.Background(), existing, replacement, candidate.Manifest, testSecrets(),
		func(value string) error {
			bound = value
			return io.ErrUnexpectedEOF
		},
	)
	if !errors.Is(err, io.ErrUnexpectedEOF) || bound != ContainerName ||
		containerID != ContainerName || operationID != replacement.OperationID {
		t.Fatalf("callback failure lost replacement witness: container=%q operation=%q bound=%q err=%v", containerID, operationID, bound, err)
	}
	if !runtimeFake.exists || runtimeFake.current != replacement {
		t.Fatalf("callback failure mutated exact replacement: %#v", runtimeFake)
	}
	for _, event := range events {
		if event == "http-probe" || event == "runtime-stop" || event == "runtime-delete" || event == "runtime-start" {
			t.Fatalf("callback failure crossed health/mutation boundary: %#v", events)
		}
	}
}

func TestManagerInspectRevalidatesActiveRuntimeWithoutCreatingCredentials(t *testing.T) {
	manager, runtimeFake, _, _ := activateManagerFixture(t)
	events := []string{}
	keychain := &managerInspectKeychainFake{events: &events, secrets: testSecrets()}
	prober := &managerProberFake{events: &events}
	manager.keychain = keychain
	manager.prober = prober
	runtimeFake.events = &events

	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.State != StateHealthy || inspection.RecoveryRequired {
		t.Fatalf("inspection = %#v", inspection)
	}
	if keychain.loadCount != 1 || keychain.ensureCount != 0 {
		t.Fatalf("Keychain calls: load=%d ensure=%d", keychain.loadCount, keychain.ensureCount)
	}
	assertEventOrder(t, events, "runtime-verify", "runtime-state", "keychain-load", "http-probe")
}

func TestManagerInspectRejectsNonRunningContainerBeforeCredentials(t *testing.T) {
	for _, fixedState := range []FixedContainerState{
		FixedContainerStoppedOwned,
		FixedContainerAbsent,
		FixedContainerUnknown,
		FixedContainerForeign,
	} {
		t.Run(string(fixedState), func(t *testing.T) {
			manager, runtimeFake, _, _ := activateManagerFixture(t)
			events := []string{}
			keychain := &managerInspectKeychainFake{events: &events, secrets: testSecrets()}
			manager.keychain = keychain
			manager.prober = &managerProberFake{events: &events}
			runtimeFake.events = &events
			runtimeFake.containerState = fixedState

			inspection, err := manager.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if inspection.State != StateRecoveryRequired || !inspection.RecoveryRequired {
				t.Fatalf("inspection = %#v", inspection)
			}
			if keychain.loadCount != 0 || keychain.ensureCount != 0 {
				t.Fatalf("non-running container accessed Keychain: load=%d ensure=%d", keychain.loadCount, keychain.ensureCount)
			}
			for _, event := range events {
				if event == "keychain-load" || event == "http-probe" {
					t.Fatalf("non-running container crossed credential/probe boundary: %#v", events)
				}
			}
			assertEventOrder(t, events, "runtime-verify", "runtime-state")
		})
	}
}

func TestValidateActiveRoutingAuthorityRequiresCommittedLifecycleState(t *testing.T) {
	manager, _, _, active := activateManagerFixture(t)
	fixtureKey := manager.publicKeyPEM
	if err := ValidateActiveRoutingAuthority(manager.store.root, active.RoutingGeneration, "0.3.9", fixtureKey); err != nil {
		t.Fatalf("committed lifecycle authority rejected: %v", err)
	}
	if err := ValidateActiveRoutingAuthority(manager.store.root, active.RoutingGeneration+1, "0.3.9", fixtureKey); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("mismatched routing generation error = %v", err)
	}
	_, originalSignature, err := manager.store.loadManifest(active.Active.ManifestSHA256)
	if err != nil {
		t.Fatal(err)
	}
	signaturePath := filepath.Join(manager.store.root, "manifests", active.Active.ManifestSHA256+".sig")
	invalidSignature := []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
	if err := writeOwnerFile(signaturePath, invalidSignature); err != nil {
		t.Fatal(err)
	}
	if err := ValidateActiveRoutingAuthority(manager.store.root, active.RoutingGeneration, "0.3.9", fixtureKey); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("invalid stored signature error = %v", err)
	}
	if err := writeOwnerFile(signaturePath, originalSignature); err != nil {
		t.Fatal(err)
	}

	state, found, err := manager.store.load()
	if err != nil || !found || state.Active == nil {
		t.Fatalf("load active state: found=%t err=%v state=%#v", found, err, state)
	}
	originalState := state
	state.Active.IndexDigest = "sha256:" + strings.Repeat("9", 64)
	if err := manager.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := ValidateActiveRoutingAuthority(manager.store.root, active.RoutingGeneration, "0.3.9", fixtureKey); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("manifest/state digest mismatch error = %v", err)
	}
	state = originalState
	if err := manager.store.save(state); err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: strings.Repeat("9", 64),
		Phase: phasePrepared, ExpectedStateDigest: active.StateDigest,
		ExpectedRoutingGeneration: active.RoutingGeneration,
		OldArtifact:               cloneRecord(state.Active), NewArtifact: *state.Active,
		OldGeneration: state.ActiveGeneration, NewGeneration: state.ActiveGeneration + 1,
	}
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	if err := ValidateActiveRoutingAuthority(manager.store.root, active.RoutingGeneration, "0.3.9", fixtureKey); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("unfinished lifecycle journal error = %v", err)
	}
}

func TestValidateActiveRuntimeBeforeCredentialsRequiresExactRunningOwnedPeer(t *testing.T) {
	manager, runtimeFake, _, active := activateManagerFixture(t)
	for _, test := range []struct {
		name  string
		state FixedContainerState
		ok    bool
	}{
		{name: "running owned", state: FixedContainerRunningOwned, ok: true},
		{name: "stopped owned", state: FixedContainerStoppedOwned},
		{name: "absent", state: FixedContainerAbsent},
		{name: "foreign", state: FixedContainerForeign},
		{name: "unknown", state: FixedContainerUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtimeFake.containerState = test.state
			err := ValidateActiveRuntimeBeforeCredentials(
				context.Background(),
				manager.store.root,
				active.RoutingGeneration,
				manager.relayVersion,
				manager.publicKeyPEM,
				runtimeFake,
			)
			if test.ok && err != nil {
				t.Fatalf("running owned peer rejected: %v", err)
			}
			if !test.ok && !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("state %q error = %v", test.state, err)
			}
		})
	}
}

func TestManagerInspectProjectsCredentialAndHealthFailureAsRecoveryRequired(t *testing.T) {
	for _, test := range []struct {
		name         string
		keychainErr  error
		probeErr     error
		expectProber bool
	}{
		{name: "credential load", keychainErr: ErrCredential},
		{name: "authenticated runtime probe", probeErr: ErrUnavailable, expectProber: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, runtimeFake, _, _ := activateManagerFixture(t)
			events := []string{}
			keychain := &managerInspectKeychainFake{events: &events, secrets: testSecrets(), err: test.keychainErr}
			manager.keychain = keychain
			manager.prober = &managerProberFake{events: &events, err: test.probeErr}
			runtimeFake.events = &events

			inspection, err := manager.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if inspection.State != StateRecoveryRequired || !inspection.RecoveryRequired {
				t.Fatalf("inspection = %#v", inspection)
			}
			if keychain.loadCount != 1 || keychain.ensureCount != 0 {
				t.Fatalf("Keychain calls: load=%d ensure=%d", keychain.loadCount, keychain.ensureCount)
			}
			probed := false
			for _, event := range events {
				probed = probed || event == "http-probe"
			}
			if probed != test.expectProber {
				t.Fatalf("probe=%t events=%#v", probed, events)
			}
			state, found, loadErr := manager.store.load()
			if loadErr != nil || !found || state.Status != StateHealthy {
				t.Fatalf("Inspect mutated durable state: %#v found=%t err=%v", state, found, loadErr)
			}
		})
	}
}

func TestManagerInspectDistinguishesCapabilityLossFromActiveRuntimeDrift(t *testing.T) {
	for _, test := range []struct {
		name     string
		reason   string
		expected State
	}{
		{name: "system service stopped", reason: "apple_container_service_not_running", expected: StateUnavailable},
		{name: "foreign fixed container", reason: "apple_container_foreign_container", expected: StateRecoveryRequired},
		{name: "owned container disappeared and port was taken", reason: "apple_container_port_unavailable", expected: StateRecoveryRequired},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, runtimeFake, _, _ := activateManagerFixture(t)
			events := []string{}
			keychain := &managerInspectKeychainFake{events: &events, secrets: testSecrets()}
			manager.keychain = keychain
			manager.prober = &managerProberFake{events: &events}
			runtimeFake.events = &events
			runtimeFake.capability = &Capability{Reason: test.reason, SystemServiceState: "unknown"}

			inspection, err := manager.Inspect(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if inspection.State != test.expected || inspection.RecoveryRequired != (test.expected == StateRecoveryRequired) {
				t.Fatalf("inspection = %#v", inspection)
			}
			if keychain.loadCount != 0 || keychain.ensureCount != 0 {
				t.Fatalf("unavailable capability accessed Keychain: load=%d ensure=%d", keychain.loadCount, keychain.ensureCount)
			}
		})
	}
}

func TestManagerInspectStoppedStateNeverLoadsSecretsAndDetectsOrphan(t *testing.T) {
	fixture := newManagerFixture(t)
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{}, runtimeFake, routingFake, &events)
	if _, err := manager.Check(context.Background()); err != nil {
		t.Fatal(err)
	}
	keychain := &managerInspectKeychainFake{events: &events, secrets: testSecrets()}
	manager.keychain = keychain

	inspection, err := manager.Inspect(context.Background())
	if err != nil || inspection.State != StateStopped {
		t.Fatalf("stopped inspection = %#v err=%v", inspection, err)
	}
	if keychain.loadCount != 0 || keychain.ensureCount != 0 {
		t.Fatal("stopped inspection accessed Keychain")
	}
	runtimeFake.exists = true
	inspection, err = manager.Inspect(context.Background())
	if err != nil || inspection.State != StateRecoveryRequired || !inspection.RecoveryRequired {
		t.Fatalf("orphan inspection = %#v err=%v", inspection, err)
	}
}

func TestManagerStopThenActivateRecreatesSameDigestAndStateGeneration(t *testing.T) {
	manager, runtimeFake, routingFake, active := activateManagerFixture(t)
	before, _, err := manager.store.load()
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Stop(context.Background(), StopRequest{ExpectedStateDigest: active.StateDigest, ExpectedRoutingGeneration: active.RoutingGeneration, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.State != StateStopped || stopped.Active == nil {
		t.Fatalf("stopped receipt = %#v", stopped)
	}
	events := []string{}
	runtimeFake.events = &events
	routingFake.events = &events
	manager.cloner = &managerClonerFake{events: &events}
	manager.prober = &managerProberFake{events: &events}

	restarted, err := manager.Activate(context.Background(), ActivateRequest{stopped.StateDigest, stopped.RoutingGeneration, true})
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := manager.store.load()
	if err != nil || !found {
		t.Fatal(err)
	}
	if restarted.State != StateHealthy || restarted.Active == nil ||
		restarted.Active.ManifestSHA256 != active.Active.ManifestSHA256 || state.ActiveGeneration != before.ActiveGeneration ||
		state.NextGeneration != before.NextGeneration || runtimeFake.current.Generation != before.ActiveGeneration {
		t.Fatalf("restart changed immutable runtime generation: receipt=%#v state=%#v spec=%#v", restarted, state, runtimeFake.current)
	}
	for _, event := range events {
		if event == "clone" {
			t.Fatalf("same-digest restart cloned state: %#v", events)
		}
	}
	assertEventOrder(t, events, "runtime-start", "runtime-verify", "http-probe", "route-activate")
}

func TestManagerStoppedUpgradeFailureKeepsPriorRuntimeStopped(t *testing.T) {
	fixture := newManagerFixture(t)
	oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
	newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
	checker := &managerCheckerFake{candidate: oldCandidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true})
	if err != nil {
		t.Fatal(err)
	}
	checker.candidate = newCandidate
	checked, err = manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged, err = manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := manager.Stop(context.Background(), StopRequest{ExpectedStateDigest: staged.StateDigest, ExpectedRoutingGeneration: staged.RoutingGeneration, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}

	events = events[:0]
	runtimeFake.failManifest = newCandidate.ManifestSHA256
	_, err = manager.Activate(context.Background(), ActivateRequest{stopped.StateDigest, stopped.RoutingGeneration, true})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("stopped upgrade error = %v", err)
	}
	state, found, loadErr := manager.store.load()
	if loadErr != nil || !found {
		t.Fatal(loadErr)
	}
	if state.Status != StateStopped || state.Active == nil || active.Active == nil ||
		state.Active.ManifestSHA256 != active.Active.ManifestSHA256 || state.Staged == nil ||
		state.Staged.ManifestSHA256 != newCandidate.ManifestSHA256 || state.ContainerID != "" || runtimeFake.exists || routingFake.snapshot.AppleActive {
		t.Fatalf("failed stopped upgrade did not preserve stopped runtime: state=%#v runtime=%#v route=%#v", state, runtimeFake.current, routingFake.snapshot)
	}
	starts := 0
	for _, event := range events {
		if event == "runtime-start" {
			starts++
		}
		if event == "route-activate" {
			t.Fatalf("failed runtime was routed: %#v", events)
		}
	}
	if starts != 1 {
		t.Fatalf("failed stopped upgrade unexpectedly recreated the old runtime: %#v", events)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("failed stopped upgrade retained journal: found=%t err=%v", found, err)
	}
}

func TestManagerRecoverReacquiresLostMaintenanceWitnessBeforeRollback(t *testing.T) {
	fixture := newManagerFixture(t)
	oldCandidate := fixture.candidate(t, "2.40.0", 1, 1)
	newCandidate := fixture.candidate(t, "2.41.0", 2, 2)
	checker := &managerCheckerFake{candidate: oldCandidate}
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, checker, runtimeFake, routingFake, &events)
	checked, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := manager.Stage(context.Background(), StageRequest{oldCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true}); err != nil {
		t.Fatal(err)
	}
	checker.candidate = newCandidate
	checked, err = manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Stage(context.Background(), StageRequest{newCandidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration}); err != nil {
		t.Fatal(err)
	}
	state, found, err := manager.store.load()
	if err != nil || !found || state.Active == nil || state.Staged == nil {
		t.Fatalf("staged update state = %#v found=%t err=%v", state, found, err)
	}
	expectedDigest, err := manager.store.digest(state)
	if err != nil {
		t.Fatal(err)
	}
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: strings.Repeat("8", 64),
		Phase: phaseRecoveryRequired, ExpectedStateDigest: expectedDigest,
		ExpectedRoutingGeneration: routingFake.snapshot.Generation,
		OldArtifact:               cloneRecord(state.Active), NewArtifact: *state.Staged,
		OldGeneration: state.ActiveGeneration, NewGeneration: state.NextGeneration,
		OldContainerID: state.ContainerID, OldOperationID: state.ActiveOperationID,
	}
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	state.Status = StateRecoveryRequired
	if err := manager.store.save(state); err != nil {
		t.Fatal(err)
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	events = events[:0]
	runtimeFake.events = &events
	routingFake.events = &events
	manager.prober = &managerProberFake{events: &events}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateHealthy || recovered.Active == nil || recovered.Staged == nil ||
		recovered.Active.ManifestSHA256 != oldCandidate.ManifestSHA256 || recovered.Staged.ManifestSHA256 != newCandidate.ManifestSHA256 {
		t.Fatalf("rollback receipt = %#v", recovered)
	}
	if routingFake.prepareCount != 1 || routingFake.lastRollback == nil ||
		routingFake.lastRollback.Intent.OperationID != journal.OperationID ||
		routingFake.lastRollback.Intent.OldManifestSHA256 != oldCandidate.ManifestSHA256 ||
		routingFake.lastRollback.Intent.NewManifestSHA256 != newCandidate.ManifestSHA256 {
		t.Fatalf("lost routing witness was not reacquired exactly: prepare=%d rollback=%#v", routingFake.prepareCount, routingFake.lastRollback)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("rollback retained runtime journal: found=%t err=%v", found, err)
	}
	assertEventOrder(t, events, "route-prepare", "runtime-verify", "http-probe", "route-rollback")
	for _, event := range events {
		if event == "runtime-start" || event == "runtime-stop" || event == "runtime-delete" {
			t.Fatalf("exact old runtime was unnecessarily mutated during rollback: %#v", events)
		}
	}
}

func TestManagerRecoverCompletesRestartCommittedBeforeJournalRemoval(t *testing.T) {
	manager, runtimeFake, routingFake, active := activateManagerFixture(t)
	stopped, err := manager.Stop(context.Background(), StopRequest{ExpectedStateDigest: active.StateDigest, ExpectedRoutingGeneration: active.RoutingGeneration, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	state, found, err := manager.store.load()
	if err != nil || !found || state.Active == nil {
		t.Fatalf("stopped state = %#v found=%t err=%v", state, found, err)
	}
	expectedDigest, err := manager.store.digest(state)
	if err != nil {
		t.Fatal(err)
	}
	operationID := strings.Repeat("9", 64)
	statePath, err := manager.store.generationPath(state.ActiveGeneration)
	if err != nil {
		t.Fatal(err)
	}
	spec := startSpec(state.InstallationID, operationID, *state.Active, state.ActiveGeneration, statePath, "")
	runtimeFake.current, runtimeFake.exists = spec, true
	routingFake.snapshot.Generation = stopped.RoutingGeneration + 3
	routingFake.snapshot.AppleActive = true
	routingFake.snapshot.RuntimeRoutingPending = true
	journal := transactionJournal{
		Schema: SchemaVersion, InstallationID: state.InstallationID, OperationID: operationID,
		Phase: phaseVerified, ExpectedStateDigest: expectedDigest,
		ExpectedRoutingGeneration: stopped.RoutingGeneration,
		OldArtifact:               cloneRecord(state.Active), NewArtifact: *state.Active,
		OldGeneration: state.ActiveGeneration, NewGeneration: state.ActiveGeneration,
		NewContainerID: ContainerName, ReuseGeneration: true,
	}
	state.Status = StateHealthy
	state.ContainerID = ContainerName
	state.ActiveOperationID = operationID
	state.RoutingGeneration = routingFake.snapshot.Generation
	if err := manager.store.save(state); err != nil {
		t.Fatal(err)
	}
	if err := manager.store.saveJournal(journal); err != nil {
		t.Fatal(err)
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil || inspection.State != StateRecoveryRequired {
		t.Fatalf("restart crash inspection = %#v err=%v", inspection, err)
	}
	recovered, err := manager.Recover(context.Background(), RecoverRequest{ExpectedStateDigest: inspection.StateDigest, ConfirmDesktopExited: true})
	if err != nil {
		t.Fatal(err)
	}
	if recovered.State != StateHealthy || recovered.Active == nil ||
		recovered.Active.ManifestSHA256 != state.Active.ManifestSHA256 || recovered.RoutingGeneration != routingFake.snapshot.Generation {
		t.Fatalf("restart recovery receipt = %#v", recovered)
	}
	if _, found, err := manager.store.loadJournal(); err != nil || found {
		t.Fatalf("restart recovery retained journal: found=%t err=%v", found, err)
	}
}

func activateManagerFixture(t *testing.T) (*Manager, *managerRuntimeFake, *managerRoutingFake, MutationReceipt) {
	t.Helper()
	fixture := newManagerFixture(t)
	candidate := fixture.candidate(t, "2.40.0", 1, 1)
	events := []string{}
	runtimeFake := &managerRuntimeFake{events: &events}
	routingFake := &managerRoutingFake{events: &events, snapshot: RoutingSnapshot{Generation: 1}}
	manager := fixture.manager(t, &managerCheckerFake{candidate: candidate}, runtimeFake, routingFake, &events)
	checked, err := manager.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	staged, err := manager.Stage(context.Background(), StageRequest{candidate.ManifestSHA256, checked.StateDigest, checked.RoutingGeneration})
	if err != nil {
		t.Fatal(err)
	}
	active, err := manager.Activate(context.Background(), ActivateRequest{staged.StateDigest, staged.RoutingGeneration, true})
	if err != nil {
		t.Fatal(err)
	}
	return manager, runtimeFake, routingFake, active
}

type managerFixture struct {
	root         string
	publicKey    []byte
	privateKey   ed25519.PrivateKey
	keyID        string
	secretServer *managerSecretServerFake
}

func newManagerFixture(t *testing.T) *managerFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(publicKey)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(der)
	return &managerFixture{
		root:       filepath.Join(t.TempDir(), "ContainerRuntime"),
		publicKey:  pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}),
		privateKey: privateKey, keyID: hex.EncodeToString(digest[:]),
		secretServer: &managerSecretServerFake{lease: &managerSecretLeaseFake{path: "/private/tmp/fake-runtime.sock"}},
	}
}

func (f *managerFixture) candidate(t *testing.T, version string, sequence uint64, variant byte) runtimemanifest.Candidate {
	t.Helper()
	artifact := version + "-r1"
	digit := string("abcdef"[int(variant)%6])
	manifest := runtimemanifest.Manifest{
		Schema: 1, ArtifactKind: runtimemanifest.ArtifactKind, ArtifactVersion: artifact,
		ReleaseSequence: sequence, Channel: runtimemanifest.StableChannel,
		Source: runtimemanifest.Source{Repository: runtimemanifest.ProductionSourceRepo, Revision: strings.Repeat(digit, 40), UpstreamLockSHA256: strings.Repeat(digit, 64)},
		Upstream: runtimemanifest.Upstream{
			Repository: runtimemanifest.ProductionUpstreamRepo, ReleaseID: int64(100 + sequence), ReleaseTag: "v" + version,
			Version: version, Revision: strings.Repeat(digit, 40), NPMPackage: runtimemanifest.NPMPackage,
			NPMIntegrity: "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, 64)),
		},
		Image: runtimemanifest.Image{Repository: runtimemanifest.ProductionImageRepository, IndexDigest: "sha256:" + strings.Repeat(digit, 64), Platforms: []runtimemanifest.Platform{
			{OS: "linux", Arch: "amd64", Digest: "sha256:" + strings.Repeat(string("123456"[int(variant)%6]), 64)},
			{OS: "linux", Arch: "arm64", Digest: "sha256:" + strings.Repeat(string("789abc"[int(variant)%6]), 64)},
		}},
		Compatibility: runtimemanifest.Compatibility{MinimumRelayVersion: "0.3.9", MinimumMacOS: "26.0", MinimumAppleContainer: "1.3.1", ManagementAPIRevision: 1, SecretDelivery: runtimemanifest.SecretDeliveryUDSV1, StateFormatRevision: 1},
		Canary:        runtimemanifest.Canary{SourceRevision: strings.Repeat(digit, 40), WorkflowRunID: "123", WorkflowRunAttempt: 1, Result: "passed"},
		TrustKeyID:    f.keyID,
	}
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(f.privateKey, body)))
	return runtimemanifest.Candidate{
		ReleaseID: int64(100 + sequence), Tag: runtimemanifest.RuntimeReleasePrefix + artifact,
		ManifestSHA256: runtimemanifest.ManifestSHA256(body), Manifest: manifest,
		ManifestBytes: body, SignatureBytes: signature,
	}
}

func (f *managerFixture) manager(t *testing.T, checker *managerCheckerFake, runtimeFake *managerRuntimeFake, routingFake *managerRoutingFake, events *[]string) *Manager {
	t.Helper()
	manager, err := NewManager(ManagerOptions{
		Root: f.root, Account: "test-user", RelayVersion: "0.3.9", PublicKeyPEM: f.publicKey,
		Checker: checker, Runtime: runtimeFake, Prober: &managerProberFake{events: events},
		Cloner: &managerClonerFake{events: events}, SecretServer: f.secretServer,
		Keychain: managerKeychainFake{}, Routing: routingFake, Enroller: managerEnrollerFake{}, Locker: managerLockerFake{},
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

type managerCheckerFake struct {
	candidate   runtimemanifest.Candidate
	unavailable bool
}

func (f *managerCheckerFake) Check(context.Context, runtimemanifest.CheckRequest) (runtimemanifest.CheckResult, error) {
	if f.unavailable {
		return runtimemanifest.CheckResult{
			Status: runtimemanifest.CheckStatusUnavailable,
			Reason: "stable_runtime_manifest_unavailable",
		}, nil
	}
	if f.candidate.ManifestSHA256 == "" {
		return runtimemanifest.CheckResult{Status: runtimemanifest.CheckStatusCurrent}, nil
	}
	candidate := f.candidate
	return runtimemanifest.CheckResult{Status: runtimemanifest.CheckStatusUpdateAvailable, Candidate: &candidate}, nil
}

func (f *managerCheckerFake) ResolveExpected(_ context.Context, expected string, _ runtimemanifest.CheckRequest) (runtimemanifest.Candidate, error) {
	if expected != f.candidate.ManifestSHA256 {
		return runtimemanifest.Candidate{}, ErrStateChanged
	}
	return f.candidate, nil
}

type managerRuntimeFake struct {
	events                  *[]string
	pullCount               int
	failManifest            string
	current                 StartSpec
	exists                  bool
	deleteFailure           bool
	cleanupWait             bool
	cleanupDeadlineObserved bool
	startHook               func()
	startError              error
	stopHook                func()
	foreignExisting         bool
	containerState          FixedContainerState
	capability              *Capability
}

func (f *managerRuntimeFake) event(value string) { *f.events = append(*f.events, value) }
func (f *managerRuntimeFake) Capability(context.Context, string, string) (Capability, error) {
	if f.capability != nil {
		return *f.capability, nil
	}
	return Capability{Available: true, MacOSVersion: "26.5.1", AppleContainerVersion: "1.3.1", SystemServiceState: "running"}, nil
}
func (f *managerRuntimeFake) Pull(context.Context, string) error { f.pullCount++; return nil }
func (f *managerRuntimeFake) VerifyImage(context.Context, StartSpec, runtimemanifest.Manifest) error {
	return nil
}
func (f *managerRuntimeFake) Start(_ context.Context, spec StartSpec) (string, error) {
	f.event("runtime-start")
	if f.exists {
		return "", ErrStateChanged
	}
	f.current, f.exists = spec, true
	if f.startHook != nil {
		hook := f.startHook
		f.startHook = nil
		hook()
	}
	if f.startError != nil {
		err := f.startError
		f.startError = nil
		return "", err
	}
	return ContainerName, nil
}
func (f *managerRuntimeFake) Stop(ctx context.Context, _ string, spec StartSpec) error {
	f.event("runtime-stop")
	if f.exists && !sameRuntimeSpec(f.current, spec) {
		return ErrForeignResource
	}
	if f.stopHook != nil {
		hook := f.stopHook
		f.stopHook = nil
		hook()
	}
	if f.cleanupWait && f.exists {
		_, f.cleanupDeadlineObserved = ctx.Deadline()
		<-ctx.Done()
		return ErrUnavailable
	}
	return nil
}
func (f *managerRuntimeFake) Delete(ctx context.Context, _ string, spec StartSpec) error {
	f.event("runtime-delete")
	if f.exists && !sameRuntimeSpec(f.current, spec) {
		return ErrForeignResource
	}
	if f.cleanupWait && f.exists {
		_, deadline := ctx.Deadline()
		f.cleanupDeadlineObserved = f.cleanupDeadlineObserved && deadline
		return ErrUnavailable
	}
	if f.deleteFailure {
		return ErrUnavailable
	}
	f.exists = false
	return nil
}
func (f *managerRuntimeFake) VerifyContainer(_ context.Context, _ string, spec StartSpec) error {
	f.event("runtime-verify")
	if f.exists && f.foreignExisting {
		return ErrForeignResource
	}
	if !f.exists || !sameRuntimeSpec(f.current, spec) || spec.ManifestSHA256 == f.failManifest {
		return ErrUnavailable
	}
	return nil
}
func (f *managerRuntimeFake) ContainerState(_ context.Context, _ string, spec StartSpec) (FixedContainerState, error) {
	f.event("runtime-state")
	if f.exists && f.foreignExisting {
		return FixedContainerForeign, nil
	}
	if !f.exists || !sameRuntimeSpec(f.current, spec) || spec.ManifestSHA256 == f.failManifest {
		return FixedContainerAbsent, nil
	}
	if f.containerState != "" {
		return f.containerState, nil
	}
	return FixedContainerRunningOwned, nil
}
func (f *managerRuntimeFake) VerifyAbsent(context.Context, string) error {
	if f.exists {
		if f.foreignExisting {
			return ErrForeignResource
		}
		return ErrStateChanged
	}
	return nil
}

func sameRuntimeSpec(left, right StartSpec) bool {
	left.SocketPath = ""
	right.SocketPath = ""
	return left == right
}

type managerRoutingFake struct {
	events                   *[]string
	snapshot                 RoutingSnapshot
	prepareCount             int
	commitCount              int
	rollbackCount            int
	reconcileCount           int
	acknowledgeCount         int
	reconcileAdvance         uint64
	activateError            error
	stopError                error
	acknowledgeResponseError error
	acknowledgeHook          func()
	leaveRouteRecovery       bool
	commitError              error
	rollbackError            error
	lastRequest              *RoutingRequest
	lastRollback             *MaintenanceWitness
}

func (f *managerRoutingFake) event(value string) { *f.events = append(*f.events, value) }
func (f *managerRoutingFake) Current(context.Context) (RoutingSnapshot, error) {
	return f.snapshot, nil
}
func (f *managerRoutingFake) ActivateApple(_ context.Context, request RoutingRequest, desktopExited bool) (uint64, error) {
	if !desktopExited {
		return 0, ErrInvalidRequest
	}
	f.event("route-activate")
	copy := request
	f.lastRequest = &copy
	f.snapshot.Generation = request.ExpectedOriginRoutingGeneration + 3
	f.snapshot.AppleActive = !f.leaveRouteRecovery
	f.snapshot.RecoveryRequired = f.leaveRouteRecovery
	f.snapshot.MaintenancePending = false
	f.snapshot.RuntimeRoutingPending = true
	if f.activateError != nil {
		err := f.activateError
		f.activateError = nil
		return 0, err
	}
	return f.snapshot.Generation, nil
}

func (f *managerRoutingFake) Reconcile(_ context.Context, request RoutingRequest, desktopExited bool) (uint64, error) {
	if !desktopExited {
		return 0, ErrInvalidRequest
	}
	if !f.snapshot.RuntimeRoutingPending {
		return 0, ErrRoutingChanged
	}
	f.reconcileCount++
	copy := request
	f.lastRequest = &copy
	selectedApple := request.TargetAppleActive
	if request.Direction == RoutingRestoreOrigin {
		selectedApple = !request.TargetAppleActive
		f.event("route-stop")
	} else {
		f.event("route-activate")
	}
	if f.snapshot.AppleActive != selectedApple || f.snapshot.RecoveryRequired {
		advance := f.reconcileAdvance
		if advance == 0 {
			advance = 3
		}
		f.snapshot.Generation += advance
	}
	f.snapshot.AppleActive = selectedApple
	f.snapshot.RecoveryRequired = false
	f.snapshot.MaintenancePending = false
	f.snapshot.RuntimeRoutingPending = true
	return f.snapshot.Generation, nil
}

func (f *managerRoutingFake) Acknowledge(_ context.Context, _ RoutingRequest, generation uint64) error {
	if !f.snapshot.RuntimeRoutingPending || generation != f.snapshot.Generation {
		return ErrRoutingChanged
	}
	f.acknowledgeCount++
	if f.acknowledgeHook != nil {
		f.acknowledgeHook()
	}
	f.snapshot.RuntimeRoutingPending = false
	if f.acknowledgeResponseError != nil {
		err := f.acknowledgeResponseError
		f.acknowledgeResponseError = nil
		return err
	}
	return nil
}
func (f *managerRoutingFake) Prepare(_ context.Context, request MaintenanceRequest) (MaintenanceWitness, error) {
	f.event("route-prepare")
	f.prepareCount++
	witness := MaintenanceWitness{Schema: 1, Backend: "local_apple_container", OriginRoutingGeneration: request.ExpectedRoutingGeneration, PreparedRoutingGeneration: request.ExpectedRoutingGeneration + 1, FinalRoutingGeneration: request.ExpectedRoutingGeneration + 2, Intent: MaintenanceIntent{
		OperationID: request.OperationID, InstallationID: request.InstallationID,
		OldManifestSHA256: request.OldManifestSHA256, NewManifestSHA256: request.NewManifestSHA256,
		OldImageDigest: request.OldImageDigest, NewImageDigest: request.NewImageDigest,
		OldStateGeneration: request.OldStateGeneration, NewStateGeneration: request.NewStateGeneration,
	}}
	f.snapshot.Generation = witness.PreparedRoutingGeneration
	f.snapshot.AppleActive = false
	f.snapshot.RecoveryRequired = true
	f.snapshot.MaintenancePending = true
	return witness, nil
}
func (f *managerRoutingFake) Commit(_ context.Context, witness MaintenanceWitness) (uint64, error) {
	f.event("route-commit")
	f.commitCount++
	f.snapshot.Generation, f.snapshot.AppleActive = witness.FinalRoutingGeneration, true
	f.snapshot.RecoveryRequired = false
	f.snapshot.MaintenancePending = false
	if f.commitError != nil {
		err := f.commitError
		f.commitError = nil
		return 0, err
	}
	return witness.FinalRoutingGeneration, nil
}
func (f *managerRoutingFake) Rollback(_ context.Context, witness MaintenanceWitness) (uint64, error) {
	f.event("route-rollback")
	f.rollbackCount++
	copy := witness
	f.lastRollback = &copy
	f.snapshot.Generation, f.snapshot.AppleActive = witness.FinalRoutingGeneration, true
	f.snapshot.RecoveryRequired = false
	f.snapshot.MaintenancePending = false
	if f.rollbackError != nil {
		err := f.rollbackError
		f.rollbackError = nil
		return 0, err
	}
	return witness.FinalRoutingGeneration, nil
}
func (f *managerRoutingFake) StopApple(_ context.Context, request RoutingRequest, desktopExited bool) (uint64, error) {
	if !desktopExited {
		return 0, ErrInvalidRequest
	}
	f.event("route-stop")
	copy := request
	f.lastRequest = &copy
	f.snapshot.Generation = request.ExpectedOriginRoutingGeneration + 3
	f.snapshot.AppleActive = f.leaveRouteRecovery
	f.snapshot.RecoveryRequired = f.leaveRouteRecovery
	f.snapshot.MaintenancePending = false
	f.snapshot.RuntimeRoutingPending = true
	if f.stopError != nil {
		err := f.stopError
		f.stopError = nil
		return 0, err
	}
	return f.snapshot.Generation, nil
}

type managerClonerFake struct {
	events *[]string
	err    error
}

type managerClonerFunc func(context.Context, string, string) error

func (f managerClonerFunc) Clone(ctx context.Context, source, destination string) error {
	return f(ctx, source, destination)
}

func (f *managerClonerFake) Clone(_ context.Context, _, destination string) error {
	*f.events = append(*f.events, "clone")
	if f.err != nil {
		return f.err
	}
	return os.Mkdir(destination, 0o700)
}

type managerSecretServerFake struct{ lease *managerSecretLeaseFake }

func (f *managerSecretServerFake) Open(context.Context, Secrets) (SecretLease, error) {
	return f.lease, nil
}

type managerSecretLeaseFake struct {
	path   string
	onWait func() error
}

func (f *managerSecretLeaseFake) Path() string { return f.path }
func (f *managerSecretLeaseFake) Wait(context.Context) error {
	if f.onWait != nil {
		return f.onWait()
	}
	return nil
}
func (*managerSecretLeaseFake) Close() error { return nil }

type managerKeychainFake struct{}

func (managerKeychainFake) Load(context.Context, string) (Secrets, error) { return testSecrets(), nil }
func (managerKeychainFake) Ensure(context.Context, string) (Secrets, error) {
	return testSecrets(), nil
}

type managerInspectKeychainFake struct {
	events                 *[]string
	secrets                Secrets
	err                    error
	loadCount, ensureCount int
}

func (f *managerInspectKeychainFake) Load(context.Context, string) (Secrets, error) {
	f.loadCount++
	if f.events != nil {
		*f.events = append(*f.events, "keychain-load")
	}
	if f.err != nil {
		return Secrets{}, f.err
	}
	return Secrets{APIToken: append([]byte(nil), f.secrets.APIToken...), AdminToken: append([]byte(nil), f.secrets.AdminToken...)}, nil
}

func (f *managerInspectKeychainFake) Ensure(context.Context, string) (Secrets, error) {
	f.ensureCount++
	return Secrets{}, errors.New("unexpected Keychain Ensure")
}

type managerProberFake struct {
	events       *[]string
	err          error
	verifyErrors []error
}

func (f *managerProberFake) Verify(_ context.Context, _, _ []byte, guard func(context.Context) error) error {
	if guard == nil {
		return ErrUnavailable
	}
	if f.events != nil {
		*f.events = append(*f.events, "http-probe")
	}
	if len(f.verifyErrors) > 0 {
		err := f.verifyErrors[0]
		f.verifyErrors = f.verifyErrors[1:]
		return err
	}
	return f.err
}

type managerEnrollerFake struct{}

func (managerEnrollerFake) Ensure(context.Context) (string, error) { return "test-user", nil }

type managerLockerFake struct{}

func (managerLockerFake) Lock(context.Context) (func() error, error) {
	return func() error { return nil }, nil
}

func assertEventOrder(t *testing.T, events []string, expected ...string) {
	t.Helper()
	position := -1
	for _, want := range expected {
		found := -1
		for index := position + 1; index < len(events); index++ {
			if events[index] == want {
				found = index
				break
			}
		}
		if found < 0 {
			t.Fatalf("event %q not found after %d in %#v", want, position, events)
		}
		position = found
	}
}
