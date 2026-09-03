package routing

import (
	"context"
	"encoding/base64"
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

func TestRuntimeRoutingHappyPathUsesThreeGenerationsAndWaitsForLifecycleAcknowledgement(t *testing.T) {
	controller, store, request, applied := runtimeRoutingFixture(t, nil)

	final, err := controller.SwitchRuntimeRouting(context.Background(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	if final != request.ExpectedOriginRoutingGeneration+3 {
		t.Fatalf("final generation=%d, want E+3", final)
	}
	if len(*applied) != 1 || (*applied)[0] != BackendLocalAppleContainer {
		t.Fatalf("runtime applies=%#v", *applied)
	}
	if pending, err := controller.RuntimeRoutingPending(context.Background()); err != nil || !pending {
		t.Fatalf("lifecycle witness released before manager commit: pending=%t err=%v", pending, err)
	}
	state, err := store.Load()
	if err != nil || !stableStateForBackend(state, BackendLocalAppleContainer) {
		t.Fatalf("stable Apple state=%#v err=%v", state, err)
	}
	// A lost Switch response after the routing journal was removed is accepted
	// only through the same operation witness, and does not apply the runtime a
	// second time.
	replayed, err := controller.ReconcileRuntimeRouting(context.Background(), request, true)
	if err != nil || replayed != final || len(*applied) != 1 {
		t.Fatalf("lost response reconciliation generation=%d applies=%#v err=%v", replayed, *applied, err)
	}
	if err := controller.AcknowledgeRuntimeRouting(context.Background(), request, final); err != nil {
		t.Fatal(err)
	}
	if pending, err := controller.RuntimeRoutingPending(context.Background()); err != nil || pending {
		t.Fatalf("acknowledged lifecycle witness: pending=%t err=%v", pending, err)
	}
	if _, err := os.Lstat(store.RuntimeRoutingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("runtime routing witness remains after ack: %v", err)
	}
}

func TestRuntimeRoutingReconcileRequiresFreshDesktopExitBeforeMutation(t *testing.T) {
	controller, store, request, applied := runtimeRoutingFixture(t, nil)
	if err := controller.beginRuntimeRouting(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	before, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := controller.ReconcileRuntimeRouting(context.Background(), request, false); !errors.Is(err, ErrDesktopExitConfirmation) {
		t.Fatalf("error=%v, want Desktop exit confirmation", err)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after != before || len(*applied) != 0 {
		t.Fatalf("reconcile mutated without fresh confirmation: before=%#v after=%#v applies=%#v", before, after, *applied)
	}
}

func TestRuntimeRoutingRecoversRequestAndApplyingSaveCrashPositions(t *testing.T) {
	for _, crash := range []string{"request_save", "applying_save"} {
		t.Run(crash, func(t *testing.T) {
			controller, store, request, applied := runtimeRoutingFixture(t, nil)
			if err := controller.beginRuntimeRouting(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			state, err := store.Load()
			if err != nil {
				t.Fatal(err)
			}
			lock, err := store.Lock(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			switch crash {
			case "request_save":
				origin := state
				origin.Generation = request.ExpectedOriginRoutingGeneration
				origin.DesiredBackend = BackendExternal
				origin.AppliedBackend = BackendExternal
				origin.DesiredMode = ModeRelay
				origin.AppliedMode = ModeRelay
				origin.Phase = PhaseRelayActive
				if err := lock.Replace(origin); err != nil {
					t.Fatal(err)
				}
			case "applying_save":
				state.Generation++
				state.Phase = PhaseApplying
				if err := lock.Save(state); err != nil {
					t.Fatal(err)
				}
			}
			_ = lock.Close()

			final, err := controller.ReconcileRuntimeRouting(context.Background(), request, true)
			if err != nil {
				t.Fatal(err)
			}
			if len(*applied) != 1 {
				t.Fatalf("runtime applies=%#v", *applied)
			}
			state, err = store.Load()
			if err != nil || state.Generation != final || !stableStateForBackend(state, BackendLocalAppleContainer) {
				t.Fatalf("recovered state=%#v final=%d err=%v", state, final, err)
			}
		})
	}
}

func TestRuntimeRoutingRecoversResidentAcknowledgementLoss(t *testing.T) {
	ackLost := true
	controller, store, request, applied := runtimeRoutingFixture(t, nil)
	controller.runtimeControl = runtimeControlFunc(func(_ context.Context, _ uint64, backend Backend) error {
		*applied = append(*applied, backend)
		if ackLost {
			ackLost = false
			return ErrControlUnavailable
		}
		return nil
	})
	if _, err := controller.SwitchRuntimeRouting(context.Background(), request, true); !errors.Is(err, ErrRelayAcknowledgement) {
		t.Fatalf("lost resident ACK error=%v", err)
	}
	state, err := store.Load()
	if err != nil || state.Phase != PhaseRecoveryRequired {
		t.Fatalf("ACK loss state=%#v err=%v", state, err)
	}
	final, err := controller.ReconcileRuntimeRouting(context.Background(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.Load()
	if err != nil || state.Generation != final || !stableStateForBackend(state, BackendLocalAppleContainer) {
		t.Fatalf("ACK recovery state=%#v final=%d err=%v", state, final, err)
	}
	if len(*applied) != 2 {
		t.Fatalf("ACK recovery runtime applies=%#v", *applied)
	}
	if final != request.ExpectedOriginRoutingGeneration+5 {
		t.Fatalf("ACK recovery generation=%d, want exact E+5", final)
	}
}

func TestRuntimeRoutingNestedRecoveryRetainsSourceJournalAndNeverRegressesGeneration(t *testing.T) {
	controller, store, request, applied := runtimeRoutingFixture(t, nil)
	controller.runtimeControl = runtimeControlFunc(func(_ context.Context, _ uint64, backend Backend) error {
		*applied = append(*applied, backend)
		return ErrControlUnavailable
	})
	if _, err := controller.SwitchRuntimeRouting(context.Background(), request, true); !errors.Is(err, ErrRelayAcknowledgement) {
		t.Fatalf("initial apply failure=%v", err)
	}
	state, err := store.Load()
	if err != nil || state.Generation != request.ExpectedOriginRoutingGeneration+3 || state.Phase != PhaseRecoveryRequired {
		t.Fatalf("initial recovery state=%#v err=%v", state, err)
	}
	source, found, err := controller.loadJournal()
	if err != nil || !found || source.Generation != request.ExpectedOriginRoutingGeneration+2 {
		t.Fatalf("source journal=%#v found=%t err=%v", source, found, err)
	}
	witness, found, err := store.loadRuntimeRouting(controller.codexConfigPath)
	if err != nil || !found {
		t.Fatalf("runtime witness found=%t err=%v", found, err)
	}
	if err := witness.setRecoveryAttempt(request.Direction, state, state.Generation, source); err != nil {
		t.Fatal(err)
	}
	if err := store.writeRuntimeRouting(controller.codexConfigPath, witness); err != nil {
		t.Fatal(err)
	}

	// Model a crash after E+4 applying was saved but before its replacement
	// routing journal, followed by failApplying's E+5 recovery save. The exact
	// E+2 source journal is intentionally retained.
	lock, err := store.Lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	applying := state
	applying.Generation++
	applying.Phase = PhaseApplying
	if err := lock.Replace(applying); err != nil {
		t.Fatal(err)
	}
	postFailure := applying
	postFailure.Generation++
	postFailure.Phase = PhaseRecoveryRequired
	if err := lock.Replace(postFailure); err != nil {
		t.Fatal(err)
	}
	_ = lock.Close()
	if pending, err := controller.RuntimeRoutingPending(context.Background()); err != nil || !pending {
		t.Fatalf("nested source witness pending=%t err=%v", pending, err)
	}

	controller.runtimeControl = runtimeControlFunc(func(_ context.Context, _ uint64, backend Backend) error {
		*applied = append(*applied, backend)
		return nil
	})
	final, err := controller.ReconcileRuntimeRouting(context.Background(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	if final <= postFailure.Generation || final != postFailure.Generation+2 {
		t.Fatalf("nested recovery regressed or guessed generation: post-failure=%d final=%d", postFailure.Generation, final)
	}
	state, err = store.Load()
	if err != nil || state.Generation != final || !stableStateForBackend(state, BackendLocalAppleContainer) {
		t.Fatalf("nested recovered state=%#v final=%d err=%v", state, final, err)
	}
}

func TestRuntimeRoutingConfigMutationFailsClosedWithoutFurtherMutation(t *testing.T) {
	controller, store, request, applied := runtimeRoutingFixture(t, nil)
	controller.runtimeControl = runtimeControlFunc(func(_ context.Context, _ uint64, backend Backend) error {
		*applied = append(*applied, backend)
		return os.WriteFile(controller.codexConfigPath, []byte("openai_base_url = \"https://foreign.example/v1\"\n"), 0o600)
	})
	if _, err := controller.SwitchRuntimeRouting(context.Background(), request, true); err == nil {
		t.Fatal("foreign config mutation unexpectedly completed")
	}
	before, err := os.ReadFile(controller.codexConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	applyCount := len(*applied)
	if _, err := controller.ReconcileRuntimeRouting(context.Background(), request, true); !errors.Is(err, ErrRuntimeRoutingConflict) {
		t.Fatalf("foreign config reconciliation error=%v", err)
	}
	after, err := os.ReadFile(controller.codexConfigPath)
	if err != nil || string(after) != string(before) || len(*applied) != applyCount {
		t.Fatalf("foreign config was mutated: before=%q after=%q applies=%#v err=%v", before, after, *applied, err)
	}
	if _, found, err := controller.loadJournal(); err != nil || !found {
		t.Fatalf("crash journal not retained: found=%t err=%v", found, err)
	}
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRoutingFinalSaveBeforeJournalRemovalAndRestoreOrigin(t *testing.T) {
	controller, store, request, applied := runtimeRoutingFixture(t, nil)
	final, err := controller.SwitchRuntimeRouting(context.Background(), request, true)
	if err != nil {
		t.Fatal(err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	witness, found, err := store.loadRuntimeRouting(controller.codexConfigPath)
	if err != nil || !found {
		t.Fatalf("witness found=%t err=%v", found, err)
	}
	witness.Phase = runtimeRoutingTransitioning
	witness.ResolvedGeneration = 0
	witness.ResolvedAppleActive = false
	if err := store.writeRuntimeRouting(controller.codexConfigPath, witness); err != nil {
		t.Fatal(err)
	}
	transaction, err := controller.newJournal(State{
		Schema: SchemaVersion, Generation: final - 1,
		DesiredMode: ModeRelay, AppliedMode: ModeRelay,
		DesiredBackend: BackendLocalAppleContainer, AppliedBackend: BackendExternal,
		Phase: PhaseApplying, BoundConfigPath: state.BoundConfigPath, BoundCodexConfigPath: state.BoundCodexConfigPath,
	}, BackendLocalAppleContainer, false)
	if err != nil {
		t.Fatal(err)
	}
	transaction.Stage = transactionConfigMutated
	if err := controller.writeJournal(transaction); err != nil {
		t.Fatal(err)
	}
	reconciled, err := controller.ReconcileRuntimeRouting(context.Background(), request, true)
	if err != nil || reconciled != final {
		t.Fatalf("final-save reconciliation=%d err=%v", reconciled, err)
	}
	if _, found, err := controller.loadJournal(); err != nil || found {
		t.Fatalf("final routing journal remains: found=%t err=%v", found, err)
	}

	restore := request
	restore.Direction = RuntimeRoutingRestoreOrigin
	restored, err := controller.ReconcileRuntimeRouting(context.Background(), restore, true)
	if err != nil {
		t.Fatal(err)
	}
	state, err = store.Load()
	if err != nil || restored != final+3 || !stableStateForBackend(state, BackendExternal) {
		t.Fatalf("restored state=%#v generation=%d err=%v", state, restored, err)
	}
	if len(*applied) != 2 || (*applied)[1] != BackendExternal {
		t.Fatalf("restore runtime applies=%#v", *applied)
	}
	if err := controller.AcknowledgeRuntimeRouting(context.Background(), restore, restored); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeRoutingRejectsUnrelatedOperationAndUnsafeWitness(t *testing.T) {
	controller, store, request, applied := runtimeRoutingFixture(t, nil)
	if err := controller.beginRuntimeRouting(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	stateBefore, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	foreign := request
	foreign.Intent.OperationID = strings.Repeat("9", 64)
	if _, err := controller.ReconcileRuntimeRouting(context.Background(), foreign, true); !errors.Is(err, ErrRuntimeRoutingConflict) {
		t.Fatalf("foreign operation error=%v", err)
	}
	stateAfter, err := store.Load()
	if err != nil || stateAfter != stateBefore || len(*applied) != 0 {
		t.Fatalf("foreign operation mutated state: before=%#v after=%#v applies=%#v err=%v", stateBefore, stateAfter, *applied, err)
	}
	if _, err := controller.RequestBackend(context.Background(), BackendNone); !errors.Is(err, ErrRecoveryRequired) {
		t.Fatalf("ordinary routing bypassed lifecycle witness: %v", err)
	}

	path := store.RuntimeRoutingPath()
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RuntimeRoutingPending(context.Background()); err == nil {
		t.Fatal("broad-mode runtime witness was accepted")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(payload, &document); err != nil {
		t.Fatal(err)
	}
	document["unknown"] = true
	payload, _ = json.Marshal(document)
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := controller.RuntimeRoutingPending(context.Background()); err == nil {
		t.Fatal("unknown runtime witness field was accepted")
	}
}

func TestRuntimeRoutingRejectsGenerationOverflowBeforeWitness(t *testing.T) {
	controller, store, request, _ := runtimeRoutingFixture(t, nil)
	request.ExpectedOriginRoutingGeneration = ^uint64(0) - 2
	request.Intent.OperationID = strings.Repeat("8", 64)
	if _, err := controller.SwitchRuntimeRouting(context.Background(), request, true); !errors.Is(err, ErrRuntimeRoutingWitness) {
		t.Fatalf("overflow error=%v", err)
	}
	if _, err := os.Lstat(store.RuntimeRoutingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("overflow created witness: %v", err)
	}
}

func runtimeRoutingFixture(t *testing.T, runtimeApply runtimeControlFunc) (*Controller, *Store, RuntimeRoutingRequest, *[]Backend) {
	t.Helper()
	directory := t.TempDir()
	relayPath := filepath.Join(directory, "relay.json")
	codexPath := filepath.Join(directory, "config.toml")
	cfg, err := config.NewDefault("https://gateway.example.test/v1", config.CredentialsSourceNone)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Catalog.Path = filepath.Join(directory, "external-catalog.json")
	cfg.LocalAppleContainer, err = config.NewLocalAppleContainerProfileForCodexConfig(codexPath)
	if err != nil {
		t.Fatal(err)
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
	state, err := NewRelayState(relayPath)
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
		t.Fatal(err)
	}
	_ = lock.Close()

	apiToken := base64.RawURLEncoding.EncodeToString(make([]byte, 32))
	applied := &[]Backend{}
	if runtimeApply == nil {
		runtimeApply = runtimeControlFunc(func(_ context.Context, _ uint64, backend Backend) error {
			*applied = append(*applied, backend)
			return nil
		})
	}
	controller, err := NewController(
		relayPath, codexPath,
		WithTransitionTiming(time.Second, time.Millisecond),
		withLocalProfileAllowed(true),
		withCredentialLoader(func(config.CredentialsConfig) (credentials.Values, error) {
			return credentials.Values{LocalOpenCodexAPIKey: apiToken}, nil
		}),
		withLocalTargetPreflight(func(context.Context, localopencodex.Target) localopencodex.Result {
			return localopencodex.Result{Availability: localopencodex.AvailabilityReady, ModelCount: 1}
		}),
		withLocalCatalogMaterializer(func(context.Context, config.Config) error { return nil }),
		WithRuntimeControl(runtimeApply),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller.health = stateHealth{store: store, active: 0}
	request := RuntimeRoutingRequest{
		Intent: RuntimeRoutingIntent{
			OperationID: strings.Repeat("1", 64), InstallationID: strings.Repeat("2", 64),
			OldManifestSHA256: "absent", NewManifestSHA256: strings.Repeat("3", 64),
			OldImageDigest: "absent", NewImageDigest: "sha256:" + strings.Repeat("4", 64),
			NewStateGeneration: 1,
		},
		ExpectedOriginRoutingGeneration: state.Generation,
		TargetAppleActive:               true,
		Direction:                       RuntimeRoutingCompleteTarget,
	}
	return controller, store, request, applied
}
