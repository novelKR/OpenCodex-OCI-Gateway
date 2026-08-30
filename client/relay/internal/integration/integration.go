// Package integration installs the bundled Relay into the current user's
// launchd and routing namespaces. It never writes outside the user's home and
// never edits Codex config.toml.
package integration

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/lifecyclelock"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

const schemaVersion = 1

var (
	ErrAppLocationInvalid = errors.New("integration app location is invalid")
	ErrArtifactInvalid    = errors.New("integration bundle artifact is invalid")
	ErrStateChanged       = errors.New("integration state changed")
	ErrUnsafeState        = errors.New("integration state is unsafe")
	ErrRecoveryRequired   = errors.New("integration recovery is required")
	ErrActivationFailed   = errors.New("integration activation failed")
)

type Candidate struct {
	UpstreamBaseURL        string `json:"upstream_base_url"`
	AuthenticationProfile  string `json:"authentication_profile"`
	AllowInsecurePrivateIP bool   `json:"allow_insecure_private_ip,omitempty"`
}

type Inspection struct {
	SchemaVersion       int      `json:"schema_version"`
	State               string   `json:"state"`
	StateDigest         string   `json:"state_digest"`
	CredentialAccount   string   `json:"credential_account"`
	RequiredCredentials []string `json:"required_credentials,omitempty"`
}

type Receipt struct {
	SchemaVersion     int    `json:"schema_version"`
	OK                bool   `json:"ok"`
	State             string `json:"state"`
	ConfigDigest      string `json:"config_digest"`
	RoutingGeneration uint64 `json:"routing_generation"`
}

type Paths struct {
	Home         string
	App          string
	Relay        string
	Relayctl     string
	InstallRoot  string
	Config       string
	CodexConfig  string
	ServicePlist string
	Binding      string
	Journal      string
	LogDirectory string
	Label        string
	Scope        string
}

type CommandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type Manager struct {
	Paths          Paths
	Version        string
	Runner         CommandRunner
	ValidateBundle func(context.Context, Paths) error
	VerifyHealth   func(context.Context, config.Config, routing.State) error
}

type systemRunner struct{}

func (systemRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func NewDefault(version string) (*Manager, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil, ErrArtifactInvalid
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, ErrUnsafeState
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, ErrArtifactInvalid
	}
	executable, err = filepath.Abs(executable)
	if err != nil {
		return nil, ErrArtifactInvalid
	}
	app, err := appBundlePath(executable)
	if err != nil {
		return nil, err
	}
	name := filepath.Base(app)
	paths := Paths{Home: filepath.Clean(home), App: app, Relayctl: executable}
	switch name {
	case "OpenCodexRelay.app":
		paths.Scope = config.InstallationScopeProduction
		paths.Label = "io.github.novelkr.opencodex-relay"
		paths.InstallRoot = filepath.Join(home, ".local/lib/opencodex-relay/relay")
		paths.Config = filepath.Join(home, ".config/opencodex-relay/relay.json")
		paths.Binding = filepath.Join(home, "Library/Application Support/OpenCodexRelay/routing-binding.json")
		paths.LogDirectory = filepath.Join(home, "Library/Logs/opencodex-relay")
	case "OpenCodexRelay Dev.app":
		paths.Scope = config.InstallationScopeLocalDevelopment
		paths.Label = "io.github.novelkr.opencodex-relay.dev"
		paths.InstallRoot = filepath.Join(home, ".local/lib/opencodex-relay/relay-dev")
		paths.Config = filepath.Join(home, ".config/opencodex-relay/relay-dev/relay.json")
		paths.Binding = filepath.Join(home, "Library/Application Support/OpenCodexRelayDev/routing-binding.json")
		paths.LogDirectory = filepath.Join(home, "Library/Logs/opencodex-relay-dev")
	default:
		return nil, ErrAppLocationInvalid
	}
	paths.Relay = filepath.Join(filepath.Dir(executable), "opencodex-relay")
	paths.CodexConfig = filepath.Join(home, ".codex/config.toml")
	paths.ServicePlist = filepath.Join(home, "Library/LaunchAgents", paths.Label+".plist")
	paths.Journal = filepath.Join(filepath.Dir(paths.Binding), "integration-journal.json")
	return &Manager{
		Paths:          paths,
		Version:        version,
		Runner:         systemRunner{},
		ValidateBundle: validateBundle,
		VerifyHealth:   verifyHealth,
	}, nil
}

