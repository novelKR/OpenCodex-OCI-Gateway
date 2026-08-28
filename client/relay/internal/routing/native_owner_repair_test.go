package routing

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/codexconfig"
)

const ownedOpenCodexTOML = "# Auto-injected by opencodex\nopenai_base_url = \"http://127.0.0.1:10100/v1\"\nmodel_catalog_json = \"/private/opaque/opencodex-catalog.json\"\nmodel = \"gpt-5.6-sol\"\n"

func openCodexNativeRepairFixture(t *testing.T) (*Controller, *Store, string, State) {
	t.Helper()
	controller, store, codexPath := localDevelopmentNativeVerifyFixture(t)
	replaceVerifyNativeState(t, store, func(state *State) {
		state.Phase = PhaseRecoveryRequired
		state.DesiredMode = ModeRelay
		state.AppliedMode = ModeNative
		state.DesiredBackend = BackendExternal
		state.AppliedBackend = BackendNone
	})
	if err := os.WriteFile(codexPath, []byte(ownedOpenCodexTOML), 0o600); err != nil {
		t.Fatal(err)
	}
	state, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	return controller, store, codexPath, state
}

func ownerReady(integration NativeOwnerIntegration) NativeRepairOwnerStatus {
	return NativeRepairOwnerStatus{Configuration: NativeOwnerConfigurationValid, Integration: integration, Reason: NativeOwnerReasonReady}
}

