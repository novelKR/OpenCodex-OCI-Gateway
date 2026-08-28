package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const (
	testRemovalID          = "0123456789abcdef01234567"
	testRemovalFingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testDataItemID         = "ocx-data-v1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func TestDiscoveryFingerprintBindsExactNodeAndNPMTools(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home := t.TempDir()
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	options := DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	}
	before, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(before.Candidates) != 1 {
		t.Fatalf("initial discovery = %#v, %v", before, err)
	}
	writeExecutable(t, filepath.Join(prefix, "bin", "node"))
	if err := os.WriteFile(filepath.Join(prefix, "bin", "node"), []byte("#!/bin/sh\n# changed\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	after, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(after.Candidates) != 1 {
		t.Fatalf("changed discovery = %#v, %v", after, err)
	}
	if before.Candidates[0].Fingerprint == after.Candidates[0].Fingerprint || before.Candidates[0].ID == after.Candidates[0].ID {
		t.Fatalf("node drift did not change removal proof: before=%#v after=%#v", before.Candidates[0], after.Candidates[0])
	}
}

func TestRemovalResolverRejectsDriftAndManualCandidates(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home := t.TempDir()
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	launcher := filepath.Join(prefix, "bin", "ocx")
	resolvedLauncher, launcherFingerprint, err := VerifyExecutable(launcher)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(home, "relay.json")
	if err := WriteRecord(configPath, Record{
		Schema: schemaVersion, Executable: resolvedLauncher, Fingerprint: launcherFingerprint, Action: RetainProxyKeepShim,
	}); err != nil {
		t.Fatal(err)
	}
	options := DiscoveryOptions{
		Tier: DiscoveryTierA, RelayConfigPath: configPath, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	}
	discovery, err := DiscoverNPMInstallations(context.Background(), options)
	if err != nil || len(discovery.Candidates) != 1 {
		t.Fatalf("discovery = %#v, %v", discovery, err)
	}
	resolver := DiscoveryRemovalResolver{Options: options}
	candidate := discovery.Candidates[0]
	resolved, err := resolver.Resolve(context.Background(), NPMRemovalSelection{ID: candidate.ID, Fingerprint: candidate.Fingerprint})
	if err != nil || !sameRemovalCriticalInstallation(candidate, resolved) {
		t.Fatalf("resolved candidate = %#v, %v", resolved, err)
	}
	if err := os.WriteFile(candidate.NodeExecutable, []byte("#!/bin/sh\n# replacement\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Revalidate(context.Background(), candidate); !errors.Is(err, ErrRemovalCandidateChanged) {
		t.Fatalf("node replacement revalidation error = %v", err)
	}

	manual := candidate
	manual.RemovalCapability = RemovalCapabilityManual
	if err := validateAutomaticRemovalCandidate(manual, os.Geteuid()); !errors.Is(err, ErrRemovalManualOnly) {
		t.Fatalf("manual candidate validation error = %v", err)
	}
}

type recordedRemovalProcess struct {
	calls []removalProcessCall
}

type removalProcessCall struct {
	program string
	args    []string
	env     []string
	maximum int64
}

func (r *recordedRemovalProcess) Run(_ context.Context, program string, args, environment []string, maximum int64) (RemovalExecutionResult, error) {
	r.calls = append(r.calls, removalProcessCall{
		program: program,
		args:    append([]string(nil), args...),
		env:     append([]string(nil), environment...),
		maximum: maximum,
	})
	return RemovalExecutionResult{ExitCode: 0, Started: true, CleanupVerified: true}, nil
}

func TestExactNPMRunnerUsesOnlyFixedProgramsArgumentsAndEnvironment(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home := t.TempDir()
	resolvedHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(resolvedHome, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	discovery, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: resolvedHome, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil || len(discovery.Candidates) != 1 {
		t.Fatalf("discovery = %#v, %v", discovery, err)
	}
	candidate := discovery.Candidates[0]
	process := &recordedRemovalProcess{}
	runner := ExactNPMRunner{HomeDir: resolvedHome, process: process}
	if _, err := runner.Inventory(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Preflight(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Teardown(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Trash(context.Background(), candidate, []string{testDataItemID}); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Uninstall(context.Background(), candidate); err != nil {
		t.Fatal(err)
	}
	if len(process.calls) != 5 {
		t.Fatalf("process calls = %#v", process.calls)
	}
	for index, call := range process.calls {
		if !filepath.IsAbs(call.program) || call.program == candidate.BunExecutable || call.program == candidate.NodeExecutable {
			t.Fatalf("call %d did not use a private executable snapshot: %#v", index, call)
		}
		switch index {
		case 0, 3:
			wantOperation := []string{"data", "inventory", "--json"}
			if index == 3 {
				wantOperation = []string{"data", "trash", "--item", testDataItemID, "--json"}
			}
			if !strings.HasSuffix(filepath.ToSlash(call.program), "/package/node_modules/bun/bin/bun.exe") || len(call.args) < 6 ||
				call.args[0] != "--config" || !strings.HasSuffix(filepath.ToSlash(call.args[1]), "/bunfig.toml") ||
				!reflect.DeepEqual(call.args[2:6], []string{"--no-install", "--no-orphans", "--no-env-file", filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(call.program)))), "src", "cli", "index.ts")}) ||
				!reflect.DeepEqual(call.args[6:], wantOperation) {
				t.Fatalf("OCX call %d = %#v", index, call)
			}
		case 1, 2:
			wantMode := "--preflight"
			if index == 2 {
				wantMode = "--execute"
			}
			if !strings.HasSuffix(filepath.ToSlash(call.program), "/package/node_modules/bun/bin/bun.exe") || len(call.args) != 9 ||
				call.args[0] != "--config" || !strings.HasSuffix(filepath.ToSlash(call.args[1]), "/bunfig.toml") ||
				!reflect.DeepEqual(call.args[2:5], []string{"--no-install", "--no-orphans", "--no-env-file"}) ||
				!strings.HasSuffix(filepath.ToSlash(call.args[5]), "/relay-preserve-v1.ts") || call.args[6] != wantMode ||
				call.args[7] != "--adapter-id" || call.args[8] != candidate.TeardownAdapterID {
				t.Fatalf("adapter call %d = %#v", index, call)
			}
		case 4:
			wantTail := []string{"uninstall", "--global", "--prefix", candidate.Prefix, "--ignore-scripts", "--no-audit", "--no-fund", "--offline", OpenCodexPackageName}
			if !strings.HasSuffix(filepath.ToSlash(call.program), "/node") || len(call.args) != len(wantTail)+1 ||
				!strings.HasSuffix(filepath.ToSlash(call.args[0]), "/npm/bin/npm-cli.js") || !reflect.DeepEqual(call.args[1:], wantTail) {
				t.Fatalf("npm call = %#v", call)
			}
		}
		if _, err := os.Lstat(call.program); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private snapshot survived call %d: %v", index, err)
		}
		environment := strings.Join(call.env, "\n")
		for _, forbidden := range []string{"OPENCODEX_HOME", "NODE_OPTIONS", "bearer", "sudo", "sh -c"} {
			if strings.Contains(environment, forbidden) {
				t.Fatalf("call %d environment contains %q: %s", index, forbidden, environment)
			}
		}
		if !strings.Contains(environment, "BUN_FEATURE_FLAG_NO_ORPHANS=1") {
			t.Fatalf("call %d environment did not enable Bun descendant cleanup: %s", index, environment)
		}
	}
}

