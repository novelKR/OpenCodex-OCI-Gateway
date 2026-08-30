package integration

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

type fakeRunner struct {
	mu      sync.Mutex
	active  bool
	actions []string
}

func TestAppBundlePathUsesNestedHelpersBoundary(t *testing.T) {
	app := filepath.Join(string(os.PathSeparator), "Applications", "OpenCodexRelay.app")
	executable := filepath.Join(app, "Contents", "Library", "Helpers", "opencodex-relayctl")
	observed, err := appBundlePath(executable)
	if err != nil || observed != app {
		t.Fatalf("app bundle=%q err=%v", observed, err)
	}
	for _, invalid := range []string{
		filepath.Join(app, "Contents", "MacOS", "opencodex-relayctl"),
		filepath.Join(string(os.PathSeparator), "Applications", "opencodex-relayctl"),
	} {
		if _, err := appBundlePath(invalid); !errors.Is(err, ErrAppLocationInvalid) {
			t.Fatalf("invalid executable %q error=%v", invalid, err)
		}
	}
}

func (r *fakeRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(args) == 0 {
		return nil, errors.New("missing action")
	}
	r.actions = append(r.actions, args[0])
	switch args[0] {
	case "print":
		if !r.active {
			return nil, errors.New("not active")
		}
	case "bootout":
		r.active = false
	case "bootstrap":
		r.active = true
	}
	return nil, nil
}

func TestApplyCreatesNativeParkedUserIntegrationAndNoCodexMutation(t *testing.T) {
	manager, runner, cleanup := integrationFixture(t)
	defer cleanup()
	if err := os.MkdirAll(filepath.Dir(manager.Paths.CodexConfig), 0o700); err != nil {
		t.Fatal(err)
	}
	originalCodex := []byte("model = \"gpt-test\"\n")
	if err := os.WriteFile(manager.Paths.CodexConfig, originalCodex, 0o600); err != nil {
		t.Fatal(err)
	}

	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.State != "integration_required" || len(inspection.StateDigest) != 64 {
		t.Fatalf("inspection = %#v", inspection)
	}
	receipt, err := manager.Apply(context.Background(), Candidate{
		UpstreamBaseURL:       "https://gateway.example.test",
		AuthenticationProfile: config.RemoteAuthenticationGatewayAPIKey,
	}, inspection.StateDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.OK || receipt.State != "ready" || receipt.RoutingGeneration == 0 {
		t.Fatalf("receipt = %#v", receipt)
	}
	cfg, err := config.Load(manager.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamBaseURL != "https://gateway.example.test/v1" || cfg.Credentials.RemoteAuthenticationProfile() != config.RemoteAuthenticationGatewayAPIKey {
		t.Fatalf("config = %#v", cfg)
	}
	store, err := routing.Open(manager.Paths.Config)
	if err != nil {
		t.Fatal(err)
	}
	state, legacy, err := store.Read()
	if err != nil || legacy || state.Phase != routing.PhaseNativeActive || state.AppliedBackend != routing.BackendNone {
		t.Fatalf("state=%#v legacy=%t err=%v", state, legacy, err)
	}
	observedCodex, err := os.ReadFile(manager.Paths.CodexConfig)
	if err != nil || string(observedCodex) != string(originalCodex) {
		t.Fatalf("Codex config changed: %q err=%v", observedCodex, err)
	}
	if _, err := os.Stat(manager.Paths.Binding); err != nil {
		t.Fatalf("binding unavailable: %v", err)
	}
	if _, err := os.Stat(manager.Paths.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", err)
	}
	if !runner.active {
		t.Fatal("LaunchAgent was not activated")
	}
	ready, err := manager.Inspect(context.Background())
	if err != nil || ready.State != "ready" {
		t.Fatalf("post-apply inspection = %#v err=%v", ready, err)
	}
	runner.active = false
	stopped, err := manager.Inspect(context.Background())
	if err != nil || stopped.State != "integration_required" {
		t.Fatalf("stopped service inspection = %#v err=%v", stopped, err)
	}
}

func TestApplyAndRecoverRefuseStandaloneRemovalJournalBeforeMutation(t *testing.T) {
	manager, runner, cleanup := integrationFixture(t)
	defer cleanup()
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	anchor, err := handoff.StandaloneRemovalAnchorPath(manager.Paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(handoff.RemovalCleanupPath(anchor)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handoff.RemovalCleanupPath(anchor), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	actionsBefore := len(runner.actions)
	request := Candidate{
		UpstreamBaseURL:       "https://gateway.example.test/v1",
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}
	if _, err := manager.Apply(context.Background(), request, inspection.StateDigest); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("apply error=%v", err)
	}
	if _, err := manager.Recover(context.Background()); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("recover error=%v", err)
	}
	for _, path := range []string{manager.Paths.Config, manager.Paths.ServicePlist, manager.Paths.Binding, manager.Paths.Journal} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("standalone journal admitted integration mutation at %s: %v", filepath.Base(path), err)
		}
	}
	if len(runner.actions) != actionsBefore {
		t.Fatalf("standalone journal invoked launchctl: before=%d after=%d", actionsBefore, len(runner.actions))
	}
}

