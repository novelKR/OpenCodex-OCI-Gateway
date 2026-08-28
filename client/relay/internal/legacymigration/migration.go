package legacymigration

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"time"
)

const newStem = "opencodex-relay"

type Options struct {
	Home        string
	CodexConfig string
	Keychain    Keychain
}

type Result struct {
	SchemaVersion int              `json:"schema_version"`
	Operation     string           `json:"operation"`
	Status        string           `json:"status"`
	Artifacts     []ArtifactStatus `json:"artifacts"`
	Keychain      []KeychainStatus `json:"keychain"`
	Journal       string           `json:"journal,omitempty"`
}

type ArtifactStatus struct {
	Name              string `json:"name"`
	SourceExists      bool   `json:"source_exists"`
	DestinationExists bool   `json:"destination_exists"`
	State             string `json:"state"`
}

type KeychainStatus struct {
	Name        string `json:"name"`
	Legacy      bool   `json:"legacy_exists"`
	Destination bool   `json:"destination_exists"`
	State       string `json:"state"`
}

type Keychain interface {
	Inspect(oldService, newService, account string) (oldExists, newExists, equal bool, err error)
	Fingerprint(service, account string) (fingerprint string, exists bool, err error)
	Copy(oldService, newService, account, expectedSHA256 string) (created bool, err error)
	DeleteIfMatching(service, account, expectedSHA256 string) (deleted bool, err error)
}

type systemKeychain struct{}

type mapping struct {
	Name        string
	Source      string
	Destination string
	InPlace     bool
}

type journal struct {
	SchemaVersion int               `json:"schema_version"`
	Phase         string            `json:"phase"`
	Artifacts     []journalArtifact `json:"artifacts"`
	Keychain      []journalKeychain `json:"keychain"`
	UpdatedAt     string            `json:"updated_at"`
}

type journalArtifact struct {
	Name        string `json:"name"`
	Path        string `json:"path"`
	Created     bool   `json:"created,omitempty"`
	Backup      string `json:"backup,omitempty"`
	AfterSHA256 string `json:"after_sha256"`
}

type journalKeychain struct {
	Name        string `json:"name"`
	Service     string `json:"service"`
	Created     bool   `json:"created"`
	AfterSHA256 string `json:"after_sha256"`
}

func legacyStem() string {
	return strings.Join([]string{"pw", "opencodex"}, "-")
}

func Run(operation string, options Options) (Result, error) {
	if options.Home == "" || !filepath.IsAbs(options.Home) || filepath.Clean(options.Home) != options.Home {
		return Result{}, errors.New("migration home must be an absolute clean path")
	}
	if options.CodexConfig == "" {
		options.CodexConfig = filepath.Join(options.Home, ".codex", "config.toml")
	}
	if !filepath.IsAbs(options.CodexConfig) || filepath.Clean(options.CodexConfig) != options.CodexConfig {
		return Result{}, errors.New("Codex config must be an absolute clean path")
	}
	if options.Keychain == nil {
		options.Keychain = systemKeychain{}
	}
	switch operation {
	case "inspect":
		return inspect(options)
	case "apply":
		return apply(options)
	case "rollback":
		return rollback(options)
	default:
		return Result{}, fmt.Errorf("unsupported migration operation %q", operation)
	}
}

