package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/integration"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/legacymigration"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/lifecyclelock"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/localopencodex"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/release"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
)

var version = "dev"

var errHandoffCodexConfigPreflight = errors.New("selected Codex config must be a missing or regular non-symlink file")

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "init":
		initConfig(os.Args[2:])
	case "enable":
		enable(os.Args[2:])
	case "disable":
		disable(os.Args[2:])
	case "mode":
		modeCommand(os.Args[2:])
	case "migrate":
		migrationCommand(os.Args[2:])
	case "migrate-legacy":
		migrateLegacy(os.Args[2:])
	case "status":
		status(os.Args[2:])
	case "catalog":
		catalogCommand(os.Args[2:])
	case "gateway":
		gatewayCommand(os.Args[2:])
	case "integration":
		integrationCommand(os.Args[2:])
	case "lifecycle":
		lifecycleCommand(os.Args[2:])
	case "release":
		releaseCommand(os.Args[2:])
	case "version", "--version", "-version":
		fmt.Println(version)
	default:
		usage()
		os.Exit(2)
	}
}

func usage() {
	writeUsage(os.Stderr)
}

func writeUsage(writer io.Writer) {
	fmt.Fprint(writer, `Usage:
  opencodex-relayctl init --upstream URL [--upstream-mode external_gateway|local_opencodex]
      [--listen 127.0.0.1:PORT] [--interactive-listen 127.0.0.1:PORT]
      [--installation-scope production|local_development]
      [--config PATH] [--credentials keychain|file|none]
      [--responses-websocket-mode passthrough|http_fallback]
      [--bounded-json-model MODEL] [--catalog-owner relay|remote_manager]
  opencodex-relayctl enable [--config PATH] [--codex-config PATH] # deprecated request alias; does not apply
  opencodex-relayctl disable [--config PATH] [--codex-config PATH] # deprecated request alias; does not apply
  opencodex-relayctl mode status [--config PATH] [--codex-config PATH] --json
  opencodex-relayctl mode request native|external|local_opencodex|relay [--config PATH] [--codex-config PATH] [--json]
  opencodex-relayctl mode seed-native [--config PATH] [--codex-config PATH] [--json] # local-dev installer only
  opencodex-relayctl mode verify-native [--config PATH] [--codex-config PATH] [--json] # local-dev installer only
  opencodex-relayctl mode repair-native --expected-routing-generation N
      --confirm-local-development-native-repair [--config PATH] [--codex-config PATH] [--json] # local-dev only
  opencodex-relayctl mode inspect-native-repair --expected-routing-generation N
      [--config PATH] [--codex-config PATH] --json # local-dev only
  opencodex-relayctl mode inspect-native-repair-owner --expected-routing-generation N --expected-owner opencodex
      --installation-id ID --installation-fingerprint SHA256 --native-restore-fingerprint SHA256
      --ocx-executable PATH --ocx-sha256 LOWERCASE_SHA256 [--config PATH] [--codex-config PATH] --json # local-dev only
  opencodex-relayctl mode repair-native-routing --expected-routing-generation N
      --expected-owner local_relay|opencodex --confirm-desktop-exited
      --confirm-local-development-native-routing-repair
      [--installation-id ID --installation-fingerprint SHA256 --native-restore-fingerprint SHA256
       --ocx-executable PATH --ocx-sha256 LOWERCASE_SHA256]
      [--config PATH] [--codex-config PATH] --json # local-dev only
  opencodex-relayctl mode apply --confirm-desktop-exited [--config PATH] [--codex-config PATH] [--json]
  opencodex-relayctl mode cancel [--config PATH] [--codex-config PATH] [--json]
  opencodex-relayctl mode recover --complete|--rollback --confirm-desktop-exited
      [--expected-routing-generation N --installation-id ID --installation-fingerprint SHA256]
      [--config PATH] [--codex-config PATH] [--json]
  opencodex-relayctl mode discover-open-codex --tier a|b|c [--confirm-broad-scan] [--config PATH] --json
  opencodex-relayctl mode discover-open-codex-native [--acknowledge-terminal-receipt-digest SHA256] --json
  opencodex-relayctl mode inspect-open-codex-native-removal --installation-id ID
      --installation-fingerprint SHA256 --native-restore-fingerprint SHA256
      --expected-boundary-revision SHA256 --json
  opencodex-relayctl mode inspect-open-codex-native-data --installation-id ID
      --installation-fingerprint SHA256 --native-restore-fingerprint SHA256
      --expected-boundary-revision SHA256 --json
  opencodex-relayctl mode remove-open-codex-native --installation-id ID
      --installation-fingerprint SHA256 --native-restore-fingerprint SHA256
      --expected-boundary-revision SHA256 --removal-mode preserve_data|trash_selected
      [--data-item INVENTORY_ID ...] [--expected-inventory-revision SHA256]
      --confirm-opencodex-native-removal --confirm-desktop-exited [--confirm-data-trash]
      [--confirm-interrupted-data-refresh] [--confirm-rebooted-process-recovery] --json
  opencodex-relayctl mode inspect-open-codex-data --installation-id ID --installation-fingerprint SHA256 [--config PATH] --json
  opencodex-relayctl mode remove-open-codex --installation-id ID --installation-fingerprint SHA256
      --removal-mode preserve_data|trash_selected --expected-routing-generation N
      [--data-item INVENTORY_ID ...] --confirm-opencodex-removal [--confirm-data-trash]
      [--confirm-interrupted-data-refresh] [--confirm-rebooted-process-recovery]
      --confirm-desktop-exited [--config PATH] [--codex-config PATH] [--json]
  opencodex-relayctl mode handoff --ocx-executable PATH --action retain_proxy_remove_shim|retain_proxy_keep_shim
      --ocx-sha256 LOWERCASE_SHA256 --confirm-opencodex-handoff --confirm-desktop-exited [--config PATH] [--codex-config PATH] [--json]
  opencodex-relayctl migrate-legacy [--codex-config PATH]
  opencodex-relayctl migrate legacy-pw inspect|apply|rollback [--home PATH] [--codex-config PATH] --json
  opencodex-relayctl status [--config PATH]
  opencodex-relayctl catalog refresh [--config PATH] [--no-apply]
  opencodex-relayctl gateway inspect [--config PATH] [--codex-config PATH] --json
  opencodex-relayctl gateway test [--config PATH] [--codex-config PATH] --json < candidate.json
  opencodex-relayctl gateway apply --expected-config-digest SHA256 --expected-routing-generation N
      [--config PATH] [--codex-config PATH] --json < candidate.json
	  opencodex-relayctl integration inspect --json
	  opencodex-relayctl integration apply --expected-state-digest SHA256 --json < candidate.json
	  opencodex-relayctl integration recover --json
	  opencodex-relayctl lifecycle source-install-capability --json
	  opencodex-relayctl lifecycle reserve-source-install --scope production|local_development
	      --recovery-file ABSOLUTE_PATH --json
  opencodex-relayctl lifecycle release-source-install --scope production|local_development
      --token SHA256 [--remove-created-root] --json
  opencodex-relayctl release verify --manifest PATH --signature PATH --public-key PATH
`)
}

