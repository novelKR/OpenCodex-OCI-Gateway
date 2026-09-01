package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/lifecyclelock"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/release"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

const integrationProtocolVersion = 1

var (
	ErrUpgradeIncompatible       = errors.New("runtime upgrade is incompatible")
	ErrRestartConfirmationNeeded = errors.New("relay restart confirmation is required")
)

type UpgradeState string

const (
	UpgradeNotIntegrated    UpgradeState = "not_integrated"
	UpgradeCurrent          UpgradeState = "current"
	UpgradeAvailable        UpgradeState = "upgrade_available"
	UpgradeRecoveryRequired UpgradeState = "recovery_required"
	UpgradeIncompatible     UpgradeState = "incompatible"
)

type UpgradeInspection struct {
	SchemaVersion           int          `json:"schema_version"`
	State                   UpgradeState `json:"state"`
	StateDigest             string       `json:"state_digest"`
	InstalledRuntimeVersion string       `json:"installed_runtime_version"`
	InstalledRuntimeDigest  string       `json:"installed_runtime_digest"`
	BundledRuntimeVersion   string       `json:"bundled_runtime_version"`
	BundledRuntimeDigest    string       `json:"bundled_runtime_digest"`
	IntegrationProtocol     int          `json:"integration_protocol"`
	RestartRequired         bool         `json:"restart_required"`
}

type UpgradeReceipt struct {
	SchemaVersion           int          `json:"schema_version"`
	OK                      bool         `json:"ok"`
	State                   UpgradeState `json:"state"`
	StateDigest             string       `json:"state_digest"`
	InstalledRuntimeVersion string       `json:"installed_runtime_version"`
	InstalledRuntimeDigest  string       `json:"installed_runtime_digest"`
	BundledRuntimeVersion   string       `json:"bundled_runtime_version"`
	BundledRuntimeDigest    string       `json:"bundled_runtime_digest"`
	IntegrationProtocol     int          `json:"integration_protocol"`
	RestartRequired         bool         `json:"restart_required"`
}

type installedRuntime struct {
	Target         string
	Directory      string
	Relay          string
	Relayctl       string
	Version        string
	RelayDigest    string
	RelayctlDigest string
	RuntimeDigest  string
}

type upgradeInvariant struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Digest  string `json:"digest,omitempty"`
	Mode    uint32 `json:"mode,omitempty"`
	UID     int    `json:"uid,omitempty"`
}

type runtimeUpgradeJournal struct {
	SchemaVersion         int                `json:"schema_version"`
	Phase                 string             `json:"phase"`
	OriginStateDigest     string             `json:"origin_state_digest"`
	CredentialAccount     string             `json:"credential_account"`
	RoutingGeneration     uint64             `json:"routing_generation"`
	RoutingPhase          routing.Phase      `json:"routing_phase"`
	PreviousTarget        string             `json:"previous_target"`
	PreviousRelay         string             `json:"previous_relay"`
	PreviousRelayDigest   string             `json:"previous_relay_digest"`
	PreviousRuntimeDigest string             `json:"previous_runtime_digest"`
	NewTarget             string             `json:"new_target"`
	NewRelay              string             `json:"new_relay"`
	NewRelayDigest        string             `json:"new_relay_digest"`
	NewRuntimeVersion     string             `json:"new_runtime_version"`
	NewRuntimeDigest      string             `json:"new_runtime_digest"`
	StagingDirectory      string             `json:"staging_directory,omitempty"`
	RuntimeCreated        bool               `json:"runtime_created"`
	ServiceWasActive      bool               `json:"service_was_active"`
	MutableFiles          []fileSnapshot     `json:"mutable_files"`
	Invariants            []upgradeInvariant `json:"invariants"`
}

type runtimeUpgradeWitness struct {
	SchemaVersion     int    `json:"schema_version"`
	Version           string `json:"version"`
	RuntimeDigest     string `json:"runtime_digest"`
	CurrentTarget     string `json:"current_target"`
	PreviousTarget    string `json:"previous_target"`
	RoutingGeneration uint64 `json:"routing_generation"`
	VerifiedAt        string `json:"verified_at"`
}

