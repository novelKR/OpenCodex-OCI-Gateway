package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/containerruntime"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/lifecyclelock"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
)

const runtimeTrustKeyName = "opencodex-runtime-release-ed25519.pub"

type containerRuntimeServices struct {
	manager *containerruntime.Manager
	oauth   *containerruntime.OAuthSessionManager
}

var containerRuntimeServicesFactory = newContainerRuntimeServices

// runtimeCanaryAPIBaseURL is empty in every production and local-development
// build. The dedicated Apple lifecycle workflow injects a TLS loopback URL at
// link time so it can exercise the real release-check/stage path before a
// candidate has been promoted. It is intentionally not configurable by argv
// or environment.
var runtimeCanaryAPIBaseURL string

func containerRuntimeCommand(arguments []string) {
	if len(arguments) == 0 {
		fatal(containerruntime.ErrInvalidRequest)
	}
	switch arguments[0] {
	case "inspect":
		containerRuntimeInspect(arguments[1:])
	case "check":
		containerRuntimeCheck(arguments[1:])
	case "stage":
		containerRuntimeStage(arguments[1:])
	case "activate":
		containerRuntimeActivate(arguments[1:])
	case "stop":
		containerRuntimeStop(arguments[1:])
	case "recover":
		containerRuntimeRecover(arguments[1:])
	case "oauth":
		containerRuntimeOAuth(arguments[1:])
	default:
		fatal(containerruntime.ErrInvalidRequest)
	}
}

func containerRuntimeInspect(arguments []string) {
	services, ctx, cancel := parseContainerRuntimeBase("container-runtime inspect", arguments, 20*time.Second)
	defer cancel()
	receipt, err := services.manager.Inspect(ctx)
	writeContainerRuntimeReceipt(receipt, err)
}

func containerRuntimeCheck(arguments []string) {
	services, ctx, cancel := parseContainerRuntimeBase("container-runtime check", arguments, 45*time.Second)
	defer cancel()
	receipt, err := services.manager.Check(ctx)
	writeContainerRuntimeReceipt(receipt, err)
}

func containerRuntimeStage(arguments []string) {
	configPath, codexPath := defaultPaths()
	flags := containerRuntimeFlags("container-runtime stage")
	manifestDigest, stateDigest := "", ""
	var routingGeneration uint64
	var jsonOutput bool
	flags.StringVar(&manifestDigest, "expected-manifest-sha256", "", "signed runtime manifest digest")
	flags.StringVar(&stateDigest, "expected-state-digest", "", "runtime state digest")
	flags.Uint64Var(&routingGeneration, "expected-routing-generation", 0, "routing generation")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "Codex config path")
	flags.BoolVar(&jsonOutput, "json", false, "emit JSON")
	parseContainerRuntimeFlags(flags, arguments)
	if !jsonOutput || !lowerSHA256(manifestDigest) || !lowerSHA256(stateDigest) || routingGeneration == 0 {
		fatal(containerruntime.ErrInvalidRequest)
	}
	services, err := containerRuntimeServicesFactory(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	receipt, err := services.manager.Stage(ctx, containerruntime.StageRequest{
		ExpectedManifestSHA256: manifestDigest, ExpectedStateDigest: stateDigest,
		ExpectedRoutingGeneration: routingGeneration,
	})
	writeContainerRuntimeReceipt(receipt, err)
}

func containerRuntimeActivate(arguments []string) {
	configPath, codexPath := defaultPaths()
	flags := containerRuntimeFlags("container-runtime activate")
	stateDigest := ""
	var routingGeneration uint64
	var confirm, jsonOutput bool
	flags.StringVar(&stateDigest, "expected-state-digest", "", "runtime state digest")
	flags.Uint64Var(&routingGeneration, "expected-routing-generation", 0, "routing generation")
	flags.BoolVar(&confirm, "confirm-desktop-exited", false, "confirm Codex Desktop exit")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "Codex config path")
	flags.BoolVar(&jsonOutput, "json", false, "emit JSON")
	parseContainerRuntimeFlags(flags, arguments)
	if !jsonOutput || !confirm || !lowerSHA256(stateDigest) || routingGeneration == 0 {
		fatal(containerruntime.ErrInvalidRequest)
	}
	services, err := containerRuntimeServicesFactory(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	receipt, err := services.manager.Activate(ctx, containerruntime.ActivateRequest{
		ExpectedStateDigest: stateDigest, ExpectedRoutingGeneration: routingGeneration,
		ConfirmDesktopExited: true,
	})
	writeContainerRuntimeReceipt(receipt, err)
}