type fakeRemovalResolver struct {
	candidate       NPMInstallation
	resolveErr      error
	revalidateErrs  []error
	verifyErr       error
	resolveCalls    int
	revalidateCalls int
	verifyCalls     int
}

func (r *fakeRemovalResolver) Resolve(context.Context, NPMRemovalSelection) (NPMInstallation, error) {
	r.resolveCalls++
	return r.candidate, r.resolveErr
}

func (r *fakeRemovalResolver) Revalidate(context.Context, NPMInstallation) error {
	index := r.revalidateCalls
	r.revalidateCalls++
	if index < len(r.revalidateErrs) {
		return r.revalidateErrs[index]
	}
	return nil
}

func (r *fakeRemovalResolver) VerifyRemoved(NPMInstallation) error {
	r.verifyCalls++
	return r.verifyErr
}

type guardedFakeRemovalResolver struct {
	*fakeRemovalResolver
	validateErr   error
	validateCalls int
}

func (r *guardedFakeRemovalResolver) ValidateForMutation(context.Context, NPMInstallation) error {
	r.validateCalls++
	return r.validateErr
}

type fakeOpenCodexRemovalRunner struct {
	inventory      RemovalExecutionResult
	preflight      RemovalExecutionResult
	teardown       RemovalExecutionResult
	trash          RemovalExecutionResult
	uninstall      RemovalExecutionResult
	inventoryErr   error
	preflightErr   error
	teardownErr    error
	trashErr       error
	uninstallErr   error
	calls          []string
	preflightCalls int
}