func (m *Manager) InspectUpgrade(ctx context.Context) (UpgradeInspection, error) {
	if err := m.validateUpgrade(ctx); err != nil {
		return UpgradeInspection{}, err
	}
	bundledRelayDigest, err := fileDigest(m.Paths.Relay)
	if err != nil {
		return UpgradeInspection{}, ErrArtifactInvalid
	}
	bundledRelayctlDigest, err := fileDigest(m.Paths.Relayctl)
	if err != nil {
		return UpgradeInspection{}, ErrArtifactInvalid
	}
	bundledDigest := combinedRuntimeDigest(bundledRelayDigest, bundledRelayctlDigest)
	if _, err := release.ParseSemanticVersion(m.Version); err != nil {
		return UpgradeInspection{}, ErrArtifactInvalid
	}
	stateDigest, err := m.upgradeStateDigest()
	if err != nil {
		return UpgradeInspection{}, err
	}
	result := UpgradeInspection{
		SchemaVersion:         schemaVersion,
		State:                 UpgradeIncompatible,
		StateDigest:           stateDigest,
		BundledRuntimeVersion: m.Version,
		BundledRuntimeDigest:  bundledDigest,
		IntegrationProtocol:   integrationProtocolVersion,
	}
	if m.upgradeRecoveryPresent() {
		result.State = UpgradeRecoveryRequired
		result.RestartRequired = true
		if installed, runtimeErr := m.readInstalledRuntime(); runtimeErr == nil {
			result.InstalledRuntimeVersion = installed.Version
			result.InstalledRuntimeDigest = installed.RuntimeDigest
		}
		return result, nil
	}

	presence, err := m.integrationArtifactPresence()
	if err != nil {
		return UpgradeInspection{}, err
	}
	if presence == 0 && !m.serviceActive(ctx) {
		result.State = UpgradeNotIntegrated
		return result, nil
	}
	if presence != 6 || !m.ready(ctx) {
		return result, nil
	}
	installed, err := m.readInstalledRuntime()
	if err != nil {
		return result, nil
	}
	result.InstalledRuntimeVersion = installed.Version
	result.InstalledRuntimeDigest = installed.RuntimeDigest
	store, err := routing.Open(m.Paths.Config)
	if err != nil {
		return result, nil
	}
	routingState, legacy, err := store.Read()
	if err != nil || legacy {
		return result, nil
	}
	if routingState.Phase == routing.PhaseApplying || routingState.Phase == routing.PhaseRecoveryRequired {
		result.State = UpgradeRecoveryRequired
		result.RestartRequired = true
		return result, nil
	}
	installedVersion, err := release.ParseSemanticVersion(installed.Version)
	if err != nil {
		return result, nil
	}
	bundledVersion, _ := release.ParseSemanticVersion(m.Version)
	switch comparison := installedVersion.Compare(bundledVersion); {
	case comparison > 0:
		return result, nil
	case comparison == 0 && installed.RuntimeDigest != bundledDigest:
		return result, nil
	case comparison == 0:
		result.State = UpgradeCurrent
	case comparison < 0:
		result.State = UpgradeAvailable
		result.RestartRequired = true
	}
	return result, nil
}

