package containerruntime

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
)

const (
	stateFileName          = "state.json"
	journalFileName        = "active.json"
	stopJournalFileName    = "stop.json"
	maximumStateBytes      = 64 << 10
	maximumOAuthAgeSeconds = 15 * 60
)

type artifactRecord struct {
	ArtifactSummary
	ReleaseID      int64  `json:"release_id"`
	ReleaseTag     string `json:"release_tag"`
	SourceRevision string `json:"source_revision"`
}

type durableState struct {
	Schema              int             `json:"schema"`
	InstallationID      string          `json:"installation_id"`
	Status              State           `json:"status"`
	HighestSeenSequence uint64          `json:"highest_seen_sequence"`
	Staged              *artifactRecord `json:"staged,omitempty"`
	Active              *artifactRecord `json:"active,omitempty"`
	Previous            *artifactRecord `json:"previous,omitempty"`
	ActiveGeneration    uint64          `json:"active_generation"`
	PreviousGeneration  uint64          `json:"previous_generation"`
	NextGeneration      uint64          `json:"next_generation"`
	ContainerID         string          `json:"container_id,omitempty"`
	ActiveOperationID   string          `json:"active_operation_id,omitempty"`
	RoutingGeneration   uint64          `json:"routing_generation"`
}

type transactionPhase string

const (
	phasePrepared         transactionPhase = "prepared"
	phaseOldStopped       transactionPhase = "old_stopped"
	phaseNewStarted       transactionPhase = "new_started"
	phaseVerified         transactionPhase = "verified"
	phaseRecoveryRequired transactionPhase = "recovery_required"
)

type transactionJournal struct {
	Schema                    int                 `json:"schema"`
	InstallationID            string              `json:"installation_id"`
	OperationID               string              `json:"operation_id"`
	Phase                     transactionPhase    `json:"phase"`
	ExpectedStateDigest       string              `json:"expected_state_digest"`
	ExpectedRoutingGeneration uint64              `json:"expected_routing_generation"`
	OldArtifact               *artifactRecord     `json:"old_artifact,omitempty"`
	NewArtifact               artifactRecord      `json:"new_artifact"`
	OldGeneration             uint64              `json:"old_generation"`
	NewGeneration             uint64              `json:"new_generation"`
	OldContainerID            string              `json:"old_container_id,omitempty"`
	OldOperationID            string              `json:"old_operation_id,omitempty"`
	NewContainerID            string              `json:"new_container_id,omitempty"`
	ReuseGeneration           bool                `json:"reuse_generation,omitempty"`
	CleanupNewGeneration      bool                `json:"cleanup_new_generation,omitempty"`
	ObsoleteGeneration        uint64              `json:"obsolete_generation,omitempty"`
	Maintenance               *MaintenanceWitness `json:"maintenance,omitempty"`
}

type stopTransactionPhase string

const (
	stopPhasePrepared         stopTransactionPhase = "prepared"
	stopPhaseRouteStopped     stopTransactionPhase = "route_stopped"
	stopPhaseRuntimeStopped   stopTransactionPhase = "runtime_stopped"
	stopPhaseRecoveryRequired stopTransactionPhase = "recovery_required"
)

type stopTransactionJournal struct {
	Schema                    int                  `json:"schema"`
	InstallationID            string               `json:"installation_id"`
	OperationID               string               `json:"operation_id"`
	Phase                     stopTransactionPhase `json:"phase"`
	ExpectedStateDigest       string               `json:"expected_state_digest"`
	ExpectedRoutingGeneration uint64               `json:"expected_routing_generation"`
	FinalRoutingGeneration    uint64               `json:"final_routing_generation,omitempty"`
	Artifact                  artifactRecord       `json:"artifact"`
	StateGeneration           uint64               `json:"state_generation"`
	ContainerID               string               `json:"container_id"`
	ActiveOperationID         string               `json:"active_operation_id"`
}

type stateStore struct{ root string }

