// Package codexconfig edits only the relay-owned root block in a user Codex
// config. It refuses to overwrite a user's independently managed base URL.
package codexconfig

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	BeginMarker                = "# >>> opencodex-relay managed begin >>>"
	EndMarker                  = "# <<< opencodex-relay managed end <<<"
	InteractiveProfileFilename = "opencodex-relay-interactive.config.toml"
	InteractiveProfileMarker   = "# opencodex-relay-managed-interactive-profile-v1"
)

// Owner names one complete relay routing namespace.  Production and the
// local-only development distribution deliberately use distinct names so a
// single Codex home can never silently transfer ownership between them.
// Values are constants rather than user configuration: a caller can select a
// known namespace, but cannot inject arbitrary TOML markers or profile paths.
type Owner struct {
	ID                         string
	BeginMarker                string
	EndMarker                  string
	InteractiveProfileFilename string
	InteractiveProfileMarker   string
}

var (
	ProductionOwner = Owner{
		ID:                         "production",
		BeginMarker:                BeginMarker,
		EndMarker:                  EndMarker,
		InteractiveProfileFilename: InteractiveProfileFilename,
		InteractiveProfileMarker:   InteractiveProfileMarker,
	}
	LocalDevelopmentOwner = Owner{
		ID:                         "local_development",
		BeginMarker:                "# >>> opencodex-relay-dev managed begin >>>",
		EndMarker:                  "# <<< opencodex-relay-dev managed end <<<",
		InteractiveProfileFilename: "opencodex-relay-dev-interactive.config.toml",
		InteractiveProfileMarker:   "# opencodex-relay-dev-managed-interactive-profile-v1",
	}
)

// OwnerForID resolves only the two compiled-in namespaces.
func OwnerForID(id string) (Owner, error) {
	switch id {
	case "", ProductionOwner.ID:
		return ProductionOwner, nil
	case LocalDevelopmentOwner.ID:
		return LocalDevelopmentOwner, nil
	default:
		return Owner{}, fmt.Errorf("unsupported Codex routing owner %q", id)
	}
}

func knownOwners() []Owner { return []Owner{ProductionOwner, LocalDevelopmentOwner} }

// RoutingInspection describes only the relay-owned routing artifacts. It never
// parses or returns user-provided configuration values, which keeps status
// callers from accidentally exposing unrelated Codex settings.
type RoutingInspection struct {
	ManagedRoot               bool
	InteractiveProfileExists  bool
	InteractiveProfileManaged bool
	ForeignManagedRoot        bool
	ForeignInteractiveProfile bool
	// UnmanagedOpenAIBaseURL, UnmanagedModelCatalog, and
	// UnmanagedModelProvider deliberately carry no user supplied values. They
	// let the routing controller refuse a native switch that would otherwise
	// leave a foreign routing override in place.
	UnmanagedOpenAIBaseURL bool
	UnmanagedModelCatalog  bool
	UnmanagedModelProvider bool
}

// InspectRouting verifies the ownership and structural integrity of the
// relay-managed root block and side-session profile without changing either
// file. A malformed managed block or an unmanaged same-name profile is an
// error: callers must not infer that native routing is safe from an ambiguous
// configuration.
func InspectRouting(path string) (RoutingInspection, error) {
	return InspectRoutingForOwner(path, ProductionOwner)
}