func containerRuntimeStop(arguments []string) {
	configPath, codexPath := defaultPaths()
	flags := containerRuntimeFlags("container-runtime stop")
	stateDigest := ""
	var routingGeneration uint64
	var confirm, jsonOutput bool
	flags.StringVar(&stateDigest, "expected-state-digest", "", "runtime state digest")
	flags.Uint64Var(&routingGeneration, "expected-routing-generation", 0, "routing generation")
	flags.BoolVar(&confirm, "confirm-desktop-exited", false, "confirm Codex Desktop exit")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "Codex config path")
	flags.BoolVar(&jsonOutput, "json", false, "emit JSON")
	parseContainerRuntimeFlags(flags, arguments)
	if !jsonOutput || !confirm || !lowerSHA256(stateDigest) || routingGeneration == 0 {
		fatal(containerruntime.ErrInvalidRequest)
	}
	services, err := containerRuntimeServicesFactory(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	receipt, err := services.manager.Stop(ctx, containerruntime.StopRequest{
		ExpectedStateDigest: stateDigest, ExpectedRoutingGeneration: routingGeneration,
		ConfirmDesktopExited: true,
	})
	writeContainerRuntimeReceipt(receipt, err)
}

func containerRuntimeRecover(arguments []string) {
	configPath, codexPath := defaultPaths()
	flags := containerRuntimeFlags("container-runtime recover")
	stateDigest := ""
	var confirm, jsonOutput bool
	flags.StringVar(&stateDigest, "expected-state-digest", "", "runtime state digest")
	flags.BoolVar(&confirm, "confirm-desktop-exited", false, "confirm Codex Desktop exit")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "Codex config path")
	flags.BoolVar(&jsonOutput, "json", false, "emit JSON")
	parseContainerRuntimeFlags(flags, arguments)
	if !jsonOutput || !confirm || !lowerSHA256(stateDigest) {
		fatal(containerruntime.ErrInvalidRequest)
	}
	services, err := containerRuntimeServicesFactory(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	receipt, err := services.manager.Recover(ctx, containerruntime.RecoverRequest{
		ExpectedStateDigest: stateDigest, ConfirmDesktopExited: true,
	})
	writeContainerRuntimeReceipt(receipt, err)
}

func containerRuntimeOAuth(arguments []string) {
	if len(arguments) == 0 {
		fatal(containerruntime.ErrInvalidRequest)
	}
	operation := arguments[0]
	configPath, codexPath := defaultPaths()
	flags := containerRuntimeFlags("container-runtime oauth " + operation)
	provider, kindText, operationID := "", "", ""
	var jsonOutput bool
	flags.StringVar(&provider, "provider", "", "fixed provider identifier")
	flags.StringVar(&kindText, "kind", "", "generic or codex")
	flags.StringVar(&operationID, "operation-id", "", "opaque runtime OAuth operation")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "Codex config path")
	flags.BoolVar(&jsonOutput, "json", false, "emit JSON")
	parseContainerRuntimeFlags(flags, arguments[1:])
	if !jsonOutput {
		fatal(containerruntime.ErrInvalidRequest)
	}
	services, err := containerRuntimeServicesFactory(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	inspection, err := services.manager.Inspect(ctx)
	if err != nil || inspection.State != containerruntime.StateHealthy || inspection.Active == nil || inspection.RecoveryRequired {
		fatal(containerruntime.ErrUnavailable)
	}
	switch operation {
	case "providers":
		if provider != "" || kindText != "" || operationID != "" {
			fatal(containerruntime.ErrInvalidRequest)
		}
		receipt, callErr := services.oauth.Providers(ctx)
		writeContainerRuntimeReceipt(receipt, callErr)
	case "start":
		kind := containerruntime.OAuthKind(kindText)
		if operationID != "" || provider == "" || kind != containerruntime.OAuthKindGeneric && kind != containerruntime.OAuthKindCodex {
			fatal(containerruntime.ErrInvalidRequest)
		}
		receipt, callErr := services.oauth.Start(ctx, provider, kind)
		writeContainerRuntimeReceipt(receipt, callErr)
	case "status":
		if provider != "" || kindText != "" || !lowerSHA256(operationID) {
			fatal(containerruntime.ErrInvalidRequest)
		}
		receipt, callErr := services.oauth.Status(ctx, operationID)
		writeContainerRuntimeReceipt(receipt, callErr)
	case "submit":
		if provider != "" || kindText != "" || !lowerSHA256(operationID) {
			fatal(containerruntime.ErrInvalidRequest)
		}
		input := io.LimitReader(os.Stdin, containerruntime.MaximumOAuthInputBytes+1)
		receipt, callErr := services.oauth.Submit(ctx, containerruntime.OAuthSubmitRequest{OperationID: operationID, Input: input})
		writeContainerRuntimeReceipt(receipt, callErr)
	case "cancel":
		if provider != "" || kindText != "" || !lowerSHA256(operationID) {
			fatal(containerruntime.ErrInvalidRequest)
		}
		receipt, callErr := services.oauth.Cancel(ctx, operationID)
		writeContainerRuntimeReceipt(receipt, callErr)
	default:
		fatal(containerruntime.ErrInvalidRequest)
	}
}