func (r *fakeOpenCodexRemovalRunner) Preflight(context.Context, NPMInstallation) (RemovalExecutionResult, error) {
	r.preflightCalls++
	if r.preflight.Output == nil && r.preflightErr == nil {
		return teardownPreflightResult("ready"), nil
	}
	return r.preflight, r.preflightErr
}

func (r *fakeOpenCodexRemovalRunner) Inventory(context.Context, NPMInstallation) (RemovalExecutionResult, error) {
	r.calls = append(r.calls, "inventory")
	return r.inventory, r.inventoryErr
}

func (r *fakeOpenCodexRemovalRunner) Teardown(context.Context, NPMInstallation) (RemovalExecutionResult, error) {
	r.calls = append(r.calls, "teardown")
	return r.teardown, r.teardownErr
}

func (r *fakeOpenCodexRemovalRunner) Trash(_ context.Context, _ NPMInstallation, _ []string) (RemovalExecutionResult, error) {
	r.calls = append(r.calls, "trash")
	return r.trash, r.trashErr
}

func (r *fakeOpenCodexRemovalRunner) Uninstall(context.Context, NPMInstallation) (RemovalExecutionResult, error) {
	r.calls = append(r.calls, "uninstall")
	return r.uninstall, r.uninstallErr
}

func testRemovalCandidate() NPMInstallation {
	return NPMInstallation{
		ID: testRemovalID, Fingerprint: testRemovalFingerprint,
		TeardownCapability:    TeardownCapabilityRelayPreserveV1,
		DataCapability:        DataCapabilitySelectiveTrashV1,
		TeardownCompatibility: teardownCompatibilityCompatible,
		TeardownAdapterID:     "test_preserve_v1",
	}
}

func testRemovalRequest(mode OpenCodexRemovalMode) OpenCodexRemovalRequest {
	request := OpenCodexRemovalRequest{
		Selection:        NPMRemovalSelection{ID: testRemovalID, Fingerprint: testRemovalFingerprint},
		Mode:             mode,
		ConfirmedRemoval: true,
	}
	if mode == RemovalModeTrashSelected {
		request.DataItemIDs = []string{testDataItemID}
		request.ConfirmedTrash = true
	}
	return request
}