// InspectRoutingForOwner verifies one namespace and records whether another
// known relay namespace owns the same Codex home.  A foreign owner is not
// removed, inferred, or migrated by this package.
func InspectRoutingForOwner(path string, owner Owner) (RoutingInspection, error) {
	if err := validateOwner(owner); err != nil {
		return RoutingInspection{}, err
	}
	content, err := readOptional(path)
	if err != nil {
		return RoutingInspection{}, err
	}
	inspection := RoutingInspection{}
	start := strings.Index(content, owner.BeginMarker)
	end := strings.Index(content, owner.EndMarker)
	withoutManaged := content
	if start != -1 || end != -1 {
		var err error
		withoutManaged, err = removeManagedForOwner(content, owner)
		if err != nil {
			return RoutingInspection{}, err
		}
		inspection.ManagedRoot = true
	}
	for _, other := range knownOwners() {
		if other.ID == owner.ID {
			continue
		}
		if strings.Contains(withoutManaged, other.BeginMarker) || strings.Contains(withoutManaged, other.EndMarker) {
			inspection.ForeignManagedRoot = true
		}
	}
	inspection.UnmanagedOpenAIBaseURL = rootKeyExists(withoutManaged, "openai_base_url")
	inspection.UnmanagedModelCatalog = rootKeyExists(withoutManaged, "model_catalog_json")
	if provider, found := rootString(withoutManaged, "model_provider"); found && provider != "openai" {
		inspection.UnmanagedModelProvider = true
	}

	profilePath := InteractiveProfilePathForOwner(path, owner)
	if err := preflightInteractiveProfilePath(profilePath, owner); err != nil {
		return RoutingInspection{}, err
	}
	profile, exists, err := snapshotOptional(profilePath)
	if err != nil {
		return RoutingInspection{}, err
	}
	inspection.InteractiveProfileExists = exists
	if exists {
		if !validInteractiveProfile(profile, owner) {
			return RoutingInspection{}, fmt.Errorf("existing %s is not managed by %s", owner.InteractiveProfileFilename, owner.ID)
		}
		inspection.InteractiveProfileManaged = true
	}
	for _, other := range knownOwners() {
		if other.ID == owner.ID {
			continue
		}
		otherPath := InteractiveProfilePathForOwner(path, other)
		if err := preflightInteractiveProfilePath(otherPath, other); err != nil {
			return RoutingInspection{}, err
		}
		if _, exists, err := snapshotOptional(otherPath); err != nil {
			return RoutingInspection{}, err
		} else if exists {
			inspection.ForeignInteractiveProfile = true
		}
	}
	return inspection, nil
}

func validateOwner(owner Owner) error {
	resolved, err := OwnerForID(owner.ID)
	if err != nil {
		return err
	}
	if resolved != owner {
		return errors.New("Codex routing owner is not canonical")
	}
	return nil
}

// ValidateManagedRouting proves that the two relay-owned artifacts still
// contain exactly the expected listener/catalog bindings. It returns only an
// error, never user TOML content or a discovered URL, so callers can use it
// in a fail-closed watcher without widening the status surface.
func ValidateManagedRouting(path, generalBaseURL, interactiveBaseURL, catalogPath string) error {
	return ValidateManagedRoutingForOwner(path, ProductionOwner, generalBaseURL, interactiveBaseURL, catalogPath)
}

func ValidateManagedRoutingForOwner(path string, owner Owner, generalBaseURL, interactiveBaseURL, catalogPath string) error {
	inspection, err := InspectRoutingForOwner(path, owner)
	if err != nil {
		return err
	}
	if !inspection.ManagedRoot || !inspection.InteractiveProfileManaged || inspection.ForeignManagedRoot || inspection.ForeignInteractiveProfile || inspection.UnmanagedOpenAIBaseURL || inspection.UnmanagedModelCatalog || inspection.UnmanagedModelProvider {
		return errors.New("relay-managed routing artifacts are incomplete or foreign")
	}
	content, err := readOptional(path)
	if err != nil {
		return err
	}
	start := strings.Index(content, owner.BeginMarker)
	end := strings.Index(content, owner.EndMarker)
	if start < 0 || end < start {
		return errors.New("relay-managed routing root block is missing")
	}
	end += len(owner.EndMarker)
	expectedRoot := strings.Join([]string{
		owner.BeginMarker,
		`openai_base_url = "` + escapeTOML(generalBaseURL) + `"`,
		`model_catalog_json = "` + escapeTOML(catalogPath) + `"`,
		owner.EndMarker,
	}, "\n")
	if content[start:end] != expectedRoot {
		return errors.New("relay-managed routing root block drifted")
	}
	profile, err := os.ReadFile(InteractiveProfilePathForOwner(path, owner))
	if err != nil {
		return fmt.Errorf("read relay-managed interactive profile: %w", err)
	}
	if string(profile) != renderInteractiveProfile(interactiveBaseURL, catalogPath, owner) {
		return errors.New("relay-managed interactive profile drifted")
	}
	return nil
}