func appBundlePath(executable string) (string, error) {
	clean := filepath.Clean(executable)
	helpersDirectory := filepath.Dir(clean)
	libraryDirectory := filepath.Dir(helpersDirectory)
	contentsDirectory := filepath.Dir(libraryDirectory)
	app := filepath.Dir(contentsDirectory)
	if filepath.Base(helpersDirectory) != "Helpers" || filepath.Base(libraryDirectory) != "Library" ||
		filepath.Base(contentsDirectory) != "Contents" ||
		filepath.Ext(app) != ".app" {
		return "", ErrAppLocationInvalid
	}
	return app, nil
}

func (m *Manager) Inspect(ctx context.Context) (Inspection, error) {
	if err := m.validate(ctx); err != nil {
		return Inspection{}, err
	}
	digest, err := m.stateDigest()
	if err != nil {
		return Inspection{}, err
	}
	account, err := user.Current()
	if err != nil || account.Username == "" {
		return Inspection{}, ErrUnsafeState
	}
	state := "integration_required"
	if present, err := regularOwnerFile(m.Paths.Journal, 0o600); err != nil {
		return Inspection{}, err
	} else if present {
		state = "recovery_required"
	} else if m.ready(ctx) {
		state = "ready"
	}
	return Inspection{
		SchemaVersion:     schemaVersion,
		State:             state,
		StateDigest:       digest,
		CredentialAccount: account.Username,
	}, nil
}

func (m *Manager) Apply(ctx context.Context, request Candidate, expectedDigest string) (Receipt, error) {
	lifecycle, err := lifecyclelock.AcquireWriter(ctx, m.Paths.Home, "")
	if err != nil {
		return Receipt{}, ErrUnsafeState
	}
	defer lifecycle.Close()
	if err := m.requireStandaloneRemovalInactive(); err != nil {
		return Receipt{}, err
	}
	if len(expectedDigest) != 64 {
		return Receipt{}, ErrStateChanged
	}
	inspection, err := m.Inspect(ctx)
	if err != nil {
		return Receipt{}, err
	}
	if inspection.State == "recovery_required" {
		return Receipt{}, ErrRecoveryRequired
	}
	if inspection.StateDigest != expectedDigest {
		return Receipt{}, ErrStateChanged
	}
	cfg, err := m.candidateConfig(request)
	if err != nil {
		return Receipt{}, err
	}
	journal, err := m.captureJournal(expectedDigest)
	if err != nil {
		return Receipt{}, err
	}
	confirmedDigest, err := m.stateDigest()
	if err != nil || confirmedDigest != expectedDigest || journal.ServiceWasActive != m.serviceActive(ctx) {
		return Receipt{}, ErrStateChanged
	}
	if err := m.writeJournal(journal); err != nil {
		return Receipt{}, err
	}
	fail := func(cause error) (Receipt, error) {
		if rollbackErr := m.rollback(ctx, journal); rollbackErr != nil {
			return Receipt{}, fmt.Errorf("%w: %v", ErrRecoveryRequired, cause)
		}
		return Receipt{}, cause
	}
	if err := secureDirectory(filepath.Dir(m.Paths.Config)); err != nil {
		return fail(ErrUnsafeState)
	}

	runtimeRelay, runtimeRelayctl, currentTarget, runtimeCreated, err := m.prepareRuntime()
	if err != nil {
		return fail(err)
	}
	journal.RuntimeDirectory = filepath.Dir(runtimeRelay)
	journal.RuntimeCreated = runtimeCreated
	journal.CurrentTarget = currentTarget
	journal.Stage = "runtime_prepared"
	if err := m.writeJournal(journal); err != nil {
		return fail(err)
	}
	if err := config.Write(m.Paths.Config, cfg); err != nil {
		return fail(ErrUnsafeState)
	}
	controller, err := routing.NewController(
		m.Paths.Config,
		m.Paths.CodexConfig,
		routing.WithCodexConfigOwner(ownerForScope(m.Paths.Scope)),
	)
	if err != nil {
		return fail(ErrUnsafeState)
	}
	if _, err := controller.SeedNativeParked(ctx); err != nil {
		return fail(ErrUnsafeState)
	}
	state, legacy, err := controller.Store().Read()
	if err != nil || legacy || state.Phase != routing.PhaseNativeActive {
		return fail(ErrUnsafeState)
	}
	journal.Stage = "state_prepared"
	if err := m.writeJournal(journal); err != nil {
		return fail(err)
	}
	if err := m.installService(ctx, runtimeRelay); err != nil {
		return fail(ErrActivationFailed)
	}
	journal.Stage = "service_started"
	if err := m.writeJournal(journal); err != nil {
		return fail(err)
	}
	if err := m.VerifyHealth(ctx, cfg, state); err != nil {
		return fail(ErrActivationFailed)
	}
	if err := m.writeBinding(); err != nil {
		return fail(ErrUnsafeState)
	}
	if err := validateRuntimeLeaf(runtimeRelayctl); err != nil {
		return fail(ErrArtifactInvalid)
	}
	digest, err := fileDigest(m.Paths.Config)
	if err != nil {
		return fail(ErrRecoveryRequired)
	}
	if err := os.Remove(m.Paths.Journal); err != nil {
		return fail(ErrRecoveryRequired)
	}
	_ = syncDirectory(filepath.Dir(m.Paths.Journal))
	return Receipt{
		SchemaVersion:     schemaVersion,
		OK:                true,
		State:             "ready",
		ConfigDigest:      digest,
		RoutingGeneration: state.Generation,
	}, nil
}

