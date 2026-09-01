package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

func TestUpgradeInspectStatesAndRecoveryGate(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()

	notIntegrated, err := manager.InspectUpgrade(context.Background())
	if err != nil || notIntegrated.State != UpgradeNotIntegrated || notIntegrated.RestartRequired || !isLowerSHA256(notIntegrated.StateDigest) {
		t.Fatalf("not integrated inspection=%#v err=%v", notIntegrated, err)
	}
	installIntegration(t, manager)
	current, err := manager.InspectUpgrade(context.Background())
	if err != nil || current.State != UpgradeCurrent || current.RestartRequired || current.InstalledRuntimeVersion != "1.2.3" || !isLowerSHA256(current.InstalledRuntimeDigest) {
		t.Fatalf("current inspection=%#v err=%v", current, err)
	}

	upgradeBundledRuntime(t, manager, "1.2.4")
	available, err := manager.InspectUpgrade(context.Background())
	if err != nil || available.State != UpgradeAvailable || !available.RestartRequired || available.InstalledRuntimeVersion != "1.2.3" || available.BundledRuntimeVersion != "1.2.4" || available.InstalledRuntimeDigest == available.BundledRuntimeDigest {
		t.Fatalf("available inspection=%#v err=%v", available, err)
	}

	manager.Version = "1.2.2"
	incompatible, err := manager.InspectUpgrade(context.Background())
	if err != nil || incompatible.State != UpgradeIncompatible || incompatible.RestartRequired {
		t.Fatalf("downgrade inspection=%#v err=%v", incompatible, err)
	}
	manager.Version = "1.2.4"
	relocation := filepath.Join(filepath.Dir(manager.Paths.Binding), "application-relocation.json")
	if err := os.WriteFile(relocation, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovery, err := manager.InspectUpgrade(context.Background())
	if err != nil || recovery.State != UpgradeRecoveryRequired || !recovery.RestartRequired {
		t.Fatalf("recovery inspection=%#v err=%v", recovery, err)
	}
}

func TestApplyUpgradeSwitchesRuntimeAndPreservesIntegrationInvariants(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	installIntegration(t, manager)
	oldRuntime, err := manager.readInstalledRuntime()
	if err != nil {
		t.Fatal(err)
	}
	before := invariantFingerprints(t, manager)
	helperBefore := invariantFingerprintsForPaths(t, []string{
		filepath.Join(manager.Paths.App, "Contents/Library/HelperTools/OpenCodexRelayPrivilegedHelper"),
		filepath.Join(manager.Paths.App, "Contents/Library/Helpers/OpenCodexRelayHelperInstaller"),
	})
	upgradeBundledRuntime(t, manager, "1.2.4")
	inspection, err := manager.InspectUpgrade(context.Background())
	if err != nil || inspection.State != UpgradeAvailable {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	if _, err := manager.ApplyUpgrade(context.Background(), inspection.StateDigest, false); !errors.Is(err, ErrRestartConfirmationNeeded) {
		t.Fatalf("unconfirmed apply error=%v", err)
	}
	receipt, err := manager.ApplyUpgrade(context.Background(), inspection.StateDigest, true)
	if err != nil || !receipt.OK || receipt.State != UpgradeCurrent || receipt.RestartRequired || receipt.InstalledRuntimeVersion != "1.2.4" || receipt.InstalledRuntimeDigest != receipt.BundledRuntimeDigest {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	assertInvariantFingerprints(t, manager, before)
	assertInvariantFingerprints(t, manager, helperBefore)
	installed, err := manager.readInstalledRuntime()
	if err != nil || installed.Target == oldRuntime.Target || installed.RuntimeDigest != receipt.BundledRuntimeDigest || !strings.HasSuffix(installed.Target, receipt.BundledRuntimeDigest) {
		t.Fatalf("installed=%#v err=%v", installed, err)
	}
	if _, err := os.Stat(oldRuntime.Directory); err != nil {
		t.Fatalf("previous runtime was not retained: %v", err)
	}
	service, err := os.ReadFile(manager.Paths.ServicePlist)
	if err != nil || !bytes.Equal(service, manager.servicePayload(installed.Relay)) {
		t.Fatalf("service did not bind new runtime: err=%v", err)
	}
	if _, err := os.Lstat(manager.upgradeJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("upgrade journal remains: %v", err)
	}
	if info, err := os.Lstat(manager.upgradeWitnessPath()); err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		t.Fatalf("success witness invalid: info=%v err=%v", info, err)
	}
}

func TestApplyUpgradeRejectsStaleExpectedStateBeforeMutation(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	installIntegration(t, manager)
	oldRuntime, err := manager.readInstalledRuntime()
	if err != nil {
		t.Fatal(err)
	}
	upgradeBundledRuntime(t, manager, "1.2.4")
	inspection, err := manager.InspectUpgrade(context.Background())
	if err != nil || inspection.State != UpgradeAvailable {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	if err := os.WriteFile(manager.upgradeWitnessPath(), []byte("changed-after-inspection\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyUpgrade(context.Background(), inspection.StateDigest, true); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("stale apply error=%v", err)
	}
	installed, err := manager.readInstalledRuntime()
	if err != nil || installed.Target != oldRuntime.Target {
		t.Fatalf("stale apply changed runtime=%#v err=%v", installed, err)
	}
	if _, err := os.Lstat(manager.upgradeJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale apply created journal: %v", err)
	}
}

func TestApplyUpgradeReadinessFailureRollsBackOldRuntime(t *testing.T) {
	manager, runner, cleanup := integrationFixture(t)
	defer cleanup()
	installIntegration(t, manager)
	oldRuntime, err := manager.readInstalledRuntime()
	if err != nil {
		t.Fatal(err)
	}
	upgradeBundledRuntime(t, manager, "1.2.4")
	manager.VerifyUpgrade = func(_ context.Context, _ config.Config, _ routing.State, path, digest string) error {
		if path != oldRuntime.Relay {
			return ErrActivationFailed
		}
		observed, err := fileDigest(path)
		if err != nil || observed != digest || !runner.active {
			return ErrActivationFailed
		}
		return nil
	}
	inspection, err := manager.InspectUpgrade(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyUpgrade(context.Background(), inspection.StateDigest, true); !errors.Is(err, ErrActivationFailed) {
		t.Fatalf("apply error=%v", err)
	}
	installed, err := manager.readInstalledRuntime()
	if err != nil || installed.Target != oldRuntime.Target || installed.RelayDigest != oldRuntime.RelayDigest || !runner.active {
		t.Fatalf("rollback runtime=%#v active=%t err=%v", installed, runner.active, err)
	}
	if _, err := os.Lstat(manager.upgradeJournalPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rollback retained journal: %v", err)
	}
	entries, err := os.ReadDir(manager.bundledRoot())
	if err != nil || len(entries) != 1 {
		t.Fatalf("failed new runtime was retained: entries=%v err=%v", entries, err)
	}
}

func TestRecoverUpgradeChoosesRollbackOrVerifiedForwardCompletion(t *testing.T) {
	for _, test := range []struct {
		name        string
		phase       string
		wantVersion string
		wantWitness bool
	}{
		{name: "prepared rolls back", phase: "prepared", wantVersion: "1.2.3"},
		{name: "runtime staged rolls back", phase: "runtime_staged", wantVersion: "1.2.3"},
		{name: "switched rolls back", phase: "switched", wantVersion: "1.2.3"},
		{name: "restarting rolls back", phase: "restarting", wantVersion: "1.2.3"},
		{name: "verified completes", phase: "verified", wantVersion: "1.2.4", wantWitness: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, _, cleanup := integrationFixture(t)
			defer cleanup()
			installIntegration(t, manager)
			upgradeBundledRuntime(t, manager, "1.2.4")
			journal := prepareInterruptedUpgrade(t, manager, test.phase)
			receipt, err := manager.RecoverUpgrade(context.Background())
			if err != nil || !receipt.OK || receipt.InstalledRuntimeVersion != test.wantVersion {
				t.Fatalf("receipt=%#v err=%v journal=%#v", receipt, err, journal)
			}
			if _, err := os.Lstat(manager.upgradeJournalPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("journal remains: %v", err)
			}
			_, witnessErr := os.Lstat(manager.upgradeWitnessPath())
			if test.wantWitness && witnessErr != nil {
				t.Fatalf("witness missing: %v", witnessErr)
			}
			if !test.wantWitness && !errors.Is(witnessErr, os.ErrNotExist) {
				t.Fatalf("rollback created witness: %v", witnessErr)
			}
		})
	}
}

func TestApplyUpgradeCleansBoundedOrphanedStaging(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	installIntegration(t, manager)
	upgradeBundledRuntime(t, manager, "1.2.4")
	if err := os.MkdirAll(manager.bundledRoot(), 0o700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(manager.bundledRoot(), ".runtime-upgrade.interrupted")
	if err := os.Mkdir(orphan, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "opencodex-relay"), []byte("partial"), 0o700); err != nil {
		t.Fatal(err)
	}
	inspection, err := manager.InspectUpgrade(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	relocation := filepath.Join(filepath.Dir(manager.Paths.Binding), "application-relocation.json")
	if err := os.WriteFile(relocation, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.ApplyUpgrade(context.Background(), inspection.StateDigest, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("apply with external recovery error=%v", err)
	}
	if _, err := os.Lstat(orphan); err != nil {
		t.Fatalf("external recovery mutated orphan staging: %v", err)
	}
	if err := os.Remove(relocation); err != nil {
		t.Fatal(err)
	}
	receipt, err := manager.ApplyUpgrade(context.Background(), inspection.StateDigest, true)
	if err != nil || receipt.State != UpgradeCurrent {
		t.Fatalf("receipt=%#v err=%v", receipt, err)
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan staging remains: %v", err)
	}
}

func TestUpgradeJournalRejectsUnknownDuplicateAndPathSubstitution(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	installIntegration(t, manager)
	upgradeBundledRuntime(t, manager, "1.2.4")
	journal := prepareInterruptedUpgrade(t, manager, "restarting")
	payload, err := json.Marshal(journal)
	if err != nil {
		t.Fatal(err)
	}
	base := string(payload)
	for name, body := range map[string]string{
		"unknown":           strings.Replace(base, `"schema_version":1`, `"schema_version":1,"unknown":true`, 1),
		"duplicate":         strings.Replace(base, `"phase":"restarting"`, `"phase":"restarting","phase":"verified"`, 1),
		"path substitution": strings.Replace(base, manager.Paths.ServicePlist, filepath.Join(manager.Paths.Home, "other.plist"), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(manager.upgradeJournalPath(), []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.readUpgradeJournal(); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("journal error=%v", err)
			}
		})
	}
}

func TestSharedUpgradeInspectionFixtureMatchesGoSchema(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("..", "..", "testdata", "runtime-upgrade", "inspect-upgrade-available-v1.json"))
	if err != nil || rejectDuplicateUpgradeJSONKeys(payload) != nil {
		t.Fatalf("fixture read error=%v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var inspection UpgradeInspection
	if decoder.Decode(&inspection) != nil || requireUpgradeJSONEOF(decoder) != nil {
		t.Fatal("shared upgrade fixture did not decode strictly")
	}
	if inspection.SchemaVersion != 1 || inspection.State != UpgradeAvailable ||
		inspection.InstalledRuntimeVersion != "0.3.8-rc.7" ||
		inspection.BundledRuntimeVersion != "0.3.8-rc.8" ||
		!inspection.RestartRequired || !isLowerSHA256(inspection.StateDigest) ||
		!isLowerSHA256(inspection.InstalledRuntimeDigest) ||
		!isLowerSHA256(inspection.BundledRuntimeDigest) || inspection.IntegrationProtocol != 1 {
		t.Fatalf("fixture=%#v", inspection)
	}
}

func installIntegration(t *testing.T, manager *Manager) {
	t.Helper()
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Apply(context.Background(), Candidate{
		UpstreamBaseURL:       "https://gateway.example.test/v1",
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}, inspection.StateDigest)
	if err != nil {
		t.Fatal(err)
	}
}

func upgradeBundledRuntime(t *testing.T, manager *Manager, version string) {
	t.Helper()
	manager.Version = version
	for path, body := range map[string]string{manager.Paths.Relay: "new-relay-" + version, manager.Paths.Relayctl: "new-relayctl-" + version} {
		if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
			t.Fatal(err)
		}
	}
}

func invariantFingerprints(t *testing.T, manager *Manager) map[string]string {
	t.Helper()
	return invariantFingerprintsForPaths(t, []string{manager.Paths.Config, routing.StatePath(manager.Paths.Config), routing.InitializedPath(manager.Paths.Config), manager.Paths.Binding, manager.Paths.CodexConfig})
}

func invariantFingerprintsForPaths(t *testing.T, paths []string) map[string]string {
	t.Helper()
	result := map[string]string{}
	for _, path := range paths {
		value, err := fingerprint(path)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = value
	}
	return result
}

func assertInvariantFingerprints(t *testing.T, manager *Manager, expected map[string]string) {
	t.Helper()
	for path, want := range expected {
		got, err := fingerprint(path)
		if err != nil || got != want {
			t.Fatalf("invariant changed at %s: got=%q want=%q err=%v", path, got, want, err)
		}
	}
}

func prepareInterruptedUpgrade(t *testing.T, manager *Manager, phase string) runtimeUpgradeJournal {
	t.Helper()
	inspection, err := manager.InspectUpgrade(context.Background())
	if err != nil || inspection.State != UpgradeAvailable {
		t.Fatalf("inspection=%#v err=%v", inspection, err)
	}
	oldRuntime, err := manager.readInstalledRuntime()
	if err != nil {
		t.Fatal(err)
	}
	bundledRelayDigest, err := fileDigest(manager.Paths.Relay)
	if err != nil {
		t.Fatal(err)
	}
	staging, finalDirectory, created, err := manager.prepareUpgradeRuntime(inspection.BundledRuntimeDigest, bundledRelayDigest)
	if err != nil {
		t.Fatal(err)
	}
	invariants, generation, routingPhase, account, err := manager.captureUpgradeInvariants()
	if err != nil {
		t.Fatal(err)
	}
	currentSnapshot, err := snapshotFile(filepath.Join(manager.Paths.InstallRoot, "current"))
	if err != nil {
		t.Fatal(err)
	}
	serviceSnapshot, err := snapshotFile(manager.Paths.ServicePlist)
	if err != nil {
		t.Fatal(err)
	}
	journal := runtimeUpgradeJournal{
		SchemaVersion: schemaVersion, Phase: "prepared", OriginStateDigest: inspection.StateDigest,
		CredentialAccount: account, RoutingGeneration: generation, RoutingPhase: routingPhase,
		PreviousTarget: oldRuntime.Target, PreviousRelay: oldRuntime.Relay, PreviousRelayDigest: oldRuntime.RelayDigest, PreviousRuntimeDigest: oldRuntime.RuntimeDigest,
		NewTarget: filepath.Join("bundled", filepath.Base(finalDirectory)), NewRelay: filepath.Join(finalDirectory, "opencodex-relay"), NewRelayDigest: bundledRelayDigest,
		NewRuntimeVersion: manager.Version, NewRuntimeDigest: inspection.BundledRuntimeDigest,
		StagingDirectory: staging, RuntimeCreated: created, ServiceWasActive: true,
		MutableFiles: []fileSnapshot{currentSnapshot, serviceSnapshot}, Invariants: invariants,
	}
	if err := manager.writeUpgradeJournal(journal); err != nil {
		t.Fatal(err)
	}
	if phase != "prepared" && staging != "" {
		if err := os.Rename(staging, finalDirectory); err != nil {
			t.Fatal(err)
		}
		journal.StagingDirectory = ""
	}
	if phase == "switched" || phase == "restarting" || phase == "verified" {
		if err := manager.switchCurrentTarget(journal.NewTarget); err != nil {
			t.Fatal(err)
		}
		if err := atomicWrite(manager.Paths.ServicePlist, manager.servicePayload(journal.NewRelay), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	journal.Phase = phase
	if err := manager.writeUpgradeJournal(journal); err != nil {
		t.Fatal(err)
	}
	return journal
}