func defaultPaths() (string, string) {
	configPath, err := config.DefaultConfigPath()
	if err != nil {
		fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	return configPath, home + "/.codex/config.toml"
}

// routingController is the CLI-only constructor. Canonical External installs
// bind Apply to the resident relay's control socket, so a macOS profile
// switch cannot claim a live swap without changing the runtime. The older
// static local_opencodex topology deliberately has no runtime manager/socket;
// retain its pre-existing controller behavior instead of making the new
// macOS-only mechanism a breaking Linux dependency.
func routingController(configPath, codexPath string) (*routing.Controller, error) {
	return routingControllerWithRecoveryGate(
		configPath,
		codexPath,
		func() error { return handoff.RemovalRoutingGate(configPath) },
	)
}

func routingControllerWithRecoveryGate(
	configPath string,
	codexPath string,
	recoveryGate func() error,
	additionalOptions ...routing.ControllerOption,
) (*routing.Controller, error) {
	if recoveryGate == nil {
		recoveryGate = func() error { return handoff.RemovalRoutingGate(configPath) }
	}
	if cfg, err := config.Load(configPath); err == nil {
		return routingControllerForConfigWithRecoveryGate(
			configPath,
			codexPath,
			cfg,
			recoveryGate,
			additionalOptions...,
		)
	}
	options := []routing.ControllerOption{
		routing.WithControllerRecoveryGate(recoveryGate),
		routing.WithControllerRecoveryGateReleasable(func() bool { return handoff.RemovalRoutingGateReleasable(configPath) }),
	}
	options = append(options, additionalOptions...)
	return routing.NewController(configPath, codexPath, options...)
}

func routingControllerForConfig(configPath, codexPath string, cfg config.Config) (*routing.Controller, error) {
	return routingControllerForConfigWithRecoveryGate(
		configPath,
		codexPath,
		cfg,
		func() error { return handoff.RemovalRoutingGate(configPath) },
	)
}

func routingControllerForConfigWithRecoveryGate(
	configPath string,
	codexPath string,
	cfg config.Config,
	recoveryGate func() error,
	additionalOptions ...routing.ControllerOption,
) (*routing.Controller, error) {
	if recoveryGate == nil {
		recoveryGate = func() error { return handoff.RemovalRoutingGate(configPath) }
	}
	owner, err := codexconfig.OwnerForID(cfg.Scope())
	if err != nil {
		return nil, err
	}
	options := []routing.ControllerOption{
		routing.WithCodexConfigOwner(owner),
		routing.WithControllerRecoveryGate(recoveryGate),
		routing.WithControllerRecoveryGateReleasable(func() bool {
			return handoff.RemovalRoutingGateReleasable(configPath)
		}),
	}
	if cfg.UpstreamMode == config.UpstreamModeExternalGateway {
		options = append(options, routing.WithRuntimeControl(routing.NewSocketRuntimeControl(configPath)))
	}
	options = append(options, additionalOptions...)
	return routing.NewController(configPath, codexPath, options...)
}

// localDevelopmentRoutingController keeps native verification and repair inside
// the development namespace. Production routing may expose ordinary status, but
// it must never use local-development ownership proof to authorize mutation.
func localDevelopmentRoutingController(configPath, codexPath string) (*routing.Controller, error) {
	cfg, err := config.Load(configPath)
	if err != nil || cfg.Scope() != config.InstallationScopeLocalDevelopment {
		return nil, errorsNew("local-development routing operation requires a local_development relay config")
	}
	return routingControllerForConfig(configPath, codexPath, cfg)
}

func initConfig(args []string) {
	configPath, _ := defaultPaths()
	flags := flag.NewFlagSet("init", flag.ExitOnError)
	listenAddress := flags.String("listen", config.DefaultListenAddress, "loopback listen address")
	interactiveListenAddress := flags.String("interactive-listen", "", "interactive numeric-loopback listen address (default: port 18182 in the primary address family)")
	upstream := flags.String("upstream", "", "OpenCodex-compatible /v1 endpoint")
	upstreamMode := flags.String("upstream-mode", config.UpstreamModeExternalGateway, "static upstream topology")
	source := flags.String("credentials", "", "credential source (default: platform source for external, none for local)")
	webSocketMode := flags.String("responses-websocket-mode", config.ResponsesWebSocketModePassthrough, "Responses WebSocket handling")
	catalogOwner := flags.String("catalog-owner", "", "catalog lifecycle owner (default: relay for external, remote_manager for local)")
	var boundedModels stringListFlag
	flags.Var(&boundedModels, "bounded-json-model", "exact Responses model to normalize; repeatable")
	force := flags.Bool("force", false, "replace an existing relay configuration")
	catalogPath := flags.String("catalog-path", "", "model catalog path consumed by Codex")
	codexExecutable := flags.String("codex-executable", "", "Codex executable used to discover the client version")
	manageAppServer := flags.Bool("manage-app-server", false, "explicitly permit restart of an identified local AppServer after a catalog change")
	appServerHome := flags.String("app-server-home", "", "exact absolute CODEX_HOME eligible for automatic restart")
	connectionProbeEnabled := flags.Bool("connection-probe-enabled", false, "enable the macOS-only, low-frequency external gateway connection probe")
	installationScope := flags.String("installation-scope", config.InstallationScopeProduction, "distribution routing ownership scope")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.Parse(args)
	if *upstream == "" {
		fatal(fmt.Errorf("--upstream is required"))
	}
	credentialSource := *source
	if credentialSource == "" {
		if *upstreamMode == config.UpstreamModeLocalOpenCodex {
			credentialSource = config.CredentialsSourceNone
		} else {
			credentialSource = defaultCredentialSource()
		}
	}
	owner := *catalogOwner
	if owner == "" {
		if *upstreamMode == config.UpstreamModeLocalOpenCodex {
			owner = config.CatalogOwnerRemoteManager
		} else {
			owner = config.CatalogOwnerRelay
		}
	}
	// Config initialization is a lifecycle writer: it must not create even a
	// partial Relay asset beside an active, retained-terminal, or malformed
	// standalone Native removal journal. Hold the same user lock through the
	// final config write and recheck the anchor only after acquiring it.
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	if runtime.GOOS == "darwin" {
		home, err := os.UserHomeDir()
		if err != nil || requireStandaloneRemovalInactiveForInit(home) != nil {
			fatal(handoff.ErrNativeRemovalRecoveryRequired)
		}
	}
	if _, err := os.Lstat(configPath); err == nil && !*force {
		fatal(fmt.Errorf("relay config already exists; inspect it or pass --force to replace it"))
	} else if err != nil && !os.IsNotExist(err) {
		fatal(fmt.Errorf("inspect relay config: %w", err))
	}
	cfg, err := config.NewDefault(strings.TrimRight(*upstream, "/"), credentialSource)
	if err != nil {
		fatal(err)
	}
	cfg.UpstreamMode = *upstreamMode
	cfg.InstallationScope = *installationScope
	cfg.ListenAddress = *listenAddress
	cfg.Responses.Scheduler.InteractiveListenAddress = *interactiveListenAddress
	cfg.Responses.WebSocketMode = *webSocketMode
	if len(boundedModels) > 0 {
		cfg.Responses.ModelModes = make(map[string]string, len(boundedModels))
		seenModels := make(map[string]struct{}, len(boundedModels))
		for _, model := range boundedModels {
			if model == "" || strings.TrimSpace(model) != model {
				fatal(fmt.Errorf("--bounded-json-model must be non-empty and contain no surrounding whitespace"))
			}
			folded := strings.ToLower(model)
			if _, duplicate := seenModels[folded]; duplicate {
				fatal(fmt.Errorf("--bounded-json-model values must be unique case-insensitively"))
			}
			seenModels[folded] = struct{}{}
			cfg.Responses.ModelModes[model] = config.ResponsesModelModeBoundedJSON
		}
	}
	cfg.Catalog.Owner = owner
	if *catalogPath != "" {
		cfg.Catalog.Path = *catalogPath
	}
	if *codexExecutable != "" {
		cfg.Catalog.CodexExecutable = *codexExecutable
	}
	if *manageAppServer && *appServerHome == "" {
		fatal(fmt.Errorf("--app-server-home is required when --manage-app-server=true"))
	}
	cfg.Catalog.ManageAppServer = *manageAppServer
	cfg.Catalog.AppServerHome = *appServerHome
	cfg.ConnectionProbe.Enabled = *connectionProbeEnabled
	if cfg.Scope() == config.InstallationScopeLocalDevelopment && (runtime.GOOS != "darwin" || runtime.GOARCH != "arm64") {
		fatal(fmt.Errorf("local_development installation scope requires macOS Apple Silicon"))
	}
	if err := config.Write(configPath, cfg); err != nil {
		fatal(err)
	}
	fmt.Printf("relay_config=%s\n", configPath)
}

func enable(args []string) {
	configPath, codexPath := defaultPaths()
	flags := flag.NewFlagSet("enable", flag.ExitOnError)
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	flags.Parse(args)
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	status, err := controller.EnableCompatibility(context.Background())
	if err != nil {
		fatal(err)
	}
	fmt.Printf("codex_routing=requested mode=%s phase=%s deprecated_request_alias=true desktop_restart_required=%t\n", status.DesiredMode, status.Phase, status.DesktopRestartRequired)
}

func disable(args []string) {
	configPath, codexPath := defaultPaths()
	flags := flag.NewFlagSet("disable", flag.ExitOnError)
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	flags.Parse(args)
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	status, err := controller.DisableCompatibility(context.Background())
	if err != nil {
		fatal(err)
	}
	fmt.Printf("codex_routing=requested mode=%s phase=%s deprecated_request_alias=true desktop_restart_required=%t\n", status.DesiredMode, status.Phase, status.DesktopRestartRequired)
}

func migrationCommand(args []string) {
	if len(args) < 2 || args[0] != "legacy-pw" {
		fatal(errors.New("usage: opencodex-relayctl migrate legacy-pw inspect|apply|rollback --json"))
	}
	operation := args[1]
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	codexPath := filepath.Join(home, ".codex", "config.toml")
	flags := flag.NewFlagSet("migrate legacy-pw "+operation, flag.ExitOnError)
	flags.StringVar(&home, "home", home, "migration home directory")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit the secret-free migration result")
	flags.Parse(args[2:])
	if !*jsonOutput {
		fatal(errors.New("--json is required for namespace migration"))
	}
	var lifecycle *lifecyclelock.Lock
	if operation != "inspect" {
		lifecycle = acquireUserLifecycleLock(context.Background())
		defer lifecycle.Close()
		if err := requireStandaloneRemovalInactiveForInit(home); err != nil {
			fatal(err)
		}
	}
	result, err := legacymigration.Run(operation, legacymigration.Options{
		Home:        home,
		CodexConfig: codexPath,
	})
	if err != nil {
		fatal(err)
	}
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(result); err != nil {
		fatal(err)
	}
}

func migrateLegacy(args []string) {
	_, codexPath := defaultPaths()
	flags := flag.NewFlagSet("migrate-legacy", flag.ExitOnError)
	flags.StringVar(&codexPath, "codex-config", codexPath, "legacy Codex config.toml")
	flags.Parse(args)
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	home, err := os.UserHomeDir()
	if err != nil || requireStandaloneRemovalInactiveForInit(home) != nil {
		fatal(handoff.ErrNativeRemovalRecoveryRequired)
	}
	backup, err := codexconfig.MigrateLegacy(codexPath)
	if err != nil {
		fatal(err)
	}
	fmt.Printf("legacy_codex_routing=migrated backup=%s\n", backup)
}

func lifecycleCommand(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	switch args[0] {
	case "source-install-capability":
		flags := newModeFlagSet("lifecycle source-install-capability")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		flags.Parse(args[1:])
		if flags.NArg() != 0 || !*jsonOutput {
			fatal(lifecyclelock.ErrReservationUnsafe)
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
			"schema_version": 2,
			"state":          "ready",
		}); err != nil {
			fatal(err)
		}
	case "reserve-source-install":
		flags := newModeFlagSet("lifecycle reserve-source-install")
		scopeValue := flags.String("scope", "", "source installation scope")
		recoveryPath := flags.String("recovery-file", "", "durable private reservation response")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		flags.Parse(args[1:])
		if flags.NArg() != 0 || !*jsonOutput {
			fatal(lifecyclelock.ErrReservationUnsafe)
		}
		reservation, err := lifecyclelock.ReserveSourceInstall(
			ctx, home, lifecyclelock.SourceInstallScope(*scopeValue), *recoveryPath,
		)
		if err != nil {
			fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(reservation); err != nil {
			fatal(err)
		}
	case "release-source-install":
		flags := newModeFlagSet("lifecycle release-source-install")
		scopeValue := flags.String("scope", "", "source installation scope")
		token := flags.String("token", "", "reservation token")
		removeCreatedRoot := flags.Bool("remove-created-root", false, "remove a newly created empty install root")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		flags.Parse(args[1:])
		if flags.NArg() != 0 || !*jsonOutput {
			fatal(lifecyclelock.ErrReservationUnsafe)
		}
		if err := lifecyclelock.ReleaseSourceInstall(
			ctx,
			home,
			lifecyclelock.SourceInstallScope(*scopeValue),
			*token,
			*removeCreatedRoot,
		); err != nil {
			fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(map[string]any{
			"schema_version": 1,
			"state":          "released",
		}); err != nil {
			fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func status(args []string) {
	configPath, codexPath := defaultPaths()
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit the safe routing status JSON contract")
	flags.Parse(args)
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(controller.Status(context.Background()), *jsonOutput)
}

func modeCommand(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "status":
		modeStatus(args[1:])
	case "request":
		modeRequest(args[1:])
	case "seed-native":
		modeSeedNative(args[1:])
	case "verify-native":
		modeVerifyNative(args[1:])
	case "repair-native":
		modeRepairNative(args[1:])
	case "inspect-native-repair":
		modeInspectNativeRepair(args[1:])
	case "inspect-native-repair-owner":
		modeInspectNativeRepairOwner(args[1:])
	case "repair-native-routing":
		modeRepairNativeRouting(args[1:])
	case "apply":
		modeApply(args[1:])
	case "cancel":
		modeCancel(args[1:])
	case "recover":
		modeRecover(args[1:])
	case "discover-open-codex":
		modeDiscoverOpenCodex(args[1:])
	case "discover-open-codex-native":
		modeDiscoverOpenCodexNative(args[1:])
	case "inspect-open-codex-native-removal":
		modeInspectOpenCodexNativeRemoval(args[1:])
	case "inspect-open-codex-native-data":
		modeInspectOpenCodexNativeData(args[1:])
	case "remove-open-codex-native":
		modeRemoveOpenCodexNative(args[1:])
	case "inspect-open-codex-data":
		modeInspectOpenCodexData(args[1:])
	case "remove-open-codex":
		modeRemoveOpenCodex(args[1:])
	case "handoff":
		modeHandoff(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

type modeFlagSet struct {
	*flag.FlagSet
}

func newModeFlagSet(name string) *modeFlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return &modeFlagSet{FlagSet: flags}
}

func (f *modeFlagSet) Parse(args []string) {
	if err := f.FlagSet.Parse(args); err != nil {
		fatal(err)
	}
}

func acquireUserLifecycleLock(parent context.Context) *lifecyclelock.Lock {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	lock, err := lifecyclelock.AcquireWriter(
		ctx,
		home,
		os.Getenv(lifecyclelock.SourceInstallReservationEnvironment),
	)
	if err != nil {
		fatal(err)
	}
	return lock
}

func requireStandaloneRemovalInactiveForInit(home string) error {
	anchor, err := handoff.StandaloneRemovalAnchorPath(home)
	if err != nil {
		return handoff.ErrNativeRemovalRecoveryRequired
	}
	_, exists, err := handoff.ReadRemovalCleanup(anchor)
	if err != nil || exists {
		return handoff.ErrNativeRemovalRecoveryRequired
	}
	return nil
}

func modeSeedNative(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode seed-native")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(errorsNew("mode seed-native accepts flags only"))
	}
	cfg, err := config.Load(configPath)
	if err != nil || cfg.Scope() != config.InstallationScopeLocalDevelopment {
		fatal(errorsNew("mode seed-native is reserved for a local_development relay config"))
	}
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	result, err := controller.SeedNativeParked(context.Background())
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(result, *jsonOutput)
}

func modeVerifyNative(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode verify-native")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(errorsNew("mode verify-native accepts flags only"))
	}
	controller, err := localDevelopmentRoutingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	result, err := controller.VerifyNative(context.Background())
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(result, *jsonOutput)
}

func modeRepairNative(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode repair-native")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	expectedGeneration := flags.Uint64("expected-routing-generation", 0, "exact recovery generation shown to the user")
	confirmed := flags.Bool("confirm-local-development-native-repair", false, "confirm repair after native routing ownership is independently verified")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 || *expectedGeneration == 0 || !*confirmed {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	controller, err := localDevelopmentRoutingController(configPath, codexPath)
	if err != nil {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	result, err := controller.RepairNative(context.Background(), *expectedGeneration, *confirmed)
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(result, *jsonOutput)
}

func modeInspectNativeRepair(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode inspect-native-repair")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	expectedGeneration := flags.Uint64("expected-routing-generation", 0, "exact recovery generation shown to the user")
	jsonOutput := flags.Bool("json", false, "emit bounded JSON")
	flags.Parse(args)
	if flags.NArg() != 0 || *expectedGeneration == 0 || !*jsonOutput {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	controller, err := localDevelopmentRoutingController(configPath, codexPath)
	if err != nil {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	result, err := controller.InspectNativeRepair(context.Background(), *expectedGeneration)
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fatal(err)
	}
}

func modeInspectNativeRepairOwner(args []string) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode inspect-native-repair-owner")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	expectedGeneration := flags.Uint64("expected-routing-generation", 0, "exact recovery generation shown to the user")
	expectedOwnerValue := flags.String("expected-owner", "", "bounded routing owner")
	installationID := flags.String("installation-id", "", "opaque OpenCodex installation ID")
	installationFingerprint := flags.String("installation-fingerprint", "", "opaque OpenCodex installation fingerprint")
	nativeRestoreFingerprint := flags.String("native-restore-fingerprint", "", "verified native restore execution fingerprint")
	ocxExecutable := flags.String("ocx-executable", "", "exact absolute OpenCodex executable")
	ocxSHA256 := flags.String("ocx-sha256", "", "selected OpenCodex SHA-256")
	jsonOutput := flags.Bool("json", false, "emit bounded JSON")
	flags.Parse(args)
	if flags.NArg() != 0 || *expectedGeneration == 0 || *expectedOwnerValue != string(codexconfig.NativeRepairOpenCodex) ||
		*installationID == "" || *installationFingerprint == "" || *nativeRestoreFingerprint == "" ||
		*ocxExecutable == "" || *ocxSHA256 == "" || !*jsonOutput {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	controller, err := localDevelopmentRoutingController(configPath, codexPath)
	if err != nil {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resolver, candidate, session, err := openNativeRestoreSession(ctx, configPath, codexPath, handoff.NativeRestoreSelection{
		InstallationID:           *installationID,
		InstallationFingerprint:  *installationFingerprint,
		NativeRestoreFingerprint: *nativeRestoreFingerprint,
		Executable:               *ocxExecutable,
		ExecutableSHA256:         *ocxSHA256,
	})
	if err != nil {
		fatal(mapNativeOwnerError(err))
	}
	defer session.Close()
	result, err := controller.InspectNativeRepairOwner(ctx, *expectedGeneration, codexconfig.NativeRepairOpenCodex, func(ctx context.Context) (routing.NativeRepairOwnerStatus, error) {
		if revalidateErr := resolver.Revalidate(ctx, candidate); revalidateErr != nil {
			return routing.NativeRepairOwnerStatus{}, mapNativeOwnerError(revalidateErr)
		}
		return routingOwnerStatus(session.Inspect(ctx)), nil
	})
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fatal(err)
	}
}

func modeRepairNativeRouting(args []string) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode repair-native-routing")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	expectedGeneration := flags.Uint64("expected-routing-generation", 0, "exact recovery generation shown to the user")
	expectedOwnerValue := flags.String("expected-owner", "", "bounded routing owner observed by inspect-native-repair")
	confirmedDesktop := flags.Bool("confirm-desktop-exited", false, "confirm the selected Codex Desktop app has exited")
	confirmedRepair := flags.Bool("confirm-local-development-native-routing-repair", false, "confirm owner-scoped TOML repair")
	installationID := flags.String("installation-id", "", "opaque OpenCodex installation ID")
	installationFingerprint := flags.String("installation-fingerprint", "", "opaque OpenCodex installation fingerprint")
	nativeRestoreFingerprint := flags.String("native-restore-fingerprint", "", "verified native restore execution fingerprint")
	ocxExecutable := flags.String("ocx-executable", "", "exact absolute OpenCodex executable selected by the user")
	ocxSHA256 := flags.String("ocx-sha256", "", "SHA-256 fingerprint observed for the selected OpenCodex executable")
	jsonOutput := flags.Bool("json", false, "emit bounded JSON")
	flags.Parse(args)
	if flags.NArg() != 0 || *expectedGeneration == 0 || !*confirmedDesktop || !*confirmedRepair || !*jsonOutput {
		fatal(routing.ErrNativeRepairUnavailable)
	}
	expectedOwner := codexconfig.NativeRepairKind(*expectedOwnerValue)
	switch expectedOwner {
	case codexconfig.NativeRepairLocalRelay:
		if *installationID != "" || *installationFingerprint != "" || *nativeRestoreFingerprint != "" ||
			*ocxExecutable != "" || *ocxSHA256 != "" {
			fatal(routing.ErrNativeRepairUnavailable)
		}
	case codexconfig.NativeRepairOpenCodex:
		if *installationID == "" || *installationFingerprint == "" || *nativeRestoreFingerprint == "" ||
			*ocxExecutable == "" || *ocxSHA256 == "" {
			fatal(routing.ErrNativeRepairUnavailable)
		}
	default:
		fatal(routing.ErrNativeRepairUnavailable)
	}
	controller, err := localDevelopmentRoutingController(configPath, codexPath)
	if err != nil {
		fatal(routing.ErrNativeRepairUnavailable)
	}

	operationContext, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	operationContext, cancel := context.WithTimeout(operationContext, 85*time.Second)
	defer cancel()
	lifecycle := acquireUserLifecycleLock(operationContext)
	defer lifecycle.Close()
	var inspect routing.NativeRepairOwnerInspect
	var restore routing.NativeRepairOwnerRestore
	if expectedOwner == codexconfig.NativeRepairOpenCodex {
		resolver, candidate, session, openErr := openNativeRestoreSession(operationContext, configPath, codexPath, handoff.NativeRestoreSelection{
			InstallationID:           *installationID,
			InstallationFingerprint:  *installationFingerprint,
			NativeRestoreFingerprint: *nativeRestoreFingerprint,
			Executable:               *ocxExecutable,
			ExecutableSHA256:         *ocxSHA256,
		})
		if openErr != nil {
			fatal(mapNativeOwnerError(openErr))
		}
		defer session.Close()
		inspect = func(ctx context.Context) (routing.NativeRepairOwnerStatus, error) {
			if revalidateErr := resolver.Revalidate(ctx, candidate); revalidateErr != nil {
				return routing.NativeRepairOwnerStatus{}, mapNativeOwnerError(revalidateErr)
			}
			return routingOwnerStatus(session.Inspect(ctx)), nil
		}
		restore = func(ctx context.Context) (routing.NativeRepairOwnerRestoreResult, error) {
			if revalidateErr := resolver.Revalidate(ctx, candidate); revalidateErr != nil {
				return routing.NativeRepairOwnerRestoreResult{}, mapNativeOwnerError(revalidateErr)
			}
			result, restoreErr := session.Execute(ctx)
			return routingOwnerRestoreResult(result), mapNativeOwnerError(restoreErr)
		}
	}
	receipt, err := controller.RepairNativeRouting(
		operationContext, *expectedGeneration, expectedOwner,
		*confirmedRepair, *confirmedDesktop, inspect, restore,
	)
	if err != nil {
		fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(receipt); err != nil {
		fatal(err)
	}
}

func openNativeRestoreSession(
	ctx context.Context,
	configPath string,
	codexPath string,
	selection handoff.NativeRestoreSelection,
) (handoff.DiscoveryNativeRestoreResolver, handoff.NPMInstallation, *handoff.NativeRestoreSession, error) {
	options, err := handoff.ProductionDiscoveryOptions(configPath)
	if err != nil {
		return handoff.DiscoveryNativeRestoreResolver{}, handoff.NPMInstallation{}, nil, err
	}
	resolver := handoff.DiscoveryNativeRestoreResolver{Options: options}
	candidate, err := resolver.Resolve(ctx, selection)
	if err != nil {
		return resolver, handoff.NPMInstallation{}, nil, err
	}
	session, err := (handoff.NativeRestoreExecutor{HomeDir: options.HomeDir}).Open(ctx, candidate, codexPath)
	if err != nil {
		return resolver, handoff.NPMInstallation{}, nil, err
	}
	return resolver, candidate, session, nil
}

func routingOwnerStatus(value handoff.NativeOwnerInspection) routing.NativeRepairOwnerStatus {
	return routing.NativeRepairOwnerStatus{
		Configuration: routing.NativeOwnerConfiguration(value.Configuration),
		Integration:   routing.NativeOwnerIntegration(value.Integration),
		Reason:        routing.NativeOwnerReason(value.Reason),
	}
}

func routingOwnerRestoreResult(value handoff.NativeRestoreResult) routing.NativeRepairOwnerRestoreResult {
	return routing.NativeRepairOwnerRestoreResult{
		Outcome:              routing.NativeRepairOwnerOutcome(value.Outcome),
		NonRoutingIncomplete: value.NonRoutingWarning,
	}
}

func mapNativeOwnerError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, handoff.ErrInvalidNativeRestoreSelection),
		errors.Is(err, handoff.ErrNativeRestoreCandidateMissing),
		errors.Is(err, handoff.ErrNativeRestoreCandidateChanged),
		errors.Is(err, handoff.ErrNativeRestoreProofUnavailable):
		return routing.ErrNativeRepairOwnerChanged
	case errors.Is(err, handoff.ErrUnsafeExecutable):
		return err
	case errors.Is(err, handoff.ErrNativeOwnerConfigurationInvalid):
		return routing.ErrNativeOwnerConfigurationInvalid
	case errors.Is(err, handoff.ErrNativeRestoreFailed):
		return routing.ErrNativeOwnerRestoreFailed
	case errors.Is(err, handoff.ErrNativeRestoreOutput):
		return routing.ErrNativeOwnerResultInvalid
	default:
		return routing.ErrNativeOwnerResultInvalid
	}
}
func modeStatus(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode status")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(errorsNew("mode status accepts no positional arguments"))
	}
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(controller.Status(context.Background()), *jsonOutput)
}

func modeRequest(args []string) {
	if len(args) == 0 {
		fatal(errorsNew("mode request requires native, external, local_opencodex, or relay"))
	}
	target, err := parseBackendRequest(args[0])
	if err != nil {
		fatal(err)
	}
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode request")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	knownLegacyBackupAndMigrate := flags.Bool(
		"known-legacy-backup-and-migrate",
		false,
		"back up and migrate only a documented legacy OpenCodex route during External apply",
	)
	expectedConfigDigest := flags.String(
		"expected-config-digest",
		"",
		"exact gateway config SHA-256 that passed connection validation",
	)
	expectedRoutingGeneration := flags.Uint64(
		"expected-routing-generation",
		0,
		"exact routing generation that passed connection validation",
	)
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args[1:])
	if flags.NArg() != 0 {
		fatal(errorsNew("mode request accepts one mode and flags only"))
	}
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	var result routing.Status
	if *knownLegacyBackupAndMigrate {
		if !routingDigest(*expectedConfigDigest) || *expectedRoutingGeneration == 0 {
			fatal(routing.ErrGatewayRoutingChanged)
		}
		result, err = controller.RequestBackendWithIntentAndWitness(
			context.Background(),
			target,
			true,
			*expectedConfigDigest,
			*expectedRoutingGeneration,
		)
	} else {
		if *expectedConfigDigest != "" || *expectedRoutingGeneration != 0 {
			fatal(routing.ErrGatewayRoutingChanged)
		}
		result, err = controller.RequestBackend(context.Background(), target)
	}
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(result, *jsonOutput)
}

func parseBackendRequest(value string) (routing.Backend, error) {
	switch value {
	case "native":
		return routing.BackendNone, nil
	case "relay", "external":
		return routing.BackendExternal, nil
	case "local_opencodex":
		return routing.BackendLocalOpenCodex, nil
	default:
		return routing.BackendUnknown, errorsNew("mode request requires native, external, local_opencodex, or relay")
	}
}

func modeApply(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode apply")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	confirmed := flags.Bool("confirm-desktop-exited", false, "confirm the registered Codex Desktop app has exited")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(errorsNew("mode apply accepts flags only"))
	}
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	result, err := controller.Apply(context.Background(), *confirmed)
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(result, *jsonOutput)
}