func TestCandidateConfigUsesScopeSpecificExternalCatalog(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	candidate := Candidate{
		UpstreamBaseURL:       "https://gateway.example.test",
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}

	production, err := manager.candidateConfig(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(production.Catalog.Path); got != "catalog.json" {
		t.Fatalf("production catalog basename=%q", got)
	}

	manager.Paths.Scope = config.InstallationScopeLocalDevelopment
	development, err := manager.candidateConfig(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Base(development.Catalog.Path); got != config.LocalDevelopmentExternalCatalog {
		t.Fatalf("local-development catalog basename=%q", got)
	}
	if development.ListenAddress != config.LocalDevelopmentListenAddress ||
		development.Responses.Scheduler.InteractiveListenAddress != config.LocalDevelopmentInteractiveListen {
		t.Fatalf("local-development listeners=%q/%q", development.ListenAddress, development.Responses.Scheduler.InteractiveListenAddress)
	}
}

func TestApplyPrivateHTTPWithoutRelayCredentialsRequiresAcknowledgementBeforeMutation(t *testing.T) {
	manager, runner, cleanup := integrationFixture(t)
	defer cleanup()
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	actionsBeforeApply := len(runner.actions)
	candidate := Candidate{
		UpstreamBaseURL:       "http://192.168.1.50/v1",
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}
	if _, err := manager.Apply(context.Background(), candidate, inspection.StateDigest); !errors.Is(err, routing.ErrGatewayInvalidAddress) {
		t.Fatalf("unacknowledged private HTTP error=%v", err)
	}
	for _, path := range []string{manager.Paths.Config, manager.Paths.ServicePlist, manager.Paths.Binding, manager.Paths.Journal} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unacknowledged private HTTP changed %s: %v", filepath.Base(path), err)
		}
	}
	for _, action := range runner.actions[actionsBeforeApply:] {
		if action != "print" {
			t.Fatalf("unacknowledged private HTTP invoked mutating launchctl action=%q", action)
		}
	}

	candidate.AllowInsecurePrivateIP = true
	receipt, err := manager.Apply(context.Background(), candidate, inspection.StateDigest)
	if err != nil || !receipt.OK || receipt.State != "ready" {
		t.Fatalf("acknowledged private HTTP receipt=%#v err=%v", receipt, err)
	}
}