func parseContainerRuntimeBase(name string, arguments []string, timeout time.Duration) (*containerRuntimeServices, context.Context, context.CancelFunc) {
	configPath, codexPath := defaultPaths()
	flags := containerRuntimeFlags(name)
	var jsonOutput bool
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "Codex config path")
	flags.BoolVar(&jsonOutput, "json", false, "emit JSON")
	parseContainerRuntimeFlags(flags, arguments)
	if !jsonOutput {
		fatal(containerruntime.ErrInvalidRequest)
	}
	services, err := containerRuntimeServicesFactory(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return services, ctx, cancel
}

func containerRuntimeFlags(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func parseContainerRuntimeFlags(flags *flag.FlagSet, arguments []string) {
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		fatal(containerruntime.ErrInvalidRequest)
	}
}

func writeContainerRuntimeReceipt(value any, err error) {
	if err != nil {
		fatal(err)
	}
	data, err := json.Marshal(value)
	if err != nil || len(data)+1 > containerruntime.MaximumReceiptBytes {
		fatal(containerruntime.ErrUnsafeState)
	}
	data = append(data, '\n')
	if _, err := os.Stdout.Write(data); err != nil {
		fatal(err)
	}
}

func newContainerRuntimeServices(configPath, codexPath string) (*containerRuntimeServices, error) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return nil, containerruntime.ErrUnavailable
	}
	if _, err := loadProductionContainerRuntimeConfig(configPath); err != nil {
		return nil, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, containerruntime.ErrUnavailable
	}
	account, err := credentials.ResolveKeychainAccount("")
	if err != nil {
		return nil, containerruntime.ErrCredential
	}
	root, err := containerruntime.DefaultRoot(filepath.Clean(home))
	if err != nil {
		return nil, err
	}
	checker, err := newContainerRuntimeChecker()
	if err != nil {
		return nil, err
	}
	publicKey, err := readBundledRuntimeTrustKey()
	if err != nil {
		return nil, err
	}
	if _, _, err := runtimemanifest.ParsePublicKey(publicKey); err != nil {
		return nil, containerruntime.ErrUnsafeState
	}
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		return nil, err
	}
	secretServer, err := containerruntime.NewUnixSecretServer(20 * time.Second)
	if err != nil {
		return nil, err
	}
	keychain := containerruntime.NewSystemKeychain()
	bridge := &containerRoutingBridge{
		controller: controller, maintenance: routing.NewSocketRuntimeMaintenance(configPath),
	}
	manager, err := containerruntime.NewManager(containerruntime.ManagerOptions{
		Root: root, Account: account, RelayVersion: productionRuntimeRelayVersion(), PublicKeyPEM: publicKey,
		Checker: checker, Runtime: containerruntime.NewAppleCLI(), Prober: containerruntime.NewRuntimeHTTPProber(),
		Cloner: containerruntime.NewAPFSCloner(), SecretServer: secretServer, Keychain: keychain,
		Routing: bridge, Enroller: &containerProfileEnroller{
			configPath: configPath, codexPath: codexPath, account: account,
		}, Locker: &containerLifecycleLocker{home: filepath.Clean(home)},
	})
	if err != nil {
		return nil, err
	}
	oauth, err := containerruntime.NewOAuthSessionManager(root, containerruntime.NewHTTPManagementAPI(), keychain, account)
	if err != nil {
		return nil, err
	}
	return &containerRuntimeServices{manager: manager, oauth: oauth}, nil
}

