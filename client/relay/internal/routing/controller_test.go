package routing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/config"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/credentials"
	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/localopencodex"
)

type runtimeControlFunc func(context.Context, uint64, Backend) error

func (f runtimeControlFunc) Apply(ctx context.Context, generation uint64, backend Backend) error {
	return f(ctx, generation, backend)
}

type healthReaderFunc func(context.Context, config.Config) LocalRelay

func (f healthReaderFunc) Read(ctx context.Context, cfg config.Config) LocalRelay { return f(ctx, cfg) }

func TestRequestNativeOnlyRecordsIntent(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, nil)
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}

	status, err := controller.Request(context.Background(), ModeNative)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("request mutated Codex configuration before apply")
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseNativePendingRestart || state.DesiredMode != ModeNative || state.AppliedMode != ModeRelay {
		t.Fatalf("request state = %#v", state)
	}
	if status.Phase != PhaseNativePendingRestart || status.RelayAdmission != "allow" || status.CatalogRefresh != "run" {
		t.Fatalf("request status = %#v", status)
	}
}

func TestExternalLegacyMigrationIntentBacksUpOnlyDuringApply(t *testing.T) {
	directory := t.TempDir()
	relayPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceNone)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	if err := config.Write(relayPath, cfg); err != nil {
		t.Fatal(err)
	}
	legacy := "model_provider = \"opencodex\"\nmodel_catalog_json = \"/old/catalog.json\"\nmodel = \"gpt\"\n\n[model_providers.opencodex]\nbase_url = \"https://legacy.example.test/v1\"\n"
	if err := os.WriteFile(codexPath, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewRelayState(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredMode = ModeNative
	state.AppliedMode = ModeNative
	state.DesiredBackend = BackendNone
	state.AppliedBackend = BackendNone
	state.Phase = PhaseNativeActive
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}

	controller, err := NewController(
		relayPath,
		codexPath,
		WithTransitionTiming(time.Second, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}

	configDigest, err := fingerprintOptional(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	requested, err := controller.RequestBackendWithIntentAndWitness(
		context.Background(),
		BackendExternal,
		true,
		configDigest,
		state.Generation,
	)
	if err != nil {
		t.Fatal(err)
	}
	if requested.Phase != PhaseRelayPendingRestart {
		t.Fatalf("request status=%#v", requested)
	}
	beforeApply, err := os.ReadFile(codexPath)
	if err != nil || string(beforeApply) != legacy {
		t.Fatalf("request mutated legacy config=%q err=%v", beforeApply, err)
	}
	if backups, err := filepath.Glob(codexPath + ".pre-opencodex-relay-*"); err != nil || len(backups) != 0 {
		t.Fatalf("request created backups=%v err=%v", backups, err)
	}
	pending, err := store.Load()
	if err != nil || !pending.KnownLegacyBackupAndMigrate {
		t.Fatalf("pending state=%#v err=%v", pending, err)
	}

	completed, err := controller.Apply(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Phase != PhaseRelayActive || completed.AppliedBackend != BackendExternal {
		t.Fatalf("completed status=%#v", completed)
	}
	afterApply, err := os.ReadFile(codexPath)
	if err != nil || strings.Contains(string(afterApply), "model_provider =") ||
		!strings.Contains(string(afterApply), codexconfig.BeginMarker) ||
		!strings.Contains(string(afterApply), "[model_providers.opencodex]") {
		t.Fatalf("migrated config=%q err=%v", afterApply, err)
	}
	backups, err := filepath.Glob(codexPath + ".pre-opencodex-relay-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("apply backups=%v err=%v", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil || string(backup) != legacy {
		t.Fatalf("backup=%q err=%v", backup, err)
	}
	final, err := store.Load()
	if err != nil || final.KnownLegacyBackupAndMigrate {
		t.Fatalf("final state=%#v err=%v", final, err)
	}
}

func TestLegacyMigrationIntentJournalUsesActualMigrationRequirement(t *testing.T) {
	for _, test := range []struct {
		name                  string
		codexContent          string
		migrationRequired     bool
		originAuthoritative   bool
		rollbackShouldSucceed bool
	}{
		{
			name:                  "clean native",
			codexContent:          "model = \"gpt-5.6-sol\"\n",
			migrationRequired:     false,
			originAuthoritative:   true,
			rollbackShouldSucceed: true,
		},
		{
			name:                  "recognized legacy",
			codexContent:          "model_provider = \"opencodex\"\nmodel_catalog_json = \"/old/catalog.json\"\n\n[model_providers.opencodex]\nbase_url = \"https://legacy.example.test/v1\"\n",
			migrationRequired:     true,
			originAuthoritative:   false,
			rollbackShouldSucceed: false,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, store, codexPath := externalNativeControllerFixture(t, test.codexContent)
			controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
				return ErrRelayAcknowledgement
			})
			state, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			configDigest, err := fingerprintOptional(store.ConfigPath())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := controller.RequestBackendWithIntentAndWitness(
				context.Background(),
				BackendExternal,
				true,
				configDigest,
				state.Generation,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := controller.Apply(context.Background(), true); !errors.Is(err, ErrRelayAcknowledgement) {
				t.Fatalf("apply error=%v", err)
			}
			journal, found, err := controller.loadJournal()
			if err != nil || !found {
				t.Fatalf("journal=%#v found=%t err=%v", journal, found, err)
			}
			if journal.KnownLegacyBackupAndMigrate != test.migrationRequired ||
				journal.OriginAuthoritative != test.originAuthoritative {
				t.Fatalf("journal=%#v", journal)
			}
			observed, err := os.ReadFile(codexPath)
			if err != nil || string(observed) != test.codexContent {
				t.Fatalf("failed apply changed Codex config=%q err=%v", observed, err)
			}

			controller.runtimeControl = nil
			if test.rollbackShouldSucceed {
				status, err := controller.Recover(context.Background(), RecoveryRollback, true)
				if err != nil || status.Phase != PhaseNativeActive || status.AppliedBackend != BackendNone {
					t.Fatalf("rollback status=%#v err=%v", status, err)
				}
			} else if _, err := controller.Recover(context.Background(), RecoveryRollback, true); !errors.Is(err, ErrRecoveryRequired) {
				t.Fatalf("legacy rollback error=%v", err)
			}
		})
	}
}

func TestExternalLegacyMigrationWitnessRejectsConfigAndGenerationDrift(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, nil)
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	digest, err := fingerprintOptional(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RequestBackendWithIntentAndWitness(
		context.Background(),
		BackendExternal,
		true,
		digest,
		state.Generation+1,
	); !errors.Is(err, ErrGatewayRoutingChanged) {
		t.Fatalf("generation drift error=%v", err)
	}
	if _, err := controller.RequestBackendWithIntentAndWitness(
		context.Background(),
		BackendExternal,
		true,
		strings.Repeat("f", 64),
		state.Generation,
	); !errors.Is(err, ErrGatewayConfigChanged) {
		t.Fatalf("config drift error=%v", err)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("witness rejection mutated config=%q err=%v", after, err)
	}
	unchanged, err := store.Load()
	if err != nil || unchanged != state {
		t.Fatalf("witness rejection mutated state=%#v err=%v", unchanged, err)
	}
}

func TestLocalOpenCodexRequestAndApplyUseExplicitBackendBoundary(t *testing.T) {
	directory := t.TempDir()
	relayPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	cfg.LocalOpenCodex = &config.LocalOpenCodexProfile{
		UpstreamBaseURL: "http://127.0.0.1:10100/v1",
		CatalogPath:     filepath.Join(directory, "local-catalog.json"),
	}
	if err := config.Write(relayPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("model = \"gpt\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(codexPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", cfg.Catalog.Path); err != nil {
		t.Fatal(err)
	}
	store, err := Open(relayPath)
	if err != nil {
		t.Fatal(err)
	}
	var applied []Backend
	var materialized []config.Config
	controller, err := NewController(
		relayPath,
		codexPath,
		WithTransitionTiming(time.Second, time.Millisecond),
		withLocalProfileAllowed(true),
		withLocalOpenCodexPreflight(func(context.Context, string) localopencodex.Result {
			return localopencodex.Result{Availability: localopencodex.AvailabilityReady, ModelCount: 1}
		}),
		withLocalCatalogMaterializer(func(_ context.Context, local config.Config) error {
			materialized = append(materialized, local)
			if local.UpstreamMode != config.UpstreamModeLocalOpenCodex || local.Credentials.Source != config.CredentialsSourceNone || local.Catalog.Owner != config.CatalogOwnerRelay {
				t.Fatalf("local catalog barrier received unsafe runtime config: %#v", local)
			}
			return nil
		}),
		WithRuntimeControl(runtimeControlFunc(func(_ context.Context, _ uint64, backend Backend) error {
			applied = append(applied, backend)
			return nil
		})),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}

	requested, err := controller.RequestBackend(context.Background(), BackendLocalOpenCodex)
	if err != nil {
		t.Fatal(err)
	}
	if requested.Phase != PhaseBackendPendingRestart || requested.DesiredBackend != BackendLocalOpenCodex || requested.AppliedBackend != BackendExternal || !requested.DesktopRestartRequired {
		t.Fatalf("local request status = %#v", requested)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseBackendPendingRestart || !state.AllowsDataPlane() || !state.AllowsCatalog() {
		t.Fatalf("local request state interrupted current external traffic: %#v", state)
	}

	completed, err := controller.Apply(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if completed.Phase != PhaseRelayActive || completed.DesiredBackend != BackendLocalOpenCodex || completed.AppliedBackend != BackendLocalOpenCodex || completed.RelayAdmission != "allow" {
		t.Fatalf("local apply status = %#v", completed)
	}
	if len(applied) != 1 || applied[0] != BackendLocalOpenCodex {
		t.Fatalf("runtime apply calls = %#v", applied)
	}
	if len(materialized) != 1 || materialized[0].Catalog.Path != cfg.LocalOpenCodex.CatalogPath {
		t.Fatalf("local catalog barrier calls = %#v", materialized)
	}
	content, err := os.ReadFile(codexPath)
	if err != nil || !strings.Contains(string(content), cfg.LocalOpenCodex.CatalogPath) {
		t.Fatalf("local apply did not bind local catalog: %q err=%v", content, err)
	}
}

func TestAppleContainerGenericRequestApplyAndRecoveryAreLifecycleOnly(t *testing.T) {
	controller, store, codexPath := optionalLocalControllerFixture(t, nil, true)
	credentialLoads, runtimeApplies := 0, 0
	controller.loadCredentials = func(config.CredentialsConfig) (credentials.Values, error) {
		credentialLoads++
		return credentials.Values{}, errors.New("generic Apple routing must not load credentials")
	}
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
		runtimeApplies++
		return nil
	})

	before, err := NewRelayState(store.ConfigPath())
	if err == nil {
		before, err = BindCodexConfig(before, codexPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Replace(before); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RequestBackend(context.Background(), BackendLocalAppleContainer); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("generic Apple request error = %v", err)
	}
	after, err := store.Load()
	if err != nil || after != before || credentialLoads != 0 || runtimeApplies != 0 {
		t.Fatalf("rejected request mutated state or crossed credential/runtime boundary: before=%#v after=%#v loads=%d applies=%d err=%v", before, after, credentialLoads, runtimeApplies, err)
	}

	legacyPending := before
	legacyPending.Generation++
	legacyPending.DesiredBackend = BackendLocalAppleContainer
	legacyPending.DesiredMode = ModeRelay
	legacyPending.Phase = PhaseBackendPendingRestart
	lock, err = store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Replace(legacyPending); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("generic Apple apply error = %v", err)
	}

	legacyRecovery := legacyPending
	legacyRecovery.Generation++
	legacyRecovery.Phase = PhaseRecoveryRequired
	lock, err = store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Replace(legacyRecovery); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("generic Apple recovery error = %v", err)
	}
	if credentialLoads != 0 || runtimeApplies != 0 {
		t.Fatalf("generic Apple paths crossed credential/runtime boundary: loads=%d applies=%d", credentialLoads, runtimeApplies)
	}

	stableApple := before
	stableApple.Generation++
	stableApple.DesiredBackend = BackendLocalAppleContainer
	stableApple.AppliedBackend = BackendLocalAppleContainer
	stableApple.DesiredMode = ModeRelay
	stableApple.AppliedMode = ModeRelay
	stableApple.Phase = PhaseRelayActive
	lock, err = store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Replace(stableApple); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	for _, target := range []Backend{BackendExternal, BackendNone, BackendLocalOpenCodex} {
		if _, err := controller.RequestBackend(context.Background(), target); !errors.Is(err, ErrRecoveryRequired) {
			t.Fatalf("generic request away from Apple to %q error = %v", target, err)
		}
	}
	unchanged, err := store.Load()
	if err != nil || unchanged != stableApple || credentialLoads != 0 || runtimeApplies != 0 {
		t.Fatalf("generic request away from Apple mutated state or crossed boundary: state=%#v loads=%d applies=%d err=%v", unchanged, credentialLoads, runtimeApplies, err)
	}
}

func TestLocalCatalogBarrierFailsClosedBeforeRuntimeOrCodexConfigMutation(t *testing.T) {
	controller, store, codexPath := optionalLocalControllerFixture(t, nil, true)
	controller.localPreflight = func(context.Context, string) localopencodex.Result {
		return localopencodex.Result{Availability: localopencodex.AvailabilityReady, ModelCount: 1}
	}
	barrierCalls := 0
	controller.materializeLocalCatalog = func(context.Context, config.Config) error {
		barrierCalls++
		return errors.New("local catalog cannot be materialized")
	}
	runtimeCalls := 0
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
		runtimeCalls++
		return nil
	})
	controller.health = stateHealth{store: store, active: 0}
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RequestBackend(context.Background(), BackendLocalOpenCodex); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), true); !errors.Is(err, ErrLocalOpenCodexPreflight) {
		t.Fatalf("apply error = %v, want local catalog preflight failure", err)
	}
	if barrierCalls != 1 || runtimeCalls != 0 {
		t.Fatalf("barrier/runtime calls = %d/%d, want 1/0", barrierCalls, runtimeCalls)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("failed local barrier changed Codex routing: %q err=%v", after, err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseRecoveryRequired {
		t.Fatalf("failed local barrier state = %#v err=%v", state, err)
	}
}

func TestAwaitFinalizedRequiresLocalCatalogLifecycle(t *testing.T) {
	state := State{
		Generation:     9,
		DesiredMode:    ModeRelay,
		AppliedMode:    ModeRelay,
		DesiredBackend: BackendLocalOpenCodex,
		AppliedBackend: BackendLocalOpenCodex,
		Phase:          PhaseRelayActive,
	}
	calls := 0
	endpoint := func(catalogLifecycle string) LocalRelayEndpoint {
		return LocalRelayEndpoint{
			Valid:            true,
			Generation:       state.Generation,
			DesiredMode:      state.DesiredMode,
			AppliedMode:      state.AppliedMode,
			Phase:            state.Phase,
			RelayAdmission:   "allow",
			CatalogRefresh:   "run",
			CatalogLifecycle: catalogLifecycle,
			LocalOpenCodex:   string(LocalOpenCodexReady),
		}
	}
	controller := &Controller{
		health: healthReaderFunc(func(context.Context, config.Config) LocalRelay {
			calls++
			lifecycle := "paused"
			if calls >= 3 {
				lifecycle = "running"
			}
			value := endpoint(lifecycle)
			return LocalRelay{General: value, Interactive: value}
		}),
		ackTimeout:   100 * time.Millisecond,
		pollInterval: time.Millisecond,
	}
	cfg := config.Config{Catalog: config.CatalogConfig{Owner: config.CatalogOwnerRelay}}
	if err := controller.awaitFinalized(context.Background(), cfg, state); err != nil {
		t.Fatal(err)
	}
	if calls < 3 {
		t.Fatalf("final acknowledgement returned before local catalog lifecycle ran: calls=%d", calls)
	}

	native := state
	native.DesiredMode, native.AppliedMode = ModeNative, ModeNative
	native.DesiredBackend, native.AppliedBackend = BackendNone, BackendNone
	native.Phase = PhaseNativeActive
	parked := endpoint("paused")
	parked.DesiredMode, parked.AppliedMode, parked.Phase = native.DesiredMode, native.AppliedMode, native.Phase
	parked.RelayAdmission, parked.CatalogRefresh = "deny", "pause"
	if !finalHealthAcknowledges(parked, native, false) {
		t.Fatal("native final acknowledgement did not require parked health")
	}
	parked.CatalogLifecycle = "running"
	if finalHealthAcknowledges(parked, native, false) {
		t.Fatal("native final acknowledgement accepted a running catalog")
	}
}

func TestLocalOpenCodexRequestIsRejectedOutsideTheMacOSAppleSiliconBoundary(t *testing.T) {
	controller, store, _ := optionalLocalControllerFixture(t, unavailableHealth{}, false)
	before, legacy, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RequestBackend(context.Background(), BackendLocalOpenCodex); !errors.Is(err, ErrLocalOpenCodexPreflight) {
		t.Fatalf("unsupported-platform local request error = %v", err)
	}
	after, afterLegacy, err := store.Read()
	if err != nil || legacy != afterLegacy || after.Phase != before.Phase || after.Generation != before.Generation {
		t.Fatalf("unsupported-platform local request mutated state: before=%#v after=%#v legacy=%t/%t err=%v", before, after, legacy, afterLegacy, err)
	}
}

func TestLocalOpenCodexRequestRefusesUnavailableBeforeStateMutation(t *testing.T) {
	controller, store, _ := optionalLocalControllerFixture(t, unavailableHealth{}, true)
	before, _, err := store.Read()
	if err != nil {
		t.Fatal(err)
	}
	controller.localPreflight = func(context.Context, string) localopencodex.Result {
		return localopencodex.Result{Availability: localopencodex.AvailabilityUnavailable}
	}
	if _, err := controller.RequestBackend(context.Background(), BackendLocalOpenCodex); !errors.Is(err, ErrLocalOpenCodexPreflight) {
		t.Fatalf("unavailable local request error = %v", err)
	}
	after, legacy, err := store.Read()
	if err != nil || !legacy || after.Phase != before.Phase {
		t.Fatalf("unavailable local request mutated state = %#v legacy=%t err=%v", after, legacy, err)
	}
}

func TestStatusDoesNotAdvertiseLocalReadyWhenRelayIsUnreachable(t *testing.T) {
	directory := t.TempDir()
	relayPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.LocalOpenCodex = &config.LocalOpenCodexProfile{
		UpstreamBaseURL: "http://127.0.0.1:10100/v1",
		CatalogPath:     filepath.Join(directory, "local-catalog.json"),
	}
	if err := config.Write(relayPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(codexPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", cfg.Catalog.Path); err != nil {
		t.Fatal(err)
	}
	var probes int
	controller, err := NewController(
		relayPath,
		codexPath,
		WithHealthReader(unavailableHealth{}),
		withLocalProfileAllowed(true),
		withLocalOpenCodexPreflight(func(context.Context, string) localopencodex.Result {
			probes++
			return localopencodex.Result{Availability: localopencodex.AvailabilityReady}
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	status := controller.Status(context.Background())
	if probes != 0 || status.Connection.LocalRelay != LocalRelayUnreachable || status.Connection.LocalOpenCodex != LocalOpenCodexUnknown {
		t.Fatalf("unreachable status probed/advertised local readiness: probes=%d status=%#v", probes, status)
	}
}

func TestLegacyStaticLocalOpenCodexPersistsTheLocalBackend(t *testing.T) {
	controller, store, _ := controllerFixture(t, unavailableHealth{})
	status := controller.Status(context.Background())
	if status.DesiredBackend != BackendLocalOpenCodex || status.AppliedBackend != BackendLocalOpenCodex || status.Phase != PhaseRelayActive {
		t.Fatalf("legacy static-local status = %#v", status)
	}
	if _, err := controller.EnableCompatibility(context.Background()); err != nil {
		t.Fatalf("legacy enable alias: %v", err)
	}
	state, legacy, err := store.Read()
	if err != nil || legacy {
		t.Fatalf("persisted legacy local state error=%v legacy=%t", err, legacy)
	}
	if state.DesiredBackend != BackendLocalOpenCodex || state.AppliedBackend != BackendLocalOpenCodex || state.Phase != PhaseRelayActive {
		t.Fatalf("persisted legacy local state = %#v", state)
	}
	if _, err := controller.RequestBackend(context.Background(), BackendExternal); err == nil {
		t.Fatal("static local topology accepted an unavailable External backend")
	}
}

func TestStaticLocalTopologyDoesNotTreatUnknownRuntimeObservationAsLocalLoss(t *testing.T) {
	state := State{
		Schema:          SchemaVersion,
		Generation:      7,
		DesiredMode:     ModeRelay,
		AppliedMode:     ModeRelay,
		DesiredBackend:  BackendLocalOpenCodex,
		AppliedBackend:  BackendLocalOpenCodex,
		Phase:           PhaseRelayActive,
		BoundConfigPath: filepath.Join(t.TempDir(), "relay.json"),
	}
	state, err := BindCodexConfig(state, filepath.Join(filepath.Dir(state.BoundConfigPath), "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	active := int64(0)
	endpoint := LocalRelayEndpoint{
		Valid:            true,
		Generation:       state.Generation,
		DesiredMode:      state.DesiredMode,
		AppliedMode:      state.AppliedMode,
		Phase:            state.Phase,
		RelayAdmission:   "allow",
		CatalogRefresh:   "run",
		CatalogLifecycle: "unknown",
		LocalOpenCodex:   string(LocalOpenCodexUnknown),
		RemoteGateway:    string(RemoteGatewayNotApplicable),
		ActiveRequests:   &active,
	}
	status := statusFromState(state, true).withHealth(LocalRelay{General: endpoint, Interactive: endpoint}, state, true, false)
	if status.RelayAdmission != "allow" || status.CatalogRefresh != "run" {
		t.Fatalf("static local status was incorrectly parked: %#v", status)
	}
}

func TestObservedCanonicalProfileDistinguishesLocalCatalogBinding(t *testing.T) {
	controller, _, codexPath := optionalLocalControllerFixture(t, unavailableHealth{}, true)
	cfg, err := config.Load(controller.Store().ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	local, err := cfg.LocalOpenCodexRuntimeConfig()
	if err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(
		codexPath,
		"http://"+local.ListenAddress+"/v1",
		"http://"+local.Responses.Scheduler.InteractiveListenAddress+"/v1",
		local.Catalog.Path,
	); err != nil {
		t.Fatal(err)
	}
	credentialLoads := 0
	controller.loadCredentials = func(config.CredentialsConfig) (credentials.Values, error) {
		credentialLoads++
		return credentials.Values{}, nil
	}
	observed, err := controller.observedRoutingState()
	if err != nil {
		t.Fatal(err)
	}
	if observed.DesiredBackend != BackendLocalOpenCodex || observed.AppliedBackend != BackendLocalOpenCodex || observed.Phase != PhaseRelayActive {
		t.Fatalf("observed local catalog binding = %#v", observed)
	}
	if credentialLoads != 0 {
		t.Fatalf("observed catalog classification loaded external credentials: %d", credentialLoads)
	}
}

func TestAppleConnectionAuthorizerCancelsContextualCredentialLoad(t *testing.T) {
	started := make(chan struct{})
	controller := &Controller{
		appleRuntimeCredentialGuard: func(context.Context) error { return nil },
		loadCredentialsContext: func(ctx context.Context, _ config.CredentialsConfig) (credentials.Values, error) {
			if _, ok := ctx.Deadline(); !ok {
				return credentials.Values{}, errors.New("Apple credential context is unbounded")
			}
			close(started)
			<-ctx.Done()
			return credentials.Values{}, ctx.Err()
		},
	}
	cfg := config.Config{Credentials: config.CredentialsConfig{
		Source:                config.CredentialsSourceKeychain,
		AuthenticationProfile: config.LocalAuthenticationOpenCodexAPIKey,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := controller.appleConnectionAuthorizer(cfg)(ctx)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("contextual Apple credential load did not begin")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, ErrCredentialPreflight) {
			t.Fatalf("cancelled Apple credential load error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled Apple credential load did not unwind")
	}
}

func TestApplyNativeWaitsForParkThenRemovesOnlyManagedRouting(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, nil)
	controller.health = stateHealth{store: store, active: 0}
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}

	status, err := controller.Apply(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseNativeActive || state.DesiredMode != ModeNative || state.AppliedMode != ModeNative {
		t.Fatalf("applied state = %#v", state)
	}
	inspection, err := codexconfig.InspectRouting(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.ManagedRoot || inspection.InteractiveProfileExists {
		t.Fatalf("native apply left relay routing behind: %#v", inspection)
	}
	if _, err := os.Lstat(store.TransactionPath()); !os.IsNotExist(err) {
		t.Fatalf("transaction journal remains after successful apply: %v", err)
	}
	if status.RelayAdmission != "deny" || status.CatalogRefresh != "pause" || status.ActiveRequests == nil || *status.ActiveRequests != 0 {
		t.Fatalf("native status = %#v", status)
	}
}

func TestApplyRejectsMissingDesktopExitConfirmationWithoutMutation(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, stateHealth{})
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), false); !errors.Is(err, ErrDesktopExitConfirmation) {
		t.Fatalf("apply error = %v, want desktop confirmation", err)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("unconfirmed apply changed config = %q, err=%v", after, err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseNativePendingRestart {
		t.Fatalf("unconfirmed apply changed state = %#v, err=%v", state, err)
	}
}

func TestApplyFailureParksForExplicitRecoveryAndRecoveryCompletes(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	controller.ackTimeout = 10 * time.Millisecond
	controller.pollInterval = time.Millisecond
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), true); !errors.Is(err, ErrRelayAcknowledgement) {
		t.Fatalf("apply error = %v, want acknowledgement timeout", err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != PhaseRecoveryRequired {
		t.Fatalf("failed apply did not park recovery: %#v", state)
	}
	inspection, err := codexconfig.InspectRouting(codexPath)
	if err != nil || !inspection.ManagedRoot || !inspection.InteractiveProfileManaged {
		t.Fatalf("failed apply changed routing: %#v, %v", inspection, err)
	}
	if err := ValidateTransaction(store.ConfigPath()); err != nil {
		t.Fatalf("recovery journal is not valid: %v", err)
	}
	capabilityStatus := controller.Status(context.Background())
	if !capabilityStatus.RecoveryCapabilities.CanComplete || !capabilityStatus.RecoveryCapabilities.CanRollback ||
		!capabilityStatus.RecoveryCapabilities.AuthoritativeJournal ||
		capabilityStatus.RecoveryCapabilities.Target != BackendNone ||
		capabilityStatus.RecoveryCapabilities.TargetConfidence != recoveryTargetJournal {
		t.Fatalf("authoritative recovery capabilities = %#v", capabilityStatus.RecoveryCapabilities)
	}

	controller.health = stateHealth{store: store, active: 0}
	status, err := controller.Recover(context.Background(), RecoveryComplete, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseNativeActive {
		t.Fatalf("recovery status = %#v", status)
	}
	state, err = store.Load()
	if err != nil || state.Phase != PhaseNativeActive {
		t.Fatalf("recovery state = %#v, err = %v", state, err)
	}
}

func TestRecoveryRefusesManagedArtifactEditedAfterCrashWitness(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	controller.ackTimeout = 10 * time.Millisecond
	controller.pollInterval = time.Millisecond
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), true); !errors.Is(err, ErrRelayAcknowledgement) {
		t.Fatalf("apply error = %v, want acknowledgement failure", err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(
		codexPath,
		"http://127.0.0.1:18180/v1",
		"http://127.0.0.1:18182/v1",
		filepath.Join(filepath.Dir(codexPath), "edited-catalog.json"),
	); err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}
	capabilityStatus := controller.Status(context.Background())
	if capabilityStatus.RecoveryCapabilities.CanComplete || capabilityStatus.RecoveryCapabilities.CanRollback ||
		capabilityStatus.RecoveryCapabilities.CompleteReason != recoveryReasonJournalMismatch ||
		capabilityStatus.RecoveryCapabilities.RollbackReason != recoveryReasonJournalMismatch {
		t.Fatalf("drifted journal capabilities = %#v", capabilityStatus.RecoveryCapabilities)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("recovery after managed edit error = %v, want recovery required", err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseRecoveryRequired {
		t.Fatalf("recovery after managed edit changed state = %#v err=%v", state, err)
	}
}

func TestApplyNativeRejectsForeignOverrideWithoutParkingOrDeletingIt(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, stateHealth{})
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	foreign := "openai_base_url = \"https://foreign.example/v1\"\n"
	if err := os.WriteFile(codexPath, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), true); err == nil {
		t.Fatal("foreign override was accepted")
	}
	actual, err := os.ReadFile(codexPath)
	if err != nil || string(actual) != foreign {
		t.Fatalf("foreign config changed = %q, err = %v", actual, err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseNativePendingRestart {
		t.Fatalf("preflight failure changed state = %#v, err = %v", state, err)
	}
}

func TestApplyNativeRejectsForeignCatalogOverrideWithoutParkingOrDeletingIt(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, stateHealth{})
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	foreign := "model_catalog_json = \"/foreign-catalog.json\"\n"
	if err := os.WriteFile(codexPath, []byte(foreign), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.Apply(context.Background(), true); err == nil {
		t.Fatal("foreign catalog override was accepted")
	}
	actual, err := os.ReadFile(codexPath)
	if err != nil || string(actual) != foreign {
		t.Fatalf("foreign config changed = %q, err = %v", actual, err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseNativePendingRestart {
		t.Fatalf("preflight failure changed state = %#v, err = %v", state, err)
	}
}

func TestLegacyInferenceRejectsForeignCatalogOverride(t *testing.T) {
	controller, _, codexPath := controllerFixture(t, unavailableHealth{})
	if err := codexconfig.DisableWithInteractiveProfile(codexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("model_catalog_json = \"/foreign-catalog.json\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := NewRelayState(controller.Store().ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.inferLegacyState(state); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("legacy foreign catalog inference error = %v, want recovery required", err)
	}
}

func TestStatusIsSafeWhenLocalRelayIsUnreachable(t *testing.T) {
	controller, _, _ := controllerFixture(t, unavailableHealth{})
	status := controller.Status(context.Background())
	if status.SchemaVersion != 4 {
		t.Fatalf("status schema = %d, want 4", status.SchemaVersion)
	}
	if status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback ||
		status.RecoveryCapabilities.CompleteReason != recoveryReasonNotRequired ||
		status.RecoveryCapabilities.RollbackReason != recoveryReasonNotRequired {
		t.Fatalf("active status advertised recovery: %#v", status.RecoveryCapabilities)
	}
	if status.Connection.LocalRelay != LocalRelayUnreachable || status.Connection.RoutingSync != RoutingSyncUnreachable || status.ActiveRequests != nil {
		t.Fatalf("unreachable status = %#v", status)
	}
	payload, err := json.Marshal(status)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"127.0.0.1", "gateway.example", "credential", "config.toml"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("safe status leaked %q: %s", forbidden, payload)
		}
	}
}

func TestStatusProjectsWatcherDriftAsRecoveryAndDenial(t *testing.T) {
	controller, store, _ := controllerFixture(t, nil)
	if _, err := controller.EnableCompatibility(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	active := int64(0)
	endpoint := LocalRelayEndpoint{
		Valid:               true,
		Generation:          state.Generation,
		DesiredMode:         state.DesiredMode,
		AppliedMode:         state.AppliedMode,
		Phase:               state.Phase,
		RelayAdmission:      "deny",
		CatalogRefresh:      "pause",
		RoutingStateInvalid: true,
		CatalogLifecycle:    "paused",
		RemoteGateway:       string(RemoteGatewayNotApplicable),
		LocalOpenCodex:      string(LocalOpenCodexUnknown),
		ActiveRequests:      &active,
	}
	controller.health = healthReaderFunc(func(context.Context, config.Config) LocalRelay {
		return LocalRelay{General: endpoint, Interactive: endpoint}
	})

	status := controller.Status(context.Background())
	if status.Phase != PhaseRecoveryRequired || status.DesiredBackend != BackendUnknown || status.AppliedBackend != BackendUnknown ||
		status.RelayAdmission != "deny" || status.CatalogRefresh != "pause" || status.Connection.RoutingSync != RoutingSyncInvalid || !status.DesktopRestartRequired {
		t.Fatalf("drift status did not project the parked runtime: %#v", status)
	}
}

func TestValidateTransactionRejectsMalformedJournal(t *testing.T) {
	controller, store, _ := controllerFixture(t, unavailableHealth{})
	_ = controller
	if err := os.WriteFile(store.TransactionPath(), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTransaction(store.ConfigPath()); err == nil {
		t.Fatal("malformed transaction journal was accepted")
	}
}

func TestSchemaV2JournalRejectsMissingOrMismatchedBackends(t *testing.T) {
	base := transactionJournal{
		Schema:                 SchemaVersion,
		Generation:             1,
		Target:                 ModeRelay,
		Origin:                 ModeNative,
		Stage:                  transactionPrepared,
		RelayConfigFingerprint: "absent",
		CodexConfigFingerprint: "absent",
		InteractiveFingerprint: "absent",
	}

	if err := normalizeTransactionJournal(base).validate(); err == nil {
		t.Fatal("schema v2 journal with missing backend labels was accepted")
	}
	mismatched := base
	mismatched.TargetBackend = BackendLocalOpenCodex
	mismatched.OriginBackend = BackendExternal
	if err := normalizeTransactionJournal(mismatched).validate(); err == nil {
		t.Fatal("schema v2 journal with mode/backend mismatch was accepted")
	}

	legacy := base
	legacy.Schema = legacySchemaVersion
	migrated := normalizeTransactionJournal(legacy)
	if migrated.TargetBackend != BackendExternal || migrated.OriginBackend != BackendNone || migrated.validate() != nil {
		t.Fatalf("legacy journal migration = %#v", migrated)
	}

	v2 := base
	v2.Schema = explicitBackendSchemaVersion
	v2.TargetBackend = BackendLocalOpenCodex
	v2.OriginBackend = BackendNone
	migrated = normalizeTransactionJournal(v2)
	if migrated.Schema != SchemaVersion || migrated.TargetBackend != BackendLocalOpenCodex || migrated.OriginBackend != BackendNone || migrated.validate() != nil {
		t.Fatalf("schema v2 journal migration changed backend meaning = %#v", migrated)
	}

	v2Future := v2
	v2Future.TargetBackend = BackendLocalAppleContainer
	if migrated := normalizeTransactionJournal(v2Future); migrated.validate() == nil {
		t.Fatalf("schema v2 journal accepted future Apple backend = %#v", migrated)
	}
}

func TestCatalogAdmissionFailsClosedAfterApplyStarts(t *testing.T) {
	controller, _, _ := controllerFixture(t, unavailableHealth{})
	if !controller.CatalogAdmissionAllowed() {
		t.Fatal("legacy relay-active routing should admit catalog refresh")
	}
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	if !controller.CatalogAdmissionAllowed() {
		t.Fatal("native pending restart must preserve catalog traffic until apply")
	}
	controller.ackTimeout = 10 * time.Millisecond
	controller.pollInterval = time.Millisecond
	if _, err := controller.Apply(context.Background(), true); !errors.Is(err, ErrRelayAcknowledgement) {
		t.Fatalf("apply error = %v", err)
	}
	if controller.CatalogAdmissionAllowed() {
		t.Fatal("recovery routing admitted catalog refresh")
	}
}

func TestDeprecatedCompatibilityAliasesOnlyRecordIntent(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}

	status, err := controller.DisableCompatibility(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseNativePendingRestart || status.DesiredMode != ModeNative || status.AppliedMode != ModeRelay {
		t.Fatalf("disable alias status = %#v", status)
	}
	afterDisable, err := os.ReadFile(codexPath)
	if err != nil || string(afterDisable) != string(before) {
		t.Fatalf("disable alias changed Codex config = %q, err=%v", afterDisable, err)
	}

	status, err = controller.EnableCompatibility(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseRelayActive || status.DesiredMode != ModeRelay || status.AppliedMode != ModeRelay {
		t.Fatalf("enable alias status = %#v", status)
	}
	afterEnable, err := os.ReadFile(codexPath)
	if err != nil || string(afterEnable) != string(before) {
		t.Fatalf("enable alias changed Codex config = %q, err=%v", afterEnable, err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseRelayActive {
		t.Fatalf("alias did not persist requested state = %#v, err=%v", state, err)
	}
}

func TestFirstRelayRequestInfersNativeArtifactsAndStaysPending(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	if err := codexconfig.DisableWithInteractiveProfile(codexPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}

	status, err := controller.EnableCompatibility(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseRelayPendingRestart || status.DesiredMode != ModeRelay || status.AppliedMode != ModeNative {
		t.Fatalf("first relay request status = %#v", status)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("first relay request changed native config = %q, err=%v", after, err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseRelayPendingRestart {
		t.Fatalf("persisted first request state = %#v, err=%v", state, err)
	}
	snapshot := NewWatcher(store, 0).Snapshot()
	if snapshot.AllowsDataPlane() || snapshot.AllowsCatalog() {
		t.Fatalf("fresh native-to-relay request admitted traffic before apply: %#v", snapshot)
	}
}

func TestJournalFreeRecoveryReconcilesDeletedStateThroughParkedApply(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.DisableWithInteractiveProfile(codexPath); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.StatePath()); err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}

	capabilityStatus := controller.Status(context.Background())
	if !capabilityStatus.RecoveryCapabilities.CanComplete || capabilityStatus.RecoveryCapabilities.CanRollback ||
		capabilityStatus.RecoveryCapabilities.RollbackReason != recoveryReasonJournalMissing ||
		capabilityStatus.RecoveryCapabilities.Target != BackendNone ||
		capabilityStatus.RecoveryCapabilities.TargetConfidence != recoveryTargetObserved {
		t.Fatalf("journal-free capabilities = %#v", capabilityStatus.RecoveryCapabilities)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, false); !errors.Is(err, ErrDesktopExitConfirmation) {
		t.Fatalf("journal-free recovery without Desktop exit = %v", err)
	}
	status, err := controller.Recover(context.Background(), RecoveryComplete, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseNativeActive || status.DesiredMode != ModeNative || status.AppliedMode != ModeNative {
		t.Fatalf("journal-free recovery status = %#v", status)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("journal-free recovery changed config = %q, err=%v", after, err)
	}
	if _, err := controller.Recover(context.Background(), RecoveryRollback, false); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("journal-free rollback error = %v, want recovery required", err)
	}
}

func TestRecoveryCompleteReplacesMalformedJournalThroughParkedApply(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	before, err := os.ReadFile(codexPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(store.TransactionPath(), []byte("{not-json}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}

	capabilityStatus := controller.Status(context.Background())
	if !capabilityStatus.RecoveryCapabilities.CanComplete || capabilityStatus.RecoveryCapabilities.CanRollback ||
		capabilityStatus.RecoveryCapabilities.RollbackReason != recoveryReasonJournalMalformed ||
		!validBackend(capabilityStatus.RecoveryCapabilities.Target) ||
		capabilityStatus.RecoveryCapabilities.TargetConfidence != recoveryTargetObserved {
		t.Fatalf("malformed-journal capabilities = %#v", capabilityStatus.RecoveryCapabilities)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, false); !errors.Is(err, ErrDesktopExitConfirmation) {
		t.Fatalf("malformed-journal recovery without Desktop exit = %v", err)
	}
	status, err := controller.Recover(context.Background(), RecoveryComplete, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseRelayActive || status.DesiredMode != ModeRelay || status.AppliedMode != ModeRelay {
		t.Fatalf("malformed-journal recovery status = %#v", status)
	}
	if _, err := os.Lstat(store.TransactionPath()); !os.IsNotExist(err) {
		t.Fatalf("malformed journal remains after complete recovery: %v", err)
	}
	after, err := os.ReadFile(codexPath)
	if err != nil || string(after) != string(before) {
		t.Fatalf("malformed-journal recovery changed config = %q, err=%v", after, err)
	}
}

func TestRecoveryCompleteReconcilesApplyingStateWithoutJournal(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	state, err := lock.Load()
	if err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	applying := state
	applying.Phase = PhaseApplying
	applying.Generation++
	if err := lock.Save(applying); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a process crash after a previous version removed its journal
	// but before it saved the final state: the marker-owned configuration is
	// already native while the durable phase remains applying.
	if err := codexconfig.DisableWithInteractiveProfile(codexPath); err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}

	status, err := controller.Recover(context.Background(), RecoveryComplete, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseNativeActive || status.AppliedMode != ModeNative {
		t.Fatalf("journal-free applying recovery status = %#v", status)
	}
	if _, err := os.Lstat(store.TransactionPath()); !os.IsNotExist(err) {
		t.Fatalf("unexpected journal after recovery: %v", err)
	}
}

func TestObservedRecoveryJournalCannotInventRollbackOrigin(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, nil)
	controller.health = stateHealth{store: store, active: 0}
	var runtimeCalls int
	controller.runtimeControl = runtimeControlFunc(func(context.Context, uint64, Backend) error {
		runtimeCalls++
		return ErrRelayAcknowledgement
	})
	if _, err := controller.Request(context.Background(), ModeNative); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.DisableWithInteractiveProfile(codexPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(store.StatePath()); err != nil {
		t.Fatal(err)
	}

	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRelayAcknowledgement) {
		t.Fatalf("observed recovery failure = %v", err)
	}
	journal, found, err := controller.loadJournal()
	if err != nil || !found || journal.OriginAuthoritative {
		t.Fatalf("observed recovery journal = %#v found=%t err=%v", journal, found, err)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRelayAcknowledgement) {
		t.Fatalf("second observed recovery failure = %v", err)
	}
	journal, found, err = controller.loadJournal()
	if err != nil || !found || journal.OriginAuthoritative {
		t.Fatalf("second observed recovery journal = %#v found=%t err=%v", journal, found, err)
	}
	if _, err := controller.Recover(context.Background(), RecoveryRollback, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("synthetic recovery rollback error = %v, want recovery required", err)
	}
	if runtimeCalls != 2 {
		t.Fatalf("synthetic recovery rollback invoked runtime again: %d", runtimeCalls)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseRecoveryRequired || state.AllowsDataPlane() || state.AllowsCatalog() {
		t.Fatalf("synthetic recovery rollback reopened routing: %#v err=%v", state, err)
	}
}

func TestRecoveryRepairsStaleJournalBesideFinalState(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, nil)
	controller.health = stateHealth{store: store, active: 0}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation++
	state.DesiredMode, state.AppliedMode, state.Phase = ModeNative, ModeRelay, PhaseNativePendingRestart
	writeState(t, store, state)
	state.Generation++
	state.DesiredMode, state.AppliedMode, state.Phase = ModeRelay, ModeRelay, PhaseRelayActive
	writeState(t, store, state)
	applying := state
	applying.DesiredMode = ModeNative
	applying.DesiredBackend = BackendNone
	applying.Phase = PhaseApplying
	journal, err := controller.newJournal(applying, BackendNone, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.writeJournal(journal); err != nil {
		t.Fatal(err)
	}

	status, err := controller.Recover(context.Background(), RecoveryComplete, true)
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseNativeActive {
		t.Fatalf("stale-journal recovery status = %#v", status)
	}
}

func controllerFixture(t *testing.T, health HealthReader) (*Controller, *Store, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("http://127.0.0.1:10100/v1", config.CredentialsSourceNone)
	if err != nil {
		t.Fatal(err)
	}
	cfg.UpstreamMode = config.UpstreamModeLocalOpenCodex
	cfg.Catalog.Owner = config.CatalogOwnerRemoteManager
	cfg.Catalog.Path = filepath.Join(directory, "catalog.json")
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-5.6-sol\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(codexPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", cfg.Catalog.Path); err != nil {
		t.Fatal(err)
	}
	options := []ControllerOption{WithTransitionTiming(time.Second, time.Millisecond)}
	if health != nil {
		options = append(options, WithHealthReader(health))
	}
	controller, err := NewController(configPath, codexPath, options...)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return controller, store, codexPath
}

func externalNativeControllerFixture(t *testing.T, codexContent string) (*Controller, *Store, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceNone)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte(codexContent), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err := NewRelayState(configPath)
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.DesiredMode = ModeNative
	state.AppliedMode = ModeNative
	state.DesiredBackend = BackendNone
	state.AppliedBackend = BackendNone
	state.Phase = PhaseNativeActive
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	controller, err := NewController(
		configPath,
		codexPath,
		WithTransitionTiming(time.Second, time.Millisecond),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}
	return controller, store, codexPath
}

func optionalLocalControllerFixture(t *testing.T, health HealthReader, localProfileAllowed bool) (*Controller, *Store, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceFile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	cfg.LocalOpenCodex = &config.LocalOpenCodexProfile{
		UpstreamBaseURL: "http://127.0.0.1:10100/v1",
		CatalogPath:     filepath.Join(directory, "local-catalog.json"),
	}
	if err := config.Write(configPath, cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codexPath, []byte("model = \"gpt-5.6-sol\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.EnableWithInteractiveProfile(codexPath, "http://127.0.0.1:18180/v1", "http://127.0.0.1:18182/v1", cfg.Catalog.Path); err != nil {
		t.Fatal(err)
	}
	options := []ControllerOption{WithTransitionTiming(time.Second, time.Millisecond), withLocalProfileAllowed(localProfileAllowed)}
	if health != nil {
		options = append(options, WithHealthReader(health))
	}
	controller, err := NewController(configPath, codexPath, options...)
	if err != nil {
		t.Fatal(err)
	}
	store, err := Open(configPath)
	if err != nil {
		t.Fatal(err)
	}
	return controller, store, codexPath
}

type unavailableHealth struct{}

func (unavailableHealth) Read(context.Context, config.Config) LocalRelay { return LocalRelay{} }

type stateHealth struct {
	store  *Store
	active int64
}

func (h stateHealth) Read(_ context.Context, _ config.Config) LocalRelay {
	if h.store == nil {
		return LocalRelay{}
	}
	state, _, err := h.store.Read()
	if err != nil {
		return LocalRelay{}
	}
	active := h.active
	localAvailability := string(LocalOpenCodexUnknown)
	if state.AppliedBackend == BackendLocalOpenCodex || state.AppliedBackend == BackendLocalAppleContainer {
		localAvailability = string(LocalOpenCodexReady)
	}
	endpoint := LocalRelayEndpoint{
		Valid:            true,
		Generation:       state.Generation,
		DesiredMode:      state.DesiredMode,
		AppliedMode:      state.AppliedMode,
		Phase:            state.Phase,
		RelayAdmission:   admissionForState(state),
		CatalogRefresh:   catalogForState(state),
		CatalogLifecycle: lifecycleForState(state),
		RemoteGateway:    string(RemoteGatewayNotApplicable),
		LocalOpenCodex:   localAvailability,
		ActiveRequests:   &active,
	}
	return LocalRelay{General: endpoint, Interactive: endpoint}
}

func admissionForState(state State) string {
	if state.AllowsDataPlane() {
		return "allow"
	}
	return "deny"
}

func catalogForState(state State) string {
	if state.AllowsCatalog() {
		return "run"
	}
	return "pause"
}

func lifecycleForState(state State) string {
	if state.AllowsCatalog() {
		return "running"
	}
	return "paused"
}

func TestControllerExternalRecoveryGateBlocksOrdinaryMutationsAndStatus(t *testing.T) {
	controller, store, _ := controllerFixture(t, unavailableHealth{})
	state, err := NewRelayState(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, controller.codexConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
	status := controller.Status(context.Background())
	durable, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if status.Phase != PhaseRecoveryRequired || status.RelayAdmission != "deny" || status.CatalogRefresh != "pause" ||
		status.Generation != durable.Generation ||
		status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback || controller.CatalogAdmissionAllowed() {
		t.Fatalf("externally gated status=%#v", status)
	}
	if _, err := controller.RequestBackend(context.Background(), BackendNone); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("externally gated request error=%v", err)
	}
	if _, err := controller.Cancel(context.Background()); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("externally gated cancel error=%v", err)
	}
	if _, err := controller.Apply(context.Background(), true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("externally gated apply error=%v", err)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("externally gated recovery error=%v", err)
	}
}

func TestControllerAdvertisesRecoverableExternalGateOnlyWhenDurablyParked(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	state, err := NewRelayState(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = PhaseRecoveryRequired
	state.Generation++
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	if err := codexconfig.DisableWithInteractiveProfile(codexPath); err != nil {
		t.Fatal(err)
	}
	controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
	controller.recoveryGateReleasable = func() bool { return true }
	status := controller.Status(context.Background())
	if status.Phase != PhaseRecoveryRequired || !status.RecoveryCapabilities.CanComplete ||
		status.RecoveryCapabilities.CompleteReason != recoveryReasonObservedStateVerified ||
		status.Generation != state.Generation ||
		status.DesiredBackend != BackendUnknown || status.AppliedBackend != BackendUnknown ||
		status.RelayAdmission != "deny" || status.CatalogRefresh != "pause" {
		t.Fatalf("durably parked external gate status=%#v", status)
	}
	controller.recoveryGateReleasable = func() bool { return false }
	status = controller.Status(context.Background())
	if status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback {
		t.Fatalf("non-releasable external gate advertised recovery=%#v", status.RecoveryCapabilities)
	}
}

func TestControllerDoesNotAdvertiseReleasableParkedGateForObservedLocalTarget(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	state, err := NewRelayState(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Phase = PhaseRecoveryRequired
	state.Generation++
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
	controller.recoveryGateReleasable = func() bool { return true }
	status := controller.Status(context.Background())
	if status.Phase != PhaseRecoveryRequired ||
		status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback ||
		status.DesiredBackend != BackendUnknown || status.AppliedBackend != BackendUnknown ||
		status.RelayAdmission != "deny" || status.CatalogRefresh != "pause" {
		t.Fatalf("observed Local target advertised removal recovery=%#v", status)
	}
}

func TestControllerAdvertisesCompleteOnlyForReleasableGateAfterStableCommit(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	state, err := NewRelayState(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
	controller.recoveryGateReleasable = func() bool { return true }

	status := controller.Status(context.Background())
	if status.Phase != PhaseRecoveryRequired ||
		status.DesiredBackend != BackendUnknown || status.AppliedBackend != BackendUnknown ||
		status.RelayAdmission != "deny" || status.CatalogRefresh != "pause" ||
		status.Generation != state.Generation ||
		!status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback ||
		status.RecoveryCapabilities.CompleteReason != recoveryReasonObservedStateVerified {
		t.Fatalf("stable gated status=%#v", status)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("ordinary controller bypassed stable gate: %v", err)
	}
}

func TestControllerAdvertisesCompleteOnlyForReleasableStableGateWithRecoveryJournal(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	state, err := NewRelayState(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = 4
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := controller.newJournal(state, state.DesiredBackend, false)
	if err != nil {
		t.Fatal(err)
	}
	journal.Generation = state.Generation - 1
	journal.Origin = ModeNative
	journal.OriginBackend = BackendNone
	journal.OriginAuthoritative = false
	journal.Stage = transactionConfigMutated
	if err := controller.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
	controller.recoveryGateReleasable = func() bool { return true }

	status := controller.Status(context.Background())
	if status.Phase != PhaseRecoveryRequired ||
		status.DesiredBackend != BackendUnknown || status.AppliedBackend != BackendUnknown ||
		status.RelayAdmission != "deny" || status.CatalogRefresh != "pause" ||
		status.Generation != state.Generation ||
		!status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback ||
		status.RecoveryCapabilities.CompleteReason != recoveryReasonJournalVerified {
		t.Fatalf("stable journal-gated status=%#v", status)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("ordinary controller bypassed stable journal gate: %v", err)
	}
}

func TestControllerAdvertisesCompleteOnlyForReleasableParkedGateWithRecoveryJournal(t *testing.T) {
	controller, store, codexPath := controllerFixture(t, unavailableHealth{})
	state, err := NewRelayState(store.ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	state, err = BindCodexConfig(state, codexPath)
	if err != nil {
		t.Fatal(err)
	}
	state.Generation = 4
	state.AppliedMode = ModeNative
	state.AppliedBackend = BackendNone
	state.Phase = PhaseRecoveryRequired
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Save(state); err != nil {
		_ = lock.Close()
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	journal, err := controller.newJournal(state, state.DesiredBackend, false)
	if err != nil {
		t.Fatal(err)
	}
	journal.Generation = state.Generation - 1
	journal.OriginAuthoritative = false
	journal.Stage = transactionConfigMutated
	if err := controller.writeJournal(journal); err != nil {
		t.Fatal(err)
	}
	controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
	controller.recoveryGateReleasable = func() bool { return true }

	status := controller.Status(context.Background())
	if status.Phase != PhaseRecoveryRequired ||
		status.DesiredBackend != BackendUnknown || status.AppliedBackend != BackendUnknown ||
		status.Generation != state.Generation ||
		!status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback ||
		status.RecoveryCapabilities.CompleteReason != recoveryReasonJournalVerified {
		t.Fatalf("parked journal-gated status=%#v", status)
	}
	if _, err := controller.Recover(context.Background(), RecoveryComplete, true); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("ordinary controller bypassed parked journal gate: %v", err)
	}
}

func TestControllerStableGateCapabilityRejectsUnsafeStateOrRoutingJournal(t *testing.T) {
	t.Run("legacy state", func(t *testing.T) {
		controller, _, _ := controllerFixture(t, unavailableHealth{})
		controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
		controller.recoveryGateReleasable = func() bool { return true }
		status := controller.Status(context.Background())
		if status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback {
			t.Fatalf("legacy stable gate advertised recovery=%#v", status)
		}
	})

	t.Run("local backend", func(t *testing.T) {
		controller, store, codexPath := controllerFixture(t, unavailableHealth{})
		state, err := NewRelayState(store.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		state, err = BindCodexConfig(state, codexPath)
		if err != nil {
			t.Fatal(err)
		}
		state.DesiredBackend = BackendLocalOpenCodex
		state.AppliedBackend = BackendLocalOpenCodex
		lock, err := store.Lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Save(state); err != nil {
			_ = lock.Close()
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
		controller.recoveryGateReleasable = func() bool { return true }
		status := controller.Status(context.Background())
		if status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback {
			t.Fatalf("local stable gate advertised recovery=%#v", status)
		}
	})

	t.Run("routing journal", func(t *testing.T) {
		controller, store, codexPath := controllerFixture(t, unavailableHealth{})
		state, err := NewRelayState(store.ConfigPath())
		if err != nil {
			t.Fatal(err)
		}
		state, err = BindCodexConfig(state, codexPath)
		if err != nil {
			t.Fatal(err)
		}
		lock, err := store.Lock(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := lock.Save(state); err != nil {
			_ = lock.Close()
			t.Fatal(err)
		}
		if err := lock.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(store.TransactionPath(), validTransactionJournal(t, state), 0o600); err != nil {
			t.Fatal(err)
		}
		controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
		controller.recoveryGateReleasable = func() bool { return true }
		status := controller.Status(context.Background())
		if status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback {
			t.Fatalf("journaled stable gate advertised recovery=%#v", status)
		}
	})

	t.Run("invalid state", func(t *testing.T) {
		controller, store, _ := controllerFixture(t, unavailableHealth{})
		if err := os.WriteFile(StatePath(store.ConfigPath()), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		controller.recoveryGate = func() error { return errors.New("bounded external recovery witness") }
		controller.recoveryGateReleasable = func() bool { return true }
		status := controller.Status(context.Background())
		if status.Generation != 0 ||
			status.RecoveryCapabilities.CanComplete || status.RecoveryCapabilities.CanRollback {
			t.Fatalf("invalid stable gate advertised recovery=%#v", status)
		}
	})
}

func TestRecoveryProjectionNeverExposesSyntheticGeneration(t *testing.T) {
	projected := recoveryProjectionState(filepath.Join(t.TempDir(), "relay.json"), State{Generation: 42}, false)
	if projected.Generation != 0 {
		t.Fatalf("invalid projection generation=%d", projected.Generation)
	}
	durable := recoveryProjectionState(filepath.Join(t.TempDir(), "relay.json"), State{Generation: 42}, true)
	if durable.Generation != 42 {
		t.Fatalf("durable projection generation=%d", durable.Generation)
	}
}
