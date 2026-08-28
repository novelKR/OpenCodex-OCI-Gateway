package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/catalog"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
)

var gatewayTestCredentials = credentials.Values{
	CFClientID: "client-id", CFClientSecret: "client-secret", GatewayKey: "gateway-key",
}

func TestGatewayInspectAndTestUseBoundConfigWithoutMutation(t *testing.T) {
	controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
	validationCalls := 0
	controller.validateGateway = func(_ context.Context, cfg config.Config, values credentials.Values) (catalog.Result, error) {
		validationCalls++
		if cfg.UpstreamBaseURL != "https://candidate.example.test/v1" || values != gatewayTestCredentials {
			t.Fatalf("gateway validation input = %#v %#v", cfg, values)
		}
		return catalog.Result{Count: 2, Hash: "catalog-hash"}, nil
	}

	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if inspection.SchemaVersion != 2 || inspection.UpstreamBaseURL != origin || inspection.CredentialAccount != "test-account" || !inspection.CredentialsEditable || inspection.ConfigDigest == "" || inspection.AuthenticationProfile != config.RemoteAuthenticationCloudflareAccessAndGatewayKey || len(inspection.RequiredCredentials) != 3 {
		t.Fatalf("gateway inspection = %#v", inspection)
	}
	validation, err := controller.GatewayTest(context.Background(), GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if !validation.OK || validation.ModelCount != 2 || validation.ConfigDigest != inspection.ConfigDigest || validation.RoutingGeneration != inspection.RoutingGeneration || validationCalls != 1 {
		t.Fatalf("gateway validation = %#v calls=%d", validation, validationCalls)
	}
	cfg, err := config.Load(store.ConfigPath())
	if err != nil || cfg.UpstreamBaseURL != origin {
		t.Fatalf("gateway test changed config: %#v err=%v", cfg, err)
	}
	if _, err := os.Stat(gatewayBackupPath(store.ConfigPath())); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gateway test created backup: %v", err)
	}
	if _, err := controller.GatewayTest(context.Background(), GatewayCandidate{UpstreamBaseURL: "http://invalid.example.test/v1"}); !errors.Is(err, ErrGatewayInvalidAddress) {
		t.Fatalf("invalid address error = %v", err)
	}
}