func newContainerRuntimeChecker() (*runtimemanifest.Checker, error) {
	if runtimeCanaryAPIBaseURL != "" {
		return runtimemanifest.NewLoopbackCanaryChecker(runtimeCanaryAPIBaseURL)
	}
	return runtimemanifest.NewProductionChecker()
}

type containerLifecycleLocker struct{ home string }

func (l *containerLifecycleLocker) Lock(ctx context.Context) (func() error, error) {
	lock, err := lifecyclelock.AcquireWriter(ctx, l.home, os.Getenv(lifecyclelock.SourceInstallReservationEnvironment))
	if err != nil {
		return nil, err
	}
	return lock.Close, nil
}

type containerProfileEnroller struct {
	configPath string
	codexPath  string
	account    string
}

func (e *containerProfileEnroller) Ensure(context.Context) (string, error) {
	if e == nil || !filepath.IsAbs(e.configPath) || !filepath.IsAbs(e.codexPath) || e.account == "" {
		return "", containerruntime.ErrInvalidRequest
	}
	cfg, err := loadProductionContainerRuntimeConfig(e.configPath)
	if err != nil {
		return "", err
	}
	if cfg.UpstreamMode != config.UpstreamModeExternalGateway {
		return "", containerruntime.ErrInvalidRequest
	}
	profile, err := config.NewLocalAppleContainerProfileForCodexConfigWithCatalogName(
		e.codexPath, config.LocalAppleContainerCatalog,
	)
	if err != nil {
		return "", err
	}
	profile.CredentialAccount = e.account
	if cfg.LocalAppleContainer != nil {
		existing := *cfg.LocalAppleContainer
		if existing.CredentialAccount != "" && existing.CredentialAccount != e.account {
			return "", containerruntime.ErrUnsafeState
		}
		existing.CredentialAccount = e.account
		if existing != *profile {
			return "", containerruntime.ErrUnsafeState
		}
		if cfg.LocalAppleContainer.CredentialAccount == "" {
			cfg.LocalAppleContainer = profile
			if err := config.Write(e.configPath, cfg); err != nil {
				return "", err
			}
		}
	} else {
		cfg.LocalAppleContainer = profile
		if err := config.Write(e.configPath, cfg); err != nil {
			return "", err
		}
	}
	return e.account, nil
}

func loadProductionContainerRuntimeConfig(configPath string) (config.Config, error) {
	if !filepath.IsAbs(configPath) {
		return config.Config{}, containerruntime.ErrInvalidRequest
	}
	info, err := os.Lstat(configPath)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o077 != 0 || ownerUID(info) != os.Geteuid() {
		return config.Config{}, containerruntime.ErrUnsafeState
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return config.Config{}, err
	}
	if cfg.Scope() != config.InstallationScopeProduction {
		return config.Config{}, containerruntime.ErrUnavailable
	}
	return cfg, nil
}

type containerRoutingBridge struct {
	controller  *routing.Controller
	maintenance *routing.SocketRuntimeMaintenance
}

func (b *containerRoutingBridge) Current(ctx context.Context) (containerruntime.RoutingSnapshot, error) {
	if b == nil || b.controller == nil || b.maintenance == nil {
		return containerruntime.RoutingSnapshot{}, containerruntime.ErrUnavailable
	}
	status, err := b.maintenance.Status(ctx)
	if err != nil {
		return containerruntime.RoutingSnapshot{}, containerruntime.ErrRecoveryRequired
	}
	snapshot, err := routingSnapshotFromMaintenanceStatus(status)
	if err != nil {
		return containerruntime.RoutingSnapshot{}, err
	}
	runtimeRoutingPending, pendingErr := b.controller.RuntimeRoutingPending(ctx)
	if pendingErr != nil || runtimeRoutingPending && snapshot.MaintenancePending {
		return containerruntime.RoutingSnapshot{}, containerruntime.ErrRecoveryRequired
	}
	snapshot.RuntimeRoutingPending = runtimeRoutingPending
	return snapshot, nil
}