func modeCancel(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode cancel")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(errorsNew("mode cancel accepts flags only"))
	}
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	result, err := controller.Cancel(context.Background())
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(result, *jsonOutput)
}

func modeRecover(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode recover")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	complete := flags.Bool("complete", false, "complete the parked target mode")
	rollback := flags.Bool("rollback", false, "return to the opposite mode")
	expectedGeneration := flags.Uint64("expected-routing-generation", 0, "routing generation shown for a saved OpenCodex removal recovery")
	installationID := flags.String("installation-id", "", "exact saved OpenCodex installation ID for removal recovery")
	installationFingerprint := flags.String("installation-fingerprint", "", "exact saved OpenCodex installation fingerprint for removal recovery")
	confirmed := flags.Bool("confirm-desktop-exited", false, "confirm the registered Codex Desktop app has exited before recovery applies a runtime profile")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 || *complete == *rollback {
		fatal(errorsNew("mode recover requires exactly one of --complete or --rollback"))
	}
	action := routing.RecoveryComplete
	if *rollback {
		action = routing.RecoveryRollback
	}
	lifecycle := acquireUserLifecycleLock(context.Background())
	defer lifecycle.Close()
	preparation, err := prepareRemovalRoutingRecovery(
		context.Background(),
		configPath,
		codexPath,
		action,
		*expectedGeneration,
		handoff.NPMRemovalSelection{
			ID:          *installationID,
			Fingerprint: *installationFingerprint,
		},
	)
	if err != nil {
		fatal(err)
	}
	controller, err := routingControllerWithRecoveryGate(
		configPath,
		codexPath,
		preparation.recoveryGate(configPath),
		routing.WithControllerRemovalRecoveryWitness(preparation.routingWitness),
	)
	if err != nil {
		fatal(err)
	}
	result, err := executeRemovalRoutingRecovery(
		context.Background(),
		configPath,
		codexPath,
		action,
		*confirmed,
		preparation,
		controller.Recover,
		controller.Status,
	)
	if err != nil {
		fatal(err)
	}
	emitRoutingStatus(result, *jsonOutput)
}

