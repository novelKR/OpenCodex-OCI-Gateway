package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/containerruntime"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/handoff"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/lifecyclelock"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/proxy"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/routing"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/scheduler"
)

var version = "dev"

const runtimeTrustKeyName = "opencodex-runtime-release-ed25519.pub"

func main() {
	configPath, err := config.DefaultConfigPath()
	if err != nil {
		fatal(err)
	}
	flags := flag.NewFlagSet("opencodex-relay", flag.ExitOnError)
	flags.StringVar(&configPath, "config", configPath, "path to non-secret relay JSON configuration")
	check := flags.Bool("check", false, "validate configuration and credential access, then exit")
	showVersion := flags.Bool("version", false, "print relay version")
	flags.Parse(os.Args[1:])
	if *showVersion {
		fmt.Println(version)
		return
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		fatal(err)
	}
	loadCredentials := func() (credentials.Values, error) { return credentials.Load(cfg.Credentials) }
	if *check {
		if cfg.UpstreamMode == config.UpstreamModeExternalGateway {
			if _, err := loadCredentials(); err != nil {
				fatal(err)
			}
		}
		fmt.Printf("relay_config=valid listen=%s interactive_listen=%s upstream_mode=%s catalog_owner=%s voice_enabled=%t\n", cfg.ListenAddress, cfg.Responses.Scheduler.InteractiveListenAddress, cfg.UpstreamMode, cfg.Catalog.Owner, cfg.VoiceEnabled)
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	tracker := proxy.NewTracker()
	routingStore, err := routing.Open(configPath)
	if err != nil {
		fatal(err)
	}
	home, homeErr := os.UserHomeDir()
	runtimeRoot, runtimeRootErr := containerruntime.DefaultRoot(filepath.Clean(home))
	runtimePublicKey, runtimePublicKeyErr := readBundledRuntimeTrustKey()
	appleCLI := containerruntime.NewAppleCLI()
	routingWatcher := routing.NewWatcher(
		routingStore,
		250*time.Millisecond,
		routing.WithDriftCheck(routing.AppliedRoutingDriftCheck(configPath)),
		routing.WithWatcherRecoveryGate(func() error { return handoff.RemovalRoutingGate(configPath) }),
		routing.WithWatcherStateRecoveryGate(func(state routing.State) error {
			if homeErr != nil || runtimeRootErr != nil || runtimePublicKeyErr != nil {
				return containerruntime.ErrRecoveryRequired
			}
			return containerruntime.ValidateActiveRoutingAuthority(runtimeRoot, state.Generation, version, runtimePublicKey)
		}),
	)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go routingWatcher.Run(ctx)

	// The optional macOS Local profile is available only when relay.json keeps
	// External as its canonical topology. In that case RuntimeManager preserves
	// the listener/PID while its owner-only control socket swaps immutable
	// upstream runtimes after the routing controller has drained requests.
	var runtime *relayRuntime
	var control *routing.ControlServer
	var generalHandler, interactiveHandler http.Handler
	if cfg.UpstreamMode == config.UpstreamModeExternalGateway {
		appleLease := func(leaseCtx context.Context) (func() error, error) {
			if homeErr != nil {
				return nil, containerruntime.ErrRecoveryRequired
			}
			lock, lockErr := lifecyclelock.AcquireReader(
				leaseCtx,
				filepath.Clean(home),
				os.Getenv(lifecyclelock.SourceInstallReservationEnvironment),
			)
			if lockErr != nil {
				return nil, lockErr
			}
			return lock.Close, nil
		}
		appleGuard := func(guardCtx context.Context, routingGeneration uint64) error {
			if homeErr != nil || runtimeRootErr != nil || runtimePublicKeyErr != nil {
				return containerruntime.ErrRecoveryRequired
			}
			return containerruntime.ValidateActiveRuntimeBeforeCredentials(
				guardCtx,
				runtimeRoot,
				routingGeneration,
				version,
				runtimePublicKey,
				appleCLI,
			)
		}
		runtime, err = newRelayRuntime(
			ctx,
			configPath,
			cfg,
			routingWatcher,
			tracker,
			logger,
			appleRuntimeAccess{
				lease:          appleLease,
				guard:          appleGuard,
				loadCredential: credentials.LoadContext,
			},
		)
		if err != nil {
			fatal(err)
		}
		maintenance, maintenanceErr := routing.NewMaintenanceCoordinator(configPath, routingWatcher, runtime)
		if maintenanceErr != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = runtime.Close(shutdown)
			cancel()
			fatal(maintenanceErr)
		}
		controlHandler := func(controlCtx context.Context, request routing.ControlRequest) (routing.ControlResponse, error) {
			if request.Operation == routing.ControlOperationApply {
				return runtime.apply(controlCtx, request)
			}
			return maintenance.Handle(controlCtx, request)
		}
		control, err = routing.StartControlServer(ctx, configPath, controlHandler)
		if err != nil {
			shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = runtime.Close(shutdown)
			cancel()
			fatal(err)
		}
		defer control.Close()
		generalHandler = runtime.Handler().HandlerForLane(scheduler.LaneGeneral)
		interactiveHandler = runtime.Handler().HandlerForLane(scheduler.LaneInteractive)
	} else {
		// Preserve the existing Linux remote-manager local_opencodex topology.
		// It has no macOS profile switch or runtime control socket.
		observation := proxy.NewConnectionObservation(cfg.UpstreamMode)
		handler, buildErr := proxy.New(
			cfg,
			loadCredentials,
			tracker,
			logger,
			proxy.WithRouting(routingWatcher),
			proxy.WithConnectionObservation(observation),
		)
		if buildErr != nil {
			fatal(buildErr)
		}
		if cfg.Catalog.Owner == config.CatalogOwnerRelay {
			go runCatalogLifecycle(ctx, cfg, loadCredentials, tracker, logger, routingWatcher, observation)
		} else {
			observation.SetCatalogLifecycle(proxy.CatalogLifecycleUnknown)
			logger.Info("catalog lifecycle owned externally", "owner", cfg.Catalog.Owner)
		}
		generalHandler = handler.HandlerForLane(scheduler.LaneGeneral)
		interactiveHandler = handler.HandlerForLane(scheduler.LaneInteractive)
	}
	generalListener, interactiveListener, err := listenPair(
		cfg.ListenAddress,
		cfg.Responses.Scheduler.InteractiveListenAddress,
	)
	if err != nil {
		fatal(err)
	}
	defer generalListener.Close()
	defer interactiveListener.Close()
	generalServer := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           generalHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	interactiveServer := &http.Server{
		Addr:              cfg.Responses.Scheduler.InteractiveListenAddress,
		Handler:           interactiveHandler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	serveErrors := make(chan error, 2)
	go serveListener(generalServer, generalListener, serveErrors)
	go serveListener(interactiveServer, interactiveListener, serveErrors)
	logger.Info(
		"opencodex-relay compatibility relay started",
		"listen", cfg.ListenAddress,
		"interactive_listen", cfg.Responses.Scheduler.InteractiveListenAddress,
		"upstream_mode", cfg.UpstreamMode,
		"catalog_owner", cfg.Catalog.Owner,
		"voice_enabled", cfg.VoiceEnabled,
	)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-serveErrors:
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	_ = generalServer.Shutdown(shutdown)
	_ = interactiveServer.Shutdown(shutdown)
	if runtime != nil {
		_ = runtime.Close(shutdown)
	}
	if serveErr != nil {
		fatal(serveErr)
	}
}

func serveListener(server *http.Server, listener net.Listener, results chan<- error) {
	err := server.Serve(listener)
	if err == http.ErrServerClosed {
		err = nil
	}
	results <- err
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "ERROR: %v\n", err)
	os.Exit(1)
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
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0o022 != 0 || info.Size() <= 0 || info.Size() > runtimemanifest.MaximumPublicKeyBytes {
		return nil, containerruntime.ErrUnsafeState
	}
	return os.ReadFile(path)
}