func (m *Manager) Recover(ctx context.Context) (Receipt, error) {
	lifecycle, err := lifecyclelock.AcquireWriter(ctx, m.Paths.Home, "")
	if err != nil {
		return Receipt{}, ErrUnsafeState
	}
	defer lifecycle.Close()
	if err := m.requireStandaloneRemovalInactive(); err != nil {
		return Receipt{}, err
	}
	if err := m.validate(ctx); err != nil {
		return Receipt{}, err
	}
	journal, err := m.readJournal()
	if err != nil {
		return Receipt{}, ErrRecoveryRequired
	}
	if err := m.rollback(ctx, journal); err != nil {
		return Receipt{}, ErrRecoveryRequired
	}
	return Receipt{SchemaVersion: schemaVersion, OK: true, State: "integration_required"}, nil
}

// A retained standalone-Native removal journal is consumed only by the
// Native discovery acknowledgement path after it revalidates the fixed Codex
// boundary and package absence. Relay setup must not create a binding or
// integration journal beside that evidence, including when it is malformed.
func (m *Manager) requireStandaloneRemovalInactive() error {
	anchor, err := handoff.StandaloneRemovalAnchorPath(m.Paths.Home)
	if err != nil {
		return ErrUnsafeState
	}
	_, exists, err := handoff.ReadRemovalCleanup(anchor)
	if err != nil || exists {
		return ErrRecoveryRequired
	}
	return nil
}

func (m *Manager) candidateConfig(request Candidate) (config.Config, error) {
	normalized, err := config.NormalizeExternalGatewayURL(request.UpstreamBaseURL)
	if err != nil {
		return config.Config{}, routing.ErrGatewayInvalidAddress
	}
	credentialSource := config.CredentialsSourceKeychain
	cfg, err := config.NewDefault(normalized, credentialSource)
	if err != nil {
		return config.Config{}, ErrUnsafeState
	}
	cfg.InstallationScope = m.Paths.Scope
	if m.Paths.Scope == config.InstallationScopeLocalDevelopment {
		cfg.ListenAddress = config.LocalDevelopmentListenAddress
		cfg.Responses.Scheduler.InteractiveListenAddress = config.LocalDevelopmentInteractiveListen
	}
	cfg.Credentials.AuthenticationProfile = request.AuthenticationProfile
	cfg.Credentials.AllowInsecurePrivateIP = request.AllowInsecurePrivateIP
	catalogName := "catalog.json"
	if m.Paths.Scope == config.InstallationScopeLocalDevelopment {
		catalogName = config.LocalDevelopmentExternalCatalog
	}
	cfg.Catalog.Path = filepath.Join(filepath.Dir(m.Paths.Config), catalogName)
	if err := cfg.Validate(); err != nil {
		return config.Config{}, routing.ErrGatewayInvalidAddress
	}
	return cfg, nil
}