func newStateStore(root string) (*stateStore, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return nil, ErrUnsafeState
	}
	return &stateStore{root: root}, nil
}

func (s *stateStore) initialize() (durableState, error) {
	id, err := randomHex(32)
	if err != nil {
		return durableState{}, err
	}
	return durableState{Schema: SchemaVersion, InstallationID: id, Status: StateStopped, NextGeneration: 1}, nil
}

func (s *stateStore) load() (durableState, bool, error) {
	path := filepath.Join(s.root, stateFileName)
	data, found, err := s.readOptional(path, maximumStateBytes)
	if err != nil || !found {
		return durableState{}, found, err
	}
	var state durableState
	if err := decodeStrict(data, &state); err != nil || validateDurableState(state) != nil {
		return durableState{}, true, ErrUnsafeState
	}
	return state, true, nil
}

func (s *stateStore) save(state durableState) error {
	if err := validateDurableState(state); err != nil {
		return err
	}
	return s.writeJSON(filepath.Join(s.root, stateFileName), state)
}

func (s *stateStore) digest(state durableState) (string, error) {
	if err := validateDurableState(state); err != nil {
		return "", err
	}
	data, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (s *stateStore) loadJournal() (transactionJournal, bool, error) {
	data, found, err := s.readOptional(filepath.Join(s.root, "transactions", journalFileName), maximumStateBytes)
	if err != nil || !found {
		return transactionJournal{}, found, err
	}
	var journal transactionJournal
	if err := decodeStrict(data, &journal); err != nil || validateJournal(journal) != nil {
		return transactionJournal{}, true, ErrUnsafeState
	}
	return journal, true, nil
}

func (s *stateStore) saveJournal(journal transactionJournal) error {
	if err := validateJournal(journal); err != nil {
		return err
	}
	return s.writeJSON(filepath.Join(s.root, "transactions", journalFileName), journal)
}

func (s *stateStore) removeJournal() error {
	path := filepath.Join(s.root, "transactions", journalFileName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !safeOwnerFile(info) {
		return ErrUnsafeState
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (s *stateStore) loadStopJournal() (stopTransactionJournal, bool, error) {
	data, found, err := s.readOptional(filepath.Join(s.root, "transactions", stopJournalFileName), maximumStateBytes)
	if err != nil || !found {
		return stopTransactionJournal{}, found, err
	}
	var journal stopTransactionJournal
	if err := decodeStrict(data, &journal); err != nil || validateStopJournal(journal) != nil {
		return stopTransactionJournal{}, true, ErrUnsafeState
	}
	return journal, true, nil
}

func (s *stateStore) saveStopJournal(journal stopTransactionJournal) error {
	if err := validateStopJournal(journal); err != nil {
		return err
	}
	return s.writeJSON(filepath.Join(s.root, "transactions", stopJournalFileName), journal)
}

func (s *stateStore) removeStopJournal() error {
	path := filepath.Join(s.root, "transactions", stopJournalFileName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !safeOwnerFile(info) {
		return ErrUnsafeState
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func (s *stateStore) saveManifest(candidate runtimemanifest.Candidate) error {
	if !isSHA256(candidate.ManifestSHA256) || runtimemanifest.ManifestSHA256(candidate.ManifestBytes) != candidate.ManifestSHA256 {
		return ErrUnsafeState
	}
	directory := filepath.Join(s.root, "manifests")
	if err := s.prepareDirectory(directory); err != nil {
		return err
	}
	if err := writeOwnerFile(filepath.Join(directory, candidate.ManifestSHA256+".json"), candidate.ManifestBytes); err != nil {
		return err
	}
	return writeOwnerFile(filepath.Join(directory, candidate.ManifestSHA256+".sig"), candidate.SignatureBytes)
}

func (s *stateStore) loadManifest(manifestSHA256 string) ([]byte, []byte, error) {
	if !isSHA256(manifestSHA256) {
		return nil, nil, ErrUnsafeState
	}
	manifest, found, err := s.readOptional(filepath.Join(s.root, "manifests", manifestSHA256+".json"), runtimemanifest.MaximumManifestBytes)
	if err != nil || !found || runtimemanifest.ManifestSHA256(manifest) != manifestSHA256 {
		return nil, nil, ErrUnsafeState
	}
	signature, found, err := s.readOptional(filepath.Join(s.root, "manifests", manifestSHA256+".sig"), runtimemanifest.MaximumSignatureBytes)
	if err != nil || !found {
		return nil, nil, ErrUnsafeState
	}
	return manifest, signature, nil
}

func (s *stateStore) generationPath(generation uint64) (string, error) {
	if generation == 0 {
		return "", ErrUnsafeState
	}
	return filepath.Join(s.root, "homes", fmt.Sprintf("generation-%04d", generation)), nil
}

func (s *stateStore) createEmptyGeneration(generation uint64) (string, error) {
	path, err := s.generationPath(generation)
	if err != nil {
		return "", err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return "", ErrUnsafeState
	}
	if err := s.prepareDirectory(filepath.Dir(path)); err != nil {
		return "", err
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		return "", err
	}
	config := []byte("{\"hostname\":\"0.0.0.0\",\"port\":10100,\"oauthOpenBrowser\":false,\"codexAutoStart\":false}\n")
	if err := writeOwnerFile(filepath.Join(path, "config.json"), config); err != nil {
		return "", err
	}
	return path, nil
}

func (s *stateStore) removeGeneration(generation uint64) error {
	path, err := s.generationPath(generation)
	if err != nil {
		return err
	}
	// Do not let a lexical generation path reach an external tree through a
	// replaced homes/root directory. WalkDir and RemoveAll do not provide a
	// trusted-root guarantee on their own when an ancestor is a symlink.
	if err := s.validateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) {
		return ErrUnsafeState
	}
	// Generation directories are created only by this package below the
	// owner-only homes directory. Walk without following symlinks and refuse
	// hard-linked regular files before removing an abandoned, uncommitted
	// generation.
	err = filepath.WalkDir(path, func(candidate string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		entryInfo, statErr := entry.Info()
		extendedACL, aclErr := hasExtendedACL(candidate)
		if statErr != nil || entryInfo.Mode()&os.ModeSymlink != 0 || !ownedByCurrentUser(entryInfo) || aclErr != nil || extendedACL {
			return ErrUnsafeState
		}
		if entryInfo.Mode().IsRegular() {
			stat, ok := entryInfo.Sys().(*syscall.Stat_t)
			if !ok || stat.Nlink != 1 {
				return ErrUnsafeState
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}

func (s *stateStore) prepareRoot() error { return s.prepareDirectory(s.root) }

func (s *stateStore) prepareDirectory(path string) error {
	if !filepath.IsAbs(path) || !pathWithin(s.root, path) {
		return ErrUnsafeState
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	return s.validateDirectory(path)
}

func (s *stateStore) validateDirectory(path string) error {
	if !filepath.IsAbs(path) || !pathWithin(s.root, path) {
		return ErrUnsafeState
	}
	current := path
	for pathWithin(s.root, current) {
		info, err := os.Lstat(current)
		extendedACL, aclErr := hasExtendedACL(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(info) || aclErr != nil || extendedACL {
			return ErrUnsafeState
		}
		if current == s.root {
			break
		}
		current = filepath.Dir(current)
	}
	return nil
}

func (s *stateStore) writeJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if int64(len(data)) > maximumStateBytes {
		return ErrUnsafeState
	}
	if err := s.prepareDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return writeOwnerFile(path, data)
}

func writeOwnerFile(path string, data []byte) error {
	if !filepath.IsAbs(path) || len(data) == 0 {
		return ErrUnsafeState
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".runtime.*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	complete := false
	defer func() {
		_ = temporary.Close()
		if !complete {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && !safeOwnerFile(info) {
		return ErrUnsafeState
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	complete = true
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *stateStore) readOptional(path string, maximum int64) ([]byte, bool, error) {
	if !filepath.IsAbs(path) || !pathWithin(s.root, path) {
		return nil, false, ErrUnsafeState
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	if err != nil || !safeOwnerFile(info) || info.Size() < 2 || info.Size() > maximum {
		return nil, true, ErrUnsafeState
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, true, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(data)) > maximum {
		return nil, true, ErrUnsafeState
	}
	return data, true, nil
}

func validateDurableState(state durableState) error {
	if state.Schema != SchemaVersion || !isSHA256(state.InstallationID) || state.NextGeneration == 0 || !validState(state.Status) {
		return ErrUnsafeState
	}
	if state.Staged != nil && validateArtifact(*state.Staged) != nil || state.Active != nil && validateArtifact(*state.Active) != nil || state.Previous != nil && validateArtifact(*state.Previous) != nil {
		return ErrUnsafeState
	}
	if state.Active == nil && (state.ActiveGeneration != 0 || state.ContainerID != "" || state.ActiveOperationID != "") {
		return ErrUnsafeState
	}
	if state.Active != nil && state.ActiveGeneration == 0 {
		return ErrUnsafeState
	}
	if state.Previous == nil != (state.PreviousGeneration == 0) {
		return ErrUnsafeState
	}
	if state.Active == nil && state.Previous != nil {
		return ErrUnsafeState
	}
	if state.ActiveGeneration != 0 && state.ActiveGeneration >= state.NextGeneration ||
		state.PreviousGeneration != 0 && state.PreviousGeneration >= state.NextGeneration ||
		state.ActiveGeneration != 0 && state.ActiveGeneration == state.PreviousGeneration {
		return ErrUnsafeState
	}
	for _, record := range []*artifactRecord{state.Staged, state.Active, state.Previous} {
		if record != nil && record.ReleaseSequence > state.HighestSeenSequence {
			return ErrUnsafeState
		}
	}
	if state.Status == StateHealthy && (state.Active == nil || state.ContainerID == "") {
		return ErrUnsafeState
	}
	// A first activation can fail after the journal is durable but before an
	// active artifact exists. Recovery authority comes from that exact journal,
	// so recovery_required must remain representable without Active here.
	if state.ContainerID != "" && state.ContainerID != ContainerName {
		return ErrUnsafeState
	}
	if state.ContainerID != "" && !isSHA256(state.ActiveOperationID) || state.ContainerID == "" && state.ActiveOperationID != "" {
		return ErrUnsafeState
	}
	return nil
}

func validateArtifact(record artifactRecord) error {
	upstream, _, ok := runtimemanifest.ParseArtifactVersion(record.ArtifactVersion)
	if !ok || record.ReleaseID <= 0 || record.ReleaseTag != runtimemanifest.RuntimeReleasePrefix+record.ArtifactVersion || record.ReleaseSequence == 0 || !isSHA256(record.ManifestSHA256) || !validOCIDigest(record.IndexDigest) || !validOCIDigest(record.ARM64Digest) || record.IndexDigest == record.ARM64Digest || len(upstream) == 0 || !isLowerHex(record.SourceRevision, 40) {
		return ErrUnsafeState
	}
	return nil
}

func validateJournal(journal transactionJournal) error {
	if journal.Schema != SchemaVersion || !isSHA256(journal.InstallationID) || !isSHA256(journal.OperationID) || !isSHA256(journal.ExpectedStateDigest) || journal.ExpectedRoutingGeneration == 0 || journal.NewGeneration == 0 || validateArtifact(journal.NewArtifact) != nil {
		return ErrUnsafeState
	}
	if journal.Phase != phasePrepared && journal.Phase != phaseOldStopped && journal.Phase != phaseNewStarted && journal.Phase != phaseVerified && journal.Phase != phaseRecoveryRequired {
		return ErrUnsafeState
	}
	if journal.OldArtifact == nil != (journal.OldGeneration == 0) || journal.OldArtifact != nil && validateArtifact(*journal.OldArtifact) != nil {
		return ErrUnsafeState
	}
	if !journal.ReuseGeneration && journal.NewGeneration <= journal.OldGeneration ||
		journal.OldContainerID != "" && journal.OldContainerID != ContainerName ||
		journal.NewContainerID != "" && journal.NewContainerID != ContainerName {
		return ErrUnsafeState
	}
	if journal.ReuseGeneration && (journal.OldArtifact == nil || journal.OldContainerID != "" || journal.OldOperationID != "" ||
		journal.OldGeneration == 0 || journal.NewGeneration != journal.OldGeneration ||
		journal.NewArtifact != *journal.OldArtifact || journal.CleanupNewGeneration || journal.ObsoleteGeneration != 0 || journal.Maintenance != nil) {
		return ErrUnsafeState
	}
	if journal.CleanupNewGeneration && journal.Phase != phaseRecoveryRequired {
		return ErrUnsafeState
	}
	if journal.ObsoleteGeneration != 0 && (journal.CleanupNewGeneration || journal.OldArtifact == nil ||
		journal.OldGeneration == 0 || journal.ObsoleteGeneration >= journal.OldGeneration ||
		journal.Phase != phaseVerified && journal.Phase != phaseRecoveryRequired) {
		return ErrUnsafeState
	}
	if journal.OldContainerID != "" && !isSHA256(journal.OldOperationID) ||
		journal.OldContainerID == "" && journal.OldOperationID != "" {
		return ErrUnsafeState
	}
	if (journal.Phase == phasePrepared || journal.Phase == phaseOldStopped) && journal.NewContainerID != "" ||
		(journal.Phase == phaseNewStarted || journal.Phase == phaseVerified) && journal.NewContainerID == "" {
		return ErrUnsafeState
	}
	if journal.Maintenance != nil && validateMaintenanceWitness(*journal.Maintenance) != nil {
		return ErrUnsafeState
	}
	if journal.Maintenance != nil && (journal.OldArtifact == nil || journal.Maintenance.Intent.OperationID != journal.OperationID ||
		journal.Maintenance.Intent.InstallationID != journal.InstallationID ||
		journal.Maintenance.Intent.OldManifestSHA256 != journal.OldArtifact.ManifestSHA256 ||
		journal.Maintenance.Intent.NewManifestSHA256 != journal.NewArtifact.ManifestSHA256 ||
		journal.Maintenance.Intent.OldImageDigest != journal.OldArtifact.IndexDigest ||
		journal.Maintenance.Intent.NewImageDigest != journal.NewArtifact.IndexDigest ||
		journal.Maintenance.Intent.OldStateGeneration != journal.OldGeneration ||
		journal.Maintenance.Intent.NewStateGeneration != journal.NewGeneration) {
		return ErrUnsafeState
	}
	return nil
}

func validateStopJournal(journal stopTransactionJournal) error {
	if journal.Schema != SchemaVersion || !isSHA256(journal.InstallationID) || !isSHA256(journal.OperationID) ||
		!isSHA256(journal.ExpectedStateDigest) || journal.ExpectedRoutingGeneration == 0 ||
		validateArtifact(journal.Artifact) != nil || journal.StateGeneration == 0 ||
		journal.ContainerID != ContainerName || !isSHA256(journal.ActiveOperationID) {
		return ErrUnsafeState
	}
	if journal.Phase != stopPhasePrepared && journal.Phase != stopPhaseRouteStopped &&
		journal.Phase != stopPhaseRuntimeStopped && journal.Phase != stopPhaseRecoveryRequired {
		return ErrUnsafeState
	}
	if journal.Phase == stopPhasePrepared && journal.FinalRoutingGeneration != 0 {
		return ErrUnsafeState
	}
	// The ordinary happy path advances request/applying/final by three
	// generations. A witnessed recovery can add further exact generations (for
	// example recovery/applying/final after a lost resident acknowledgement), so
	// the stop journal records the value returned by the cross-bound routing
	// witness rather than guessing E+3. A stop always changes the backend and
	// therefore can never resolve at or below its origin generation.
	if journal.FinalRoutingGeneration != 0 && journal.FinalRoutingGeneration <= journal.ExpectedRoutingGeneration {
		return ErrUnsafeState
	}
	if journal.Phase == stopPhaseRouteStopped || journal.Phase == stopPhaseRuntimeStopped {
		if journal.FinalRoutingGeneration == 0 {
			return ErrUnsafeState
		}
	}
	return nil
}

func validateMaintenanceWitness(witness MaintenanceWitness) error {
	intent := witness.Intent
	if witness.Schema != 1 || witness.Backend != "local_apple_container" ||
		witness.OriginRoutingGeneration == 0 || witness.OriginRoutingGeneration > ^uint64(0)-2 ||
		witness.PreparedRoutingGeneration != witness.OriginRoutingGeneration+1 ||
		witness.FinalRoutingGeneration != witness.PreparedRoutingGeneration+1 ||
		!isSHA256(intent.OperationID) || !isSHA256(intent.InstallationID) ||
		!isSHA256(intent.OldManifestSHA256) || !isSHA256(intent.NewManifestSHA256) ||
		!validOCIDigest(intent.OldImageDigest) || !validOCIDigest(intent.NewImageDigest) ||
		intent.OldManifestSHA256 == intent.NewManifestSHA256 || intent.OldImageDigest == intent.NewImageDigest ||
		intent.OldStateGeneration == 0 || intent.NewStateGeneration != intent.OldStateGeneration+1 {
		return ErrUnsafeState
	}
	return nil
}

func recordFromCandidate(candidate runtimemanifest.Candidate) (artifactRecord, error) {
	arm64, ok := candidate.Manifest.ARM64Digest()
	if !ok {
		return artifactRecord{}, ErrUnsafeState
	}
	record := artifactRecord{
		ArtifactSummary: ArtifactSummary{
			ArtifactVersion: candidate.Manifest.ArtifactVersion,
			ReleaseSequence: candidate.Manifest.ReleaseSequence,
			ManifestSHA256:  candidate.ManifestSHA256,
			IndexDigest:     candidate.Manifest.Image.IndexDigest,
			ARM64Digest:     arm64,
		},
		ReleaseID:      candidate.ReleaseID,
		ReleaseTag:     candidate.Tag,
		SourceRevision: candidate.Manifest.Source.Revision,
	}
	return record, validateArtifact(record)
}

func validState(value State) bool {
	return value == StateUnavailable || value == StateStopped || value == StateStaging || value == StateHealthy || value == StateUpdating || value == StateRecoveryRequired
}

func randomHex(bytes int) (string, error) {
	value := make([]byte, bytes)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func pathWithin(root, candidate string) bool {
	root = filepath.Clean(root)
	candidate = filepath.Clean(candidate)
	relative, err := filepath.Rel(root, candidate)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func safeOwnerFile(info os.FileInfo) bool {
	return info != nil && info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 && info.Mode().Perm()&0o077 == 0 && ownedByCurrentUser(info)
}

func ownedByCurrentUser(info os.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Geteuid()
}

func isSHA256(value string) bool { return isLowerHex(value, 64) }

func validOCIDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && isSHA256(strings.TrimPrefix(value, "sha256:"))
}

func isLowerHex(value string, length int) bool {
	if len(value) != length {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func decodeStrict(data []byte, destination any) error {
	if len(data) == 0 || len(data) > maximumStateBytes {
		return ErrUnsafeState
	}
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return ErrUnsafeState
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return ErrUnsafeState
	}
	return nil
}

func scanJSON(decoder *json.Decoder) error {
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
				return ErrUnsafeState
			}
			if _, exists := seen[key]; exists {
				return ErrUnsafeState
			}
			seen[key] = struct{}{}
			if err := scanJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSON(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return ErrUnsafeState
	}
}