func paths(options Options) ([]mapping, string) {
	old := legacyStem()
	oldRoot := filepath.Join(options.Home, ".config", old)
	newRoot := filepath.Join(options.Home, ".config", newStem)
	oldCatalog := filepath.Join(options.Home, ".codex", old+"-catalog.json")
	newCatalog := filepath.Join(options.Home, ".codex", newStem+"-catalog.json")
	oldLocal := filepath.Join(options.Home, ".codex", old+"-local-catalog.json")
	newLocal := filepath.Join(options.Home, ".codex", newStem+"-local-catalog.json")
	mappings := []mapping{
		{Name: "relay_config", Source: filepath.Join(oldRoot, "relay.json"), Destination: filepath.Join(newRoot, "relay.json")},
		{Name: "routing_state", Source: filepath.Join(oldRoot, "relay.json.routing-state.json"), Destination: filepath.Join(newRoot, "relay.json.routing-state.json")},
		{Name: "routing_initialized", Source: filepath.Join(oldRoot, "relay.json.routing-initialized"), Destination: filepath.Join(newRoot, "relay.json.routing-initialized")},
		{Name: "catalog", Source: oldCatalog, Destination: newCatalog},
		{Name: "catalog_pending", Source: oldCatalog + ".restart-pending", Destination: newCatalog + ".restart-pending"},
		{Name: "catalog_previous", Source: oldCatalog + ".previous", Destination: newCatalog + ".previous"},
		{Name: "local_catalog", Source: oldLocal, Destination: newLocal},
		{Name: "local_catalog_pending", Source: oldLocal + ".restart-pending", Destination: newLocal + ".restart-pending"},
		{Name: "local_catalog_previous", Source: oldLocal + ".previous", Destination: newLocal + ".previous"},
		{Name: "interactive_profile", Source: filepath.Join(options.Home, ".codex", old+"-interactive.config.toml"), Destination: filepath.Join(options.Home, ".codex", newStem+"-interactive.config.toml")},
		{Name: "codex_markers", Source: options.CodexConfig, Destination: options.CodexConfig, InPlace: true},
	}
	return mappings, filepath.Join(newRoot, "legacy-pw-migration-journal.json")
}

func services() []struct{ Name, Old, New string } {
	old := legacyStem()
	return []struct{ Name, Old, New string }{
		{"cf_access_client_id", old + "-cf-access-client-id", newStem + "-cf-access-client-id"},
		{"cf_access_client_secret", old + "-cf-access-client-secret", newStem + "-cf-access-client-secret"},
		{"gateway_api_key", old + "-gateway-api-key", newStem + "-gateway-api-key"},
	}
}

func account() (string, error) {
	current, err := user.Current()
	if err != nil {
		return "", fmt.Errorf("resolve Keychain account: %w", err)
	}
	return current.Username, nil
}

func inspect(options Options) (Result, error) {
	mappings, journalPath := paths(options)
	result := Result{SchemaVersion: 1, Operation: "inspect", Status: "ready", Journal: journalPath}
	for _, item := range mappings {
		source, sourceErr := regular(item.Source)
		destination, destinationErr := regular(item.Destination)
		if sourceErr != nil || destinationErr != nil {
			return Result{}, firstError(sourceErr, destinationErr)
		}
		state := "absent"
		if source {
			state = "pending"
		}
		if item.InPlace && source {
			content, err := os.ReadFile(item.Source)
			if err != nil {
				return Result{}, err
			}
			converted, err := transformMapping(item, content)
			if err != nil {
				return Result{}, err
			}
			if bytes.Equal(content, converted) {
				state = "unchanged"
			}
		} else if source && destination {
			oldContent, err := os.ReadFile(item.Source)
			if err != nil {
				return Result{}, err
			}
			newContent, err := os.ReadFile(item.Destination)
			if err != nil {
				return Result{}, err
			}
			converted, err := transformMapping(item, oldContent)
			if err != nil {
				return Result{}, err
			}
			if bytes.Equal(converted, newContent) {
				state = "already_migrated"
			} else {
				state = "conflict"
				result.Status = "blocked"
			}
		}
		result.Artifacts = append(result.Artifacts, ArtifactStatus{
			Name: item.Name, SourceExists: source, DestinationExists: destination, State: state,
		})
	}
	accountName, err := account()
	if err != nil {
		return Result{}, err
	}
	for _, item := range services() {
		oldExists, newExists, equal, err := options.Keychain.Inspect(item.Old, item.New, accountName)
		if err != nil {
			return Result{}, err
		}
		state := "absent"
		if oldExists {
			state = "pending"
		}
		if oldExists && newExists {
			if equal {
				state = "already_migrated"
			} else {
				state = "conflict"
				result.Status = "blocked"
			}
		}
		result.Keychain = append(result.Keychain, KeychainStatus{
			Name: item.Name, Legacy: oldExists, Destination: newExists, State: state,
		})
	}
	return result, nil
}