func ownerForScope(scope string) codexconfig.Owner {
	if scope == config.InstallationScopeLocalDevelopment {
		return codexconfig.LocalDevelopmentOwner
	}
	return codexconfig.ProductionOwner
}

func (m *Manager) validate(ctx context.Context) error {
	if m == nil || m.Runner == nil || m.ValidateBundle == nil || m.VerifyHealth == nil {
		return ErrArtifactInvalid
	}
	// Preserve the actionable application-location error before evaluating the
	// broader path boundary. Full artifact validation remains after containment
	// so it never executes tools against an out-of-bound candidate path.
	if err := validateApplicationLocation(m.Paths); err != nil {
		return err
	}
	for _, path := range []string{m.Paths.Home, m.Paths.App, m.Paths.Relay, m.Paths.Relayctl, m.Paths.InstallRoot, m.Paths.Config, m.Paths.CodexConfig, m.Paths.ServicePlist, m.Paths.Binding, m.Paths.Journal} {
		if path == "" || !filepath.IsAbs(path) || !withinHomeOrApplications(path, m.Paths.Home) {
			return ErrUnsafeState
		}
	}
	return m.ValidateBundle(ctx, m.Paths)
}

func withinHomeOrApplications(path, home string) bool {
	clean := filepath.Clean(path)
	cleanHome := filepath.Clean(home)
	return clean == cleanHome || strings.HasPrefix(clean, cleanHome+string(os.PathSeparator)) || strings.HasPrefix(clean, "/Applications/")
}

func validateBundle(ctx context.Context, paths Paths) error {
	if err := validateApplicationLocation(paths); err != nil {
		return err
	}
	privilegedHelper := filepath.Join(paths.App, "Contents/Library/HelperTools/OpenCodexRelayPrivilegedHelper")
	installer := filepath.Join(paths.App, "Contents/Library/Helpers/OpenCodexRelayHelperInstaller")
	for _, path := range []string{paths.Relay, paths.Relayctl, privilegedHelper, installer} {
		if err := validateRuntimeLeaf(path); err != nil {
			return err
		}
	}
	info := filepath.Join(paths.App, "Contents/Info.plist")
	mode, err := exec.CommandContext(ctx, "/usr/bin/plutil", "-extract", "OpenCodexRuntimeMode", "raw", "-o", "-", info).Output()
	if err != nil || strings.TrimSpace(string(mode)) != "managed" {
		return ErrArtifactInvalid
	}
	for _, path := range []string{paths.Relay, paths.Relayctl, privilegedHelper, installer, paths.App} {
		if output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", path).CombinedOutput(); err != nil || len(output) > 64<<10 {
			return ErrArtifactInvalid
		}
	}
	return nil
}

func validateApplicationLocation(paths Paths) error {
	resolved, err := filepath.EvalSymlinks(paths.App)
	if err != nil || resolved != paths.App {
		return ErrAppLocationInvalid
	}
	parent := filepath.Dir(paths.App)
	if parent != "/Applications" && parent != filepath.Join(paths.Home, "Applications") {
		return ErrAppLocationInvalid
	}
	return nil
}

func validateRuntimeLeaf(path string) error {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0o111 == 0 || info.Mode()&os.ModeSymlink != 0 {
		return ErrArtifactInvalid
	}
	uid := int(info.Sys().(*syscall.Stat_t).Uid)
	if uid != os.Getuid() && uid != 0 {
		return ErrArtifactInvalid
	}
	return nil
}

func (m *Manager) ready(ctx context.Context) bool {
	if !m.serviceActive(ctx) {
		return false
	}
	if present, err := regularOwnerFile(m.Paths.ServicePlist, 0o600); err != nil || !present {
		return false
	}
	if value, err := fingerprint(filepath.Join(m.Paths.InstallRoot, "current")); err != nil || !strings.HasPrefix(value, "link:bundled/") {
		return false
	}
	if _, err := config.Load(m.Paths.Config); err != nil {
		return false
	}
	if present, err := regularOwnerFile(m.Paths.Binding, 0o600); err != nil || !present {
		return false
	}
	store, err := routing.Open(m.Paths.Config)
	if err != nil {
		return false
	}
	state, legacy, err := store.Read()
	return err == nil && !legacy && state.ValidateForCodexConfig(m.Paths.Config, m.Paths.CodexConfig) == nil
}