func teardownResult(status string) RemovalExecutionResult {
	exit := map[string]int{"completed": 0, "partial": 2, "failed": 1}[status]
	componentStatus := map[string]string{"completed": "completed", "partial": "refused", "failed": "failed"}[status]
	components := []map[string]any{{"component": "quiescence", "status": componentStatus}}
	if status == "completed" {
		for _, component := range []string{
			"service", "native_codex", "grok", "client_opencode", "client_pi", "client_omp", "client_hermes", "client_openclaw",
			"client_kimi", "client_gajae", "client_dsh", "client_mcode", "system_environment", "shell_hook", "codex_shim",
		} {
			components = append(components, map[string]any{"component": component, "status": "completed"})
		}
	}
	payload := map[string]any{
		"schema_version":      1,
		"operation":           "relay_preserving_teardown",
		"adapter_id":          "test_preserve_v1",
		"status":              status,
		"data_preserved":      true,
		"config_root_removed": false,
		"components":          components,
	}
	encoded, _ := json.Marshal(payload)
	return RemovalExecutionResult{Output: encoded, ExitCode: exit, Started: true, CleanupVerified: true}
}

func teardownPreflightResult(status string) RemovalExecutionResult {
	exit := 0
	components := []map[string]any{
		{"component": "config", "status": "unchanged"},
		{"component": "service", "status": "unchanged"},
		{"component": "codex_shim", "status": "unchanged"},
	}
	if status != "ready" {
		exit = 1
		components = []map[string]any{{"component": "config", "status": "refused", "code": "config_unsupported"}}
	}
	payload := map[string]any{
		"schema_version":      1,
		"operation":           "relay_preserving_teardown_preflight",
		"adapter_id":          "test_preserve_v1",
		"status":              status,
		"data_preserved":      true,
		"config_root_removed": false,
		"components":          components,
	}
	encoded, _ := json.Marshal(payload)
	return RemovalExecutionResult{Output: encoded, ExitCode: exit, Started: true, CleanupVerified: true}
}

func trashResult(status string, moved []string) RemovalExecutionResult {
	exit := map[string]int{"completed": 0, "partial": 2, "failed": 1, "unsupported": 1}[status]
	failures := []map[string]any{}
	if status != "completed" {
		failures = append(failures, map[string]any{"itemId": testDataItemID, "code": "native_trash_failed", "message": "bounded"})
	}
	payload := map[string]any{
		"schemaVersion":           1,
		"operation":               "data-trash",
		"status":                  status,
		"selected":                []string{testDataItemID},
		"moved":                   moved,
		"failures":                failures,
		"permanentDeleteFallback": false,
	}
	encoded, _ := json.Marshal(payload)
	return RemovalExecutionResult{Output: encoded, ExitCode: exit, Started: true, CleanupVerified: true}
}

func noopPrepareOperation(context.Context, NPMInstallation, OpenCodexRemovalRequest) error {
	return nil
}

func noopPreparePackageRemoval(context.Context, NPMInstallation, OpenCodexRemovalRequest, int) error {
	return nil
}

func noopRecordDataOutcome(context.Context, NPMInstallation, OpenCodexRemovalRequest, int, string) error {
	return nil
}

func noopPreparePackageExecution(context.Context) error {
	return nil
}

func noopCompletePackageExecution(context.Context, RemovalExecutionResult) error {
	return nil
}

func noopBeginExecution(context.Context, RemovalExecutionKind) error {
	return nil
}

func noopFinishExecution(context.Context, RemovalExecutionKind, RemovalExecutionResult) error {
	return nil
}