type removalRoutingRecoveryPreparation struct {
	recordToken               string
	selection                 handoff.NPMRemovalSelection
	expectedRoutingGeneration uint64
	gateState                 *removalRoutingRecoveryGateState
	routingWitness            *routing.RemovalRecoveryWitness
	alreadyRecovered          bool
}

type removalRoutingRecoveryGateState struct {
	allowedGeneration uint64
}

func (preparation removalRoutingRecoveryPreparation) active() bool {
	return preparation.recordToken != ""
}

func (preparation removalRoutingRecoveryPreparation) matches(configPath string) bool {
	if !preparation.active() {
		return false
	}
	record, exists, err := handoff.ReadRemovalCleanup(configPath)
	if err != nil || !exists || !removalRoutingRecoveryRecordEligible(record) {
		return false
	}
	if record.InstallationID != preparation.selection.ID ||
		record.Fingerprint != preparation.selection.Fingerprint {
		return false
	}
	token, err := removalRoutingRecoveryRecordToken(record)
	return err == nil && token == preparation.recordToken
}

func (preparation removalRoutingRecoveryPreparation) recoveryGate(configPath string) func() error {
	if !preparation.active() {
		return func() error { return handoff.RemovalRoutingGate(configPath) }
	}
	return func() error {
		if preparation.matches(configPath) && preparation.routingGenerationMatches(configPath) {
			return nil
		}
		// An active preparation is a one-record capability. Disappearance or
		// replacement must revoke it even when the replacement would not
		// independently activate the ordinary removal gate.
		return handoff.ErrRemovalRoutingGate
	}
}

func (preparation removalRoutingRecoveryPreparation) routingGenerationMatches(configPath string) bool {
	if preparation.gateState == nil || preparation.gateState.allowedGeneration == 0 {
		return false
	}
	store, err := routing.Open(configPath)
	if err != nil {
		return false
	}
	state, legacy, err := store.Read()
	_, transactionErr := store.HasPendingTransaction()
	return err == nil && !legacy && transactionErr == nil &&
		state.Generation == preparation.gateState.allowedGeneration
}

func (preparation removalRoutingRecoveryPreparation) release(configPath string) error {
	if !preparation.active() || !preparation.matches(configPath) {
		return handoff.ErrRemovalCleanupUnsafe
	}
	_, err := handoff.ReleaseRemovalRoutingGateForRecovery(configPath)
	return err
}

func removalRoutingRecoveryRecordEligible(record handoff.RemovalCleanupRecord) bool {
	if record.ActiveExecution != nil || record.ExecutionResolution != "" || record.FinalizationActive {
		return false
	}
	if record.RecoveryPending {
		return true
	}
	return record.Phase == handoff.RemovalCleanupPhasePackageVerified &&
		!record.RoutingRecoveryReleased
}