// ValidateNativeRouting proves the relay no longer owns either artifact. It
// treats a foreign root override as ambiguous rather than silently declaring
// a native backend healthy.
func ValidateNativeRouting(path string) error {
	return ValidateNativeRoutingForOwner(path, ProductionOwner)
}

func ValidateNativeRoutingForOwner(path string, owner Owner) error {
	inspection, err := InspectRoutingForOwner(path, owner)
	if err != nil {
		return err
	}
	if inspection.ManagedRoot || inspection.InteractiveProfileExists || inspection.ForeignManagedRoot || inspection.ForeignInteractiveProfile || inspection.UnmanagedOpenAIBaseURL || inspection.UnmanagedModelCatalog || inspection.UnmanagedModelProvider {
		return errors.New("native Codex routing artifacts drifted or are foreign")
	}
	return nil
}

// ValidateExpectedRoutingOwnership checks only the relay ownership boundary
// after an external tool such as OpenCodex performs a user-approved handoff.
// It intentionally does not reveal a TOML value or attempt to repair either
// file: a mismatch must leave the caller parked for explicit recovery.
func ValidateExpectedRoutingOwnership(path string, relayManaged bool) error {
	return ValidateExpectedRoutingOwnershipForOwner(path, ProductionOwner, relayManaged)
}

func ValidateExpectedRoutingOwnershipForOwner(path string, owner Owner, relayManaged bool) error {
	inspection, err := InspectRoutingForOwner(path, owner)
	if err != nil {
		return err
	}
	if inspection.ForeignManagedRoot || inspection.ForeignInteractiveProfile || inspection.UnmanagedOpenAIBaseURL || inspection.UnmanagedModelCatalog || inspection.UnmanagedModelProvider {
		return errors.New("Codex routing ownership is foreign")
	}
	if relayManaged {
		if !inspection.ManagedRoot || !inspection.InteractiveProfileManaged {
			return errors.New("relay-managed Codex routing artifacts changed during handoff")
		}
		return nil
	}
	if inspection.ManagedRoot || inspection.InteractiveProfileExists || inspection.InteractiveProfileManaged {
		return errors.New("native Codex routing artifacts changed during handoff")
	}
	return nil
}

// EnableWithInteractiveProfile updates the general relay route and its
// explicit side-session profile as one local transaction. It refuses to
// overwrite a same-name profile that is not marker-owned by this relay.
func EnableWithInteractiveProfile(path, generalBaseURL, interactiveBaseURL, catalogPath string) error {
	return EnableWithInteractiveProfileForOwner(path, ProductionOwner, generalBaseURL, interactiveBaseURL, catalogPath)
}

func EnableWithInteractiveProfileForOwner(path string, owner Owner, generalBaseURL, interactiveBaseURL, catalogPath string) error {
	if err := PreflightEnableWithInteractiveProfileForOwner(path, owner); err != nil {
		return err
	}
	profilePath := InteractiveProfilePathForOwner(path, owner)
	baseSnapshot, baseExisted, err := snapshotOptional(path)
	if err != nil {
		return err
	}
	if err := EnableForOwner(path, owner, generalBaseURL, catalogPath); err != nil {
		return err
	}
	profile := renderInteractiveProfile(interactiveBaseURL, catalogPath, owner)
	if err := atomicWrite(profilePath, profile); err != nil {
		if restoreErr := restoreOptional(path, baseSnapshot, baseExisted); restoreErr != nil {
			return fmt.Errorf("write interactive profile: %v; restore Codex config: %w", err, restoreErr)
		}
		return fmt.Errorf("write interactive profile: %w", err)
	}
	return nil
}

// PreflightEnableWithInteractiveProfile checks every ownership boundary that
// EnableWithInteractiveProfile would change, without writing either file. A
// routing controller uses it before it parks the resident relay, so an
// unmanaged native override is reported before traffic admission changes.
func PreflightEnableWithInteractiveProfile(path string) error {
	return PreflightEnableWithInteractiveProfileForOwner(path, ProductionOwner)
}