func apply(options Options) (result Result, resultErr error) {
	preflight, err := inspect(options)
	if err != nil {
		return Result{}, err
	}
	if preflight.Status == "blocked" {
		return preflight, errors.New("migration has conflicting destination state")
	}
	mappings, journalPath := paths(options)
	if _, err := os.Lstat(journalPath); err == nil {
		return Result{}, errors.New("migration journal already exists; inspect or rollback it before applying")
	} else if !os.IsNotExist(err) {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Dir(journalPath), 0o700); err != nil {
		return Result{}, err
	}
	record := journal{SchemaVersion: 1, Phase: "applying"}
	if err := writeJournal(journalPath, &record); err != nil {
		return Result{}, err
	}
	defer func() {
		if resultErr != nil {
			record.Phase = "recovery_required"
			_ = writeJournal(journalPath, &record)
		}
	}()
	for _, item := range mappings {
		exists, err := regular(item.Source)
		if err != nil {
			return Result{}, err
		}
		if !exists {
			continue
		}
		content, err := os.ReadFile(item.Source)
		if err != nil {
			return Result{}, err
		}
		converted, err := transformMapping(item, content)
		if err != nil {
			return Result{}, err
		}
		if bytes.Equal(content, converted) && item.InPlace {
			continue
		}
		if !item.InPlace {
			destinationExists, err := regular(item.Destination)
			if err != nil {
				return Result{}, err
			}
			if destinationExists {
				destinationContent, err := os.ReadFile(item.Destination)
				if err != nil {
					return Result{}, err
				}
				if bytes.Equal(destinationContent, converted) {
					continue
				}
				return Result{}, fmt.Errorf("%s destination conflicts with legacy state", item.Name)
			}
			entry := journalArtifact{Name: item.Name, Path: item.Destination, Created: true, AfterSHA256: digest(converted)}
			record.Artifacts = append(record.Artifacts, entry)
			if err := writeJournal(journalPath, &record); err != nil {
				return Result{}, err
			}
			if err := atomicWrite(item.Destination, converted, 0o600); err != nil {
				return Result{}, err
			}
			continue
		}
		backup := filepath.Join(filepath.Dir(journalPath), "migration-backup", item.Name)
		if err := atomicWrite(backup, content, 0o600); err != nil {
			return Result{}, err
		}
		entry := journalArtifact{Name: item.Name, Path: item.Destination, Backup: backup, AfterSHA256: digest(converted)}
		record.Artifacts = append(record.Artifacts, entry)
		if err := writeJournal(journalPath, &record); err != nil {
			return Result{}, err
		}
		if err := atomicWrite(item.Destination, converted, 0o600); err != nil {
			return Result{}, err
		}
	}
	accountName, err := account()
	if err != nil {
		return Result{}, err
	}
	for _, item := range services() {
		oldExists, newExists, equal, err := options.Keychain.Inspect(item.Old, item.New, accountName)
		if err != nil {
			return Result{}, err
		}
		if !oldExists || (newExists && equal) {
			continue
		}
		if newExists {
			return Result{}, fmt.Errorf("copy Keychain item %s: destination conflicts", item.Name)
		}
		expectedSHA256, sourceExists, err := options.Keychain.Fingerprint(item.Old, accountName)
		if err != nil {
			return Result{}, fmt.Errorf("fingerprint Keychain item %s: %w", item.Name, err)
		}
		if !sourceExists || !validDigest(expectedSHA256) {
			return Result{}, fmt.Errorf("fingerprint Keychain item %s: source changed during migration", item.Name)
		}
		entry := journalKeychain{
			Name: item.Name, Service: item.New, Created: true, AfterSHA256: expectedSHA256,
		}
		record.Keychain = append(record.Keychain, entry)
		if err := writeJournal(journalPath, &record); err != nil {
			return Result{}, err
		}
		created, err := options.Keychain.Copy(item.Old, item.New, accountName, expectedSHA256)
		if err != nil {
			return Result{}, fmt.Errorf("copy Keychain item %s: %w", item.Name, err)
		}
		if !created {
			record.Keychain[len(record.Keychain)-1].Created = false
			if err := writeJournal(journalPath, &record); err != nil {
				return Result{}, err
			}
			return Result{}, fmt.Errorf("copy Keychain item %s: destination changed during migration", item.Name)
		}
	}
	record.Phase = "applied"
	if err := writeJournal(journalPath, &record); err != nil {
		return Result{}, err
	}
	result, err = inspect(options)
	if err != nil {
		return Result{}, err
	}
	result.Operation = "apply"
	result.Status = "applied"
	return result, nil
}