func TestInspectionDigestBindsNestedBundleArtifacts(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	before, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	installer := filepath.Join(manager.Paths.App, "Contents/Library/Helpers/OpenCodexRelayHelperInstaller")
	if err := os.WriteFile(installer, []byte("changed-installer"), 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if before.StateDigest == after.StateDigest {
		t.Fatal("nested installer change did not invalidate integration state digest")
	}
}

func TestApplyFailureRollsBackEveryConsumerFacingArtifact(t *testing.T) {
	manager, runner, cleanup := integrationFixture(t)
	defer cleanup()
	manager.VerifyHealth = func(context.Context, config.Config, routing.State) error {
		return errors.New("health failed")
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Apply(context.Background(), Candidate{
		UpstreamBaseURL:       "https://gateway.example.test/v1",
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}, inspection.StateDigest)
	if !errors.Is(err, ErrActivationFailed) {
		t.Fatalf("apply error = %v", err)
	}
	for _, path := range []string{
		manager.Paths.Config,
		routing.StatePath(manager.Paths.Config),
		routing.InitializedPath(manager.Paths.Config),
		manager.Paths.ServicePlist,
		manager.Paths.Binding,
		manager.Paths.Journal,
		filepath.Join(manager.Paths.InstallRoot, "current"),
	} {
		if _, statErr := os.Lstat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("artifact was not rolled back: %s: %v", filepath.Base(path), statErr)
		}
	}
	if runner.active {
		t.Fatal("failed integration left LaunchAgent active")
	}
}

func TestApplyRollbackPreservesPreexistingVerifiedRuntime(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	relayDigest, err := fileDigest(manager.Paths.Relay)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(
		manager.Paths.InstallRoot,
		"bundled",
		safeVersionComponent(manager.Version)+"-"+relayDigest[:16],
	)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	for source, name := range map[string]string{
		manager.Paths.Relay:    "opencodex-relay",
		manager.Paths.Relayctl: "opencodex-relayctl",
	} {
		payload, readErr := os.ReadFile(source)
		if readErr != nil || os.WriteFile(filepath.Join(target, name), payload, 0o700) != nil {
			t.Fatalf("prepare existing runtime %s: %v", name, readErr)
		}
	}
	manager.VerifyHealth = func(context.Context, config.Config, routing.State) error {
		return errors.New("health failed")
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Apply(context.Background(), Candidate{
		UpstreamBaseURL:       "https://gateway.example.test/v1",
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}, inspection.StateDigest)
	if !errors.Is(err, ErrActivationFailed) {
		t.Fatalf("apply error = %v", err)
	}
	for _, name := range []string{"opencodex-relay", "opencodex-relayctl"} {
		if _, statErr := os.Stat(filepath.Join(target, name)); statErr != nil {
			t.Fatalf("preexisting runtime %s was removed: %v", name, statErr)
		}
	}
}

func TestApplyRejectsConfigDirectorySymlinkWithoutWritingOutsideHome(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()
	outside := filepath.Join(filepath.Dir(manager.Paths.Home), "outside-config")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(manager.Paths.Home, ".config")); err != nil {
		t.Fatal(err)
	}
	inspection, err := manager.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = manager.Apply(context.Background(), Candidate{
		UpstreamBaseURL:       "https://gateway.example.test/v1",
		AuthenticationProfile: config.RemoteAuthenticationNone,
	}, inspection.StateDigest)
	if !errors.Is(err, ErrUnsafeState) {
		t.Fatalf("apply error = %v", err)
	}
	if entries, readErr := os.ReadDir(outside); readErr != nil || len(entries) != 0 {
		t.Fatalf("outside directory changed: entries=%v err=%v", entries, readErr)
	}
}

func integrationFixture(t *testing.T) (*Manager, *fakeRunner, func()) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Join(root, "home")
	app := filepath.Join(home, "Applications/OpenCodexRelay.app")
	helpers := filepath.Join(app, "Contents/Library/Helpers")
	helperTools := filepath.Join(app, "Contents/Library/HelperTools")
	if err := os.MkdirAll(helpers, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(helperTools, 0o700); err != nil {
		t.Fatal(err)
	}
	relay := filepath.Join(helpers, "opencodex-relay")
	relayctl := filepath.Join(helpers, "opencodex-relayctl")
	for _, path := range []string{
		relay,
		relayctl,
		filepath.Join(helpers, "OpenCodexRelayHelperInstaller"),
		filepath.Join(helperTools, "OpenCodexRelayPrivilegedHelper"),
	} {
		if err := os.WriteFile(path, []byte("fixture-binary"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(app, "Contents/Info.plist"), []byte("fixture-plist"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	paths := Paths{
		Home:         home,
		App:          app,
		Relay:        relay,
		Relayctl:     relayctl,
		InstallRoot:  filepath.Join(home, ".local/lib/opencodex-relay/relay"),
		Config:       filepath.Join(home, ".config/opencodex-relay/relay.json"),
		CodexConfig:  filepath.Join(home, ".codex/config.toml"),
		ServicePlist: filepath.Join(home, "Library/LaunchAgents/io.github.novelkr.opencodex-relay.plist"),
		Binding:      filepath.Join(home, "Library/Application Support/OpenCodexRelay/routing-binding.json"),
		Journal:      filepath.Join(home, "Library/Application Support/OpenCodexRelay/integration-journal.json"),
		LogDirectory: filepath.Join(home, "Library/Logs/opencodex-relay"),
		Label:        "io.github.novelkr.opencodex-relay",
		Scope:        config.InstallationScopeProduction,
	}
	manager := &Manager{
		Paths:          paths,
		Version:        "1.2.3",
		Runner:         runner,
		ValidateBundle: func(context.Context, Paths) error { return nil },
		VerifyHealth:   func(context.Context, config.Config, routing.State) error { return nil },
	}
	return manager, runner, func() { _ = os.RemoveAll(root) }
}

func TestValidatePreservesApplicationLocationErrorPrecedence(t *testing.T) {
	manager, _, cleanup := integrationFixture(t)
	defer cleanup()

	manager.Paths.App = "/Volumes/Development/OpenCodexRelay.app"
	manager.ValidateBundle = func(context.Context, Paths) error {
		return ErrAppLocationInvalid
	}

	if err := manager.validate(context.Background()); !errors.Is(err, ErrAppLocationInvalid) {
		t.Fatalf("validate() error = %v, want %v", err, ErrAppLocationInvalid)
	}
}