func PreflightEnableWithInteractiveProfileForOwner(path string, owner Owner) error {
	if err := validateOwner(owner); err != nil {
		return err
	}
	inspection, err := InspectRoutingForOwner(path, owner)
	if err != nil {
		return err
	}
	if inspection.ForeignManagedRoot || inspection.ForeignInteractiveProfile {
		return errors.New("Codex config is owned by another relay installation")
	}
	profilePath := InteractiveProfilePathForOwner(path, owner)
	if err := preflightInteractiveProfilePath(profilePath, owner); err != nil {
		return err
	}
	profileSnapshot, profileExisted, err := snapshotOptional(profilePath)
	if err != nil {
		return err
	}
	if profileExisted && !validInteractiveProfile(profileSnapshot, owner) {
		return fmt.Errorf("existing %s is not managed by %s", owner.InteractiveProfileFilename, owner.ID)
	}
	content, err := readOptional(path)
	if err != nil {
		return err
	}
	_, err = prepareEnableForOwner(content, owner, "", "")
	return err
}

// DisableWithInteractiveProfile removes only relay-owned general and
// interactive routing. An unmanaged same-name profile is never deleted.
func DisableWithInteractiveProfile(path string) error {
	return DisableWithInteractiveProfileForOwner(path, ProductionOwner)
}

func DisableWithInteractiveProfileForOwner(path string, owner Owner) error {
	inspection, err := InspectRoutingForOwner(path, owner)
	if err != nil {
		return err
	}
	if inspection.ForeignManagedRoot || inspection.ForeignInteractiveProfile {
		return errors.New("Codex config is owned by another relay installation")
	}
	profilePath := InteractiveProfilePathForOwner(path, owner)
	if err := preflightInteractiveProfilePath(profilePath, owner); err != nil {
		return err
	}
	baseSnapshot, baseExisted, err := snapshotOptional(path)
	if err != nil {
		return err
	}
	profileSnapshot, profileExisted, err := snapshotOptional(profilePath)
	if err != nil {
		return err
	}
	if profileExisted && !validInteractiveProfile(profileSnapshot, owner) {
		return fmt.Errorf("existing %s is not managed by %s", owner.InteractiveProfileFilename, owner.ID)
	}
	if err := DisableForOwner(path, owner); err != nil {
		return err
	}
	if profileExisted {
		if err := os.Remove(profilePath); err != nil {
			if restoreErr := restoreOptional(path, baseSnapshot, baseExisted); restoreErr != nil {
				return fmt.Errorf("remove interactive profile: %v; restore Codex config: %w", err, restoreErr)
			}
			return fmt.Errorf("remove interactive profile: %w", err)
		}
	}
	return nil
}

func InteractiveProfilePath(codexConfigPath string) string {
	return InteractiveProfilePathForOwner(codexConfigPath, ProductionOwner)
}

func InteractiveProfilePathForOwner(codexConfigPath string, owner Owner) string {
	return filepath.Join(filepath.Dir(codexConfigPath), owner.InteractiveProfileFilename)
}

func renderInteractiveProfile(baseURL, catalogPath string, owner Owner) string {
	return strings.Join([]string{
		owner.InteractiveProfileMarker,
		`openai_base_url = "` + escapeTOML(baseURL) + `"`,
		`model_catalog_json = "` + escapeTOML(catalogPath) + `"`,
	}, "\n") + "\n"
}

func validInteractiveProfile(content string, owner Owner) bool {
	lines := strings.Split(strings.TrimSuffix(strings.ReplaceAll(content, "\r\n", "\n"), "\n"), "\n")
	return len(lines) == 3 && lines[0] == owner.InteractiveProfileMarker &&
		strings.HasPrefix(lines[1], `openai_base_url = "`) && strings.HasSuffix(lines[1], `"`) &&
		strings.HasPrefix(lines[2], `model_catalog_json = "`) && strings.HasSuffix(lines[2], `"`)
}

func preflightInteractiveProfilePath(path string, owner Owner) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect interactive profile: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("existing %s must be a regular non-symlink file", owner.InteractiveProfileFilename)
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("existing %s must have mode 0600", owner.InteractiveProfileFilename)
	}
	return nil
}