type fileSnapshot struct {
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Mode    uint32 `json:"mode,omitempty"`
	Data    string `json:"data,omitempty"`
	Symlink string `json:"symlink,omitempty"`
}

type transactionJournal struct {
	Schema           int            `json:"schema"`
	Stage            string         `json:"stage"`
	OriginDigest     string         `json:"origin_digest"`
	ServiceWasActive bool           `json:"service_was_active"`
	RuntimeDirectory string         `json:"runtime_directory,omitempty"`
	RuntimeCreated   bool           `json:"runtime_created,omitempty"`
	CurrentTarget    string         `json:"current_target,omitempty"`
	Files            []fileSnapshot `json:"files"`
}

func (m *Manager) captureJournal(digest string) (transactionJournal, error) {
	files := []string{
		m.Paths.Config,
		routing.StatePath(m.Paths.Config),
		routing.InitializedPath(m.Paths.Config),
		m.Paths.ServicePlist,
		m.Paths.Binding,
		filepath.Join(m.Paths.InstallRoot, "current"),
	}
	snapshots := make([]fileSnapshot, 0, len(files))
	for _, path := range files {
		snapshot, err := snapshotFile(path)
		if err != nil {
			return transactionJournal{}, err
		}
		snapshots = append(snapshots, snapshot)
	}
	return transactionJournal{
		Schema:           schemaVersion,
		Stage:            "prepared",
		OriginDigest:     digest,
		ServiceWasActive: m.serviceActive(context.Background()),
		Files:            snapshots,
	}, nil
}

func snapshotFile(path string) (fileSnapshot, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return fileSnapshot{Path: path}, nil
	}
	if err != nil {
		return fileSnapshot{}, ErrUnsafeState
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil || filepath.IsAbs(target) || strings.Contains(target, "..") {
			return fileSnapshot{}, ErrUnsafeState
		}
		return fileSnapshot{Path: path, Present: true, Symlink: target}, nil
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		return fileSnapshot{}, ErrUnsafeState
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return fileSnapshot{}, ErrUnsafeState
	}
	return fileSnapshot{Path: path, Present: true, Mode: uint32(info.Mode().Perm()), Data: base64.StdEncoding.EncodeToString(payload)}, nil
}

func (m *Manager) writeJournal(journal transactionJournal) error {
	if err := secureDirectory(filepath.Dir(m.Paths.Journal)); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(journal, "", "  ")
	if err != nil || len(payload) > 8<<20 {
		return ErrUnsafeState
	}
	return atomicWrite(m.Paths.Journal, append(payload, '\n'), 0o600)
}

func (m *Manager) readJournal() (transactionJournal, error) {
	payload, err := os.ReadFile(m.Paths.Journal)
	if err != nil || len(payload) > 8<<20 {
		return transactionJournal{}, ErrRecoveryRequired
	}
	var journal transactionJournal
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&journal) != nil || journal.Schema != schemaVersion || journal.Stage == "" || len(journal.Files) != 6 {
		return transactionJournal{}, ErrRecoveryRequired
	}
	return journal, nil
}

