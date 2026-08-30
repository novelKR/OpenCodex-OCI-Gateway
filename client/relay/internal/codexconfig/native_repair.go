package codexconfig

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const openCodexSectionMarker = "# Auto-injected by opencodex"

// NativeRepairKind is a bounded ownership projection for a local-development
// recovery UI. It never carries a URL, catalog path, provider value, marker
// body, or user configuration text.
type NativeRepairKind string

const (
	NativeRepairStateOnly   NativeRepairKind = "state_only"
	NativeRepairLocalRelay  NativeRepairKind = "local_relay"
	NativeRepairOpenCodex   NativeRepairKind = "opencodex"
	NativeRepairUnavailable NativeRepairKind = "unavailable"
)

// NativeRepairInspection describes only whether the two root routing
// assignments are present and which compiled-in owner can safely remove them.
// The private witnesses let one helper process revalidate the exact files at
// the irreversible boundary without exposing their contents in JSON or logs.
type NativeRepairInspection struct {
	Kind               NativeRepairKind
	OpenAIBaseURL      bool
	ModelCatalog       bool
	Reason             string
	mainFingerprint    [sha256.Size]byte
	profileFingerprint [sha256.Size]byte
	profileExists      bool
}

// InspectNativeRepairForOwner classifies only current-owner Relay artifacts,
// OpenCodex's documented marker/catalog/provider forms, or a clean native
// configuration. Mixed, foreign, malformed, and arbitrary user overrides stay
// unavailable. The caller may display only Kind, the two booleans, and Reason.
func InspectNativeRepairForOwner(path string, owner Owner) (NativeRepairInspection, error) {
	if err := validateOwner(owner); err != nil {
		return NativeRepairInspection{}, err
	}
	raw, exists, err := snapshotOptional(path)
	if err != nil {
		return NativeRepairInspection{}, err
	}
	content := strings.ReplaceAll(raw, "\r\n", "\n")
	result := NativeRepairInspection{
		OpenAIBaseURL:   rootKeyExists(content, "openai_base_url"),
		ModelCatalog:    rootKeyExists(content, "model_catalog_json"),
		mainFingerprint: sha256.Sum256([]byte(raw)),
	}
	if !exists {
		result.mainFingerprint = sha256.Sum256(nil)
	}

	profilePath := InteractiveProfilePathForOwner(path, owner)
	profile, profileExists, err := snapshotOptional(profilePath)
	if err != nil {
		return NativeRepairInspection{}, err
	}
	result.profileExists = profileExists
	result.profileFingerprint = sha256.Sum256([]byte(profile))

	currentStart := strings.Contains(content, owner.BeginMarker)
	currentEnd := strings.Contains(content, owner.EndMarker)
	if currentStart != currentEnd {
		return unavailableNativeRepair(result, "relay_marker_incomplete"), nil
	}
	for _, other := range knownOwners() {
		if other.ID == owner.ID {
			continue
		}
		if strings.Contains(content, other.BeginMarker) || strings.Contains(content, other.EndMarker) {
			return unavailableNativeRepair(result, "foreign_relay_owner"), nil
		}
		_, foreignProfileExists, profileErr := snapshotOptional(InteractiveProfilePathForOwner(path, other))
		if profileErr != nil {
			return NativeRepairInspection{}, profileErr
		}
		if foreignProfileExists {
			return unavailableNativeRepair(result, "foreign_relay_owner"), nil
		}
	}

	withoutCurrent := content
	if currentStart {
		withoutCurrent, err = removeManagedForOwner(content, owner)
		if err != nil {
			return unavailableNativeRepair(result, "relay_marker_incomplete"), nil
		}
	}
	routing, inspectionErr := InspectRoutingForOwner(path, owner)
	if inspectionErr != nil {
		return unavailableNativeRepair(result, "managed_artifact_invalid"), nil
	}

	openCodexBase, unmanagedBase := classifyOpenCodexBaseURLs(withoutCurrent)
	openCodexCatalog, unmanagedCatalog := classifyOpenCodexCatalogs(withoutCurrent)
	openCodexLegacyProvider, unmanagedProvider := classifyOpenCodexProviders(withoutCurrent)
	openCodexOwned := openCodexBase || openCodexCatalog || openCodexLegacyProvider
	relayOwned := routing.ManagedRoot || routing.InteractiveProfileManaged

	switch {
	case routing.ForeignManagedRoot || routing.ForeignInteractiveProfile:
		return unavailableNativeRepair(result, "foreign_relay_owner"), nil
	case relayOwned && (openCodexOwned || unmanagedBase || unmanagedCatalog || unmanagedProvider):
		return unavailableNativeRepair(result, "mixed_routing_owners"), nil
	case relayOwned:
		result.Kind = NativeRepairLocalRelay
		result.Reason = "local_relay_owned"
	case openCodexOwned && (unmanagedBase || unmanagedCatalog || unmanagedProvider):
		return unavailableNativeRepair(result, "mixed_routing_owners"), nil
	case openCodexOwned:
		result.Kind = NativeRepairOpenCodex
		result.Reason = "opencodex_owned"
	case unmanagedBase || unmanagedCatalog || unmanagedProvider:
		return unavailableNativeRepair(result, "unmanaged_routing_override"), nil
	default:
		result.Kind = NativeRepairStateOnly
		result.Reason = "native_routing_clean"
	}
	return result, nil
}