func snapshotOptional(path string) (string, bool, error) {
	if err := PreflightCodexConfigPath(path); err != nil {
		return "", false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

func restoreOptional(path, content string, existed bool) error {
	if existed {
		return atomicWrite(path, content)
	}
	err := os.Remove(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func Enable(path, baseURL, catalogPath string) error {
	return EnableForOwner(path, ProductionOwner, baseURL, catalogPath)
}

func EnableForOwner(path string, owner Owner, baseURL, catalogPath string) error {
	content, err := readOptional(path)
	if err != nil {
		return err
	}
	updated, err := prepareEnableForOwner(content, owner, baseURL, catalogPath)
	if err != nil {
		return err
	}
	return atomicWrite(path, updated)
}

func prepareEnable(content, baseURL, catalogPath string) (string, error) {
	return prepareEnableForOwner(content, ProductionOwner, baseURL, catalogPath)
}

func prepareEnableForOwner(content string, owner Owner, baseURL, catalogPath string) (string, error) {
	if err := validateOwner(owner); err != nil {
		return "", err
	}
	for _, other := range knownOwners() {
		if other.ID == owner.ID {
			continue
		}
		if strings.Contains(content, other.BeginMarker) || strings.Contains(content, other.EndMarker) {
			return "", errors.New("Codex config is owned by another relay installation")
		}
	}
	content, err := removeManagedForOwner(content, owner)
	if err != nil {
		return "", err
	}
	if rootKeyExists(content, "openai_base_url") || rootKeyExists(content, "model_catalog_json") {
		return "", errors.New("Codex config already contains an unmanaged openai_base_url or model_catalog_json")
	}
	if provider, found := rootString(content, "model_provider"); found && provider != "openai" {
		return "", fmt.Errorf("Codex config selects the unmanaged model_provider %q; migrate it explicitly before enabling the relay", provider)
	}
	block := []string{
		owner.BeginMarker,
		`openai_base_url = "` + escapeTOML(baseURL) + `"`,
		`model_catalog_json = "` + escapeTOML(catalogPath) + `"`,
		owner.EndMarker,
	}
	lines := strings.Split(content, "\n")
	insert := len(lines)
	for index, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), "[") {
			insert = index
			break
		}
	}
	updated := append([]string{}, lines[:insert]...)
	updated = append(updated, block...)
	updated = append(updated, lines[insert:]...)
	return strings.Join(updated, "\n"), nil
}

// MigrateLegacy removes only root assignments used by a documented prior
// OpenCodex setup. It requires an explicit caller action, accepts only known
// custom-provider names or the former local loopback base URL, preserves all
// provider tables, and writes a timestamped 0600 backup before changing the
// file. It never treats an arbitrary OpenAI base URL as legacy routing.
func MigrateLegacy(path string) (string, error) {
	content, err := readOptional(path)
	if err != nil {
		return "", err
	}
	updated, err := prepareLegacyMigration(content)
	if err != nil {
		return "", err
	}
	backup := path + ".pre-opencodex-relay-" + time.Now().UTC().Format("20060102T150405Z")
	if err := os.WriteFile(backup, []byte(content), 0o600); err != nil {
		return "", fmt.Errorf("write legacy configuration backup: %w", err)
	}
	if err := atomicWrite(path, updated); err != nil {
		return "", err
	}
	return backup, nil
}

// PreflightLegacyMigration verifies the exact documented legacy shape without
// modifying the Codex configuration or creating a backup.
func PreflightLegacyMigration(path string) error {
	content, err := readOptional(path)
	if err != nil {
		return err
	}
	_, err = prepareLegacyMigration(content)
	return err
}

// EnableWithLegacyMigrationForOwner performs the explicit legacy cleanup,
// creates its 0600 backup, then installs the marker-owned Relay profile. A
// synchronous profile-write failure restores the original Codex file.
func EnableWithLegacyMigrationForOwner(path string, owner Owner, generalBaseURL, interactiveBaseURL, catalogPath string) (string, error) {
	preflight, err := PlanLegacyMigrationWithInteractiveProfileForOwner(path, owner)
	if err != nil {
		return "", err
	}
	if !preflight.RequiresMigration {
		return "", EnableWithInteractiveProfileForOwner(
			path, owner, generalBaseURL, interactiveBaseURL, catalogPath,
		)
	}
	backup, err := MigrateLegacy(path)
	if err != nil {
		return "", err
	}
	if err := EnableWithInteractiveProfileForOwner(path, owner, generalBaseURL, interactiveBaseURL, catalogPath); err != nil {
		if restoreErr := restoreLegacyMigrationBackup(path, backup); restoreErr != nil {
			return backup, fmt.Errorf("enable migrated Relay profile: %v; restore legacy backup: %w", err, restoreErr)
		}
		return backup, err
	}
	return backup, nil
}

// LegacyMigrationPreflight distinguishes an actual recognized legacy cleanup
// from a clean native configuration that can use the ordinary managed enable.
type LegacyMigrationPreflight struct {
	RequiresMigration bool
}

// PreflightLegacyMigrationWithInteractiveProfileForOwner checks both files
// touched by the managed enable before the routing controller parks traffic.
func PreflightLegacyMigrationWithInteractiveProfileForOwner(path string, owner Owner) error {
	_, err := PlanLegacyMigrationWithInteractiveProfileForOwner(path, owner)
	return err
}

// PlanLegacyMigrationWithInteractiveProfileForOwner checks both files and
// reports whether the recognized legacy root assignments actually need a
// backup-and-migrate transaction. It never mutates either file.
func PlanLegacyMigrationWithInteractiveProfileForOwner(path string, owner Owner) (LegacyMigrationPreflight, error) {
	if err := validateOwner(owner); err != nil {
		return LegacyMigrationPreflight{}, err
	}
	if err := PreflightLegacyMigration(path); err != nil {
		// A clean native configuration needs no migration. The ordinary
		// ownership preflight remains authoritative; unknown overrides still
		// fail closed here.
		if enableErr := PreflightEnableWithInteractiveProfileForOwner(path, owner); enableErr != nil {
			return LegacyMigrationPreflight{}, enableErr
		}
		return LegacyMigrationPreflight{}, nil
	}
	profilePath := InteractiveProfilePathForOwner(path, owner)
	if err := preflightInteractiveProfilePath(profilePath, owner); err != nil {
		return LegacyMigrationPreflight{}, err
	}
	if _, existed, err := snapshotOptional(profilePath); err != nil {
		return LegacyMigrationPreflight{}, err
	} else if existed {
		return LegacyMigrationPreflight{}, fmt.Errorf("existing %s must be reviewed before legacy migration", owner.InteractiveProfileFilename)
	}
	return LegacyMigrationPreflight{RequiresMigration: true}, nil
}

func restoreLegacyMigrationBackup(path, backup string) error {
	if backup == "" || filepath.Dir(backup) != filepath.Dir(path) ||
		!strings.HasPrefix(filepath.Base(backup), filepath.Base(path)+".pre-opencodex-relay-") {
		return errors.New("legacy configuration backup is invalid")
	}
	info, err := os.Lstat(backup)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o600 {
		return errors.New("legacy configuration backup is unavailable")
	}
	payload, err := os.ReadFile(backup)
	if err != nil {
		return err
	}
	return atomicWrite(path, string(payload))
}

func prepareLegacyMigration(content string) (string, error) {
	for _, owner := range knownOwners() {
		if strings.Contains(content, owner.BeginMarker) || strings.Contains(content, owner.EndMarker) {
			return "", errors.New("Codex config is already managed by a relay installation")
		}
	}
	provider, providerFound := rootString(content, "model_provider")
	baseURL, baseURLFound := rootString(content, "openai_base_url")
	var assignments []string
	switch {
	case providerFound && isLegacyProvider(provider):
		if baseURLFound {
			return "", errors.New("Codex config combines a legacy provider with an openai_base_url; review it manually before migration")
		}
		assignments = []string{"model_provider", "model_catalog_json"}
	case (!providerFound || provider == "openai") && baseURLFound && isLegacyLoopbackBaseURL(baseURL):
		assignments = []string{"openai_base_url", "model_catalog_json"}
	default:
		return "", errors.New("Codex config does not contain a supported legacy OpenCodex provider or local loopback base URL")
	}
	return removeRootAssignments(content, assignments...), nil
}

func isLegacyProvider(provider string) bool {
	switch provider {
	case "pw_opencodex", "opencodex", "pw_opencodex_remote":
		return true
	default:
		return false
	}
}

func isLegacyLoopbackBaseURL(baseURL string) bool {
	return baseURL == "http://127.0.0.1:10100/v1" || baseURL == "http://localhost:10100/v1"
}

func Disable(path string) error {
	return DisableForOwner(path, ProductionOwner)
}

func DisableForOwner(path string, owner Owner) error {
	content, existed, err := snapshotOptional(path)
	if err != nil {
		return err
	}
	if !existed {
		return nil
	}
	updated, err := removeManagedForOwner(content, owner)
	if err != nil {
		return err
	}
	if updated == content {
		return nil
	}
	return atomicWrite(path, updated)
}

func IsEnabled(path string) (bool, error) {
	return IsEnabledForOwner(path, ProductionOwner)
}

func IsEnabledForOwner(path string, owner Owner) (bool, error) {
	content, err := readOptional(path)
	if err != nil {
		return false, err
	}
	return strings.Contains(content, owner.BeginMarker), nil
}

func readOptional(path string) (string, error) {
	if err := PreflightCodexConfigPath(path); err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read Codex config: %w", err)
	}
	return strings.ReplaceAll(string(data), "\r\n", "\n"), nil
}