func TestRemovalCoordinatorChecksLiveGuardBeforeCleanupJournal(t *testing.T) {
	base := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	resolver := &guardedFakeRemovalResolver{fakeRemovalResolver: base, validateErr: ErrRemovalCandidateChanged}
	runner := &fakeOpenCodexRemovalRunner{}
	prepareCalls := 0
	coordinator := RemovalCoordinator{
		Resolver:      resolver,
		Runner:        runner,
		VerifyRouting: func(context.Context) error { return nil },
		PrepareOperation: func(context.Context, NPMInstallation, OpenCodexRemovalRequest) error {
			prepareCalls++
			return nil
		},
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if resolver.validateCalls != 1 || prepareCalls != 0 || len(runner.calls) != 0 ||
		!hasRemovalStage(receipt.Stages, "candidate_revalidation", "candidate_changed") {
		t.Fatalf("guard was not checked before mutation: receipt=%#v resolver=%#v prepare=%d calls=%#v", receipt, resolver, prepareCalls, runner.calls)
	}
}

func TestRemovalCoordinatorPreservesDataAndUninstallsLast(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{ExitCode: 0, Started: true, CleanupVerified: true},
	}
	verifyCalls := 0
	coordinator := RemovalCoordinator{
		Resolver:                 resolver,
		Runner:                   runner,
		VerifyRouting:            func(context.Context) error { verifyCalls++; return nil },
		PrepareOperation:         noopPrepareOperation,
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if receipt.Status != RemovalStatusCompleted || !receipt.PackageRemoved || receipt.DataScope != "preserved" ||
		receipt.MovedDataItems != 0 || receipt.PermanentDeleteFallback || receipt.RoutingRecoveryRequired ||
		hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") {
		t.Fatalf("receipt = %#v", receipt)
	}
	if !reflect.DeepEqual(runner.calls, []string{"teardown", "uninstall"}) || verifyCalls != 5 || resolver.revalidateCalls != 3 || resolver.verifyCalls != 1 {
		t.Fatalf("calls runner=%#v verify=%d resolver=%#v", runner.calls, verifyCalls, resolver)
	}
}

func TestRemovalCoordinatorStopsAfterBoundedPartialTeardown(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("partial"),
		uninstall: RemovalExecutionResult{ExitCode: 0, Started: true, CleanupVerified: true},
	}
	verifyCalls := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			return nil
		},
		PrepareOperation:         noopPrepareOperation,
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if receipt.Status != RemovalStatusPartial || receipt.PackageRemoved || verifyCalls != 2 || !reflect.DeepEqual(runner.calls, []string{"teardown"}) {
		t.Fatalf("receipt=%#v verify=%d calls=%#v", receipt, verifyCalls, runner.calls)
	}
}

func TestRemovalCoordinatorTrashIsExplicitAndStopsBeforeNPMOnPartial(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		trash:     trashResult("partial", nil),
		uninstall: RemovalExecutionResult{ExitCode: 0, Started: true, CleanupVerified: true},
	}
	coordinator := RemovalCoordinator{
		Resolver: resolver, Runner: runner,
		VerifyRouting:            func(context.Context) error { return nil },
		PrepareOperation:         noopPrepareOperation,
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModeTrashSelected))
	if receipt.Status != RemovalStatusPartial || receipt.PackageRemoved || receipt.SelectedDataItems != 1 || receipt.PermanentDeleteFallback {
		t.Fatalf("receipt = %#v", receipt)
	}
	if !reflect.DeepEqual(runner.calls, []string{"teardown", "trash"}) {
		t.Fatalf("runner calls = %#v", runner.calls)
	}
}

func TestRemovalCoordinatorParksRoutingOnUncertainTeardown(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{teardownErr: ErrRemovalOutputInvalid}
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver:                 resolver,
		Runner:                   runner,
		VerifyRouting:            func(context.Context) error { return nil },
		MarkRoutingRecovery:      func() error { parked++; return nil },
		PrepareOperation:         noopPrepareOperation,
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if receipt.Status != RemovalStatusFailed || !receipt.RoutingRecoveryRequired || parked != 1 || !reflect.DeepEqual(runner.calls, []string{"teardown"}) {
		t.Fatalf("receipt=%#v parked=%d calls=%#v", receipt, parked, runner.calls)
	}
}

func TestRemovalCoordinatorRejectsImplicitAllAndSecondConfirmationMissing(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{}
	coordinator := RemovalCoordinator{Resolver: resolver, Runner: runner, VerifyRouting: func(context.Context) error { return nil }}
	request := testRemovalRequest(RemovalModeTrashSelected)
	request.DataItemIDs = nil
	receipt := coordinator.Remove(context.Background(), request)
	if receipt.Status != RemovalStatusFailed || len(runner.calls) != 0 || resolver.resolveCalls != 0 || receipt.Stages[0].Code != "confirmation_required" {
		t.Fatalf("implicit-all receipt=%#v resolver=%#v runner=%#v", receipt, resolver, runner)
	}
}