func (m *Manager) ApplyUpgrade(ctx context.Context, expectedDigest string, confirmRestart bool) (UpgradeReceipt, error) {
	if !confirmRestart {
		return UpgradeReceipt{}, ErrRestartConfirmationNeeded
	}
	if !isLowerSHA256(expectedDigest) {
		return UpgradeReceipt{}, ErrStateChanged
	}
	lifecycle, err := lifecyclelock.AcquireWriter(ctx, m.Paths.Home, "")
	if err != nil {
		return UpgradeReceipt{}, err
	}
	defer lifecycle.Close()
	if err := m.requireStandaloneRemovalInactive(); err != nil {
		return UpgradeReceipt{}, err
	}
	if m.externalUpgradeRecoveryPresent() {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	if !pathExists(m.upgradeJournalPath()) {
		if err := m.cleanupOrphanedUpgradeStaging(); err != nil {
			return UpgradeReceipt{}, err
		}
	}
	inspection, err := m.InspectUpgrade(ctx)
	if err != nil {
		return UpgradeReceipt{}, err
	}
	if inspection.State == UpgradeRecoveryRequired {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	if inspection.State != UpgradeAvailable {
		return UpgradeReceipt{}, ErrUpgradeIncompatible
	}
	if inspection.StateDigest != expectedDigest {
		return UpgradeReceipt{}, ErrStateChanged
	}
	installed, err := m.readInstalledRuntime()
	if err != nil {
		return UpgradeReceipt{}, ErrUpgradeIncompatible
	}
	bundledRelayDigest, err := fileDigest(m.Paths.Relay)
	if err != nil {
		return UpgradeReceipt{}, ErrArtifactInvalid
	}
	staging, finalDirectory, created, err := m.prepareUpgradeRuntime(inspection.BundledRuntimeDigest, bundledRelayDigest)
	if err != nil {
		return UpgradeReceipt{}, err
	}
	cleanupUnjournaled := func() { _ = removeUpgradeStaging(staging, m.bundledRoot()) }
	invariants, generation, phase, account, err := m.captureUpgradeInvariants()
	if err != nil {
		cleanupUnjournaled()
		return UpgradeReceipt{}, err
	}
	currentSnapshot, err := snapshotFile(filepath.Join(m.Paths.InstallRoot, "current"))
	if err != nil {
		cleanupUnjournaled()
		return UpgradeReceipt{}, err
	}
	serviceSnapshot, err := snapshotFile(m.Paths.ServicePlist)
	if err != nil {
		cleanupUnjournaled()
		return UpgradeReceipt{}, err
	}
	newTarget := filepath.Join("bundled", filepath.Base(finalDirectory))
	journal := runtimeUpgradeJournal{
		SchemaVersion:         schemaVersion,
		Phase:                 "prepared",
		OriginStateDigest:     expectedDigest,
		CredentialAccount:     account,
		RoutingGeneration:     generation,
		RoutingPhase:          phase,
		PreviousTarget:        installed.Target,
		PreviousRelay:         installed.Relay,
		PreviousRelayDigest:   installed.RelayDigest,
		PreviousRuntimeDigest: installed.RuntimeDigest,
		NewTarget:             newTarget,
		NewRelay:              filepath.Join(finalDirectory, "opencodex-relay"),
		NewRelayDigest:        bundledRelayDigest,
		NewRuntimeVersion:     m.Version,
		NewRuntimeDigest:      inspection.BundledRuntimeDigest,
		StagingDirectory:      staging,
		RuntimeCreated:        created,
		ServiceWasActive:      true,
		MutableFiles:          []fileSnapshot{currentSnapshot, serviceSnapshot},
		Invariants:            invariants,
	}
	if err := m.writeUpgradeJournal(journal); err != nil {
		cleanupUnjournaled()
		return UpgradeReceipt{}, err
	}
	fail := func(cause error) (UpgradeReceipt, error) {
		if rollbackErr := m.rollbackUpgrade(ctx, journal); rollbackErr != nil {
			return UpgradeReceipt{}, fmt.Errorf("%w: %v", ErrRecoveryRequired, cause)
		}
		return UpgradeReceipt{}, cause
	}
	if staging != "" {
		if err := os.Rename(staging, finalDirectory); err != nil || syncDirectory(m.bundledRoot()) != nil {
			return fail(ErrUnsafeState)
		}
		journal.StagingDirectory = ""
	}
	journal.Phase = "runtime_staged"
	if err := m.writeUpgradeJournal(journal); err != nil {
		return fail(err)
	}
	if err := m.switchCurrentTarget(journal.NewTarget); err != nil {
		return fail(err)
	}
	if err := atomicWrite(m.Paths.ServicePlist, m.servicePayload(journal.NewRelay), 0o600); err != nil {
		return fail(err)
	}
	journal.Phase = "switched"
	if err := m.writeUpgradeJournal(journal); err != nil {
		return fail(err)
	}
	journal.Phase = "restarting"
	if err := m.writeUpgradeJournal(journal); err != nil {
		return fail(err)
	}
	if err := m.restartService(ctx); err != nil {
		return fail(ErrActivationFailed)
	}
	cfg, state, err := m.loadUpgradeRuntimeState()
	if err != nil || m.VerifyUpgrade(ctx, cfg, state, journal.NewRelay, journal.NewRelayDigest) != nil {
		return fail(ErrActivationFailed)
	}
	if err := m.verifyUpgradeInvariants(journal); err != nil {
		return fail(ErrRecoveryRequired)
	}
	journal.Phase = "verified"
	if err := m.writeUpgradeJournal(journal); err != nil {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	if err := m.writeUpgradeWitness(journal); err != nil {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	if err := m.removeUpgradeJournal(); err != nil {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	return m.currentUpgradeReceipt(ctx)
}

func (m *Manager) RecoverUpgrade(ctx context.Context) (UpgradeReceipt, error) {
	lifecycle, err := lifecyclelock.AcquireWriter(ctx, m.Paths.Home, "")
	if err != nil {
		return UpgradeReceipt{}, err
	}
	defer lifecycle.Close()
	if err := m.requireStandaloneRemovalInactive(); err != nil {
		return UpgradeReceipt{}, err
	}
	if err := m.validateUpgrade(ctx); err != nil {
		return UpgradeReceipt{}, err
	}
	if m.externalUpgradeRecoveryPresent() {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	journal, err := m.readUpgradeJournal()
	if err != nil {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	if journal.Phase == "verified" {
		if err := m.completeVerifiedUpgrade(ctx, journal); err != nil {
			return UpgradeReceipt{}, ErrRecoveryRequired
		}
		return m.currentUpgradeReceipt(ctx)
	}
	if err := m.rollbackUpgrade(ctx, journal); err != nil {
		return UpgradeReceipt{}, ErrRecoveryRequired
	}
	return m.currentUpgradeReceipt(ctx)
}

func (m *Manager) currentUpgradeReceipt(ctx context.Context) (UpgradeReceipt, error) {
	inspection, err := m.InspectUpgrade(ctx)
	if err != nil {
		return UpgradeReceipt{}, err
	}
	return UpgradeReceipt{
		SchemaVersion:           inspection.SchemaVersion,
		OK:                      true,
		State:                   inspection.State,
		StateDigest:             inspection.StateDigest,
		InstalledRuntimeVersion: inspection.InstalledRuntimeVersion,
		InstalledRuntimeDigest:  inspection.InstalledRuntimeDigest,
		BundledRuntimeVersion:   inspection.BundledRuntimeVersion,
		BundledRuntimeDigest:    inspection.BundledRuntimeDigest,
		IntegrationProtocol:     inspection.IntegrationProtocol,
		RestartRequired:         inspection.RestartRequired,
	}, nil
}

func (m *Manager) validateUpgrade(ctx context.Context) error {
	if m == nil || m.VerifyUpgrade == nil {
		return ErrArtifactInvalid
	}
	return m.validate(ctx)
}

func (m *Manager) integrationArtifactPresence() (int, error) {
	paths := []string{
		m.Paths.Config,
		routing.StatePath(m.Paths.Config),
		routing.InitializedPath(m.Paths.Config),
		m.Paths.ServicePlist,
		m.Paths.Binding,
		filepath.Join(m.Paths.InstallRoot, "current"),
	}
	present := 0
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil {
			present++
		} else if !errors.Is(err, os.ErrNotExist) {
			return 0, ErrUnsafeState
		}
	}
	return present, nil
}

func (m *Manager) upgradeJournalPath() string {
	return filepath.Join(filepath.Dir(m.Paths.Binding), "runtime-upgrade-journal.json")
}

func (m *Manager) upgradeWitnessPath() string {
	return filepath.Join(filepath.Dir(m.Paths.Binding), "runtime-upgrade-witness.json")
}

func (m *Manager) bundledRoot() string {
	return filepath.Join(m.Paths.InstallRoot, "bundled")
}

func (m *Manager) upgradeRecoveryPresent() bool {
	return m.externalUpgradeRecoveryPresent() || pathExists(m.upgradeJournalPath())
}

func (m *Manager) externalUpgradeRecoveryPresent() bool {
	anchor, err := handoff.StandaloneRemovalAnchorPath(m.Paths.Home)
	if err != nil {
		return true
	}
	paths := []string{
		m.Paths.Journal,
		filepath.Join(filepath.Dir(m.Paths.Binding), "application-relocation.json"),
		routing.TransactionPath(m.Paths.Config),
		handoff.RemovalCleanupPath(m.Paths.Config),
		handoff.RemovalCleanupPath(anchor),
	}
	for _, path := range paths {
		if pathExists(path) {
			return true
		}
	}
	return false
}

func pathExists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil || !errors.Is(err, os.ErrNotExist)
}

func (m *Manager) upgradeStateDigest() (string, error) {
	base, err := m.stateDigest()
	if err != nil {
		return "", err
	}
	values := []string{base}
	for _, path := range []string{m.upgradeJournalPath(), m.upgradeWitnessPath()} {
		value, fingerprintErr := fingerprint(path)
		if fingerprintErr != nil {
			return "", fingerprintErr
		}
		values = append(values, value)
	}
	digest := sha256.Sum256([]byte(strings.Join(values, "\n")))
	return hex.EncodeToString(digest[:]), nil
}

func (m *Manager) readInstalledRuntime() (installedRuntime, error) {
	current := filepath.Join(m.Paths.InstallRoot, "current")
	info, err := os.Lstat(current)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return installedRuntime{}, ErrUpgradeIncompatible
	}
	target, err := os.Readlink(current)
	if err != nil || filepath.IsAbs(target) || filepath.Clean(target) != target || strings.Contains(target, "..") || !strings.HasPrefix(target, "bundled/") {
		return installedRuntime{}, ErrUpgradeIncompatible
	}
	directory := filepath.Join(m.Paths.InstallRoot, target)
	if !within(directory, m.bundledRoot()) {
		return installedRuntime{}, ErrUpgradeIncompatible
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		return installedRuntime{}, ErrUpgradeIncompatible
	}
	relayPath := filepath.Join(directory, "opencodex-relay")
	relayctlPath := filepath.Join(directory, "opencodex-relayctl")
	if err := validateRuntimeLeaf(relayPath); err != nil {
		return installedRuntime{}, err
	}
	if err := validateRuntimeLeaf(relayctlPath); err != nil {
		return installedRuntime{}, err
	}
	relayDigest, err := fileDigest(relayPath)
	if err != nil {
		return installedRuntime{}, err
	}
	relayctlDigest, err := fileDigest(relayctlPath)
	if err != nil {
		return installedRuntime{}, err
	}
	base := filepath.Base(directory)
	separator := strings.LastIndexByte(base, '-')
	if separator < 1 {
		return installedRuntime{}, ErrUpgradeIncompatible
	}
	version := base[:separator]
	digestSuffix := base[separator+1:]
	if _, err := release.ParseSemanticVersion(version); err != nil ||
		(len(digestSuffix) != 16 && len(digestSuffix) != 64) {
		return installedRuntime{}, ErrUpgradeIncompatible
	}
	runtimeDigest := combinedRuntimeDigest(relayDigest, relayctlDigest)
	if !strings.HasPrefix(relayDigest, digestSuffix) && !strings.HasPrefix(runtimeDigest, digestSuffix) {
		return installedRuntime{}, ErrUpgradeIncompatible
	}
	return installedRuntime{Target: target, Directory: directory, Relay: relayPath, Relayctl: relayctlPath, Version: version, RelayDigest: relayDigest, RelayctlDigest: relayctlDigest, RuntimeDigest: runtimeDigest}, nil
}

func (m *Manager) prepareUpgradeRuntime(runtimeDigest, relayDigest string) (string, string, bool, error) {
	if !isLowerSHA256(runtimeDigest) || !isLowerSHA256(relayDigest) {
		return "", "", false, ErrArtifactInvalid
	}
	if err := secureDirectory(m.bundledRoot()); err != nil {
		return "", "", false, err
	}
	finalDirectory := filepath.Join(m.bundledRoot(), safeVersionComponent(m.Version)+"-"+runtimeDigest)
	if info, err := os.Lstat(finalDirectory); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !m.runtimeDirectoryMatches(finalDirectory, runtimeDigest, relayDigest) {
			return "", "", false, ErrArtifactInvalid
		}
		return "", finalDirectory, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", "", false, ErrUnsafeState
	}
	staging, err := os.MkdirTemp(m.bundledRoot(), ".runtime-upgrade.")
	if err != nil || os.Chmod(staging, 0o700) != nil {
		return "", "", false, ErrUnsafeState
	}
	cleanup := func() { _ = removeUpgradeStaging(staging, m.bundledRoot()) }
	for _, pair := range [][2]string{{m.Paths.Relay, filepath.Join(staging, "opencodex-relay")}, {m.Paths.Relayctl, filepath.Join(staging, "opencodex-relayctl")}} {
		payload, readErr := os.ReadFile(pair[0])
		if readErr != nil || atomicWrite(pair[1], payload, 0o700) != nil {
			cleanup()
			return "", "", false, ErrArtifactInvalid
		}
	}
	if !m.runtimeDirectoryMatches(staging, runtimeDigest, relayDigest) {
		cleanup()
		return "", "", false, ErrArtifactInvalid
	}
	return staging, finalDirectory, true, nil
}

func (m *Manager) runtimeDirectoryMatches(directory, runtimeDigest, relayDigest string) bool {
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 2 {
		return false
	}
	observed := map[string]string{}
	for source, name := range map[string]string{m.Paths.Relay: "opencodex-relay", m.Paths.Relayctl: "opencodex-relayctl"} {
		expected, err := fileDigest(source)
		if err != nil {
			return false
		}
		path := filepath.Join(directory, name)
		if err := validateRuntimeLeaf(path); err != nil {
			return false
		}
		actual, err := fileDigest(path)
		if err != nil || actual != expected {
			return false
		}
		if name == "opencodex-relay" && actual != relayDigest {
			return false
		}
		observed[name] = actual
	}
	return combinedRuntimeDigest(observed["opencodex-relay"], observed["opencodex-relayctl"]) == runtimeDigest
}

func combinedRuntimeDigest(relayDigest, relayctlDigest string) string {
	digest := sha256.Sum256([]byte("relay:" + relayDigest + "\nrelayctl:" + relayctlDigest + "\n"))
	return hex.EncodeToString(digest[:])
}

func removeUpgradeStaging(path, root string) error {
	if path == "" {
		return nil
	}
	if !within(path, root) || !strings.HasPrefix(filepath.Base(path), ".runtime-upgrade.") {
		return ErrUnsafeState
	}
	entries, err := os.ReadDir(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeState
	}
	for _, entry := range entries {
		if entry.Name() != "opencodex-relay" && entry.Name() != "opencodex-relayctl" {
			return ErrUnsafeState
		}
		if err := os.Remove(filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *Manager) cleanupOrphanedUpgradeStaging() error {
	entries, err := os.ReadDir(m.bundledRoot())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return ErrUnsafeState
	}
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".runtime-upgrade.") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return ErrUnsafeState
		}
		if err := removeUpgradeStaging(filepath.Join(m.bundledRoot(), entry.Name()), m.bundledRoot()); err != nil {
			return err
		}
	}
	return syncDirectory(m.bundledRoot())
}

func within(path, root string) bool {
	clean := filepath.Clean(path)
	base := filepath.Clean(root)
	return clean != base && strings.HasPrefix(clean, base+string(os.PathSeparator))
}

func (m *Manager) captureUpgradeInvariants() ([]upgradeInvariant, uint64, routing.Phase, string, error) {
	account, err := userName()
	if err != nil {
		return nil, 0, "", "", err
	}
	paths := []string{m.Paths.Config, routing.StatePath(m.Paths.Config), routing.InitializedPath(m.Paths.Config), m.Paths.Binding, m.Paths.CodexConfig}
	result := make([]upgradeInvariant, 0, len(paths))
	for _, path := range paths {
		identity, err := inspectUpgradeInvariant(path)
		if err != nil {
			return nil, 0, "", "", err
		}
		result = append(result, identity)
	}
	store, err := routing.Open(m.Paths.Config)
	if err != nil {
		return nil, 0, "", "", err
	}
	state, legacy, err := store.Read()
	if err != nil || legacy || state.Generation == 0 || state.Phase == routing.PhaseApplying || state.Phase == routing.PhaseRecoveryRequired {
		return nil, 0, "", "", ErrRecoveryRequired
	}
	return result, state.Generation, state.Phase, account, nil
}

func inspectUpgradeInvariant(path string) (upgradeInvariant, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return upgradeInvariant{Path: path}, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 64<<20 {
		return upgradeInvariant{}, ErrUnsafeState
	}
	digest, err := fileDigest(path)
	if err != nil {
		return upgradeInvariant{}, err
	}
	return upgradeInvariant{Path: path, Present: true, Digest: digest, Mode: uint32(info.Mode().Perm()), UID: int(info.Sys().(*syscall.Stat_t).Uid)}, nil
}

func userName() (string, error) {
	account, err := os.UserHomeDir()
	if err != nil || account == "" {
		return "", ErrUnsafeState
	}
	// The credential namespace is bound to the current numeric user and home;
	// no keychain item is read or mutated by runtime upgrade.
	return strconv.Itoa(os.Getuid()) + ":" + filepath.Clean(account), nil
}

func (m *Manager) verifyUpgradeInvariants(journal runtimeUpgradeJournal) error {
	account, err := userName()
	if err != nil || account != journal.CredentialAccount {
		return ErrRecoveryRequired
	}
	for _, expected := range journal.Invariants {
		actual, err := inspectUpgradeInvariant(expected.Path)
		if err != nil || actual != expected {
			return ErrRecoveryRequired
		}
	}
	store, err := routing.Open(m.Paths.Config)
	if err != nil {
		return ErrRecoveryRequired
	}
	state, legacy, err := store.Read()
	if err != nil || legacy || state.Generation != journal.RoutingGeneration || state.Phase != journal.RoutingPhase {
		return ErrRecoveryRequired
	}
	return nil
}

func (m *Manager) writeUpgradeJournal(journal runtimeUpgradeJournal) error {
	if err := validateUpgradeJournal(m, journal); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil || len(payload) > 64<<10 {
		return ErrUnsafeState
	}
	return atomicWrite(m.upgradeJournalPath(), append(payload, '\n'), 0o600)
}

func (m *Manager) readUpgradeJournal() (runtimeUpgradeJournal, error) {
	path := m.upgradeJournalPath()
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() || info.Size() > 64<<10 {
		return runtimeUpgradeJournal{}, ErrRecoveryRequired
	}
	payload, err := os.ReadFile(path)
	if err != nil || rejectDuplicateUpgradeJSONKeys(payload) != nil {
		return runtimeUpgradeJournal{}, ErrRecoveryRequired
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	var journal runtimeUpgradeJournal
	if decoder.Decode(&journal) != nil || requireUpgradeJSONEOF(decoder) != nil || validateUpgradeJournal(m, journal) != nil {
		return runtimeUpgradeJournal{}, ErrRecoveryRequired
	}
	return journal, nil
}

func validateUpgradeJournal(m *Manager, journal runtimeUpgradeJournal) error {
	validPhase := journal.Phase == "prepared" || journal.Phase == "runtime_staged" || journal.Phase == "switched" || journal.Phase == "restarting" || journal.Phase == "verified"
	if journal.SchemaVersion != schemaVersion || !validPhase || !isLowerSHA256(journal.OriginStateDigest) || !isLowerSHA256(journal.PreviousRelayDigest) || !isLowerSHA256(journal.PreviousRuntimeDigest) || !isLowerSHA256(journal.NewRelayDigest) || !isLowerSHA256(journal.NewRuntimeDigest) || journal.NewRuntimeVersion == "" || journal.CredentialAccount == "" || journal.RoutingGeneration == 0 || len(journal.MutableFiles) != 2 || len(journal.Invariants) != 5 {
		return ErrRecoveryRequired
	}
	if _, err := release.ParseSemanticVersion(journal.NewRuntimeVersion); err != nil {
		return ErrRecoveryRequired
	}
	expectedMutable := []string{filepath.Join(m.Paths.InstallRoot, "current"), m.Paths.ServicePlist}
	for index, snapshot := range journal.MutableFiles {
		if snapshot.Path != expectedMutable[index] {
			return ErrRecoveryRequired
		}
	}
	if !validRuntimeTarget(journal.PreviousTarget) || !validRuntimeTarget(journal.NewTarget) || journal.PreviousTarget == journal.NewTarget {
		return ErrRecoveryRequired
	}
	previousDirectory := filepath.Join(m.Paths.InstallRoot, journal.PreviousTarget)
	newDirectory := filepath.Join(m.Paths.InstallRoot, journal.NewTarget)
	if journal.PreviousRelay != filepath.Join(previousDirectory, "opencodex-relay") || journal.NewRelay != filepath.Join(newDirectory, "opencodex-relay") || !within(previousDirectory, m.bundledRoot()) || !within(newDirectory, m.bundledRoot()) {
		return ErrRecoveryRequired
	}
	if journal.StagingDirectory != "" && (!within(journal.StagingDirectory, m.bundledRoot()) || !strings.HasPrefix(filepath.Base(journal.StagingDirectory), ".runtime-upgrade.")) {
		return ErrRecoveryRequired
	}
	expectedInvariantPaths := []string{m.Paths.Config, routing.StatePath(m.Paths.Config), routing.InitializedPath(m.Paths.Config), m.Paths.Binding, m.Paths.CodexConfig}
	for index, invariant := range journal.Invariants {
		if invariant.Path != expectedInvariantPaths[index] || (invariant.Present && (!isLowerSHA256(invariant.Digest) || invariant.Mode == 0)) || (!invariant.Present && (invariant.Digest != "" || invariant.Mode != 0 || invariant.UID != 0)) {
			return ErrRecoveryRequired
		}
	}
	return nil
}

func validRuntimeTarget(target string) bool {
	return target != "" && filepath.Clean(target) == target && !filepath.IsAbs(target) && !strings.Contains(target, "..") && strings.HasPrefix(target, "bundled/") && filepath.Dir(target) == "bundled"
}

func (m *Manager) switchCurrentTarget(target string) error {
	if !validRuntimeTarget(target) {
		return ErrUnsafeState
	}
	current := filepath.Join(m.Paths.InstallRoot, "current")
	temporary := current + ".runtime-upgrade"
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeState
	}
	if err := os.Symlink(target, temporary); err != nil {
		return ErrUnsafeState
	}
	if err := os.Rename(temporary, current); err != nil {
		_ = os.Remove(temporary)
		return ErrUnsafeState
	}
	return syncDirectory(filepath.Dir(current))
}

func (m *Manager) restartService(ctx context.Context) error {
	uid := strconv.Itoa(os.Getuid())
	_, _ = m.Runner.Run(ctx, "/bin/launchctl", "bootout", "gui/"+uid, m.Paths.ServicePlist)
	if _, err := m.Runner.Run(ctx, "/bin/launchctl", "bootstrap", "gui/"+uid, m.Paths.ServicePlist); err != nil {
		return err
	}
	_, err := m.Runner.Run(ctx, "/bin/launchctl", "kickstart", "-k", "gui/"+uid+"/"+m.Paths.Label)
	return err
}

func (m *Manager) loadUpgradeRuntimeState() (config.Config, routing.State, error) {
	cfg, err := config.Load(m.Paths.Config)
	if err != nil {
		return config.Config{}, routing.State{}, ErrRecoveryRequired
	}
	store, err := routing.Open(m.Paths.Config)
	if err != nil {
		return config.Config{}, routing.State{}, ErrRecoveryRequired
	}
	state, legacy, err := store.Read()
	if err != nil || legacy {
		return config.Config{}, routing.State{}, ErrRecoveryRequired
	}
	return cfg, state, nil
}

func (m *Manager) rollbackUpgrade(ctx context.Context, journal runtimeUpgradeJournal) error {
	if err := validateUpgradeJournal(m, journal); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	_, _ = m.Runner.Run(ctx, "/bin/launchctl", "bootout", "gui/"+uid, m.Paths.ServicePlist)
	for index := len(journal.MutableFiles) - 1; index >= 0; index-- {
		if err := restoreSnapshot(journal.MutableFiles[index]); err != nil {
			return err
		}
	}
	if journal.ServiceWasActive {
		if _, err := m.Runner.Run(ctx, "/bin/launchctl", "bootstrap", "gui/"+uid, m.Paths.ServicePlist); err != nil {
			return err
		}
		if _, err := m.Runner.Run(ctx, "/bin/launchctl", "kickstart", "-k", "gui/"+uid+"/"+m.Paths.Label); err != nil {
			return err
		}
		cfg, state, err := m.loadUpgradeRuntimeState()
		if err != nil || m.VerifyUpgrade(ctx, cfg, state, journal.PreviousRelay, journal.PreviousRelayDigest) != nil {
			return ErrActivationFailed
		}
	}
	if err := m.verifyUpgradeInvariants(journal); err != nil {
		return err
	}
	if err := removeUpgradeStaging(journal.StagingDirectory, m.bundledRoot()); err != nil {
		return err
	}
	if journal.RuntimeCreated {
		newDirectory := filepath.Dir(journal.NewRelay)
		if !within(newDirectory, m.bundledRoot()) {
			return ErrRecoveryRequired
		}
		for _, name := range []string{"opencodex-relay", "opencodex-relayctl"} {
			if err := os.Remove(filepath.Join(newDirectory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if err := os.Remove(newDirectory); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return m.removeUpgradeJournal()
}

func (m *Manager) completeVerifiedUpgrade(ctx context.Context, journal runtimeUpgradeJournal) error {
	current, err := os.Readlink(filepath.Join(m.Paths.InstallRoot, "current"))
	if err != nil || current != journal.NewTarget {
		return ErrRecoveryRequired
	}
	service, err := os.ReadFile(m.Paths.ServicePlist)
	if err != nil || !bytes.Equal(service, m.servicePayload(journal.NewRelay)) {
		return ErrRecoveryRequired
	}
	cfg, state, err := m.loadUpgradeRuntimeState()
	if err != nil || m.VerifyUpgrade(ctx, cfg, state, journal.NewRelay, journal.NewRelayDigest) != nil || m.verifyUpgradeInvariants(journal) != nil {
		return ErrRecoveryRequired
	}
	if err := m.writeUpgradeWitness(journal); err != nil {
		return err
	}
	return m.removeUpgradeJournal()
}

func (m *Manager) writeUpgradeWitness(journal runtimeUpgradeJournal) error {
	witness := runtimeUpgradeWitness{SchemaVersion: schemaVersion, Version: journal.NewRuntimeVersion, RuntimeDigest: journal.NewRuntimeDigest, CurrentTarget: journal.NewTarget, PreviousTarget: journal.PreviousTarget, RoutingGeneration: journal.RoutingGeneration, VerifiedAt: time.Now().UTC().Format(time.RFC3339)}
	payload, err := json.MarshalIndent(witness, "", "  ")
	if err != nil {
		return ErrRecoveryRequired
	}
	return atomicWrite(m.upgradeWitnessPath(), append(payload, '\n'), 0o600)
}

func (m *Manager) removeUpgradeJournal() error {
	if err := os.Remove(m.upgradeJournalPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(m.upgradeJournalPath()))
}

func isLowerSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

var launchctlPID = regexp.MustCompile(`(?m)^\s*pid = ([0-9]+)\s*$`)

func verifyRuntimeUpgrade(ctx context.Context, cfg config.Config, state routing.State, expectedPath, expectedDigest string) error {
	if !filepath.IsAbs(expectedPath) || !isLowerSHA256(expectedDigest) {
		return ErrActivationFailed
	}
	if err := verifyUpgradeHealthEndpoints(ctx, cfg, state); err != nil {
		return err
	}
	label := "io.github.novelkr.opencodex-relay"
	if cfg.Scope() == config.InstallationScopeLocalDevelopment {
		label += ".dev"
	}
	output, err := execCommand(ctx, "/bin/launchctl", "print", "gui/"+strconv.Itoa(os.Getuid())+"/"+label)
	if err != nil || len(output) > 64<<10 {
		return ErrActivationFailed
	}
	match := launchctlPID.FindSubmatch(output)
	if len(match) != 2 {
		return ErrActivationFailed
	}
	processPath, err := execCommand(ctx, "/bin/ps", "-p", string(match[1]), "-o", "comm=")
	if err != nil || strings.TrimSpace(string(processPath)) != expectedPath {
		return ErrActivationFailed
	}
	digest, err := fileDigest(expectedPath)
	if err != nil || digest != expectedDigest {
		return ErrActivationFailed
	}
	return nil
}

var execCommand = func(ctx context.Context, name string, args ...string) ([]byte, error) {
	return systemRunner{}.Run(ctx, name, args...)
}

func verifyUpgradeHealthEndpoints(ctx context.Context, cfg config.Config, state routing.State) error {
	deadline := time.Now().Add(12 * time.Second)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	for time.Now().Before(deadline) {
		valid := true
		for _, address := range []string{cfg.ListenAddress, cfg.Responses.Scheduler.InteractiveListenAddress} {
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/__relay/healthz", nil)
			response, err := client.Do(request)
			if err != nil || response.StatusCode != http.StatusOK {
				valid = false
				if response != nil {
					response.Body.Close()
				}
				break
			}
			var wire struct {
				Generation uint64        `json:"routing_generation"`
				Phase      routing.Phase `json:"routing_phase"`
			}
			err = json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&wire)
			response.Body.Close()
			if err != nil || wire.Generation != state.Generation || wire.Phase != state.Phase {
				valid = false
				break
			}
		}
		if valid {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(150 * time.Millisecond):
		}
	}
	return ErrActivationFailed
}

func rejectDuplicateUpgradeJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanUpgradeJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrRecoveryRequired
	}
	return nil
}

func scanUpgradeJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return ErrRecoveryRequired
			}
			if _, exists := seen[key]; exists {
				return ErrRecoveryRequired
			}
			seen[key] = struct{}{}
			if err := scanUpgradeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return ErrRecoveryRequired
		}
	case '[':
		for decoder.More() {
			if err := scanUpgradeJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return ErrRecoveryRequired
		}
	default:
		return ErrRecoveryRequired
	}
	return nil
}

func requireUpgradeJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrRecoveryRequired
	}
	return nil
}