func rollback(options Options) (Result, error) {
	_, journalPath := paths(options)
	record, err := readJournal(journalPath)
	if err != nil {
		return Result{}, err
	}
	for _, item := range record.Artifacts {
		content, err := os.ReadFile(item.Path)
		if err != nil {
			if os.IsNotExist(err) && item.Created {
				continue
			}
			return Result{}, err
		}
		if digest(content) != item.AfterSHA256 {
			return Result{}, fmt.Errorf("refuse rollback of modified artifact %s", item.Name)
		}
		if !item.Created {
			if exists, err := regular(item.Backup); err != nil || !exists {
				return Result{}, fmt.Errorf("rollback backup for %s is unavailable", item.Name)
			}
		}
	}
	accountName, err := account()
	if err != nil {
		return Result{}, err
	}
	for _, item := range record.Keychain {
		if !item.Created {
			continue
		}
		if !validDigest(item.AfterSHA256) {
			return Result{}, fmt.Errorf("migration journal Keychain witness for %s is invalid", item.Name)
		}
		currentSHA256, exists, err := options.Keychain.Fingerprint(item.Service, accountName)
		if err != nil {
			return Result{}, fmt.Errorf("inspect rollback Keychain item %s: %w", item.Name, err)
		}
		if exists && currentSHA256 != item.AfterSHA256 {
			return Result{}, fmt.Errorf("refuse rollback of modified Keychain item %s", item.Name)
		}
	}
	for index := len(record.Keychain) - 1; index >= 0; index-- {
		item := record.Keychain[index]
		if item.Created {
			if _, err := options.Keychain.DeleteIfMatching(
				item.Service, accountName, item.AfterSHA256,
			); err != nil {
				return Result{}, fmt.Errorf("rollback Keychain item %s: %w", item.Name, err)
			}
		}
	}
	for index := len(record.Artifacts) - 1; index >= 0; index-- {
		item := record.Artifacts[index]
		content, err := os.ReadFile(item.Path)
		if err != nil {
			if os.IsNotExist(err) && item.Created {
				continue
			}
			return Result{}, err
		}
		if digest(content) != item.AfterSHA256 {
			return Result{}, fmt.Errorf("refuse rollback of modified artifact %s", item.Name)
		}
		if item.Created {
			if err := os.Remove(item.Path); err != nil {
				return Result{}, err
			}
			continue
		}
		backup, err := os.ReadFile(item.Backup)
		if err != nil {
			return Result{}, err
		}
		if err := atomicWrite(item.Path, backup, 0o600); err != nil {
			return Result{}, err
		}
	}
	record.Phase = "rolled_back"
	if err := writeJournal(journalPath, record); err != nil {
		return Result{}, err
	}
	result, err := inspect(options)
	if err != nil {
		return Result{}, err
	}
	result.Operation = "rollback"
	result.Status = "rolled_back"
	return result, nil
}

func transformMapping(item mapping, content []byte) ([]byte, error) {
	if item.Name != "codex_markers" {
		return transform(content), nil
	}
	old := legacyStem()
	oldBegin := []byte("# >>> " + old + "-relay managed begin >>>")
	oldEnd := []byte("# <<< " + old + "-relay managed end <<<")
	newBegin := []byte("# >>> " + newStem + " managed begin >>>")
	newEnd := []byte("# <<< " + newStem + " managed end <<<")
	beginCount := bytes.Count(content, oldBegin)
	endCount := bytes.Count(content, oldEnd)
	if beginCount == 0 && endCount == 0 {
		return append([]byte(nil), content...), nil
	}
	if beginCount != 1 || endCount != 1 {
		return nil, errors.New("legacy Codex marker block is incomplete or duplicated")
	}
	if bytes.Contains(content, newBegin) || bytes.Contains(content, newEnd) {
		return nil, errors.New("legacy and destination Codex marker blocks coexist")
	}
	begin := bytes.Index(content, oldBegin)
	end := bytes.Index(content, oldEnd)
	if begin >= end {
		return nil, errors.New("legacy Codex marker block ordering is invalid")
	}
	end += len(oldEnd)
	converted := make([]byte, 0, len(content)+32)
	converted = append(converted, content[:begin]...)
	converted = append(converted, transform(content[begin:end])...)
	converted = append(converted, content[end:]...)
	return converted, nil
}