func TestRemovalParsersRejectUnknownFieldsAndExitMismatch(t *testing.T) {
	completed := teardownResult("completed")
	completed.ExitCode = 2
	if _, err := parseRelayTeardownReceipt(completed, "relay_preserving_teardown", "test_preserve_v1"); !errors.Is(err, ErrTeardownResultInvalid) {
		t.Fatalf("exit mismatch error = %v", err)
	}
	completed = teardownResult("completed")
	completed.Output = append(completed.Output[:len(completed.Output)-1], []byte(`,"secret_path":"/home/private"}`)...)
	if _, err := parseRelayTeardownReceipt(completed, "relay_preserving_teardown", "test_preserve_v1"); !errors.Is(err, ErrTeardownResultInvalid) {
		t.Fatalf("unknown field error = %v", err)
	}
}

func TestInventoryProjectionDropsCanonicalRootAndPaths(t *testing.T) {
	const secretRoot = "/home/example/private/.opencodex"
	payload := map[string]any{
		"schemaVersion": 1,
		"operation":     "data-inventory",
		"status":        "verified",
		"root":          secretRoot,
		"items": []map[string]any{{
			"id":            testDataItemID,
			"category":      "configuration",
			"scope":         "owned",
			"kind":          "file",
			"exists":        true,
			"sensitive":     true,
			"canonicalPath": secretRoot + "/config.json",
			"relativePath":  "config.json",
			"trashable":     true,
		}},
	}
	encoded, _ := json.Marshal(payload)
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{inventory: RemovalExecutionResult{Output: encoded, ExitCode: 0}}
	coordinator := RemovalCoordinator{Resolver: resolver, Runner: runner, VerifyRouting: func(context.Context) error { return nil }}
	receipt, err := coordinator.Inventory(context.Background(), NPMRemovalSelection{ID: testRemovalID, Fingerprint: testRemovalFingerprint}, 1)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), secretRoot) || !strings.Contains(string(out), "config.json") {
		t.Fatalf("projected inventory = %s", out)
	}
}

func TestBoundedRemovalProcessDropsStderrAndRejectsOversizedStdout(t *testing.T) {
	if os.Getenv("OPENCODEX_REMOVAL_HELPER") == "stderr" {
		fmt.Fprint(os.Stdout, `{}`)
		fmt.Fprint(os.Stderr, "/home/private bearer-secret")
		os.Exit(2)
	}
	if os.Getenv("OPENCODEX_REMOVAL_HELPER") == "overflow" {
		fmt.Fprint(os.Stdout, strings.Repeat("x", 8192))
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := boundedRemovalProcess{}
	result, err := runner.Run(context.Background(), executable, []string{"-test.run=TestBoundedRemovalProcessDropsStderrAndRejectsOversizedStdout"}, append(os.Environ(), "OPENCODEX_REMOVAL_HELPER=stderr"), 1024)
	if err != nil || result.ExitCode != 2 || !result.Started || !result.CleanupVerified || string(result.Output) != `{}` || strings.Contains(string(result.Output), "bearer-secret") {
		t.Fatalf("stderr child result=%#v err=%v", result, err)
	}
	overflowResult, overflowErr := runner.Run(context.Background(), executable, []string{"-test.run=TestBoundedRemovalProcessDropsStderrAndRejectsOversizedStdout"}, append(os.Environ(), "OPENCODEX_REMOVAL_HELPER=overflow"), 1024)
	if !errors.Is(overflowErr, ErrRemovalOutputInvalid) || !overflowResult.Started || !overflowResult.CleanupVerified {
		t.Fatalf("overflow result=%#v error=%v", overflowResult, overflowErr)
	}
}