func removalRoutingRecoveryRecordToken(record handoff.RemovalCleanupRecord) (string, error) {
	payload, err := json.Marshal(record)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

func newRemovalRoutingRecoveryPreparation(
	record handoff.RemovalCleanupRecord,
	selection handoff.NPMRemovalSelection,
) (removalRoutingRecoveryPreparation, error) {
	if !removalRoutingRecoveryRecordEligible(record) ||
		record.InstallationID != selection.ID ||
		record.Fingerprint != selection.Fingerprint {
		return removalRoutingRecoveryPreparation{}, handoff.ErrRemovalCleanupUnsafe
	}
	token, err := removalRoutingRecoveryRecordToken(record)
	if err != nil {
		return removalRoutingRecoveryPreparation{}, handoff.ErrRemovalCleanupUnsafe
	}
	return removalRoutingRecoveryPreparation{
		recordToken: token,
		selection:   selection,
	}, nil
}

func prepareRemovalRoutingRecovery(
	ctx context.Context,
	configPath string,
	codexPath string,
	action routing.RecoveryAction,
	expectedGeneration uint64,
	selection handoff.NPMRemovalSelection,
) (removalRoutingRecoveryPreparation, error) {
	if err := handoff.RemovalRoutingGate(configPath); err == nil {
		if expectedGeneration != 0 || selection.ID != "" || selection.Fingerprint != "" {
			return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
		}
		return removalRoutingRecoveryPreparation{}, nil
	}
	record, exists, err := handoff.ReadRemovalCleanup(configPath)
	if err != nil {
		return removalRoutingRecoveryPreparation{}, err
	}
	if !exists || !handoff.RemovalRoutingGateReleasable(configPath) ||
		!removalRoutingRecoveryRecordEligible(record) {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	if action != routing.RecoveryComplete {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	if expectedGeneration == 0 {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	preparation, err := newRemovalRoutingRecoveryPreparation(record, selection)
	if err != nil {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	store := controller.Store()
	lock, err := store.Lock(ctx)
	if err != nil {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	defer lock.Close()
	if !preparation.matches(configPath) {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	witness, err := controller.CaptureRemovalRecoveryWitness(lock)
	if err != nil || witness.Generation() != expectedGeneration {
		return removalRoutingRecoveryPreparation{}, routing.ErrRecoveryRequired
	}
	preparation.expectedRoutingGeneration = expectedGeneration
	preparation.gateState = &removalRoutingRecoveryGateState{allowedGeneration: expectedGeneration}
	preparation.routingWitness = witness
	preparation.alreadyRecovered = witness.AlreadyRecovered()
	return preparation, nil
}

func safeRemovalRoutingRecoveryState(state routing.State) bool {
	if state.Generation == 0 ||
		state.DesiredBackend == routing.BackendUnknown ||
		state.DesiredBackend == routing.BackendLocalOpenCodex ||
		state.AppliedBackend == routing.BackendUnknown ||
		state.AppliedBackend == routing.BackendLocalOpenCodex {
		return false
	}
	if state.Phase == routing.PhaseRecoveryRequired {
		// A failed Complete attempt may durably park with different safe
		// desired/applied backends. Keep the removal token retryable, but never
		// admit Unknown or Local OpenCodex as either side.
		return true
	}
	return (state.Phase == routing.PhaseRelayActive || state.Phase == routing.PhaseNativeActive) &&
		state.DesiredMode == state.AppliedMode &&
		state.DesiredBackend == state.AppliedBackend
}

func executeRemovalRoutingRecovery(
	ctx context.Context,
	configPath string,
	codexPath string,
	action routing.RecoveryAction,
	desktopExited bool,
	preparation removalRoutingRecoveryPreparation,
	recover func(context.Context, routing.RecoveryAction, bool) (routing.Status, error),
	status func(context.Context) routing.Status,
) (routing.Status, error) {
	if recover == nil || status == nil {
		return routing.Status{}, routing.ErrRecoveryRequired
	}
	if preparation.active() && action != routing.RecoveryComplete {
		return routing.Status{}, routing.ErrRecoveryRequired
	}
	if preparation.active() && !desktopExited {
		return routing.Status{}, routing.ErrDesktopExitConfirmation
	}
	result := routing.Status{}
	if !preparation.alreadyRecovered {
		var err error
		result, err = recover(ctx, action, desktopExited)
		if err != nil {
			return result, err
		}
		if preparation.active() {
			if preparation.gateState == nil ||
				preparation.routingWitness == nil ||
				result.Generation <= preparation.expectedRoutingGeneration ||
				preparation.routingWitness.Generation() != result.Generation ||
				!preparation.routingWitness.AlreadyRecovered() {
				return routing.Status{}, routing.ErrRecoveryRequired
			}
			// Controller.Recover may build its return status while the old
			// generation-bound token is still gating. Its validated projection
			// nevertheless carries the newly committed durable generation.
			// Bind the one-record capability to that generation before the
			// lock-bound final status/ownership proof.
			preparation.gateState.allowedGeneration = result.Generation
		}
	}
	if !preparation.active() {
		return result, nil
	}
	verified, err := releaseRecoveredRemovalRoutingToken(
		ctx,
		configPath,
		codexPath,
		preparation,
		status,
	)
	if err != nil {
		return routing.Status{}, err
	}
	// Return the exact fresh status proven under the routing writer lock. The
	// active one-record gate deliberately revokes itself after journal release,
	// so a second call through that controller would be fail-closed again.
	return verified, nil
}

func releaseRecoveredRemovalRoutingToken(
	ctx context.Context,
	configPath string,
	codexPath string,
	preparation removalRoutingRecoveryPreparation,
	status func(context.Context) routing.Status,
) (routing.Status, error) {
	if !preparation.active() || status == nil {
		return routing.Status{}, routing.ErrRecoveryRequired
	}
	store, err := routing.Open(configPath)
	if err != nil {
		return routing.Status{}, routing.ErrRecoveryRequired
	}
	lock, err := store.Lock(ctx)
	if err != nil {
		return routing.Status{}, routing.ErrRecoveryRequired
	}
	fail := func(cause error) (routing.Status, error) {
		_ = lock.Close()
		return routing.Status{}, cause
	}
	if preparation.routingWitness == nil ||
		preparation.routingWitness.ValidateStable(lock) != nil {
		return fail(routing.ErrRecoveryRequired)
	}
	state, legacy, err := store.Read()
	pendingTransaction, transactionErr := store.HasPendingTransaction()
	committedGeneration := preparation.expectedRoutingGeneration
	if !preparation.alreadyRecovered {
		if preparation.gateState == nil ||
			preparation.gateState.allowedGeneration <= preparation.expectedRoutingGeneration {
			return fail(routing.ErrRecoveryRequired)
		}
		committedGeneration = preparation.gateState.allowedGeneration
	}
	if err != nil || legacy || state.ValidateForCodexConfig(configPath, codexPath) != nil ||
		transactionErr != nil || pendingTransaction ||
		state.Generation != committedGeneration ||
		!safeRemovalRoutingRecoveryState(state) ||
		(state.Phase != routing.PhaseRelayActive && state.Phase != routing.PhaseNativeActive) {
		return fail(routing.ErrRecoveryRequired)
	}
	verified := status(ctx)
	if !safeForOpenCodexUninstall(verified) ||
		verified.Generation != state.Generation ||
		verified.DesiredMode != state.DesiredMode ||
		verified.AppliedMode != state.AppliedMode ||
		verified.DesiredBackend != state.DesiredBackend ||
		verified.AppliedBackend != state.AppliedBackend ||
		verified.Phase != state.Phase ||
		verified.Connection.LocalRelay != routing.LocalRelayHealthy ||
		!verified.RelayRunning ||
		verified.Connection.RoutingSync != routing.RoutingSyncAcknowledged {
		return fail(routing.ErrRecoveryRequired)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		return fail(routing.ErrRecoveryRequired)
	}
	owner, err := codexconfig.OwnerForID(cfg.Scope())
	if err != nil {
		return fail(routing.ErrRecoveryRequired)
	}
	ownership, err := validateHandoffResultOwnership(codexPath, owner, cfg)
	if err != nil || !handoffOwnershipMatchesAppliedBackend(ownership, verified.AppliedBackend) {
		return fail(routing.ErrRecoveryRequired)
	}
	if err := preparation.release(configPath); err != nil {
		return fail(err)
	}
	if err := lock.Close(); err != nil {
		return routing.Status{}, err
	}
	return verified, nil
}

func modeDiscoverOpenCodex(args []string) {
	configPath, _ := defaultPaths()
	flags := newModeFlagSet("mode discover-open-codex")
	tierValue := flags.String("tier", string(handoff.DiscoveryTierA), "discovery tier: a, b, or c")
	confirmedBroadScan := flags.Bool("confirm-broad-scan", false, "confirm the bounded Tier C scan")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path used only for exact enrollment discovery")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(errorsNew("mode discover-open-codex accepts flags only"))
	}
	tier := handoff.DiscoveryTier(*tierValue)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	displayOptions := handoff.DiscoveryOptions{
		Tier:              tier,
		RelayConfigPath:   configPath,
		BroadScanApproved: *confirmedBroadScan,
	}
	authorityOptions, authorityOptionsErr := handoff.ProductionDiscoveryOptions(configPath)
	var result handoff.DiscoveryResult
	var err error
	if authorityOptionsErr != nil {
		result, err = handoff.DiscoverNPMInstallations(ctx, displayOptions)
		for index := range result.Candidates {
			result.Candidates[index].RemovalAuthority = handoff.RemovalAuthorityManual
		}
	} else {
		authorityOptions.GOOS = runtime.GOOS
		authorityOptions.GOARCH = runtime.GOARCH
		result, err = handoff.DiscoverNPMInstallationsWithAuthority(ctx, displayOptions, authorityOptions)
	}
	if err != nil {
		fatal(err)
	}
	if !*jsonOutput {
		fmt.Printf("opencodex_discovery=tier_%s candidates=%d rejected=%d truncated=%t\n", result.RequestedTier, len(result.Candidates), result.Rejected, result.Truncated)
		return
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		fatal(err)
	}
}

// modeHandoff is the only relayctl path that invokes OpenCodex lifecycle
// commands. It has no PATH lookup and needs both a Desktop-exit confirmation
// and a separate OpenCodex-handoff confirmation. It deliberately does not
// apply a route or relaunch Desktop: the user still chooses Local/External/
// Native through the normal request -> apply boundary afterwards.
func preflightHandoffRemovalGate(configPath string) error {
	_, exists, err := handoff.ReadRemovalCleanup(configPath)
	if err != nil || exists {
		return errorsNew("complete or reconcile OpenCodex removal before changing OpenCodex ownership")
	}
	return nil
}

func parseHandoffAction(value string) (handoff.Action, error) {
	action := handoff.Action(value)
	switch action {
	case handoff.RetainProxyRemoveShim, handoff.RetainProxyKeepShim:
		return action, nil
	default:
		return "", handoff.ErrInvalidAction
	}
}

func modeHandoff(args []string) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		fatal(errorsNew("OpenCodex Local profile handoff is supported only on macOS Apple Silicon"))
	}
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("mode handoff")
	ocxExecutable := flags.String("ocx-executable", "", "exact absolute OpenCodex executable selected by the user")
	ocxSHA256 := flags.String("ocx-sha256", "", "SHA-256 fingerprint observed for the selected OpenCodex executable")
	action := flags.String("action", "", "approved OpenCodex handoff action")
	confirmedHandoff := flags.Bool("confirm-opencodex-handoff", false, "confirm the selected OpenCodex handoff action")
	confirmedDesktop := flags.Bool("confirm-desktop-exited", false, "confirm the selected Codex Desktop app has exited")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	flags.Parse(args)
	if flags.NArg() != 0 || *ocxExecutable == "" || *ocxSHA256 == "" || *action == "" || !*confirmedHandoff || !*confirmedDesktop {
		fatal(errorsNew("mode handoff requires --ocx-executable, --ocx-sha256, --action, --confirm-opencodex-handoff, and --confirm-desktop-exited"))
	}
	selected, err := parseHandoffAction(*action)
	if err != nil {
		fatal(err)
	}
	// Reject an unsafe selected Codex leaf before taking the routing lock or
	// evaluating any handoff precondition. An OCX action can be irreversible,
	// so a directory or symlink must not reach a later post-action ownership
	// check that would only be able to park the relay for recovery.
	if err := preflightHandoffCodexConfig(codexPath); err != nil {
		fatal(err)
	}
	lifecycleContext, cancelLifecycle := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelLifecycle()
	home, err := os.UserHomeDir()
	if err != nil {
		fatal(err)
	}
	lifecycle, err := lifecyclelock.AcquireWriter(lifecycleContext, home, "")
	if err != nil {
		fatal(err)
	}
	defer lifecycle.Close()
	// Handoff writes enrollment/configuration beside the routing state, so it
	// participates in the same single-writer lock as request/apply/recovery.
	// A legacy/no-state relay has no durable Codex binding to protect; require
	// the v2 controller to be initialized before an irreversible OCX action.
	handoffStore, err := routing.Open(configPath)
	if err != nil {
		fatal(errorsNew("routing state cannot be opened for OpenCodex handoff"))
	}
	handoffLock, err := handoffStore.Lock(context.Background())
	if err != nil {
		fatal(errorsNew("routing state is busy; retry OpenCodex handoff after the current routing operation"))
	}
	defer handoffLock.Close()
	durableState, legacy, err := handoffStore.Read()
	if err != nil || legacy || durableState.ValidateForCodexConfig(configPath, codexPath) != nil {
		fatal(errorsNew("initialize and verify the v2 routing state before changing OpenCodex ownership"))
	}
	// A complete handoff has no authority to resolve an interrupted routing
	// transaction.  Check this while holding the same writer lock so a stale or
	// malformed journal cannot be mistaken for the ordinary watcher parking
	// caused by an existing OpenCodex Shim.
	if err := preflightHandoffRoutingState(handoffStore, durableState); err != nil {
		fatal(err)
	}
	if err := preflightHandoffRemovalGate(configPath); err != nil {
		fatal(err)
	}
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	before := controller.Status(context.Background())
	if before.Connection.LocalRelay != routing.LocalRelayHealthy || !before.RelayRunning {
		fatal(errorsNew("resident relay health is required before changing OpenCodex ownership"))
	}
	// An installed OpenCodex Shim legitimately makes the relay watcher report
	// routing_sync=invalid: it sees a foreign root override and parks admission
	// before this explicit, Desktop-stopped handoff can invoke `ocx restore`.
	// The durable v2 state, journal-free lock, and healthy resident relay above
	// are still required.  Pending/unreachable synchronization remains unsafe.
	if before.Connection.RoutingSync != routing.RoutingSyncAcknowledged && before.Connection.RoutingSync != routing.RoutingSyncInvalid {
		fatal(errorsNew("resident relay routing synchronization is not stable enough for OpenCodex handoff"))
	}
	if err := handoff.PreflightRelayConfig(configPath); err != nil {
		fatal(errorsNew("relay configuration is not safe for OpenCodex handoff"))
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fatal(errorsNew("relay configuration is unavailable for OpenCodex handoff"))
	}
	owner, err := codexconfig.OwnerForID(cfg.Scope())
	if err != nil {
		fatal(errorsNew("relay configuration has an unsupported routing ownership scope"))
	}
	if cfg.UpstreamMode != config.UpstreamModeExternalGateway {
		fatal(errorsNew("OpenCodex local enrollment requires an External gateway canonical relay configuration"))
	}
	if err := handoff.PreflightRecordWrite(configPath); err != nil {
		fatal(errorsNew("OpenCodex enrollment receipt cannot be written safely"))
	}
	// Resolve and validate the complete Local profile before invoking an OCX
	// lifecycle action. An invalid custom Codex path or profile must not be
	// discovered only after `ocx restore` has already changed its own state.
	profile := cfg.LocalOpenCodex
	if profile == nil {
		catalogName := "opencodex-relay-local-catalog.json"
		if cfg.Scope() == config.InstallationScopeLocalDevelopment {
			catalogName = config.LocalDevelopmentLocalCatalog
		}
		profile, err = config.NewLocalOpenCodexProfileForCodexConfigWithCatalogName(codexPath, catalogName)
		if err != nil {
			fatal(errorsNew("Local OpenCodex catalog path cannot be bound to the selected Codex configuration"))
		}
	}
	candidate := cfg
	candidate.LocalOpenCodex = profile
	if _, err := candidate.LocalOpenCodexRuntimeConfig(); err != nil {
		fatal(errorsNew("Local OpenCodex profile is invalid for the canonical relay configuration"))
	}
	// Validate every relay-owned precondition before invoking the selected OCX
	// binary. In particular, a legacy Linux static local_opencodex installation
	// must fail before this macOS-only enrollment flow reaches OCX.
	handoffContext, stopHandoff := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopHandoff()
	handoffContext, cancelHandoff := context.WithTimeout(handoffContext, 75*time.Second)
	defer cancelHandoff()
	// Recheck at the irreversible boundary. The first check deliberately
	// happens before any relay state work; this one narrows the time between
	// validation and the first OCX invocation without changing ownership.
	record, err := executeHandoffAfterRoutingPreflight(
		handoffContext, handoffStore, durableState, codexPath,
		*ocxExecutable, *ocxSHA256, selected, handoff.Executor{},
	)
	if err != nil {
		// A second leaf check can fail only if the selected path changed after
		// the early no-mutation preflight. It did not invoke OCX, so it must not
		// be misclassified as an OCX failure and park the durable route.
		if errors.Is(err, errHandoffCodexConfigPreflight) {
			fatal(err)
		}
		// OCX owns its own rollback semantics; a failed command can still have
		// changed its files before reporting an error. Park the relay-owned
		// route until explicit recovery rather than continuing on an ownership
		// assumption that can no longer be proved.
		if parkErr := parkHandoffForRecovery(handoffLock, durableState); parkErr != nil {
			fatal(errorsNew("OpenCodex handoff failed and routing recovery could not be persisted"))
		}
		fatal(err)
	}
	// OpenCodex remains the only component allowed to mutate its own files.
	// The post-action configuration must be either native or an exact,
	// marker-owned relay profile.  It must also still agree with the durable
	// applied backend; a successful `ocx restore` that leaves native TOML while
	// state says External is useful cleanup, but not evidence that the live
	// route was safely switched.  Park it for the normal explicit recovery /
	// Desktop-restart boundary instead of rewriting either owner's TOML here.
	ownership, ownershipErr := validateHandoffResultOwnership(codexPath, owner, cfg)
	if ownershipErr != nil || !handoffOwnershipMatchesAppliedBackend(ownership, durableState.AppliedBackend) {
		if parkErr := parkHandoffForRecovery(handoffLock, durableState); parkErr != nil {
			fatal(errorsNew("OpenCodex handoff changed routing ownership and recovery could not be persisted"))
		}
		fatal(errorsNew("OpenCodex handoff changed Codex routing ownership; relay remains parked until explicit recovery"))
	}
	if result := localopencodex.Preflight(context.Background(), profile.UpstreamBaseURL); !result.Ready() {
		fatal(errorsNew("Local OpenCodex identity or model catalog is unavailable; External gateway remains the only relay profile"))
	}
	original := cfg.LocalOpenCodex
	cfg.LocalOpenCodex = profile
	if err := config.Write(configPath, cfg); err != nil {
		fatal(err)
	}
	if err := handoff.WriteRecord(configPath, record); err != nil {
		// The relay-owned config write is the only part of this handoff that
		// we can safely roll back. OpenCodex itself remains owned by `ocx`.
		cfg.LocalOpenCodex = original
		_ = config.Write(configPath, cfg)
		fatal(err)
	}
	emitRoutingStatus(controller.Status(context.Background()), *jsonOutput)
}

type handoffExecutor interface {
	ExecuteExpected(context.Context, string, string, handoff.Action, bool) (handoff.Record, error)
}

func preflightHandoffRoutingState(store *routing.Store, state routing.State) error {
	if store == nil {
		return routing.ErrRecoveryRequired
	}
	if pending, err := store.HasPendingTransaction(); err != nil || pending {
		return routing.ErrRecoveryRequired
	}
	if state.Phase == routing.PhaseApplying ||
		state.Phase == routing.PhaseRecoveryRequired ||
		state.AppliedBackend == routing.BackendUnknown {
		return routing.ErrRecoveryRequired
	}
	return nil
}

func executeHandoffAfterRoutingPreflight(
	ctx context.Context,
	store *routing.Store,
	state routing.State,
	codexPath, executable, fingerprint string,
	action handoff.Action,
	executor handoffExecutor,
) (handoff.Record, error) {
	if err := preflightHandoffRoutingState(store, state); err != nil {
		return handoff.Record{}, err
	}
	return executeHandoff(ctx, codexPath, executable, fingerprint, action, executor)
}

// executeHandoff keeps the leaf validation adjacent to the irreversible OCX
// invocation. The interface is deliberately private and narrow so tests can
// prove an unsafe Codex leaf reaches no OpenCodex command.
func executeHandoff(ctx context.Context, codexPath, executable, fingerprint string, action handoff.Action, executor handoffExecutor) (handoff.Record, error) {
	if err := preflightHandoffCodexConfig(codexPath); err != nil {
		return handoff.Record{}, err
	}
	if executor == nil {
		return handoff.Record{}, errorsNew("OpenCodex handoff executor is unavailable")
	}
	return executor.ExecuteExpected(ctx, executable, fingerprint, action, true)
}

// preflightHandoffCodexConfig keeps the handoff command's failure surface
// bounded while proving that the user-selected Codex config leaf is absent or
// a regular non-symlink file. It must run before every OCX handoff sequence;
// unlike Enable preflight, it never interprets or rejects a user's routing.
func preflightHandoffCodexConfig(path string) error {
	if err := codexconfig.PreflightCodexConfigPath(path); err != nil {
		return errHandoffCodexConfigPreflight
	}
	return nil
}

// handoffCodexOwnership is deliberately bounded: mode handoff never records
// an upstream URL or TOML value, and it does not infer a route from a foreign
// configuration.  It is used only to prove the post-OCX configuration shape
// against the already validated canonical relay profiles.
type handoffCodexOwnership uint8

const (
	handoffCodexOwnershipUnknown handoffCodexOwnership = iota
	handoffCodexOwnershipNative
	handoffCodexOwnershipExternal
	handoffCodexOwnershipLocalOpenCodex
)

// validateHandoffResultOwnership accepts only native Codex routing or an
// exact relay-owned External/Local profile.  In particular, merely retaining
// relay markers is insufficient: listener and catalog bindings must match one
// and only one profile.  This function intentionally performs no TOML write.
func validateHandoffResultOwnership(codexPath string, owner codexconfig.Owner, cfg config.Config) (handoffCodexOwnership, error) {
	if err := codexconfig.ValidateNativeRoutingForOwner(codexPath, owner); err == nil {
		return handoffCodexOwnershipNative, nil
	}

	type candidate struct {
		ownership handoffCodexOwnership
		config    config.Config
	}
	candidates := []candidate{{ownership: handoffCodexOwnershipExternal, config: cfg}}
	if cfg.LocalOpenCodex != nil {
		local, err := cfg.LocalOpenCodexRuntimeConfig()
		if err != nil {
			return handoffCodexOwnershipUnknown, errorsNew("Local OpenCodex profile cannot prove post-handoff routing ownership")
		}
		candidates = append(candidates, candidate{ownership: handoffCodexOwnershipLocalOpenCodex, config: local})
	}

	matched := handoffCodexOwnershipUnknown
	for _, candidate := range candidates {
		if err := codexconfig.ValidateManagedRoutingForOwner(
			codexPath,
			owner,
			"http://"+candidate.config.ListenAddress+"/v1",
			"http://"+candidate.config.Responses.Scheduler.InteractiveListenAddress+"/v1",
			candidate.config.Catalog.Path,
		); err != nil {
			continue
		}
		if matched != handoffCodexOwnershipUnknown {
			return handoffCodexOwnershipUnknown, errorsNew("post-handoff Codex routing matches multiple relay profiles")
		}
		matched = candidate.ownership
	}
	if matched == handoffCodexOwnershipUnknown {
		return handoffCodexOwnershipUnknown, errorsNew("post-handoff Codex routing is neither native nor an exact relay profile")
	}
	return matched, nil
}

func handoffOwnershipMatchesAppliedBackend(ownership handoffCodexOwnership, backend routing.Backend) bool {
	switch ownership {
	case handoffCodexOwnershipNative:
		return backend == routing.BackendNone
	case handoffCodexOwnershipExternal:
		return backend == routing.BackendExternal
	case handoffCodexOwnershipLocalOpenCodex:
		return backend == routing.BackendLocalOpenCodex
	default:
		return false
	}
}

func safeForOpenCodexUninstall(status routing.Status) bool {
	if status.Phase != routing.PhaseRelayActive && status.Phase != routing.PhaseNativeActive {
		return false
	}
	return status.DesiredBackend != routing.BackendLocalOpenCodex && status.AppliedBackend != routing.BackendLocalOpenCodex &&
		status.DesiredBackend != routing.BackendUnknown && status.AppliedBackend != routing.BackendUnknown
}

// parkHandoffForRecovery persists the fail-closed recovery gate while the
// routing writer lock is held. Callers must surface a save failure rather than
// claiming that admission was parked when the durable state was unchanged.
func parkHandoffForRecovery(lock *routing.Lock, state routing.State) error {
	if lock == nil {
		return errorsNew("routing recovery lock is unavailable")
	}
	recovery := state
	recovery.Phase = routing.PhaseRecoveryRequired
	recovery.Generation++
	return lock.Save(recovery)
}

func emitRoutingStatus(status routing.Status, jsonOutput bool) {
	if jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(status); err != nil {
			fatal(err)
		}
		return
	}
	activeRequests := "unknown"
	if status.ActiveRequests != nil {
		activeRequests = fmt.Sprintf("%d", *status.ActiveRequests)
	}
	fmt.Printf(
		"desired_mode=%s applied_mode=%s desired_backend=%s applied_backend=%s phase=%s relay_running=%t relay_admission=%s catalog_refresh=%s active_requests=%s local_relay=%s local_opencodex=%s routing_sync=%s remote_gateway=%s catalog=%s desktop_restart_required=%t desktop_effective_mode=%s\n",
		status.DesiredMode,
		status.AppliedMode,
		status.DesiredBackend,
		status.AppliedBackend,
		status.Phase,
		status.RelayRunning,
		status.RelayAdmission,
		status.CatalogRefresh,
		activeRequests,
		status.Connection.LocalRelay,
		status.Connection.LocalOpenCodex,
		status.Connection.RoutingSync,
		status.Connection.RemoteGateway,
		status.Connection.Catalog,
		status.DesktopRestartRequired,
		status.DesktopEffectiveMode,
	)
}

func errorsNew(message string) error { return fmt.Errorf("%s", message) }

func catalogCommand(args []string) {
	if len(args) == 0 || args[0] != "refresh" {
		usage()
		os.Exit(2)
	}
	configPath, codexPath := defaultPaths()
	flags := flag.NewFlagSet("catalog refresh", flag.ExitOnError)
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml bound to routing state")
	flags.Bool("no-apply", false, "deprecated; activation is always owned by the resident relay or Remote manager")
	flags.Parse(args[1:])
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	if !controller.CatalogAdmissionAllowed() {
		fmt.Println("catalog_refresh=paused_by_routing")
		return
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fatal(err)
	}
	if cfg.Catalog.Owner != config.CatalogOwnerRelay {
		fmt.Println(catalogRefreshOwnership(cfg.Catalog.Owner))
		return
	}
	// The resident lifecycle is the sole catalog writer for every relay-owned
	// v3 profile.  A direct CLI refresh cannot share the lifecycle's drain
	// admission lease, so it could otherwise race `mode apply`, load external
	// credentials after the relay is parked, or concurrently replace the
	// catalog/pending marker.  Retire the old ad-hoc writer rather than adding
	// another egress-capable path. Static Linux remote_manager remains above.
	fmt.Println(catalogRefreshOwnership(cfg.Catalog.Owner))
}

func catalogRefreshOwnership(owner string) string {
	if owner != config.CatalogOwnerRelay {
		return "catalog_refresh=owned_by_" + owner
	}
	return "catalog_refresh=owned_by_resident"
}

const maxGatewayCandidateBytes = 16 << 10

func gatewayCommand(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	switch args[0] {
	case "inspect":
		gatewayInspect(args[1:])
	case "test":
		gatewayTest(args[1:])
	case "apply":
		gatewayApply(args[1:])
	default:
		usage()
		os.Exit(2)
	}
}

func gatewayInspect(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("gateway inspect")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(routing.ErrGatewayUnsupported)
	}
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		fatal(err)
	}
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(inspection); err != nil {
			fatal(err)
		}
		return
	}
	fmt.Printf(
		"gateway_config_digest=%s routing_generation=%d credential_source=%s credentials_editable=%t\n",
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
		inspection.CredentialSource,
		inspection.CredentialsEditable,
	)
}