func routingSnapshotFromMaintenanceStatus(status routing.MaintenanceRoutingStatus) (containerruntime.RoutingSnapshot, error) {
	if status.Validate() != nil {
		return containerruntime.RoutingSnapshot{}, containerruntime.ErrRecoveryRequired
	}
	stable := status.Phase == routing.PhaseRelayActive || status.Phase == routing.PhaseNativeActive
	return containerruntime.RoutingSnapshot{
		Generation:         status.RoutingGeneration,
		AppleActive:        status.Phase == routing.PhaseRelayActive && status.Backend == routing.BackendLocalAppleContainer,
		RecoveryRequired:   status.Pending || !stable,
		MaintenancePending: status.Pending,
	}, nil
}

func (b *containerRoutingBridge) ActivateApple(ctx context.Context, request containerruntime.RoutingRequest, confirm bool) (uint64, error) {
	current, err := b.Current(ctx)
	if err != nil || current.Generation != request.ExpectedOriginRoutingGeneration || current.RecoveryRequired ||
		current.MaintenancePending || current.RuntimeRoutingPending || !request.TargetAppleActive ||
		request.Direction != containerruntime.RoutingCompleteTarget {
		return 0, containerruntime.ErrRoutingChanged
	}
	generation, err := b.controller.SwitchRuntimeRouting(ctx, toRuntimeRoutingRequest(request), confirm)
	if err != nil {
		return 0, err
	}
	return generation, nil
}

func (b *containerRoutingBridge) StopApple(ctx context.Context, request containerruntime.RoutingRequest, confirm bool) (uint64, error) {
	current, err := b.Current(ctx)
	if err != nil || current.Generation != request.ExpectedOriginRoutingGeneration || !current.AppleActive ||
		current.RecoveryRequired || current.MaintenancePending || current.RuntimeRoutingPending ||
		!confirm || request.TargetAppleActive || request.Direction != containerruntime.RoutingCompleteTarget {
		return 0, containerruntime.ErrRoutingChanged
	}
	return b.controller.SwitchRuntimeRouting(ctx, toRuntimeRoutingRequest(request), confirm)
}

func (b *containerRoutingBridge) Reconcile(ctx context.Context, request containerruntime.RoutingRequest, confirm bool) (uint64, error) {
	if b == nil || b.controller == nil {
		return 0, containerruntime.ErrUnavailable
	}
	if !confirm {
		return 0, containerruntime.ErrInvalidRequest
	}
	return b.controller.ReconcileRuntimeRouting(ctx, toRuntimeRoutingRequest(request), confirm)
}

func (b *containerRoutingBridge) Acknowledge(ctx context.Context, request containerruntime.RoutingRequest, generation uint64) error {
	if b == nil || b.controller == nil {
		return containerruntime.ErrUnavailable
	}
	return b.controller.AcknowledgeRuntimeRouting(ctx, toRuntimeRoutingRequest(request), generation)
}

func toRuntimeRoutingRequest(value containerruntime.RoutingRequest) routing.RuntimeRoutingRequest {
	return routing.RuntimeRoutingRequest{
		Intent: routing.RuntimeRoutingIntent{
			OperationID: value.Intent.OperationID, InstallationID: value.Intent.InstallationID,
			OldManifestSHA256: value.Intent.OldManifestSHA256, NewManifestSHA256: value.Intent.NewManifestSHA256,
			OldImageDigest: value.Intent.OldImageDigest, NewImageDigest: value.Intent.NewImageDigest,
			OldStateGeneration: value.Intent.OldStateGeneration, NewStateGeneration: value.Intent.NewStateGeneration,
		},
		ExpectedOriginRoutingGeneration: value.ExpectedOriginRoutingGeneration,
		TargetAppleActive:               value.TargetAppleActive,
		Direction:                       routing.RuntimeRoutingDirection(value.Direction),
	}
}