func removeManaged(content string) (string, error) {
	return removeManagedForOwner(content, ProductionOwner)
}

func removeManagedForOwner(content string, owner Owner) (string, error) {
	if err := validateOwner(owner); err != nil {
		return "", err
	}
	start := strings.Index(content, owner.BeginMarker)
	end := strings.Index(content, owner.EndMarker)
	if start == -1 && end == -1 {
		return content, nil
	}
	if start == -1 || end == -1 || end < start {
		return "", errors.New("Codex config contains an incomplete opencodex-relay relay managed block")
	}
	end += len(owner.EndMarker)
	if strings.HasPrefix(content[end:], "\r\n") {
		end += 2
	} else if end < len(content) && content[end] == '\n' {
		end++
	}
	return content[:start] + content[end:], nil
}

func rootKeyExists(content, key string) bool {
	_, found := rootAssignment(content, key)
	return found
}

func rootString(content, key string) (string, bool) {
	value, found := rootAssignment(content, key)
	if !found {
		return "", false
	}
	value = strings.TrimSpace(value)
	if len(value) < 2 || value[0] != '"' || value[len(value)-1] != '"' {
		return "", true
	}
	return strings.ReplaceAll(strings.TrimSuffix(strings.TrimPrefix(value, "\""), "\""), `\"`, `"`), true
}