func (m *Manager) prepareRuntime() (string, string, string, bool, error) {
	relayDigest, err := fileDigest(m.Paths.Relay)
	if err != nil {
		return "", "", "", false, ErrArtifactInvalid
	}
	targetName := safeVersionComponent(m.Version) + "-" + relayDigest[:16]
	target := filepath.Join(m.Paths.InstallRoot, "bundled", targetName)
	_, statErr := os.Lstat(target)
	targetCreated := errors.Is(statErr, os.ErrNotExist)
	if statErr != nil && !targetCreated {
		return "", "", "", false, ErrUnsafeState
	}
	if err := secureDirectory(target); err != nil {
		return "", "", "", false, err
	}
	runtimeRelay := filepath.Join(target, "opencodex-relay")
	runtimeRelayctl := filepath.Join(target, "opencodex-relayctl")
	cleanupCreatedTarget := func() {
		if targetCreated {
			_ = os.Remove(runtimeRelay)
			_ = os.Remove(runtimeRelayctl)
			_ = os.Remove(target)
		}
	}
	for source, destination := range map[string]string{m.Paths.Relay: runtimeRelay, m.Paths.Relayctl: runtimeRelayctl} {
		if existing, err := fileDigest(destination); err == nil {
			sourceDigest, _ := fileDigest(source)
			if existing != sourceDigest {
				cleanupCreatedTarget()
				return "", "", "", false, ErrArtifactInvalid
			}
			continue
		}
		payload, err := os.ReadFile(source)
		if err != nil || atomicWrite(destination, payload, 0o700) != nil {
			cleanupCreatedTarget()
			return "", "", "", false, ErrArtifactInvalid
		}
	}
	current := filepath.Join(m.Paths.InstallRoot, "current")
	previous := ""
	if info, err := os.Lstat(current); err == nil {
		if info.Mode()&os.ModeSymlink == 0 {
			cleanupCreatedTarget()
			return "", "", "", false, ErrUnsafeState
		}
		previous, err = os.Readlink(current)
		if err != nil || filepath.IsAbs(previous) || strings.Contains(previous, "..") {
			cleanupCreatedTarget()
			return "", "", "", false, ErrUnsafeState
		}
	}
	relativeTarget := filepath.Join("bundled", targetName)
	temporary := current + ".integration"
	_ = os.Remove(temporary)
	if err := os.Symlink(relativeTarget, temporary); err != nil {
		cleanupCreatedTarget()
		return "", "", "", false, ErrUnsafeState
	}
	if err := os.Rename(temporary, current); err != nil {
		_ = os.Remove(temporary)
		cleanupCreatedTarget()
		return "", "", "", false, ErrUnsafeState
	}
	return runtimeRelay, runtimeRelayctl, previous, targetCreated, nil
}

func safeVersionComponent(version string) string {
	if version == "" || len(version) > 64 {
		return "dev"
	}
	for _, value := range version {
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') ||
			(value >= '0' && value <= '9') || value == '.' || value == '-' || value == '_' {
			continue
		}
		return "dev"
	}
	return version
}

func (m *Manager) installService(ctx context.Context, relay string) error {
	if err := secureDirectory(filepath.Dir(m.Paths.ServicePlist)); err != nil {
		return err
	}
	if err := secureDirectory(m.Paths.LogDirectory); err != nil {
		return err
	}
	payload := []byte(fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>Label</key><string>%s</string>
<key>ProgramArguments</key><array><string>%s</string><string>--config</string><string>%s</string></array>
<key>RunAtLoad</key><true/><key>KeepAlive</key><true/><key>ProcessType</key><string>Background</string>
<key>StandardOutPath</key><string>%s</string><key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, html.EscapeString(m.Paths.Label), html.EscapeString(relay), html.EscapeString(m.Paths.Config), html.EscapeString(filepath.Join(m.Paths.LogDirectory, "relay.log")), html.EscapeString(filepath.Join(m.Paths.LogDirectory, "relay-error.log"))))
	if err := atomicWrite(m.Paths.ServicePlist, payload, 0o600); err != nil {
		return err
	}
	uid := strconv.Itoa(os.Getuid())
	_, _ = m.Runner.Run(ctx, "/bin/launchctl", "bootout", "gui/"+uid, m.Paths.ServicePlist)
	if _, err := m.Runner.Run(ctx, "/bin/launchctl", "bootstrap", "gui/"+uid, m.Paths.ServicePlist); err != nil {
		return err
	}
	_, err := m.Runner.Run(ctx, "/bin/launchctl", "kickstart", "-k", "gui/"+uid+"/"+m.Paths.Label)
	return err
}