func transform(content []byte) []byte {
	old := legacyStem()
	replacer := strings.NewReplacer(
		old+"-relay-dev", newStem+"-dev",
		old+"-relay", newStem,
		old+"-dev", newStem+"-dev",
		old, newStem,
	)
	return []byte(replacer.Replace(string(content)))
}

func regular(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("migration artifact must be a regular non-symlink file: %s", path)
	}
	return true, nil
}

func atomicWrite(path string, content []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".migration.")
	if err != nil {
		return err
	}
	name := temporary.Name()
	defer os.Remove(name)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func writeJournal(path string, record *journal) error {
	record.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(payload, '\n'), 0o600)
}

func readJournal(path string) (*journal, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var record journal
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&record); err != nil {
		return nil, err
	}
	if record.SchemaVersion != 1 {
		return nil, errors.New("unsupported migration journal schema")
	}
	for _, item := range record.Keychain {
		if item.Created && !validDigest(item.AfterSHA256) {
			return nil, fmt.Errorf("migration journal Keychain witness for %s is invalid", item.Name)
		}
	}
	return &record, nil
}

func digest(content []byte) string {
	value := sha256.Sum256(content)
	return hex.EncodeToString(value[:])
}

func validDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && value == strings.ToLower(value)
}

func firstError(values ...error) error {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func (systemKeychain) Inspect(oldService, newService, account string) (bool, bool, bool, error) {
	oldValue, oldExists, err := readKeychain(oldService, account)
	if err != nil {
		return false, false, false, err
	}
	newValue, newExists, err := readKeychain(newService, account)
	if err != nil {
		return false, false, false, err
	}
	equal := oldExists && newExists && subtle.ConstantTimeCompare(oldValue, newValue) == 1
	return oldExists, newExists, equal, nil
}

func (systemKeychain) Fingerprint(service, account string) (string, bool, error) {
	value, exists, err := readKeychain(service, account)
	if err != nil || !exists {
		return "", exists, err
	}
	return digest(value), true, nil
}

func (systemKeychain) Copy(oldService, newService, account, expectedSHA256 string) (bool, error) {
	oldValue, oldExists, err := readKeychain(oldService, account)
	if err != nil {
		return false, err
	}
	if !oldExists {
		return false, nil
	}
	if !validDigest(expectedSHA256) || digest(oldValue) != expectedSHA256 {
		return false, errors.New("source Keychain item changed")
	}
	newValue, newExists, err := readKeychain(newService, account)
	if err != nil {
		return false, err
	}
	if newExists {
		if subtle.ConstantTimeCompare(oldValue, newValue) != 1 {
			return false, errors.New("destination Keychain item conflicts")
		}
		return false, nil
	}
	command := exec.Command("/usr/bin/security", "add-generic-password", "-a", account, "-s", newService, "-w")
	command.Stdin = bytes.NewReader(append(oldValue, '\n'))
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return false, err
	}
	return true, nil
}

func (systemKeychain) DeleteIfMatching(service, account, expectedSHA256 string) (bool, error) {
	value, exists, err := readKeychain(service, account)
	if err != nil || !exists {
		return false, err
	}
	if !validDigest(expectedSHA256) || digest(value) != expectedSHA256 {
		return false, errors.New("destination Keychain item changed")
	}
	command := exec.Command("/usr/bin/security", "delete-generic-password", "-a", account, "-s", service)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	if err := command.Run(); err != nil {
		return false, err
	}
	return true, nil
}

func readKeychain(service, account string) ([]byte, bool, error) {
	command := exec.Command("/usr/bin/security", "find-generic-password", "-a", account, "-s", service, "-w")
	output, err := command.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) && exit.ExitCode() == 44 {
			return nil, false, nil
		}
		return nil, false, err
	}
	value := bytes.TrimSpace(output)
	if len(value) == 0 {
		return nil, false, errors.New("Keychain item is empty")
	}
	return value, true, nil
}