func unavailableNativeRepair(result NativeRepairInspection, reason string) NativeRepairInspection {
	result.Kind = NativeRepairUnavailable
	result.Reason = reason
	return result
}

// RevalidateNativeRepairInspection proves that neither the selected Codex TOML
// nor the current owner's side profile changed after the helper's preflight.
func RevalidateNativeRepairInspection(path string, owner Owner, expected NativeRepairInspection) error {
	current, err := InspectNativeRepairForOwner(path, owner)
	if err != nil {
		return err
	}
	if current.Kind != expected.Kind ||
		current.OpenAIBaseURL != expected.OpenAIBaseURL ||
		current.ModelCatalog != expected.ModelCatalog ||
		current.Reason != expected.Reason ||
		current.mainFingerprint != expected.mainFingerprint ||
		current.profileExists != expected.profileExists ||
		current.profileFingerprint != expected.profileFingerprint {
		return errors.New("Codex routing artifacts changed after native repair inspection")
	}
	return nil
}

// NativeRepairBoundaryRevision binds a UI decision to the exact canonical
// Codex path, owner, bounded classification, and both private file witnesses.
// The revision reveals no configuration content or filesystem path.
func NativeRepairBoundaryRevision(path string, owner Owner, inspection NativeRepairInspection) (string, error) {
	if err := validateOwner(owner); err != nil {
		return "", err
	}
	path = filepath.Clean(path)
	if !filepath.IsAbs(path) || inspection.Kind == "" {
		return "", errors.New("invalid native repair boundary")
	}
	hash := sha256.New()
	for _, value := range []string{
		"opencodex-standalone-native-boundary-v1",
		path,
		owner.ID,
		string(inspection.Kind),
		strconv.FormatBool(inspection.OpenAIBaseURL),
		strconv.FormatBool(inspection.ModelCatalog),
		inspection.Reason,
		strconv.FormatBool(inspection.profileExists),
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	_, _ = hash.Write(inspection.mainFingerprint[:])
	_, _ = hash.Write(inspection.profileFingerprint[:])
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// CreateNativeRepairBackup writes one exact, owner-only backup beside the
// selected Codex TOML. The path is intentionally not returned so callers cannot
// accidentally add it to status JSON or activity logs.
func CreateNativeRepairBackup(path string) (bool, error) {
	if err := PreflightCodexConfigPath(path); err != nil {
		return false, err
	}
	payload, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	base := path + ".pre-opencodex-relay-native-repair-" + stamp
	for attempt := 0; attempt < 100; attempt++ {
		candidate := base
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%02d", base, attempt)
		}
		file, createErr := os.OpenFile(candidate, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(createErr, os.ErrExist) {
			continue
		}
		if createErr != nil {
			return false, createErr
		}
		cleanup := true
		defer func() {
			if cleanup {
				_ = os.Remove(candidate)
			}
		}()
		if err := file.Chmod(0o600); err != nil {
			_ = file.Close()
			return false, err
		}
		if _, err := file.Write(payload); err != nil {
			_ = file.Close()
			return false, err
		}
		if err := file.Sync(); err != nil {
			_ = file.Close()
			return false, err
		}
		if err := file.Close(); err != nil {
			return false, err
		}
		cleanup = false
		return true, nil
	}
	return false, errors.New("native repair backup name is unavailable")
}

func classifyOpenCodexBaseURLs(content string) (owned bool, unmanaged bool) {
	lines := rootLines(content)
	for index, line := range lines {
		if !rootKeyExists(line, "openai_base_url") {
			continue
		}
		if index > 0 && strings.TrimSpace(lines[index-1]) == openCodexSectionMarker {
			owned = true
		} else {
			unmanaged = true
		}
	}
	return owned, unmanaged
}

func classifyOpenCodexCatalogs(content string) (owned bool, unmanaged bool) {
	for _, line := range rootLines(content) {
		value, found := rootString(line, "model_catalog_json")
		if !found {
			continue
		}
		if isOpenCodexCatalogPath(value) {
			owned = true
		} else {
			unmanaged = true
		}
	}
	return owned, unmanaged
}

func classifyOpenCodexProviders(content string) (owned bool, unmanaged bool) {
	for _, line := range rootLines(content) {
		value, found := rootString(line, "model_provider")
		if !found || value == "openai" {
			continue
		}
		if isLegacyProvider(value) {
			owned = true
		} else {
			unmanaged = true
		}
	}
	return owned, unmanaged
}

func rootLines(content string) []string {
	lines := strings.Split(content, "\n")
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			return lines[:index]
		}
	}
	return lines
}

func isOpenCodexCatalogPath(value string) bool {
	if value == "" {
		return false
	}
	normalized := strings.ReplaceAll(value, "\\", "/")
	return filepath.Base(normalized) == "opencodex-catalog.json"
}