func gatewayTest(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("gateway test")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 {
		fatal(routing.ErrGatewayUnsupported)
	}
	candidate := readGatewayCandidate(os.Stdin)
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := controller.GatewayTest(ctx, candidate)
	if err != nil {
		fatal(err)
	}
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fatal(err)
		}
		return
	}
	fmt.Printf("gateway_validation=ok model_count=%d\n", result.ModelCount)
}

func gatewayApply(args []string) {
	configPath, codexPath := defaultPaths()
	flags := newModeFlagSet("gateway apply")
	flags.StringVar(&configPath, "config", configPath, "relay JSON path")
	flags.StringVar(&codexPath, "codex-config", codexPath, "user Codex config.toml")
	expectedDigest := flags.String("expected-config-digest", "", "exact relay config SHA-256 from gateway inspect")
	expectedGeneration := flags.Uint64("expected-routing-generation", 0, "exact routing generation from gateway inspect")
	jsonOutput := flags.Bool("json", false, "emit JSON")
	flags.Parse(args)
	if flags.NArg() != 0 || *expectedDigest == "" || *expectedGeneration == 0 {
		fatal(routing.ErrGatewayConfigChanged)
	}
	candidate := readGatewayCandidate(os.Stdin)
	controller, err := routingController(configPath, codexPath)
	if err != nil {
		fatal(err)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	ctx, timeoutCancel := context.WithTimeout(ctx, 90*time.Second)
	defer timeoutCancel()
	lifecycle := acquireUserLifecycleLock(ctx)
	defer lifecycle.Close()
	result, err := controller.GatewayApply(ctx, candidate, *expectedDigest, *expectedGeneration)
	if err != nil {
		fatal(err)
	}
	if *jsonOutput {
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fatal(err)
		}
		return
	}
	fmt.Printf(
		"gateway_apply=ok config_digest=%s routing_generation=%d runtime_reloaded=%t\n",
		result.ConfigDigest,
		result.RoutingGeneration,
		result.RuntimeReloaded,
	)
}