func TestInspectNativeRepairOwnerBindsProbeToGenerationAndWitness(t *testing.T) {
	controller, _, _, state := openCodexNativeRepairFixture(t)
	result, err := controller.InspectNativeRepairOwner(context.Background(), state.Generation, codexconfig.NativeRepairOpenCodex, func(context.Context) (NativeRepairOwnerStatus, error) {
		return ownerReady(NativeOwnerIntegrationEnabled), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 1 || result.Generation != state.Generation || result.Owner != codexconfig.NativeRepairOpenCodex ||
		result.Configuration != NativeOwnerConfigurationValid || result.Integration != NativeOwnerIntegrationEnabled || result.Reason != NativeOwnerReasonReady {
		t.Fatalf("inspection=%#v", result)
	}
}

func TestRepairNativeRoutingRetriesOnlyBoundedNoMutationAndCommitsAfterOwnerDisabled(t *testing.T) {
	controller, store, codexPath, before := openCodexNativeRepairFixture(t)
	oldWait := waitForNativeOwnerRetry
	var delays []time.Duration
	waitForNativeOwnerRetry = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	defer func() { waitForNativeOwnerRetry = oldWait }()

	integration := NativeOwnerIntegrationEnabled
	attempts := 0
	receipt, err := controller.RepairNativeRouting(
		context.Background(), before.Generation, codexconfig.NativeRepairOpenCodex, true, true,
		func(context.Context) (NativeRepairOwnerStatus, error) { return ownerReady(integration), nil },
		func(context.Context) (NativeRepairOwnerRestoreResult, error) {
			attempts++
			if attempts < 4 {
				return NativeRepairOwnerRestoreResult{Outcome: NativeRepairOwnerRetryableNoMutation}, nil
			}
			if err := os.WriteFile(codexPath, []byte("model = \"gpt-5.6-sol\"\n"), 0o600); err != nil {
				return NativeRepairOwnerRestoreResult{}, err
			}
			integration = NativeOwnerIntegrationDisabled
			return NativeRepairOwnerRestoreResult{Outcome: NativeRepairOwnerApplied}, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 4 || !reflect.DeepEqual(delays, nativeOwnerRetryDelays) {
		t.Fatalf("attempts=%d delays=%v", attempts, delays)
	}
	if receipt.SchemaVersion != 2 || receipt.OwnerRestoreAttempts != 4 || receipt.OwnerRestoreResult != NativeRepairOwnerApplied || !receipt.BackupCreated {
		t.Fatalf("receipt=%#v", receipt)
	}
	after, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation != before.Generation+1 || after.Phase != PhaseNativeActive || after.DesiredBackend != BackendNone || after.AppliedBackend != BackendNone {
		t.Fatalf("after=%#v", after)
	}
	backups, err := filepath.Glob(codexPath + ".pre-opencodex-relay-native-repair-*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, err)
	}
}

func TestRepairNativeRoutingExhaustsBusyWithoutChangingState(t *testing.T) {
	controller, store, codexPath, before := openCodexNativeRepairFixture(t)
	oldWait := waitForNativeOwnerRetry
	waitForNativeOwnerRetry = func(context.Context, time.Duration) error { return nil }
	defer func() { waitForNativeOwnerRetry = oldWait }()
	calls := 0
	_, err := controller.RepairNativeRouting(
		context.Background(), before.Generation, codexconfig.NativeRepairOpenCodex, true, true,
		func(context.Context) (NativeRepairOwnerStatus, error) {
			return ownerReady(NativeOwnerIntegrationEnabled), nil
		},
		func(context.Context) (NativeRepairOwnerRestoreResult, error) {
			calls++
			return NativeRepairOwnerRestoreResult{Outcome: NativeRepairOwnerRetryableNoMutation}, nil
		},
	)
	if !errors.Is(err, ErrNativeOwnerBusy) || calls != 4 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	after, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if after.Generation != before.Generation || after.Phase != before.Phase {
		t.Fatalf("state changed: before=%#v after=%#v", before, after)
	}
	backups, globErr := filepath.Glob(codexPath + ".pre-opencodex-relay-native-repair-*")
	if globErr != nil || len(backups) != 1 {
		t.Fatalf("backups=%v err=%v", backups, globErr)
	}
}

func TestRepairNativeRoutingPreflightRejectsInvalidOrUnavailableBeforeBackupAndRestore(t *testing.T) {
	for _, test := range []struct {
		name   string
		status NativeRepairOwnerStatus
		want   error
	}{
		{name: "invalid", status: NativeRepairOwnerStatus{Configuration: NativeOwnerConfigurationInvalid, Integration: NativeOwnerIntegrationUnknown, Reason: NativeOwnerReasonConfiguration}, want: ErrNativeOwnerConfigurationInvalid},
		{name: "unavailable", status: NativeRepairOwnerStatus{Configuration: NativeOwnerConfigurationUnavailable, Integration: NativeOwnerIntegrationUnknown, Reason: NativeOwnerReasonUnavailable}, want: ErrNativeOwnerResultInvalid},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, store, codexPath, before := openCodexNativeRepairFixture(t)
			restoreCalls := 0
			_, err := controller.RepairNativeRouting(
				context.Background(), before.Generation, codexconfig.NativeRepairOpenCodex, true, true,
				func(context.Context) (NativeRepairOwnerStatus, error) { return test.status, nil },
				func(context.Context) (NativeRepairOwnerRestoreResult, error) {
					restoreCalls++
					return NativeRepairOwnerRestoreResult{}, nil
				},
			)
			if !errors.Is(err, test.want) || restoreCalls != 0 {
				t.Fatalf("err=%v calls=%d", err, restoreCalls)
			}
			after, loadErr := store.Load()
			if loadErr != nil || after.Generation != before.Generation {
				t.Fatalf("after=%#v loadErr=%v", after, loadErr)
			}
			backups, globErr := filepath.Glob(codexPath + ".pre-opencodex-relay-native-repair-*")
			if globErr != nil || len(backups) != 0 {
				t.Fatalf("backups=%v err=%v", backups, globErr)
			}
		})
	}
}

func TestRepairNativeRoutingDetectsWitnessChangeBetweenRetries(t *testing.T) {
	controller, store, codexPath, before := openCodexNativeRepairFixture(t)
	oldWait := waitForNativeOwnerRetry
	waitForNativeOwnerRetry = func(context.Context, time.Duration) error { return nil }
	defer func() { waitForNativeOwnerRetry = oldWait }()
	_, err := controller.RepairNativeRouting(
		context.Background(), before.Generation, codexconfig.NativeRepairOpenCodex, true, true,
		func(context.Context) (NativeRepairOwnerStatus, error) {
			return ownerReady(NativeOwnerIntegrationEnabled), nil
		},
		func(context.Context) (NativeRepairOwnerRestoreResult, error) {
			if writeErr := os.WriteFile(codexPath, []byte(ownedOpenCodexTOML+"# user edit\n"), 0o600); writeErr != nil {
				return NativeRepairOwnerRestoreResult{}, writeErr
			}
			return NativeRepairOwnerRestoreResult{Outcome: NativeRepairOwnerRetryableNoMutation}, nil
		},
	)
	if !errors.Is(err, ErrNativeRepairArtifactsChanged) {
		t.Fatalf("err=%v", err)
	}
	after, loadErr := store.Load()
	if loadErr != nil || after.Generation != before.Generation {
		t.Fatalf("after=%#v loadErr=%v", after, loadErr)
	}
}

func TestRepairNativeRoutingRequiresNativeTOMLAndDisabledIntegrationBeforeCommit(t *testing.T) {
	for _, test := range []struct {
		name        string
		writeNative bool
		disabled    bool
		want        error
	}{
		{name: "routing residue", disabled: true, want: ErrNativeVerification},
		{name: "integration enabled", writeNative: true, want: ErrNativeOwnerRestoreFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			controller, store, codexPath, before := openCodexNativeRepairFixture(t)
			integration := NativeOwnerIntegrationEnabled
			_, err := controller.RepairNativeRouting(
				context.Background(), before.Generation, codexconfig.NativeRepairOpenCodex, true, true,
				func(context.Context) (NativeRepairOwnerStatus, error) { return ownerReady(integration), nil },
				func(context.Context) (NativeRepairOwnerRestoreResult, error) {
					if test.writeNative {
						if writeErr := os.WriteFile(codexPath, []byte("model = \"gpt-5.6-sol\"\n"), 0o600); writeErr != nil {
							return NativeRepairOwnerRestoreResult{}, writeErr
						}
					}
					if test.disabled {
						integration = NativeOwnerIntegrationDisabled
					}
					return NativeRepairOwnerRestoreResult{Outcome: NativeRepairOwnerApplied}, nil
				},
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
			after, loadErr := store.Load()
			if loadErr != nil || after.Generation != before.Generation {
				t.Fatalf("after=%#v loadErr=%v", after, loadErr)
			}
		})
	}
}

func TestNativeOwnerProbePreservesBoundedCandidateError(t *testing.T) {
	controller, _, _, state := openCodexNativeRepairFixture(t)
	candidateChanged := errors.New("bounded candidate changed")
	_, err := controller.InspectNativeRepairOwner(
		context.Background(), state.Generation, codexconfig.NativeRepairOpenCodex,
		func(context.Context) (NativeRepairOwnerStatus, error) {
			return NativeRepairOwnerStatus{}, candidateChanged
		},
	)
	if !errors.Is(err, candidateChanged) {
		t.Fatalf("InspectNativeRepairOwner() error=%v, want bounded candidate error", err)
	}
}