func (m *Manager) writeBinding() error {
	if err := secureDirectory(filepath.Dir(m.Paths.Binding)); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(map[string]any{
		"schema":       1,
		"relay_config": m.Paths.Config,
		"codex_config": m.Paths.CodexConfig,
	}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(m.Paths.Binding, append(payload, '\n'), 0o600)
}

func verifyHealth(ctx context.Context, cfg config.Config, state routing.State) error {
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
			if err != nil || wire.Generation != state.Generation || wire.Phase != routing.PhaseNativeActive {
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

func (m *Manager) rollback(ctx context.Context, journal transactionJournal) error {
	uid := strconv.Itoa(os.Getuid())
	_, _ = m.Runner.Run(ctx, "/bin/launchctl", "bootout", "gui/"+uid, m.Paths.ServicePlist)
	for index := len(journal.Files) - 1; index >= 0; index-- {
		if err := restoreSnapshot(journal.Files[index]); err != nil {
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
	}
	if journal.RuntimeCreated && journal.RuntimeDirectory != "" && strings.HasPrefix(journal.RuntimeDirectory, filepath.Join(m.Paths.InstallRoot, "bundled")+string(os.PathSeparator)) {
		_ = os.Remove(filepath.Join(journal.RuntimeDirectory, "opencodex-relay"))
		_ = os.Remove(filepath.Join(journal.RuntimeDirectory, "opencodex-relayctl"))
		_ = os.Remove(journal.RuntimeDirectory)
	}
	if err := os.Remove(m.Paths.Journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func restoreSnapshot(snapshot fileSnapshot) error {
	if !snapshot.Present {
		if err := os.Remove(snapshot.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if snapshot.Symlink != "" {
		_ = os.Remove(snapshot.Path)
		return os.Symlink(snapshot.Symlink, snapshot.Path)
	}
	payload, err := base64.StdEncoding.DecodeString(snapshot.Data)
	if err != nil {
		return ErrRecoveryRequired
	}
	return atomicWrite(snapshot.Path, payload, os.FileMode(snapshot.Mode))
}

func (m *Manager) serviceActive(ctx context.Context) bool {
	uid := strconv.Itoa(os.Getuid())
	_, err := m.Runner.Run(ctx, "/bin/launchctl", "print", "gui/"+uid+"/"+m.Paths.Label)
	return err == nil
}

func (m *Manager) stateDigest() (string, error) {
	values := []string{m.Paths.Scope, strconv.FormatBool(m.serviceActive(context.Background()))}
	for _, path := range []string{
		m.Paths.Relay,
		m.Paths.Relayctl,
		filepath.Join(m.Paths.App, "Contents/Info.plist"),
		filepath.Join(m.Paths.App, "Contents/Library/HelperTools/OpenCodexRelayPrivilegedHelper"),
		filepath.Join(m.Paths.App, "Contents/Library/Helpers/OpenCodexRelayHelperInstaller"),
		m.Paths.Config,
		routing.StatePath(m.Paths.Config),
		routing.InitializedPath(m.Paths.Config),
		m.Paths.ServicePlist,
		m.Paths.Binding,
		filepath.Join(m.Paths.InstallRoot, "current"),
		m.Paths.Journal,
	} {
		value, err := fingerprint(path)
		if err != nil {
			return "", err
		}
		values = append(values, value)
	}
	digest := sha256.Sum256([]byte(strings.Join(values, "\n")))
	return hex.EncodeToString(digest[:]), nil
}

func fingerprint(path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "absent", nil
	}
	if err != nil {
		return "", ErrUnsafeState
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil || filepath.IsAbs(target) || strings.Contains(target, "..") {
			return "", ErrUnsafeState
		}
		return "link:" + target, nil
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return "", ErrUnsafeState
	}
	return fileDigest(path)
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return "", ErrUnsafeState
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func regularOwnerFile(path string, mode os.FileMode) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		return false, ErrUnsafeState
	}
	return true, nil
}

func secureDirectory(path string) error {
	clean := filepath.Clean(path)
	if !filepath.IsAbs(clean) || clean == string(os.PathSeparator) {
		return ErrUnsafeState
	}
	current := string(os.PathSeparator)
	for _, component := range strings.Split(strings.TrimPrefix(clean, string(os.PathSeparator)), string(os.PathSeparator)) {
		if component == "" || component == "." || component == ".." {
			return ErrUnsafeState
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return ErrUnsafeState
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
	}
	info, err := os.Lstat(clean)
	if err != nil || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid() {
		return ErrUnsafeState
	}
	// Existing user directories such as ~/Library/LaunchAgents keep their
	// normal mode. Refuse writable-by-others directories instead of silently
	// changing unrelated directory metadata during integration.
	if info.Mode().Perm()&0o022 != 0 {
		return ErrUnsafeState
	}
	return nil
}

func atomicWrite(path string, payload []byte, mode os.FileMode) error {
	if err := secureDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Getuid()) {
		return ErrUnsafeState
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return ErrUnsafeState
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".integration.")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(payload); err != nil || file.Sync() != nil || file.Close() != nil {
		return ErrUnsafeState
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