func (b *containerRoutingBridge) Prepare(ctx context.Context, request containerruntime.MaintenanceRequest) (containerruntime.MaintenanceWitness, error) {
	intent := routing.MaintenanceIntent{
		OperationID: request.OperationID, InstallationID: request.InstallationID,
		OldManifestSHA256: request.OldManifestSHA256, NewManifestSHA256: request.NewManifestSHA256,
		OldImageDigest: request.OldImageDigest, NewImageDigest: request.NewImageDigest,
		OldStateGeneration: request.OldStateGeneration, NewStateGeneration: request.NewStateGeneration,
	}
	witness, err := b.maintenance.Prepare(ctx, request.ExpectedRoutingGeneration, intent)
	if err != nil {
		return containerruntime.MaintenanceWitness{}, err
	}
	return fromRoutingWitness(witness), nil
}

func (b *containerRoutingBridge) Commit(ctx context.Context, witness containerruntime.MaintenanceWitness) (uint64, error) {
	routingWitness := toRoutingWitness(witness)
	if err := b.maintenance.Commit(ctx, routingWitness); err != nil {
		return 0, err
	}
	return witness.FinalRoutingGeneration, nil
}

func (b *containerRoutingBridge) Rollback(ctx context.Context, witness containerruntime.MaintenanceWitness) (uint64, error) {
	routingWitness := toRoutingWitness(witness)
	if err := b.maintenance.Rollback(ctx, routingWitness); err != nil {
		return 0, err
	}
	return witness.FinalRoutingGeneration, nil
}

func fromRoutingWitness(value routing.MaintenanceWitness) containerruntime.MaintenanceWitness {
	return containerruntime.MaintenanceWitness{
		Schema: value.Schema, Backend: string(value.Backend),
		OriginRoutingGeneration:   value.OriginRoutingGeneration,
		PreparedRoutingGeneration: value.PreparedRoutingGeneration,
		FinalRoutingGeneration:    value.FinalRoutingGeneration,
		Intent: containerruntime.MaintenanceIntent{
			OperationID: value.Intent.OperationID, InstallationID: value.Intent.InstallationID,
			OldManifestSHA256: value.Intent.OldManifestSHA256, NewManifestSHA256: value.Intent.NewManifestSHA256,
			OldImageDigest: value.Intent.OldImageDigest, NewImageDigest: value.Intent.NewImageDigest,
			OldStateGeneration: value.Intent.OldStateGeneration, NewStateGeneration: value.Intent.NewStateGeneration,
		},
	}
}

func toRoutingWitness(value containerruntime.MaintenanceWitness) routing.MaintenanceWitness {
	return routing.MaintenanceWitness{
		Schema: value.Schema, Backend: routing.Backend(value.Backend),
		OriginRoutingGeneration:   value.OriginRoutingGeneration,
		PreparedRoutingGeneration: value.PreparedRoutingGeneration,
		FinalRoutingGeneration:    value.FinalRoutingGeneration,
		Intent: routing.MaintenanceIntent{
			OperationID: value.Intent.OperationID, InstallationID: value.Intent.InstallationID,
			OldManifestSHA256: value.Intent.OldManifestSHA256, NewManifestSHA256: value.Intent.NewManifestSHA256,
			OldImageDigest: value.Intent.OldImageDigest, NewImageDigest: value.Intent.NewImageDigest,
			OldStateGeneration: value.Intent.OldStateGeneration, NewStateGeneration: value.Intent.NewStateGeneration,
		},
	}
}

func readBundledRuntimeTrustKey() ([]byte, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return nil, err
	}
	contents := filepath.Dir(filepath.Dir(filepath.Dir(resolved)))
	path := filepath.Join(contents, "Resources", "RuntimeTrust", runtimeTrustKeyName)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 || info.Size() <= 0 || info.Size() > runtimemanifest.MaximumPublicKeyBytes {
		return nil, containerruntime.ErrUnsafeState
	}
	return os.ReadFile(path)
}

func productionRuntimeRelayVersion() string {
	// Raw `go run`/unversioned binaries are not installation artifacts and must
	// not silently bypass a signed manifest's minimum Relay version. Both local
	// development bundles and release bundles inject an explicit SemVer through
	// the existing build scripts.
	if version == "dev" {
		return ""
	}
	return version
}

func lowerSHA256(value string) bool {
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

func ownerUID(info os.FileInfo) int {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return -1
	}
	return int(stat.Uid)
}

func errOr(err, fallback error) error {
	if err != nil {
		return err
	}
	return fallback
}