func readGatewayCandidate(reader io.Reader) routing.GatewayCandidate {
	candidate, err := parseGatewayCandidate(reader)
	if err != nil {
		fatal(err)
	}
	return candidate
}

func parseGatewayCandidate(reader io.Reader) (routing.GatewayCandidate, error) {
	limited := &io.LimitedReader{R: reader, N: maxGatewayCandidateBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var candidate routing.GatewayCandidate
	if err := decoder.Decode(&candidate); err != nil {
		return routing.GatewayCandidate{}, routing.ErrGatewayInvalidAddress
	}
	if limited.N == 0 {
		return routing.GatewayCandidate{}, routing.ErrGatewayInvalidAddress
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return routing.GatewayCandidate{}, routing.ErrGatewayInvalidAddress
	}
	if candidate.UpstreamBaseURL == "" || len(candidate.UpstreamBaseURL) > 4096 {
		return routing.GatewayCandidate{}, routing.ErrGatewayInvalidAddress
	}
	return candidate, nil
}

func integrationCommand(args []string) {
	if len(args) == 0 {
		usage()
		os.Exit(2)
	}
	manager, err := integration.NewDefault(version)
	if err != nil {
		fatal(err)
	}
	switch args[0] {
	case "inspect":
		flags := newModeFlagSet("integration inspect")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		flags.Parse(args[1:])
		if flags.NArg() != 0 || !*jsonOutput {
			fatal(integration.ErrUnsafeState)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		result, err := manager.Inspect(ctx)
		if err != nil {
			fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fatal(err)
		}
	case "apply":
		flags := newModeFlagSet("integration apply")
		expectedDigest := flags.String("expected-state-digest", "", "exact integration state SHA-256 from inspect")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		flags.Parse(args[1:])
		if flags.NArg() != 0 || !*jsonOutput || !routingDigest(*expectedDigest) {
			fatal(integration.ErrStateChanged)
		}
		candidate, err := parseIntegrationCandidate(os.Stdin)
		if err != nil {
			fatal(err)
		}
		ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		ctx, timeoutCancel := context.WithTimeout(ctx, 120*time.Second)
		defer timeoutCancel()
		result, err := manager.Apply(ctx, candidate, *expectedDigest)
		if err != nil {
			fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fatal(err)
		}
	case "recover":
		flags := newModeFlagSet("integration recover")
		jsonOutput := flags.Bool("json", false, "emit JSON")
		flags.Parse(args[1:])
		if flags.NArg() != 0 || !*jsonOutput {
			fatal(integration.ErrUnsafeState)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		result, err := manager.Recover(ctx)
		if err != nil {
			fatal(err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			fatal(err)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func routingDigest(value string) bool {
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

func parseIntegrationCandidate(reader io.Reader) (integration.Candidate, error) {
	limited := &io.LimitedReader{R: reader, N: maxGatewayCandidateBytes + 1}
	decoder := json.NewDecoder(limited)
	decoder.DisallowUnknownFields()
	var candidate integration.Candidate
	if err := decoder.Decode(&candidate); err != nil || limited.N == 0 {
		return integration.Candidate{}, routing.ErrGatewayInvalidAddress
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return integration.Candidate{}, routing.ErrGatewayInvalidAddress
	}
	if candidate.UpstreamBaseURL == "" || len(candidate.UpstreamBaseURL) > 4096 || candidate.AuthenticationProfile == "" {
		return integration.Candidate{}, routing.ErrGatewayInvalidAddress
	}
	return candidate, nil
}

func releaseCommand(args []string) {
	if len(args) == 0 || args[0] != "verify" {
		usage()
		os.Exit(2)
	}
	flags := flag.NewFlagSet("release verify", flag.ExitOnError)
	manifest := flags.String("manifest", "", "manifest JSON")
	signature := flags.String("signature", "", "base64 Ed25519 signature")
	publicKey := flags.String("public-key", "", "trusted PEM public key")
	flags.Parse(args[1:])
	if *manifest == "" || *signature == "" || *publicKey == "" {
		fatal(fmt.Errorf("--manifest, --signature, and --public-key are required"))
	}
	verified, err := release.VerifyFiles(*manifest, *signature, *publicKey)
	if err != nil {
		fatal(err)
	}
	var artifact release.Artifact
	if verified.CompatibilityRevision == release.CompatibilityRevisionAdHocApp {
		component := release.ComponentRelay
		if runtime.GOOS == "darwin" {
			component = release.ComponentMacOSMenuBarBundle
		}
		artifact, err = verified.SelectComponent(runtime.GOOS, runtime.GOARCH, component)
	} else {
		artifact, err = verified.Select(runtime.GOOS, runtime.GOARCH)
	}
	if err != nil {
		fatal(err)
	}
	fmt.Printf("release_version=%s compatibility_revision=%d artifact=%s sha256=%s\n", verified.Version, verified.CompatibilityRevision, artifact.File, artifact.SHA256)
}

func defaultCredentialSource() string {
	if runtime.GOOS == "darwin" {
		return "keychain"
	}
	return "file"
}

type stringListFlag []string

func (f *stringListFlag) String() string { return strings.Join(*f, ",") }

func (f *stringListFlag) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type operationErrorEnvelope struct {
	SchemaVersion int                   `json:"schema_version"`
	OK            bool                  `json:"ok"`
	Error         operationErrorPayload `json:"error"`
}

type operationErrorPayload struct {
	Code              string `json:"code"`
	MessageKey        string `json:"message_key"`
	Retryable         bool   `json:"retryable"`
	RecommendedAction string `json:"recommended_action"`
}

func safeOperationError(err error) operationErrorEnvelope {
	payload := operationErrorPayload{
		Code:              "operation_failed",
		MessageKey:        "operation_failed",
		Retryable:         true,
		RecommendedAction: "refresh_status",
	}
	switch {
	case errors.Is(err, lifecyclelock.ErrReservationBusy):
		payload.Code = "lifecycle_writer_busy"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "retry"
	case errors.Is(err, lifecyclelock.ErrReservationUnsafe), errors.Is(err, lifecyclelock.ErrUnsafe):
		payload.Code = "lifecycle_state_unsafe"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, handoff.ErrBroadScanConsentRequired), errors.Is(err, handoff.ErrInvalidDiscoveryTier),
		errors.Is(err, handoff.ErrInvalidRemovalRequest), errors.Is(err, handoff.ErrInvalidRemovalSelection),
		errors.Is(err, handoff.ErrRemovalConfirmationNeeded):
		payload.Code = "invalid_request"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "review_request"
	case errors.Is(err, handoff.ErrRemovalCandidateMissing), errors.Is(err, handoff.ErrRemovalCandidateChanged),
		errors.Is(err, handoff.ErrUnsafeExecutable):
		payload.Code = "opencodex_candidate_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "rediscover_opencodex"
	case errors.Is(err, handoff.ErrTeardownCandidateChanged):
		payload.Code = "teardown_candidate_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "rediscover_opencodex"
	case errors.Is(err, handoff.ErrTeardownUnsupported):
		payload.Code = "teardown_unsupported"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, handoff.ErrTeardownPreflightFailed):
		payload.Code = "teardown_preflight_failed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, handoff.ErrTeardownRefused):
		payload.Code = "teardown_refused"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, handoff.ErrTeardownResultInvalid):
		payload.Code = "teardown_result_invalid"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, handoff.ErrTeardownVerificationFailed):
		payload.Code = "teardown_verification_failed"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "open_recovery"
	case errors.Is(err, handoff.ErrRemovalManualOnly):
		payload.Code = "opencodex_manual_removal_required"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, handoff.ErrRemovalRoutingChanged):
		payload.Code = "routing_recovery_required"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "open_recovery"
	case errors.Is(err, handoff.ErrRemovalCleanupUnsafe):
		payload.Code = "opencodex_cleanup_journal_unsafe"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, handoff.ErrNativeRemovalBoundaryUnsafe):
		payload.Code = "native_removal_boundary_unsafe"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, handoff.ErrNativeRemovalBoundaryChanged):
		payload.Code = "native_removal_boundary_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_native_removal"
	case errors.Is(err, handoff.ErrNativeRemovalRecoveryRequired):
		payload.Code = "native_recovery_required"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "open_recovery"
	case errors.Is(err, handoff.ErrNativeRemovalCustomCodexHome):
		payload.Code = "custom_codex_home_unsupported"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "review_request"
	case errors.Is(err, routing.ErrRecoveryRequired):
		payload.Code = "routing_recovery_required"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "open_recovery"
	case errors.Is(err, routing.ErrGatewayInvalidAddress):
		payload.Code = "invalid_address"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "review_request"
	case errors.Is(err, routing.ErrGatewayCredentialUnavailable):
		payload.Code = "credential_unavailable"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "update_credentials"
	case errors.Is(err, routing.ErrGatewayAuthenticationFailed):
		payload.Code = "authentication_failed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "update_credentials"
	case errors.Is(err, routing.ErrGatewayUnreachable):
		payload.Code = "gateway_unreachable"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "retry"
	case errors.Is(err, routing.ErrGatewayCatalogInvalid):
		payload.Code = "catalog_invalid"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "review_gateway"
	case errors.Is(err, routing.ErrGatewayConfigChanged):
		payload.Code = "config_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_gateway"
	case errors.Is(err, routing.ErrGatewayRoutingChanged):
		payload.Code = "routing_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, routing.ErrGatewayTransitionPending):
		payload.Code = "transition_pending"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, routing.ErrGatewayRuntimeSwap):
		payload.Code = "runtime_swap_failed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, routing.ErrGatewayUnsupported):
		payload.Code = "gateway_unsupported"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, integration.ErrAppLocationInvalid):
		payload.Code = "integration_app_location_invalid"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "move_application"
	case errors.Is(err, integration.ErrArtifactInvalid):
		payload.Code = "integration_artifact_invalid"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "verify_download"
	case errors.Is(err, integration.ErrStateChanged):
		payload.Code = "integration_state_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, integration.ErrUnsafeState):
		payload.Code = "integration_state_unsafe"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, integration.ErrRecoveryRequired):
		payload.Code = "integration_recovery_required"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "open_recovery"
	case errors.Is(err, integration.ErrActivationFailed):
		payload.Code = "integration_activation_failed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "retry"

	case errors.Is(err, routing.ErrNativeRepairGenerationStale):
		payload.Code = "routing_generation_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, routing.ErrNativeRepairOwnerChanged), errors.Is(err, routing.ErrNativeRepairArtifactsChanged):
		payload.Code = "native_repair_owner_changed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, routing.ErrNativeRepairStateIncomplete):
		payload.Code = "native_state_repair_pending"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "open_recovery"
	case errors.Is(err, routing.ErrNativeOwnerBusy):
		payload.Code = "native_owner_busy"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "retry_owner_repair"
	case errors.Is(err, routing.ErrNativeOwnerConfigurationInvalid), errors.Is(err, handoff.ErrNativeOwnerConfigurationInvalid):
		payload.Code = "native_owner_configuration_invalid"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, routing.ErrNativeOwnerRestoreFailed), errors.Is(err, handoff.ErrNativeRestoreFailed):
		payload.Code = "native_owner_restore_failed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, routing.ErrNativeOwnerResultInvalid), errors.Is(err, handoff.ErrNativeRestoreOutput):
		payload.Code = "native_owner_result_invalid"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, routing.ErrNativeRepairConfigurationFailed):
		payload.Code = "native_owner_repair_failed"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, routing.ErrNativeVerification):
		payload.Code = "native_routing_unverified"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, routing.ErrNativeRepairUnavailable):
		payload.Code = "native_repair_unavailable"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	case errors.Is(err, routing.ErrDesktopExitConfirmation):
		payload.Code = "desktop_exit_confirmation_required"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "retry_after_desktop_exit"
	case errors.Is(err, context.DeadlineExceeded):
		payload.Code = "operation_timed_out"
		payload.MessageKey = payload.Code
		payload.RecommendedAction = "refresh_status"
	case errors.Is(err, context.Canceled):
		payload.Code = "operation_cancelled"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "none"
	case errors.Is(err, os.ErrPermission):
		payload.Code = "permission_required"
		payload.MessageKey = payload.Code
		payload.Retryable = false
		payload.RecommendedAction = "manual_remediation"
	}
	return operationErrorEnvelope{SchemaVersion: 1, OK: false, Error: payload}
}

func writeOperationError(writer io.Writer, err error) error {
	return json.NewEncoder(writer).Encode(safeOperationError(err))
}

func jsonOutputRequested(args []string) bool {
	for _, arg := range args {
		if arg == "--json" || strings.HasPrefix(arg, "--json=") {
			return true
		}
	}
	return false
}

func fatal(err error) {
	if jsonOutputRequested(os.Args[1:]) {
		if encodeErr := writeOperationError(os.Stdout, err); encodeErr != nil {
			fmt.Fprintln(os.Stderr, "ERROR: relay control failed")
		}
	} else {
		fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	}
	os.Exit(1)
}