func rootAssignment(content, key string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			return "", false
		}
		if strings.HasPrefix(trimmed, "#") || !strings.HasPrefix(trimmed, key) {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(trimmed, key))
		if strings.HasPrefix(rest, "=") {
			return strings.TrimSpace(strings.TrimPrefix(rest, "=")), true
		}
	}
	return "", false
}

func removeRootAssignments(content string, keys ...string) string {
	remove := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		remove[key] = struct{}{}
	}
	lines := strings.Split(content, "\n")
	root := true
	result := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") {
			root = false
		}
		if root {
			for key := range remove {
				if _, found := rootAssignment(line, key); found {
					goto next
				}
			}
		}
		result = append(result, line)
	next:
	}
	return strings.Join(result, "\n")
}

func escapeTOML(value string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(value)
}

func atomicWrite(path, content string) error {
	if err := PreflightCodexConfigPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create Codex config directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".config.toml.")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.WriteString(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, path)
}

// PreflightCodexConfigPath verifies that a selected Codex configuration leaf
// is either absent or a regular, non-symlink file. It does not create, modify,
// or otherwise interpret the file. Callers that need to invoke an external
// owner (such as OpenCodex) use this narrow check before that irreversible
// action, while the relay uses it before any read or atomic replacement.
func PreflightCodexConfigPath(path string) error {
	if path == "" {
		return errors.New("Codex config path must not be empty")
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect Codex config: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return errors.New("Codex config must be a regular non-symlink file")
	}
	return nil
}