func TestGatewayCandidateNormalizesAddressAndPersistsAuthenticationProfile(t *testing.T) {
	controller, store, _ := gatewayControllerFixture(t, PhaseNativeActive)
	controller.validateGateway = func(_ context.Context, cfg config.Config, values credentials.Values) (catalog.Result, error) {
		if cfg.UpstreamBaseURL != "https://candidate.example.test/v1" || cfg.Credentials.RemoteAuthenticationProfile() != config.RemoteAuthenticationGatewayAPIKey {
			t.Fatalf("candidate config = %#v", cfg)
		}
		if err := values.ValidateForProfile(config.RemoteAuthenticationGatewayAPIKey); err != nil {
			t.Fatal(err)
		}
		return catalog.Result{Count: 1}, nil
	}
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := controller.GatewayApply(
		context.Background(),
		GatewayCandidate{
			UpstreamBaseURL:       "https://candidate.example.test",
			AuthenticationProfile: config.RemoteAuthenticationGatewayAPIKey,
		},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RuntimeReloaded {
		t.Fatal("native candidate unexpectedly reloaded runtime")
	}
	cfg, err := config.Load(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if cfg.UpstreamBaseURL != "https://candidate.example.test/v1" || cfg.Credentials.AuthenticationProfile != config.RemoteAuthenticationGatewayAPIKey {
		t.Fatalf("persisted config = %#v", cfg)
	}
}

func TestGatewayPrivateHTTPProfilesRequireApplyAcknowledgement(t *testing.T) {
	for _, profile := range []string{
		config.RemoteAuthenticationNone,
		config.RemoteAuthenticationGatewayAPIKey,
	} {
		t.Run(profile, func(t *testing.T) {
			controller, _, _ := gatewayControllerFixture(t, PhaseNativeActive)
			controller.validateGateway = successfulGatewayValidator
			candidate := GatewayCandidate{
				UpstreamBaseURL:       "http://192.168.10.4",
				AuthenticationProfile: profile,
			}
			if _, err := controller.GatewayTest(context.Background(), candidate); !errors.Is(err, ErrGatewayInvalidAddress) {
				t.Fatalf("unacknowledged HTTP error = %v", err)
			}
			candidate.AllowInsecurePrivateIP = true
			if _, err := controller.GatewayTest(context.Background(), candidate); err != nil {
				t.Fatalf("acknowledged HTTP candidate rejected: %v", err)
			}
		})
	}

	controller, _, _ := gatewayControllerFixture(t, PhaseNativeActive)
	controller.validateGateway = successfulGatewayValidator
	candidate := GatewayCandidate{
		UpstreamBaseURL:        "http://192.168.10.4",
		AuthenticationProfile:  config.RemoteAuthenticationCloudflareAccessAndGatewayKey,
		AllowInsecurePrivateIP: true,
	}
	if _, err := controller.GatewayTest(context.Background(), candidate); !errors.Is(err, ErrGatewayInvalidAddress) {
		t.Fatalf("HTTP Cloudflare profile error = %v", err)
	}
}

func TestGatewayApplyReloadsActiveExternalAfterDrainWithoutDesktopBoundary(t *testing.T) {
	controller, store, _ := gatewayControllerFixture(t, PhaseRelayActive)
	controller.validateGateway = successfulGatewayValidator
	var parkedReads atomic.Int64
	controller.health = healthReaderFunc(func(ctx context.Context, cfg config.Config) LocalRelay {
		health := stateHealth{store: store, active: 0}.Read(ctx, cfg)
		state, _, err := store.Read()
		if err == nil && state.Phase == PhaseApplying && parkedReads.Add(1) == 1 {
			active := int64(1)
			health.General.ActiveRequests = &active
			health.Interactive.ActiveRequests = &active
		}
		return health
	})
	var runtimeCalls atomic.Int64
	controller.runtimeControl = runtimeControlFunc(func(_ context.Context, generation uint64, backend Backend) error {
		runtimeCalls.Add(1)
		state, _, err := store.Read()
		if err != nil || state.Phase != PhaseApplying || state.Generation != generation || backend != BackendExternal {
			t.Fatalf("runtime reload state=%#v backend=%s err=%v", state, backend, err)
		}
		cfg, err := config.Load(store.ConfigPath())
		if err != nil || cfg.UpstreamBaseURL != "https://candidate.example.test/v1" {
			t.Fatalf("runtime reload config=%#v err=%v", cfg, err)
		}
		return nil
	})
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := controller.GatewayApply(
		context.Background(),
		GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !receipt.OK || !receipt.RuntimeReloaded || runtimeCalls.Load() != 1 || parkedReads.Load() < 2 {
		t.Fatalf("gateway apply receipt=%#v runtime=%d parked_reads=%d", receipt, runtimeCalls.Load(), parkedReads.Load())
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseRelayActive || state.AppliedBackend != BackendExternal || state.Generation != inspection.RoutingGeneration+2 {
		t.Fatalf("final routing state=%#v err=%v", state, err)
	}
	assertGatewayArtifactsRemoved(t, controller, store)
}

func TestGatewayApplyWhileNativeSavesOnlyForNextExternalTransition(t *testing.T) {
	controller, store, _ := gatewayControllerFixture(t, PhaseNativeActive)
	controller.validateGateway = successfulGatewayValidator
	runtimeCalls := 0
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
		runtimeCalls++
		return nil
	})
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := controller.GatewayApply(
		context.Background(),
		GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.RuntimeReloaded || runtimeCalls != 0 || receipt.RoutingGeneration != inspection.RoutingGeneration {
		t.Fatalf("inactive gateway receipt=%#v runtime=%d", receipt, runtimeCalls)
	}
	cfg, err := config.Load(store.ConfigPath())
	if err != nil || cfg.UpstreamBaseURL != "https://candidate.example.test/v1" {
		t.Fatalf("inactive gateway config=%#v err=%v", cfg, err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseNativeActive || state.Generation != inspection.RoutingGeneration {
		t.Fatalf("inactive routing state=%#v err=%v", state, err)
	}
}

func TestGatewayApplyWhileNativeRejectsCompetingConfigWithoutOverwritingIt(t *testing.T) {
	controller, store, _ := gatewayControllerFixture(t, PhaseNativeActive)
	controller.validateGateway = successfulGatewayValidator
	competitorURL := "https://competitor.example.test/v1"
	var loads atomic.Int64
	controller.loadCredentials = func(config.CredentialsConfig) (credentials.Values, error) {
		if loads.Add(1) == 4 {
			competitor, err := config.Load(store.ConfigPath())
			if err != nil {
				return credentials.Values{}, err
			}
			competitor.UpstreamBaseURL = competitorURL
			if err := config.Write(store.ConfigPath(), competitor); err != nil {
				return credentials.Values{}, err
			}
		}
		return gatewayTestCredentials, nil
	}
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.GatewayApply(
		context.Background(),
		GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if !errors.Is(err, ErrGatewayConfigChanged) {
		t.Fatalf("competing config error = %v loads=%d", err, loads.Load())
	}
	assertGatewayURL(t, store.ConfigPath(), competitorURL)
	assertGatewayArtifactsRemoved(t, controller, store)
}

func TestGatewayApplyWhileNativeVerifiesCredentialRaceRollback(t *testing.T) {
	controller, store, origin := gatewayControllerFixture(t, PhaseNativeActive)
	controller.validateGateway = successfulGatewayValidator
	var loads atomic.Int64
	controller.loadCredentials = func(config.CredentialsConfig) (credentials.Values, error) {
		if loads.Add(1) == 4 {
			changed := gatewayTestCredentials
			changed.GatewayKey = "changed"
			return changed, nil
		}
		return gatewayTestCredentials, nil
	}
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.GatewayApply(
		context.Background(),
		GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if !errors.Is(err, ErrGatewayCredentialUnavailable) {
		t.Fatalf("credential race error = %v loads=%d", err, loads.Load())
	}
	assertGatewayURL(t, store.ConfigPath(), origin)
	digest, digestErr := fingerprintOptional(store.ConfigPath())
	if digestErr != nil || digest != inspection.ConfigDigest {
		t.Fatalf("restored digest=%q want=%q err=%v", digest, inspection.ConfigDigest, digestErr)
	}
}

func TestGatewayApplyRejectsExpectedAndObservedRaces(t *testing.T) {
	t.Run("expected config digest", func(t *testing.T) {
		controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
		controller.validateGateway = successfulGatewayValidator
		inspection, err := controller.GatewayInspect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = controller.GatewayApply(context.Background(), GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"}, "stale", inspection.RoutingGeneration)
		if !errors.Is(err, ErrGatewayConfigChanged) {
			t.Fatalf("digest race error = %v", err)
		}
		assertGatewayURL(t, store.ConfigPath(), origin)
	})

	t.Run("expected routing generation", func(t *testing.T) {
		controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
		controller.validateGateway = successfulGatewayValidator
		inspection, err := controller.GatewayInspect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = controller.GatewayApply(context.Background(), GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"}, inspection.ConfigDigest, inspection.RoutingGeneration+1)
		if !errors.Is(err, ErrGatewayRoutingChanged) {
			t.Fatalf("generation race error = %v", err)
		}
		assertGatewayURL(t, store.ConfigPath(), origin)
	})

	t.Run("credential changed after validation", func(t *testing.T) {
		controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
		controller.validateGateway = successfulGatewayValidator
		var loads atomic.Int64
		controller.loadCredentials = func(config.CredentialsConfig) (credentials.Values, error) {
			if loads.Add(1) >= 3 {
				changed := gatewayTestCredentials
				changed.GatewayKey = "changed"
				return changed, nil
			}
			return gatewayTestCredentials, nil
		}
		inspection, err := controller.GatewayInspect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = controller.GatewayApply(context.Background(), GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"}, inspection.ConfigDigest, inspection.RoutingGeneration)
		if !errors.Is(err, ErrGatewayCredentialUnavailable) {
			t.Fatalf("credential race error = %v loads=%d", err, loads.Load())
		}
		assertGatewayURL(t, store.ConfigPath(), origin)
	})

	t.Run("routing changed during validation", func(t *testing.T) {
		controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
		controller.validateGateway = func(context.Context, config.Config, credentials.Values) (catalog.Result, error) {
			lock, err := store.Lock(context.Background())
			if err != nil {
				return catalog.Result{}, err
			}
			defer lock.Close()
			state, err := lock.Load()
			if err != nil {
				return catalog.Result{}, err
			}
			state.Generation++
			return catalog.Result{Count: 1}, lock.Save(state)
		}
		inspection, err := controller.GatewayInspect(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		_, err = controller.GatewayApply(context.Background(), GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"}, inspection.ConfigDigest, inspection.RoutingGeneration)
		if !errors.Is(err, ErrGatewayRoutingChanged) {
			t.Fatalf("observed routing race error = %v", err)
		}
		assertGatewayURL(t, store.ConfigPath(), origin)
	})
}

func TestGatewayApplyRollsBackRuntimeAndConfigOnReloadFailure(t *testing.T) {
	controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
	controller.validateGateway = successfulGatewayValidator
	controller.health = stateHealth{store: store, active: 0}
	runtimeCalls := 0
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
		runtimeCalls++
		if runtimeCalls == 1 {
			return errors.New("reload failed")
		}
		return nil
	})
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.GatewayApply(context.Background(), GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"}, inspection.ConfigDigest, inspection.RoutingGeneration)
	if !errors.Is(err, ErrGatewayRuntimeSwap) {
		t.Fatalf("reload failure error = %v", err)
	}
	if runtimeCalls != 2 {
		t.Fatalf("runtime calls = %d, want failed reload plus rollback", runtimeCalls)
	}
	assertGatewayURL(t, store.ConfigPath(), origin)
	state, stateErr := store.Load()
	if stateErr != nil || state.Phase != PhaseRelayActive || state.AppliedBackend != BackendExternal {
		t.Fatalf("rollback state=%#v err=%v", state, stateErr)
	}
	assertGatewayArtifactsRemoved(t, controller, store)
}

func TestGatewayReloadFailureParksRecoveryUntilVerifiedRollback(t *testing.T) {
	controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
	controller.validateGateway = successfulGatewayValidator
	controller.health = stateHealth{store: store, active: 0}
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
		return errors.New("runtime unavailable")
	})
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.GatewayApply(context.Background(), GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"}, inspection.ConfigDigest, inspection.RoutingGeneration)
	if !errors.Is(err, ErrGatewayRuntimeSwap) {
		t.Fatalf("unrecoverable reload error = %v", err)
	}
	state, stateErr := store.Load()
	if stateErr != nil || state.Phase != PhaseRecoveryRequired || state.AllowsDataPlane() {
		t.Fatalf("parked recovery state=%#v err=%v", state, stateErr)
	}
	if _, found, journalErr := controller.loadJournal(); journalErr != nil || !found {
		t.Fatalf("recovery journal found=%t err=%v", found, journalErr)
	}
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error { return nil })
	status, err := controller.Recover(context.Background(), RecoveryRollback, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseRelayActive || status.AppliedBackend != BackendExternal {
		t.Fatalf("recovered gateway status=%#v", status)
	}
	assertGatewayURL(t, store.ConfigPath(), origin)
	assertGatewayArtifactsRemoved(t, controller, store)
}

func TestGatewayFinalConfigDriftKeepsRecoveryArtifactsUntilRollback(t *testing.T) {
	controller, store, origin := gatewayControllerFixture(t, PhaseRelayActive)
	controller.validateGateway = successfulGatewayValidator
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error { return nil })
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	candidateURL := "https://candidate.example.test/v1"
	competitorURL := "https://competitor.example.test/v1"
	var drifted atomic.Bool
	controller.health = healthReaderFunc(func(ctx context.Context, cfg config.Config) LocalRelay {
		health := stateHealth{store: store, active: 0}.Read(ctx, cfg)
		state, _, stateErr := store.Read()
		if stateErr == nil && state.Phase == PhaseRelayActive &&
			state.Generation > inspection.RoutingGeneration && drifted.CompareAndSwap(false, true) {
			competitor, loadErr := config.Load(store.ConfigPath())
			if loadErr != nil {
				t.Fatalf("load competing config: %v", loadErr)
			}
			competitor.UpstreamBaseURL = competitorURL
			if writeErr := config.Write(store.ConfigPath(), competitor); writeErr != nil {
				t.Fatalf("write competing config: %v", writeErr)
			}
		}
		return health
	})
	_, err = controller.GatewayApply(
		context.Background(),
		GatewayCandidate{UpstreamBaseURL: candidateURL},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if !errors.Is(err, ErrGatewayConfigChanged) {
		t.Fatalf("final config drift error = %v", err)
	}
	state, stateErr := store.Load()
	if stateErr != nil || state.Phase != PhaseRecoveryRequired || state.AllowsDataPlane() {
		t.Fatalf("drift recovery state=%#v err=%v", state, stateErr)
	}
	if _, found, journalErr := controller.loadJournal(); journalErr != nil || !found {
		t.Fatalf("drift recovery journal found=%t err=%v", found, journalErr)
	}
	if _, backupErr := os.Stat(gatewayBackupPath(store.ConfigPath())); backupErr != nil {
		t.Fatalf("drift recovery backup: %v", backupErr)
	}

	knownTarget, err := config.Load(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	knownTarget.UpstreamBaseURL = candidateURL
	if err := config.Write(store.ConfigPath(), knownTarget); err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}
	status, err := controller.Recover(context.Background(), RecoveryRollback, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseRelayActive || status.AppliedBackend != BackendExternal {
		t.Fatalf("drift rollback status=%#v", status)
	}
	assertGatewayURL(t, store.ConfigPath(), origin)
	assertGatewayArtifactsRemoved(t, controller, store)
}

func TestGatewayRecoveryInfersAtomicConfigMutationAfterInterruptedStageWrite(t *testing.T) {
	controller, store, _ := gatewayControllerFixture(t, PhaseRelayActive)
	controller.validateGateway = successfulGatewayValidator
	controller.health = stateHealth{store: store, active: 0}
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
		return errors.New("runtime unavailable")
	})
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	candidateURL := "https://candidate.example.test/v1"
	_, err = controller.GatewayApply(
		context.Background(),
		GatewayCandidate{UpstreamBaseURL: candidateURL},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if !errors.Is(err, ErrGatewayRuntimeSwap) {
		t.Fatalf("setup apply error = %v", err)
	}
	journal, found, err := controller.loadJournal()
	if err != nil || !found || journal.Stage != transactionPrepared {
		t.Fatalf("setup journal=%#v found=%t err=%v", journal, found, err)
	}
	// Simulate a process death after the atomic config replacement but before
	// the journal stage write. Recovery must infer the target from fingerprints.
	candidate, err := config.Load(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	candidate.UpstreamBaseURL = candidateURL
	if err := config.Write(store.ConfigPath(), candidate); err != nil {
		t.Fatal(err)
	}
	capabilities := controller.Status(context.Background()).RecoveryCapabilities
	if !capabilities.CanComplete || !capabilities.CanRollback {
		t.Fatalf("inferred recovery capabilities = %#v", capabilities)
	}
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error { return nil })
	status, err := controller.Recover(context.Background(), RecoveryComplete, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseRelayActive || status.AppliedBackend != BackendExternal {
		t.Fatalf("completed interrupted mutation status=%#v", status)
	}
	assertGatewayURL(t, store.ConfigPath(), candidateURL)
	assertGatewayArtifactsRemoved(t, controller, store)
}

func TestGatewayApplyCleansUnwitnessedBackupOnlyFromStableState(t *testing.T) {
	controller, store, _ := gatewayControllerFixture(t, PhaseNativeActive)
	controller.validateGateway = successfulGatewayValidator
	backupPath := gatewayBackupPath(store.ConfigPath())
	if err := os.Link(store.ConfigPath(), backupPath); err != nil {
		t.Fatal(err)
	}
	inspection, err := controller.GatewayInspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = controller.GatewayApply(
		context.Background(),
		GatewayCandidate{UpstreamBaseURL: "https://candidate.example.test/v1"},
		inspection.ConfigDigest,
		inspection.RoutingGeneration,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(backupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unwitnessed stable backup remains: %v", err)
	}
}

func gatewayControllerFixture(t *testing.T, phase Phase) (*Controller, *Store, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	origin := "https://origin.example.test/v1"
	cfg, err := config.NewDefault(origin, config.CredentialsSourceKeychain)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Credentials.Account = "test-account"
	cfg.Catalog.Path = filepath.Join(directory, "catalog.json")
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-test\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(
		configPath,
		codexPath,
		WithTransitionTiming(time.Second, time.Millisecond),
		withCredentialLoader(func(config.CredentialsConfig) (credentials.Values, error) { return gatewayTestCredentials, nil }),
		withGatewayValidator(successfulGatewayValidator),
	)
	if err != nil {
		t.Fatal(err)
	}
	store := controller.store
	state, err := NewRelayState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if phase == PhaseNativeActive {
		state.DesiredMode = ModeNative
		state.AppliedMode = ModeNative
		state.DesiredBackend = BackendNone
		state.AppliedBackend = BackendNone
		state.Phase = PhaseNativeActive
	}
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}
	return controller, store, origin
}

func successfulGatewayValidator(context.Context, config.Config, credentials.Values) (catalog.Result, error) {
	return catalog.Result{Count: 1, Hash: "catalog-hash"}, nil
}

func assertGatewayURL(t *testing.T, path, expected string) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil || cfg.UpstreamBaseURL != expected {
		t.Fatalf("gateway URL=%q want=%q err=%v", cfg.UpstreamBaseURL, expected, err)
	}
}

func assertGatewayArtifactsRemoved(t *testing.T, controller *Controller, store *Store) {
	t.Helper()
	for _, path := range []string{controller.journalPath, gatewayBackupPath(store.ConfigPath())} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("gateway transaction artifact remains at %s: %v", filepath.Base(path), err)
		}
	}
}
