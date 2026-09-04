package handoff

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestRemovalExecutionClosureRejectsTransitiveTreeDrift(t *testing.T) {
	tests := []struct {
		name         string
		transitive   func(prefix, packageRoot string) string
		initialBytes string
		changedBytes string
	}{
		{
			name: "package dependency",
			transitive: func(_ string, packageRoot string) string {
				return filepath.Join(packageRoot, "node_modules", "dependency", "index.js")
			},
			initialBytes: "export const value = 1\n",
			changedBytes: "export const value = 2\n",
		},
		{
			name: "npm dependency",
			transitive: func(prefix, _ string) string {
				return filepath.Join(prefix, "lib", "node_modules", "npm", "lib", "worker.js")
			},
			initialBytes: "module.exports = 1\n",
			changedBytes: "module.exports = 2\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			resolvedHome, err := filepath.EvalSymlinks(home)
			if err != nil {
				t.Fatal(err)
			}
			prefix := filepath.Join(resolvedHome, "node")
			packageRoot := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
			transitive := test.transitive(prefix, packageRoot)
			if err := os.MkdirAll(filepath.Dir(transitive), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(transitive, []byte(test.initialBytes), 0o600); err != nil {
				t.Fatal(err)
			}
			discovery, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
				Tier: DiscoveryTierA, HomeDir: resolvedHome, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
				SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
			})
			if err != nil || len(discovery.Candidates) != 1 {
				t.Fatalf("discovery = %#v, %v", discovery, err)
			}
			candidate := discovery.Candidates[0]
			if err := verifyRemovalExecutionClosure(candidate); err != nil {
				t.Fatalf("initial execution closure = %v", err)
			}
			if err := os.WriteFile(transitive, []byte(test.changedBytes), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := verifyRemovalExecutionClosure(candidate); !errors.Is(err, ErrRemovalCandidateChanged) {
				t.Fatalf("transitive drift error = %v", err)
			}
		})
	}
}

func TestRemovalCleanupJournalRoundTripIdempotenceAndTamperRefusal(t *testing.T) {
	configDirectory := t.TempDir()
	configPath := filepath.Join(configDirectory, "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModeTrashSelected)
	intent, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := NewRemovalDataOutcomeRecord(candidate, request, 1, "completed")
	if err != nil {
		t.Fatal(err)
	}
	record, err := NewRemovalCleanupRecord(candidate, request, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := PreflightRemovalCleanup(configPath); err != nil {
		t.Fatalf("initial preflight = %v", err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if read, exists, err := ReadRemovalCleanup(configPath); err != nil || !exists || read.Phase != RemovalCleanupPhaseIntent {
		t.Fatalf("intent journal read = %#v, exists=%t, err=%v", read, exists, err)
	}
	if err := VerifyRemovalCleanupAbsent(intent); !errors.Is(err, ErrRemovalOutcomeUnknown) {
		t.Fatalf("intent journal authorized package cleanup: %v", err)
	}
	if err := WriteRemovalCleanup(configPath, outcome); err != nil {
		t.Fatalf("data outcome advancement = %v", err)
	}
	if err := WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatalf("package pending advancement = %v", err)
	}
	beforeRewrite, err := os.Stat(RemovalCleanupPath(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatalf("idempotent durable rewrite = %v", err)
	}
	afterRewrite, err := os.Stat(RemovalCleanupPath(configPath))
	if err != nil {
		t.Fatal(err)
	}
	if os.SameFile(beforeRewrite, afterRewrite) {
		t.Fatal("identical cleanup record bypassed durable temp-file replacement")
	}
	read, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || !sameRemovalCleanupRecord(read, record) {
		t.Fatalf("journal read = %#v, exists=%t, err=%v", read, exists, err)
	}
	if err := WriteRemovalCleanup(configPath, intent); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("journal phase rollback error = %v", err)
	}
	if err := PreflightRemovalCleanup(configPath); err != nil {
		t.Fatalf("existing journal preflight = %v", err)
	}
	if err := RemoveRemovalCleanup(configPath); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := ReadRemovalCleanup(configPath); err != nil || exists {
		t.Fatalf("removed journal exists=%t err=%v", exists, err)
	}
	if err := WriteRemovalCleanup(configPath, record); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(RemovalCleanupPath(configPath))
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	object["unexpected_path_authority"] = "/home/private"
	tampered, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(RemovalCleanupPath(configPath), append(tampered, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRemovalCleanup(configPath); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("tampered journal error = %v", err)
	}
	if err := PreflightRemovalCleanup(configPath); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("tampered preflight error = %v", err)
	}
}

func TestResumePackageRemovalUsesJournalBoundCandidate(t *testing.T) {
	candidate := removalCleanupCandidate(t)
	record, err := NewRemovalCleanupRecord(candidate, testRemovalRequest(RemovalModePreserveData), 0)
	if err != nil {
		t.Fatal(err)
	}
	record.PackageAttempt = 1
	record.ExecutionAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	record.ProcessReconciledAfterReboot = true
	resolver := &fakeRemovalResolver{candidate: candidate}
	runner := &fakeOpenCodexRemovalRunner{uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0}}
	coordinator := RemovalCoordinator{
		Resolver:                 resolver,
		Runner:                   runner,
		VerifyRouting:            func(context.Context) error { return nil },
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.ResumePackageRemoval(context.Background(), record)
	if receipt.Status != RemovalStatusCompleted || !receipt.PackageRemoved || receipt.RoutingRecoveryRequired ||
		hasRemovalStage(receipt.Stages, "routing_recovery", "routing_recovery_persisted") ||
		!reflectRemovalCalls(runner.calls, []string{"uninstall"}) || resolver.revalidateCalls != 1 || resolver.verifyCalls != 1 {
		t.Fatalf("resume receipt=%#v runner=%#v resolver=%#v", receipt, runner.calls, resolver)
	}

	changed := record
	changedPrefix := filepath.Join(t.TempDir(), "other-node")
	changed.PackageRoot = packageRootForPrefix(changedPrefix)
	changed.Launchers = []string{
		filepath.Join(changedPrefix, "bin", "ocx"),
		filepath.Join(changedPrefix, "bin", "opencodex"),
	}
	if receipt := coordinator.ResumePackageRemoval(context.Background(), changed); receipt.PackageRemoved || receipt.Stages[len(receipt.Stages)-1].Code != "candidate_changed" {
		t.Fatalf("changed journal resume = %#v", receipt)
	}
}

func TestRemovalCoordinatorVerifiesAbsenceAfterNPMNonzero(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 1},
	}
	prepared := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			return nil
		},
		PrepareOperation:  noopPrepareOperation,
		RecordDataOutcome: noopRecordDataOutcome,
		PreparePackageRemoval: func(context.Context, NPMInstallation, OpenCodexRemovalRequest, int) error {
			prepared++
			return nil
		},
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if receipt.Status != RemovalStatusPartial || !receipt.PackageRemoved || prepared != 1 || resolver.verifyCalls != 1 ||
		!hasRemovalStage(receipt.Stages, "npm_uninstall", "npm_uninstall_failed") ||
		!hasRemovalStage(receipt.Stages, "package_verification", "package_absent") {
		t.Fatalf("nonzero npm receipt=%#v prepared=%d resolver=%#v", receipt, prepared, resolver)
	}
}

func TestInventoryProjectionRejectsUnsafeRelativePaths(t *testing.T) {
	const root = "/home/example/.opencodex"
	for _, relative := range []string{"/tmp/secret", "../secret", "logs/../secret", "logs//secret", "logs\\secret", "logs/secret\nvalue"} {
		t.Run(strings.NewReplacer("/", "_", "\\", "_").Replace(relative), func(t *testing.T) {
			payload := map[string]any{
				"schemaVersion": 1,
				"operation":     "data-inventory",
				"status":        "verified",
				"root":          root,
				"items": []map[string]any{{
					"id": testDataItemID, "category": "configuration", "scope": "owned", "kind": "file",
					"exists": true, "sensitive": true, "canonicalPath": root + "/secret", "relativePath": relative, "trashable": true,
				}},
			}
			encoded, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := parseUpstreamInventory(RemovalExecutionResult{Output: encoded, ExitCode: 0, Started: true, CleanupVerified: true}); !errors.Is(err, ErrRemovalReceiptInvalid) {
				t.Fatalf("relative path %q error = %v", relative, err)
			}
		})
	}
}

func TestVerifyRemovedRejectsAnyBoundLauncherResidual(t *testing.T) {
	prefix := filepath.Join(t.TempDir(), "node")
	candidate := NPMInstallation{
		Prefix:      prefix,
		PackageRoot: packageRootForPrefix(prefix),
		Launchers:   []string{filepath.Join(prefix, "bin", "ocx")},
	}
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(candidate.Launchers[0], []byte("residual\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (DiscoveryRemovalResolver{}).VerifyRemoved(candidate); !errors.Is(err, ErrRemovalOutcomeUnknown) {
		t.Fatalf("launcher residual verification error = %v", err)
	}
	if err := os.Remove(candidate.Launchers[0]); err != nil {
		t.Fatal(err)
	}
	if err := (DiscoveryRemovalResolver{}).VerifyRemoved(candidate); err != nil {
		t.Fatalf("fully absent verification = %v", err)
	}
}

func TestBoundedRemovalProcessTerminatesDescendantHoldingStdout(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("process-group cleanup proof is a Darwin runtime contract")
	}
	const helperEnvironment = "OPENCODEX_REMOVAL_DESCENDANT_HELPER"
	mode := os.Getenv(helperEnvironment)
	marker := os.Getenv("OPENCODEX_REMOVAL_DESCENDANT_MARKER")
	if mode == "parent" {
		executable, err := os.Executable()
		if err != nil {
			os.Exit(91)
		}
		child := exec.Command(executable, "-test.run=TestBoundedRemovalProcessTerminatesDescendantHoldingStdout")
		child.Env = environmentWith(os.Environ(), helperEnvironment, "child")
		child.Stdout = os.Stdout
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			os.Exit(92)
		}
		os.Exit(0)
	}
	if mode == "child" {
		time.Sleep(1500 * time.Millisecond)
		if marker != "" {
			_ = os.WriteFile(marker, []byte("descendant survived\n"), 0o600)
		}
		os.Exit(0)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "survived")
	environment := environmentWith(os.Environ(), helperEnvironment, "parent")
	environment = environmentWith(environment, "OPENCODEX_REMOVAL_DESCENDANT_MARKER", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	result, err := (boundedRemovalProcess{}).Run(ctx, executable, []string{"-test.run=TestBoundedRemovalProcessTerminatesDescendantHoldingStdout"}, environment, 1024)
	elapsed := time.Since(started)
	if !errors.Is(err, ErrRemovalOutputInvalid) || !result.Started || !result.CleanupVerified || elapsed >= 2*time.Second {
		t.Fatalf("descendant process result=%#v err=%v elapsed=%s", result, err, elapsed)
	}
	time.Sleep(1800 * time.Millisecond)
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descendant was not terminated with its process group: %v", err)
	}
}

func TestBoundedRemovalProcessUsesPrivateCWDWithoutCallerDotEnv(t *testing.T) {
	const helperEnvironment = "OPENCODEX_REMOVAL_CWD_HELPER"
	if os.Getenv(helperEnvironment) == "inspect" {
		workingDirectory, err := os.Getwd()
		if err != nil || workingDirectory == os.Getenv("OPENCODEX_REMOVAL_HOSTILE_CWD") {
			os.Exit(93)
		}
		if _, err := os.Lstat(filepath.Join(workingDirectory, ".env")); !errors.Is(err, os.ErrNotExist) {
			os.Exit(94)
		}
		_, _ = os.Stdout.WriteString(`{"safe":true}`)
		os.Exit(0)
	}

	hostile := t.TempDir()
	if err := os.WriteFile(filepath.Join(hostile, ".env"), []byte("OPENCODEX_HOME=/home/private\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(hostile)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	environment := environmentWith(os.Environ(), helperEnvironment, "inspect")
	environment = environmentWith(environment, "OPENCODEX_REMOVAL_HOSTILE_CWD", hostile)
	result, err := (boundedRemovalProcess{}).Run(context.Background(), executable, []string{"-test.run=TestBoundedRemovalProcessUsesPrivateCWDWithoutCallerDotEnv"}, environment, 1024)
	if err != nil || !result.Started || !result.CleanupVerified || result.ExitCode != 0 || string(result.Output) != `{"safe":true}` {
		t.Fatalf("private cwd result=%#v err=%v", result, err)
	}
}

func removalCleanupCandidate(t *testing.T) NPMInstallation {
	t.Helper()
	prefix := filepath.Join(t.TempDir(), "node")
	return NPMInstallation{
		ID:                    testRemovalID,
		Fingerprint:           testRemovalFingerprint,
		TeardownCapability:    TeardownCapabilityRelayPreserveV1,
		DataCapability:        DataCapabilitySelectiveTrashV1,
		TeardownCompatibility: teardownCompatibilityCompatible,
		TeardownAdapterID:     "test_preserve_v1",
		Prefix:                prefix,
		PackageRoot:           packageRootForPrefix(prefix),
		Launchers: []string{
			filepath.Join(prefix, "bin", "ocx"),
			filepath.Join(prefix, "bin", "opencodex"),
		},
	}
}

func reflectRemovalCalls(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range got {
		if got[index] != want[index] {
			return false
		}
	}
	return true
}

func hasRemovalStage(stages []OpenCodexRemovalStage, stage, code string) bool {
	for _, item := range stages {
		if item.Stage == stage && item.Code == code {
			return true
		}
	}
	return false
}

func environmentWith(environment []string, name, value string) []string {
	prefix := name + "="
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		if !strings.HasPrefix(item, prefix) {
			result = append(result, item)
		}
	}
	return append(result, prefix+value)
}

func TestRemovalCoordinatorPersistsIntentBeforeFirstMutatingChild(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{teardown: teardownResult("partial")}
	intentPrepared := false
	packagePrepared := false
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			return nil
		},
		PrepareOperation: func(context.Context, NPMInstallation, OpenCodexRemovalRequest) error {
			if len(runner.calls) != 0 {
				t.Fatalf("mutating child ran before intent journal: %#v", runner.calls)
			}
			intentPrepared = true
			return nil
		},
		PreparePackageRemoval: func(context.Context, NPMInstallation, OpenCodexRemovalRequest, int) error {
			packagePrepared = true
			return nil
		},
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if !intentPrepared || packagePrepared || receipt.Status != RemovalStatusPartial ||
		!hasRemovalStage(receipt.Stages, "cleanup_journal", "cleanup_intent_persisted") ||
		!reflectRemovalCalls(runner.calls, []string{"teardown"}) {
		t.Fatalf("intent ordering receipt=%#v intent=%t package=%t calls=%#v", receipt, intentPrepared, packagePrepared, runner.calls)
	}
}

func TestRemovalCoordinatorParksWhenRoutingChangesAtFinalBoundary(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0},
	}
	verifyCalls := 0
	parked := 0
	packagePrepared := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			if verifyCalls == 3 {
				return errors.New("routing changed")
			}
			return nil
		},
		MarkRoutingRecovery: func() error { parked++; return nil },
		PrepareOperation:    noopPrepareOperation,
		PreparePackageRemoval: func(context.Context, NPMInstallation, OpenCodexRemovalRequest, int) error {
			packagePrepared++
			return nil
		},
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModePreserveData))
	if receipt.Status != RemovalStatusPartial || !receipt.RoutingRecoveryRequired || receipt.PackageRemoved || parked != 1 ||
		packagePrepared != 0 || !reflectRemovalCalls(runner.calls, []string{"teardown"}) {
		t.Fatalf("final routing boundary receipt=%#v verify=%d parked=%d prepared=%d calls=%#v", receipt, verifyCalls, parked, packagePrepared, runner.calls)
	}
}

func TestRemovalCoordinatorDetectsRoutingDriftAfterNPM(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0},
	}
	verifyCalls := 0
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			if verifyCalls == 4 {
				return errors.New("routing changed")
			}
			return nil
		},
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
	if receipt.Status != RemovalStatusPartial || !receipt.PackageRemoved || !receipt.RoutingRecoveryRequired || parked != 1 || resolver.verifyCalls != 1 ||
		!hasRemovalStage(receipt.Stages, "routing_post_verification", "routing_ownership_changed") {
		t.Fatalf("post-npm routing receipt=%#v verify=%d parked=%d resolver=%#v", receipt, verifyCalls, parked, resolver)
	}
}

func TestTeardownParserRejectsContradictoryAggregateEvidence(t *testing.T) {
	completed := teardownResult("completed")
	var payload map[string]any
	if err := json.Unmarshal(completed.Output, &payload); err != nil {
		t.Fatal(err)
	}
	components := payload["components"].([]any)
	components[0].(map[string]any)["status"] = "failed"
	completed.Output, _ = json.Marshal(payload)
	if _, err := parseRelayTeardownReceipt(completed, "relay_preserving_teardown", "test_preserve_v1"); !errors.Is(err, ErrTeardownVerificationFailed) {
		t.Fatalf("completed-with-failure error = %v", err)
	}

	empty := teardownResult("completed")
	if err := json.Unmarshal(empty.Output, &payload); err != nil {
		t.Fatal(err)
	}
	payload["components"] = []any{}
	empty.Output, _ = json.Marshal(payload)
	if _, err := parseRelayTeardownReceipt(empty, "relay_preserving_teardown", "test_preserve_v1"); !errors.Is(err, ErrTeardownResultInvalid) {
		t.Fatalf("completed-without-required-components error = %v", err)
	}

	partial := teardownResult("partial")
	if err := json.Unmarshal(partial.Output, &payload); err != nil {
		t.Fatal(err)
	}
	components = payload["components"].([]any)
	components[0].(map[string]any)["status"] = "completed"
	partial.Output, _ = json.Marshal(payload)
	if _, err := parseRelayTeardownReceipt(partial, "relay_preserving_teardown", "test_preserve_v1"); !errors.Is(err, ErrTeardownResultInvalid) {
		t.Fatalf("partial-without-problem error = %v", err)
	}
}

type snapshotMutationProcess struct {
	sourceCLI       string
	sourceProgram   string
	snapshotCLI     string
	snapshotProgram string
	observedConfig  string
	err             error
}

func (p *snapshotMutationProcess) Run(_ context.Context, program string, args, _ []string, _ int64) (RemovalExecutionResult, error) {
	if len(args) < 6 {
		p.err = errors.New("snapshot argv missing")
		return RemovalExecutionResult{}, p.err
	}
	if err := os.WriteFile(p.sourceCLI, []byte("console.log('changed after snapshot')\n"), 0o600); err != nil {
		p.err = err
		return RemovalExecutionResult{}, err
	}
	if err := os.WriteFile(p.sourceProgram, []byte("#!/bin/sh\n# changed after snapshot\nexit 9\n"), 0o700); err != nil {
		p.err = err
		return RemovalExecutionResult{}, err
	}
	p.snapshotProgram = program
	p.snapshotCLI = args[5]
	cliPayload, err := os.ReadFile(p.snapshotCLI)
	if err != nil || string(cliPayload) != "console.log('fixture')\n" {
		p.err = errors.New("snapshot CLI changed with source")
		return RemovalExecutionResult{}, p.err
	}
	programPayload, err := os.ReadFile(program)
	if err != nil || string(programPayload) != "#!/bin/sh\nexit 0\n" {
		p.err = errors.New("snapshot executable changed with source")
		return RemovalExecutionResult{}, p.err
	}
	configPayload, err := os.ReadFile(args[1])
	if err != nil {
		p.err = err
		return RemovalExecutionResult{}, err
	}
	p.observedConfig = string(configPayload)
	return RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0}, nil
}

func TestExactRunnerExecutesVerifiedPrivateSnapshot(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	discovery, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil || len(discovery.Candidates) != 1 {
		t.Fatalf("discovery = %#v, %v", discovery, err)
	}
	candidate := discovery.Candidates[0]
	process := &snapshotMutationProcess{sourceCLI: candidate.CLIEntry, sourceProgram: candidate.BunExecutable}
	runner := ExactNPMRunner{HomeDir: home, process: process}
	if _, err := runner.Inventory(context.Background(), candidate); err != nil {
		t.Fatalf("snapshot inventory = %v (inspection=%v)", err, process.err)
	}
	if process.snapshotProgram == candidate.BunExecutable || process.snapshotCLI == candidate.CLIEntry ||
		!strings.Contains(process.observedConfig, "env = false") || !strings.Contains(process.observedConfig, "auto = \"disable\"") {
		t.Fatalf("snapshot process = %#v", process)
	}
	for _, path := range []string{process.snapshotProgram, process.snapshotCLI} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("snapshot path survived execution: %s: %v", path, err)
		}
	}
}

func TestBoundedRemovalProcessTerminatesDescriptorClosingDescendant(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("process-group cleanup proof is a Darwin runtime contract")
	}
	const helperEnvironment = "OPENCODEX_REMOVAL_CLOSED_DESCENDANT_HELPER"
	mode := os.Getenv(helperEnvironment)
	marker := os.Getenv("OPENCODEX_REMOVAL_CLOSED_DESCENDANT_MARKER")
	if mode == "parent" {
		executable, err := os.Executable()
		if err != nil {
			os.Exit(95)
		}
		child := exec.Command(executable, "-test.run=TestBoundedRemovalProcessTerminatesDescriptorClosingDescendant")
		child.Env = environmentWith(os.Environ(), helperEnvironment, "child")
		child.Stdout = io.Discard
		child.Stderr = io.Discard
		if err := child.Start(); err != nil {
			os.Exit(96)
		}
		os.Exit(0)
	}
	if mode == "child" {
		time.Sleep(1500 * time.Millisecond)
		if marker != "" {
			_ = os.WriteFile(marker, []byte("descriptor-closing descendant survived\n"), 0o600)
		}
		os.Exit(0)
	}

	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "survived")
	environment := environmentWith(os.Environ(), helperEnvironment, "parent")
	environment = environmentWith(environment, "OPENCODEX_REMOVAL_CLOSED_DESCENDANT_MARKER", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	result, err := (boundedRemovalProcess{}).Run(ctx, executable, []string{"-test.run=TestBoundedRemovalProcessTerminatesDescriptorClosingDescendant"}, environment, 1024)
	elapsed := time.Since(started)
	if err != nil || !result.Started || !result.CleanupVerified || result.ExitCode != 0 || elapsed >= 2*time.Second {
		t.Fatalf("descriptor-closing descendant result=%#v err=%v elapsed=%s", result, err, elapsed)
	}
	time.Sleep(1800 * time.Millisecond)
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("descriptor-closing descendant was not terminated: %v", err)
	}
}

func TestBoundedRemovalProcessDoesNotLaunchPreCancelledContext(t *testing.T) {
	const helperEnvironment = "OPENCODEX_REMOVAL_PRE_CANCEL_HELPER"
	marker := os.Getenv("OPENCODEX_REMOVAL_PRE_CANCEL_MARKER")
	if os.Getenv(helperEnvironment) == "execute" {
		if marker != "" {
			_ = os.WriteFile(marker, []byte("launched\n"), 0o600)
		}
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	marker = filepath.Join(t.TempDir(), "launched")
	environment := environmentWith(os.Environ(), helperEnvironment, "execute")
	environment = environmentWith(environment, "OPENCODEX_REMOVAL_PRE_CANCEL_MARKER", marker)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := (boundedRemovalProcess{}).Run(ctx, executable, []string{"-test.run=TestBoundedRemovalProcessDoesNotLaunchPreCancelledContext"}, environment, 1024)
	if !errors.Is(err, context.Canceled) || result.Started {
		t.Fatalf("pre-cancelled result=%#v err=%v", result, err)
	}
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pre-cancelled child launched: %v", err)
	}
}

func TestRemovalCoordinatorRequiresCleanupProofBeforePackageFinalization(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{Started: true, ExitCode: 0},
	}
	verifyCalls := 0
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			return nil
		},
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
	if receipt.PackageRemoved || !receipt.RoutingRecoveryRequired || parked != 1 || resolver.verifyCalls != 0 || verifyCalls != 4 ||
		!hasRemovalStage(receipt.Stages, "npm_uninstall", "process_cleanup_unverified") {
		t.Fatalf("missing cleanup proof receipt=%#v verify=%d parked=%d resolver=%#v", receipt, verifyCalls, parked, resolver)
	}
}

func TestRemovalCoordinatorRequiresCleanupProofBeforeTeardownReceipt(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	teardown := teardownResult("completed")
	teardown.CleanupVerified = false
	runner := &fakeOpenCodexRemovalRunner{teardown: teardown}
	verifyCalls := 0
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			return nil
		},
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
	if receipt.PackageRemoved || !receipt.RoutingRecoveryRequired || parked != 1 || verifyCalls != 2 ||
		!hasRemovalStage(receipt.Stages, "teardown", "process_cleanup_unverified") || !reflectRemovalCalls(runner.calls, []string{"teardown"}) {
		t.Fatalf("teardown cleanup proof receipt=%#v verify=%d parked=%d calls=%#v", receipt, verifyCalls, parked, runner.calls)
	}
}

func TestRemovalCoordinatorChecksRoutingAfterFailedTrashChild(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown: teardownResult("completed"),
		trash:    RemovalExecutionResult{Started: true, CleanupVerified: true},
		trashErr: ErrRemovalOutputInvalid,
	}
	verifyCalls := 0
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			if verifyCalls == 4 {
				return errors.New("routing changed after trash")
			}
			return nil
		},
		MarkRoutingRecovery:      func() error { parked++; return nil },
		PrepareOperation:         noopPrepareOperation,
		RecordDataOutcome:        noopRecordDataOutcome,
		PreparePackageRemoval:    noopPreparePackageRemoval,
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModeTrashSelected))
	if receipt.Status != RemovalStatusPartial || !receipt.RoutingRecoveryRequired || parked != 1 || verifyCalls != 4 ||
		!hasRemovalStage(receipt.Stages, "data_trash", "child_output_invalid") ||
		!hasRemovalStage(receipt.Stages, "routing_post_trash", "routing_ownership_changed") ||
		!reflectRemovalCalls(runner.calls, []string{"teardown", "trash"}) {
		t.Fatalf("failed trash routing receipt=%#v verify=%d parked=%d calls=%#v", receipt, verifyCalls, parked, runner.calls)
	}
}

func TestRemovalCoordinatorVerifiesRoutingImmediatelyBeforeTeardown(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{teardown: teardownResult("completed")}
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver:                 resolver,
		Runner:                   runner,
		VerifyRouting:            func(context.Context) error { return errors.New("routing changed before teardown") },
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
	if len(runner.calls) != 0 || !receipt.RoutingRecoveryRequired || parked != 1 ||
		!hasRemovalStage(receipt.Stages, "routing_pre_teardown", "routing_ownership_unverified") {
		t.Fatalf("pre-teardown routing receipt=%#v parked=%d calls=%#v", receipt, parked, runner.calls)
	}
}

func TestTeardownParserEnforcesCompletedCriticalStatusMatrix(t *testing.T) {
	unchangedAllowed := map[string]bool{
		"service": true, "grok": true, "client_opencode": true, "client_pi": true, "client_omp": true,
		"client_hermes": true, "client_openclaw": true, "client_kimi": true, "client_gajae": true,
		"client_dsh": true, "client_mcode": true, "system_environment": true, "shell_hook": true, "codex_shim": true,
	}
	for _, component := range []string{
		"quiescence", "service", "native_codex", "grok", "client_opencode", "client_pi", "client_omp", "client_hermes", "client_openclaw",
		"client_kimi", "client_gajae", "client_dsh", "client_mcode", "system_environment", "shell_hook", "codex_shim",
	} {
		t.Run(component+"_unchanged", func(t *testing.T) {
			result := teardownResultWithComponentStatus(t, component, "unchanged")
			_, err := parseRelayTeardownReceipt(result, "relay_preserving_teardown", "test_preserve_v1")
			if unchangedAllowed[component] && err != nil {
				t.Fatalf("allowed unchanged component %q: %v", component, err)
			}
			if !unchangedAllowed[component] && !errors.Is(err, ErrTeardownVerificationFailed) {
				t.Fatalf("critical unchanged component %q error = %v", component, err)
			}
		})
		t.Run(component+"_skipped", func(t *testing.T) {
			result := teardownResultWithComponentStatus(t, component, "skipped")
			if _, err := parseRelayTeardownReceipt(result, "relay_preserving_teardown", "test_preserve_v1"); !errors.Is(err, ErrTeardownVerificationFailed) {
				t.Fatalf("required skipped component %q error = %v", component, err)
			}
		})
	}

	unknown := teardownResult("completed")
	var payload map[string]any
	if err := json.Unmarshal(unknown.Output, &payload); err != nil {
		t.Fatal(err)
	}
	payload["components"] = append(payload["components"].([]any), map[string]any{
		"component": "client_future", "status": "completed",
	})
	unknown.Output, _ = json.Marshal(payload)
	if _, err := parseRelayTeardownReceipt(unknown, "relay_preserving_teardown", "test_preserve_v1"); !errors.Is(err, ErrTeardownVerificationFailed) {
		t.Fatalf("unknown completed component error = %v", err)
	}
}

func teardownResultWithComponentStatus(t *testing.T, component, status string) RemovalExecutionResult {
	t.Helper()
	result := teardownResult("completed")
	var payload map[string]any
	if err := json.Unmarshal(result.Output, &payload); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, raw := range payload["components"].([]any) {
		item := raw.(map[string]any)
		if item["component"] == component {
			item["status"] = status
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("component %q not found", component)
	}
	result.Output, _ = json.Marshal(payload)
	return result
}

func TestBoundedRemovalProcessCancellationStillProvesCleanup(t *testing.T) {
	const helperEnvironment = "OPENCODEX_REMOVAL_CANCELLED_HELPER"
	if os.Getenv(helperEnvironment) == "sleep" {
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, runErr := (boundedRemovalProcess{afterStart: cancel}).Run(
		ctx,
		executable,
		[]string{"-test.run=TestBoundedRemovalProcessCancellationStillProvesCleanup"},
		environmentWith(os.Environ(), helperEnvironment, "sleep"),
		1024,
	)
	if !errors.Is(runErr, context.Canceled) || !result.Started || !result.CleanupVerified {
		t.Fatalf("cancelled process result=%#v err=%v", result, runErr)
	}
}

type cancelAfterFirstWrite struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelAfterFirstWrite) Write(payload []byte) (int, error) {
	w.writes++
	if w.writes == 1 {
		w.cancel()
	}
	return len(payload), nil
}

func TestCopyWithContextChecksCancellationBetweenChunks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	writer := &cancelAfterFirstWrite{cancel: cancel}
	source := strings.NewReader(strings.Repeat("x", 256<<10))
	copied, err := copyWithContext(ctx, writer, source, 256<<10)
	if !errors.Is(err, context.Canceled) || writer.writes != 1 || copied <= 0 || copied >= 256<<10 {
		t.Fatalf("context copy copied=%d writes=%d err=%v", copied, writer.writes, err)
	}
}

func TestRemovalJournalInFlightNeverAuthorizesResumeOrAbsence(t *testing.T) {
	useAttestedRemovalBootSession(t)
	for _, packagePresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[packagePresent], func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			candidate := removalCleanupCandidate(t)
			request := testRemovalRequest(RemovalModePreserveData)
			intent, err := NewRemovalIntentRecord(candidate, request)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := NewRemovalCleanupRecord(candidate, request, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteRemovalCleanup(configPath, intent); err != nil {
				t.Fatal(err)
			}
			if err := WriteRemovalCleanup(configPath, pending); err != nil {
				t.Fatal(err)
			}
			inFlight, err := BeginRemovalPackageExecution(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if packagePresent {
				if err := os.MkdirAll(candidate.PackageRoot, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := VerifyRemovalCleanupAbsent(inFlight); !errors.Is(err, ErrRemovalOutcomeUnknown) {
				t.Fatalf("in-flight absence authorization error = %v", err)
			}
			receipt, err := InterruptedRemovalProcessReceipt(inFlight, true)
			if err != nil || receipt.PackageRemoved || !receipt.RoutingRecoveryRequired ||
				!hasRemovalStage(receipt.Stages, "npm_uninstall", "process_cleanup_unverified") {
				t.Fatalf("in-flight receipt=%#v err=%v", receipt, err)
			}
			coordinator := RemovalCoordinator{
				Resolver:                 &fakeRemovalResolver{candidate: candidate},
				Runner:                   &fakeOpenCodexRemovalRunner{},
				VerifyRouting:            func(context.Context) error { return nil },
				PreparePackageExecution:  noopPreparePackageExecution,
				CompletePackageExecution: noopCompletePackageExecution,
				BeginExecution:           noopBeginExecution,
				FinishExecution:          noopFinishExecution,
			}
			resumed := coordinator.ResumePackageRemoval(context.Background(), inFlight)
			if resumed.PackageRemoved || !hasRemovalStage(resumed.Stages, "cleanup_journal", "cleanup_journal_invalid") {
				t.Fatalf("in-flight journal resumed: %#v", resumed)
			}
		})
	}
}

func TestRemovalJournalRecordsCleanupProofAndRetryAttempts(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModePreserveData)
	intent, _ := NewRemovalIntentRecord(candidate, request)
	pending, _ := NewRemovalCleanupRecord(candidate, request, 0)
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, pending); err != nil {
		t.Fatal(err)
	}
	first, err := BeginRemovalPackageExecution(configPath)
	if err != nil || first.Phase != RemovalCleanupPhasePackageInFlight || first.PackageAttempt != 1 {
		t.Fatalf("first launch = %#v, %v", first, err)
	}
	reset, err := FinishRemovalPackageExecution(configPath, RemovalExecutionResult{})
	if err != nil || reset.Phase != RemovalCleanupPhasePackagePending || reset.PackageAttempt != 1 {
		t.Fatalf("not-started reset = %#v, %v", reset, err)
	}
	second, err := BeginRemovalPackageExecution(configPath)
	if err != nil || second.PackageAttempt != 2 {
		t.Fatalf("second launch = %#v, %v", second, err)
	}
	verified, err := FinishRemovalPackageExecution(configPath, RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 1})
	if err != nil || verified.Phase != RemovalCleanupPhasePackageVerified || verified.PackageAttempt != 2 {
		t.Fatalf("verified cleanup = %#v, %v", verified, err)
	}
	if err := VerifyRemovalCleanupAbsent(verified); err != nil {
		t.Fatalf("verified absent package = %v", err)
	}
	receipt, err := RemovedPackageCleanupReceipt(verified)
	if err != nil || !receipt.PackageRemoved {
		t.Fatalf("verified cleanup receipt=%#v err=%v", receipt, err)
	}
}

func TestRemovalJournalPersistsPartialTrashProgress(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModeTrashSelected)
	request.DataItemIDs = []string{
		testDataItemID,
		"ocx-data-v1:cccccccccccccccccccccccccccccccc",
	}
	intent, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	outcome, err := NewRemovalDataOutcomeRecord(candidate, request, 1, "partial")
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, outcome); err != nil {
		t.Fatal(err)
	}
	read, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || read.Phase != RemovalCleanupPhaseDataOutcome || read.MovedDataItems != 1 || read.DataOutcome != "partial" {
		t.Fatalf("partial data outcome=%#v exists=%t err=%v", read, exists, err)
	}
	refresh, err := MarkRemovalDataRefreshRequired(configPath)
	if err != nil || refresh.Phase != RemovalCleanupPhaseDataRefresh || refresh.MovedDataItems != 1 || refresh.DataOutcome != "partial" {
		t.Fatalf("partial refresh state=%#v err=%v", refresh, err)
	}
	receipt, err := RecordedRemovalDataRefreshReceipt(refresh)
	if err != nil || receipt.MovedDataItems != 1 || receipt.DataMovementUnknown {
		t.Fatalf("partial data receipt=%#v err=%v", receipt, err)
	}
	if _, err := AdvanceRemovalDataOutcomeToPackagePending(configPath); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("partial data outcome advanced to package removal: %v", err)
	}
}

func TestRemovalCoordinatorDurablyRecordsCompletedTrashBeforePackageLaunch(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	resolver := &fakeRemovalResolver{candidate: candidate}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		trash:     trashResult("completed", []string{testDataItemID}),
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0},
	}
	coordinator := RemovalCoordinator{
		Resolver:      resolver,
		Runner:        runner,
		VerifyRouting: func(context.Context) error { return nil },
		PrepareOperation: func(_ context.Context, candidate NPMInstallation, request OpenCodexRemovalRequest) error {
			record, err := NewRemovalIntentRecord(candidate, request)
			if err != nil {
				return err
			}
			return WriteRemovalCleanup(configPath, record)
		},
		RecordDataOutcome: func(_ context.Context, candidate NPMInstallation, request OpenCodexRemovalRequest, moved int, status string) error {
			_, err := RecordRemovalDataOutcome(configPath, candidate, request, moved, status)
			return err
		},
		PreparePackageRemoval: func(_ context.Context, candidate NPMInstallation, request OpenCodexRemovalRequest, moved int) error {
			_, err := PrepareRemovalPackageCleanup(configPath, candidate, request, moved)
			return err
		},
		PreparePackageExecution: func(context.Context) error {
			_, err := BeginRemovalPackageExecution(configPath)
			return err
		},
		CompletePackageExecution: func(_ context.Context, result RemovalExecutionResult) error {
			_, err := FinishRemovalPackageExecution(configPath, result)
			return err
		},
		BeginExecution: func(_ context.Context, kind RemovalExecutionKind) error {
			_, err := BeginExecution(configPath, kind)
			return err
		},
		FinishExecution: func(_ context.Context, kind RemovalExecutionKind, result RemovalExecutionResult) error {
			_, err := FinishExecution(configPath, kind, result)
			return err
		},
	}
	receipt := coordinator.Remove(context.Background(), testRemovalRequest(RemovalModeTrashSelected))
	if receipt.Status != RemovalStatusCompleted || !receipt.PackageRemoved || receipt.MovedDataItems != 1 {
		t.Fatalf("completed trash receipt=%#v", receipt)
	}
	record, exists, err := ReadRemovalCleanup(configPath)
	if err != nil || !exists || record.Phase != RemovalCleanupPhasePackageVerified || record.DataOutcome != "completed" || record.MovedDataItems != 1 {
		t.Fatalf("completed trash journal=%#v exists=%t err=%v", record, exists, err)
	}
}

func TestExactRunnerRejectsPackageRootReplacementAtPrestartBoundary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	discovery, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil || len(discovery.Candidates) != 1 {
		t.Fatalf("discovery=%#v err=%v", discovery, err)
	}
	candidate := discovery.Candidates[0]
	process := &recordedRemovalProcess{}
	runner := ExactNPMRunner{
		HomeDir: home,
		BeforeUninstallCandidate: func(context.Context, NPMInstallation) error {
			if err := os.Rename(candidate.PackageRoot, candidate.PackageRoot+".replaced"); err != nil {
				return err
			}
			return os.Mkdir(candidate.PackageRoot, 0o700)
		},
		process: process,
	}
	result, err := runner.Uninstall(context.Background(), candidate)
	if !errors.Is(err, ErrRemovalCandidateChanged) || result.Started || len(process.calls) != 0 {
		t.Fatalf("replacement result=%#v err=%v calls=%#v", result, err, process.calls)
	}
}

func TestExactRunnerRoutingCallbackFailureIsDefinitelyPreStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("automatic removal deliberately refuses elevated execution")
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	discovery, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"), GOOS: "darwin", GOARCH: "arm64",
		SkipDefaultPrefixes: true, Getenv: func(string) string { return "" },
	})
	if err != nil || len(discovery.Candidates) != 1 {
		t.Fatalf("discovery=%#v err=%v", discovery, err)
	}
	candidate := discovery.Candidates[0]
	for _, testCase := range []struct {
		name string
		run  func(ExactNPMRunner) (RemovalExecutionResult, error)
	}{
		{
			name: "teardown",
			run: func(runner ExactNPMRunner) (RemovalExecutionResult, error) {
				return runner.Teardown(context.Background(), candidate)
			},
		},
		{
			name: "package",
			run: func(runner ExactNPMRunner) (RemovalExecutionResult, error) {
				return runner.Uninstall(context.Background(), candidate)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			process := &recordedRemovalProcess{}
			runner := ExactNPMRunner{
				HomeDir: home,
				BeforeOCXMutation: func(context.Context) error {
					return errors.New("routing changed")
				},
				BeforeUninstall: func(context.Context) error {
					return errors.New("routing changed")
				},
				process: process,
			}
			result, err := testCase.run(runner)
			if !errors.Is(err, ErrRemovalRoutingChanged) || result.Started ||
				result.CleanupVerified || len(process.calls) != 0 ||
				removalExecutionMayHaveMutated(result, err) {
				t.Fatalf("result=%#v err=%v calls=%#v", result, err, process.calls)
			}
		})
	}
}

func TestRemovalJournalRefreshRequiresExplicitFreshNonoverlappingSelection(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	oldRequest := testRemovalRequest(RemovalModeTrashSelected)
	intent, err := NewRemovalIntentRecord(candidate, oldRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	refresh, err := MarkRemovalDataRefreshRequired(configPath)
	if err != nil || refresh.Phase != RemovalCleanupPhaseDataRefresh || refresh.SelectionRevision != 1 ||
		!sameOrderedStrings(refresh.SelectedItemIDs, oldRequest.DataItemIDs) {
		t.Fatalf("refresh record=%#v err=%v", refresh, err)
	}
	unknownReceipt, err := InterruptedRemovalDataRefreshReceipt(refresh)
	if err != nil || !unknownReceipt.DataMovementUnknown || unknownReceipt.PackageRemoved {
		t.Fatalf("unknown refresh receipt=%#v err=%v", unknownReceipt, err)
	}
	oldInventory := OpenCodexDataInventoryReceipt{
		SchemaVersion:  OpenCodexInventorySchemaVersion,
		Operation:      "open-codex-data-inventory",
		Status:         "verified",
		InstallationID: candidate.ID,
		Items: []OpenCodexDataInventoryItem{{
			ID: oldRequest.DataItemIDs[0], Exists: true, Trashable: true,
		}},
	}
	bindRefreshInventory(&oldRequest, &oldInventory, 41)
	if _, err := SupersedeRemovalDataRefreshRequired(configPath, oldRequest, oldInventory, false); !errors.Is(err, ErrRemovalConfirmationNeeded) {
		t.Fatalf("unconfirmed refresh supersession error=%v", err)
	}
	if _, err := SupersedeRemovalDataRefreshRequired(configPath, oldRequest, oldInventory, true); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("overlapping refresh supersession error=%v", err)
	}
	if read, exists, err := ReadRemovalCleanup(configPath); err != nil || !exists || read.Phase != RemovalCleanupPhaseDataRefresh {
		t.Fatalf("failed supersession discarded refresh evidence: %#v exists=%t err=%v", read, exists, err)
	}

	newRequest := testRemovalRequest(RemovalModeTrashSelected)
	newRequest.DataItemIDs = []string{"ocx-data-v1:cccccccccccccccccccccccccccccccc"}
	newInventory := OpenCodexDataInventoryReceipt{
		SchemaVersion:  OpenCodexInventorySchemaVersion,
		Operation:      "open-codex-data-inventory",
		Status:         "verified",
		InstallationID: candidate.ID,
		Items: []OpenCodexDataInventoryItem{{
			ID: newRequest.DataItemIDs[0], Exists: true, Trashable: true,
		}},
	}
	bindRefreshInventory(&newRequest, &newInventory, 41)
	for name, mutate := range map[string]func(*OpenCodexDataInventoryReceipt){
		"inventory revision": func(receipt *OpenCodexDataInventoryReceipt) { receipt.InventoryRevision = strings.Repeat("f", 64) },
		"routing generation": func(receipt *OpenCodexDataInventoryReceipt) { receipt.RoutingGeneration++ },
		"installation fingerprint": func(receipt *OpenCodexDataInventoryReceipt) {
			receipt.InstallationFingerprint = strings.Repeat("e", 64)
		},
	} {
		t.Run("rejects changed "+name, func(t *testing.T) {
			changed := newInventory
			mutate(&changed)
			if _, err := SupersedeRemovalDataRefreshRequired(configPath, newRequest, changed, true); !errors.Is(err, ErrRemovalCleanupUnsafe) {
				t.Fatalf("changed %s error=%v", name, err)
			}
		})
	}
	superseded, err := SupersedeRemovalDataRefreshRequired(configPath, newRequest, newInventory, true)
	if err != nil || superseded.Phase != RemovalCleanupPhaseIntent || superseded.SelectionRevision != 2 ||
		!sameOrderedStrings(superseded.SelectedItemIDs, newRequest.DataItemIDs) {
		t.Fatalf("superseded intent=%#v err=%v", superseded, err)
	}
	if ensured, err := EnsureRemovalIntent(configPath, candidate, newRequest); err != nil || ensured.SelectionRevision != 2 {
		t.Fatalf("superseded intent recommit=%#v err=%v", ensured, err)
	}
	if outcome, err := RecordRemovalDataOutcome(configPath, candidate, newRequest, 1, "completed"); err != nil || outcome.SelectionRevision != 2 {
		t.Fatalf("superseded data outcome=%#v err=%v", outcome, err)
	}
	if pending, err := PrepareRemovalPackageCleanup(configPath, candidate, newRequest, 1); err != nil ||
		pending.Phase != RemovalCleanupPhasePackagePending || pending.SelectionRevision != 2 {
		t.Fatalf("superseded package pending=%#v err=%v", pending, err)
	}
}

func TestRemovalJournalRefreshCanSwitchToPreserveData(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	trashRequest := testRemovalRequest(RemovalModeTrashSelected)
	intent, err := NewRemovalIntentRecord(candidate, trashRequest)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	refresh, err := MarkRemovalDataRefreshRequired(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := RemoveRemovalCleanup(configPath); err != nil {
		t.Fatal(err)
	}
	refresh.SelectionRevision = maxRemovalSelectionRevisions
	if err := WriteRemovalCleanup(configPath, refresh); err != nil {
		t.Fatal(err)
	}
	preserveRequest := testRemovalRequest(RemovalModePreserveData)
	inventory := OpenCodexDataInventoryReceipt{
		SchemaVersion:  OpenCodexInventorySchemaVersion,
		Operation:      "open-codex-data-inventory",
		Status:         "absent",
		InstallationID: candidate.ID,
	}
	bindRefreshInventory(&preserveRequest, &inventory, 53)
	changedFingerprint := inventory
	changedFingerprint.InstallationFingerprint = strings.Repeat("e", 64)
	if _, err := SupersedeRemovalDataRefreshRequired(configPath, preserveRequest, changedFingerprint, true); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("preserve refresh accepted changed installation fingerprint: %v", err)
	}
	changedGeneration := inventory
	changedGeneration.RoutingGeneration++
	if _, err := SupersedeRemovalDataRefreshRequired(configPath, preserveRequest, changedGeneration, true); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("preserve refresh accepted changed routing generation: %v", err)
	}
	superseded, err := SupersedeRemovalDataRefreshRequired(configPath, preserveRequest, inventory, true)
	if err != nil || superseded.Mode != RemovalModePreserveData || superseded.SelectedDataItems != 0 ||
		superseded.SelectionRevision != maxRemovalSelectionRevisions+1 || superseded.Phase != RemovalCleanupPhaseIntent {
		t.Fatalf("preserve supersession=%#v err=%v", superseded, err)
	}
}

func TestRemovalCoordinatorRequiresFinalRoutingProofAfterAbsence(t *testing.T) {
	resolver := &fakeRemovalResolver{candidate: testRemovalCandidate()}
	runner := &fakeOpenCodexRemovalRunner{
		teardown:  teardownResult("completed"),
		uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0},
	}
	verifyCalls := 0
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			if verifyCalls == 5 {
				return errors.New("routing changed after package absence verification")
			}
			return nil
		},
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
	if receipt.PackageRemoved || !receipt.RoutingRecoveryRequired || parked != 1 || verifyCalls != 5 || resolver.verifyCalls != 1 ||
		!hasRemovalStage(receipt.Stages, "routing_final_verification", "routing_ownership_changed") {
		t.Fatalf("final routing receipt=%#v verify=%d parked=%d resolver=%#v", receipt, verifyCalls, parked, resolver)
	}
}

func TestResumePackageRemovalRequiresFinalRoutingProofAfterAbsence(t *testing.T) {
	candidate := removalCleanupCandidate(t)
	record, err := NewRemovalCleanupRecord(candidate, testRemovalRequest(RemovalModePreserveData), 0)
	if err != nil {
		t.Fatal(err)
	}
	record.PackageAttempt = 1
	record.ExecutionAttempt = 1
	record.ExecutionBootSession = strings.Repeat("b", 64)
	record.ProcessReconciledAfterReboot = true
	resolver := &fakeRemovalResolver{candidate: candidate}
	runner := &fakeOpenCodexRemovalRunner{uninstall: RemovalExecutionResult{Started: true, CleanupVerified: true, ExitCode: 0}}
	verifyCalls := 0
	parked := 0
	coordinator := RemovalCoordinator{
		Resolver: resolver,
		Runner:   runner,
		VerifyRouting: func(context.Context) error {
			verifyCalls++
			if verifyCalls == 4 {
				return errors.New("routing changed after resumed package absence verification")
			}
			return nil
		},
		MarkRoutingRecovery:      func() error { parked++; return nil },
		PreparePackageExecution:  noopPreparePackageExecution,
		CompletePackageExecution: noopCompletePackageExecution,
		BeginExecution:           noopBeginExecution,
		FinishExecution:          noopFinishExecution,
	}
	receipt := coordinator.ResumePackageRemoval(context.Background(), record)
	if receipt.PackageRemoved || !receipt.RoutingRecoveryRequired || parked != 1 || verifyCalls != 4 || resolver.verifyCalls != 1 ||
		!hasRemovalStage(receipt.Stages, "routing_final_verification", "routing_ownership_changed") {
		t.Fatalf("resumed final routing receipt=%#v verify=%d parked=%d resolver=%#v", receipt, verifyCalls, parked, resolver)
	}
}

func TestRemovalJournalRetiresSelectionsAcrossAllRefreshRevisions(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	requestA := testRemovalRequest(RemovalModeTrashSelected)
	intent, err := NewRemovalIntentRecord(candidate, requestA)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := MarkRemovalDataRefreshRequired(configPath); err != nil {
		t.Fatal(err)
	}
	requestB := testRemovalRequest(RemovalModeTrashSelected)
	requestB.DataItemIDs = []string{"ocx-data-v1:cccccccccccccccccccccccccccccccc"}
	inventoryB := OpenCodexDataInventoryReceipt{
		SchemaVersion: OpenCodexInventorySchemaVersion, Operation: "open-codex-data-inventory", Status: "verified",
		InstallationID: candidate.ID,
		Items:          []OpenCodexDataInventoryItem{{ID: requestB.DataItemIDs[0], Exists: true, Trashable: true}},
	}
	bindRefreshInventory(&requestB, &inventoryB, 61)
	if _, err := SupersedeRemovalDataRefreshRequired(configPath, requestB, inventoryB, true); err != nil {
		t.Fatal(err)
	}
	refreshB, err := MarkRemovalDataRefreshRequired(configPath)
	if err != nil || refreshB.SelectionRevision != 2 || len(refreshB.RetiredItemIDs) != 2 {
		t.Fatalf("second refresh=%#v err=%v", refreshB, err)
	}
	inventoryA := OpenCodexDataInventoryReceipt{
		SchemaVersion: OpenCodexInventorySchemaVersion, Operation: "open-codex-data-inventory", Status: "verified",
		InstallationID: candidate.ID,
		Items:          []OpenCodexDataInventoryItem{{ID: requestA.DataItemIDs[0], Exists: true, Trashable: true}},
	}
	bindRefreshInventory(&requestA, &inventoryA, 61)
	if _, err := SupersedeRemovalDataRefreshRequired(configPath, requestA, inventoryA, true); !errors.Is(err, ErrRemovalCleanupUnsafe) {
		t.Fatalf("A-to-B-to-A replay error=%v", err)
	}
	if read, exists, err := ReadRemovalCleanup(configPath); err != nil || !exists ||
		read.Phase != RemovalCleanupPhaseDataRefresh || len(read.RetiredItemIDs) != 2 {
		t.Fatalf("retired selection evidence=%#v exists=%t err=%v", read, exists, err)
	}
}

func bindRefreshInventory(request *OpenCodexRemovalRequest, inventory *OpenCodexDataInventoryReceipt, generation uint64) {
	request.ExpectedRoutingGeneration = generation
	inventory.InstallationFingerprint = request.Selection.Fingerprint
	inventory.RoutingGeneration = generation
	inventory.InventoryRevision = inventoryRevision(*inventory)
	if request.Mode == RemovalModeTrashSelected {
		request.ExpectedInventoryRevision = inventory.InventoryRevision
	}
}

func TestRemovalRoutingGateRequiresVerifiedRecoveryRelease(t *testing.T) {
	useAttestedRemovalBootSession(t)
	configPath := filepath.Join(t.TempDir(), "relay.json")
	candidate := removalCleanupCandidate(t)
	request := testRemovalRequest(RemovalModePreserveData)
	intent, err := NewRemovalIntentRecord(candidate, request)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := NewRemovalCleanupRecord(candidate, request, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, intent); err != nil {
		t.Fatal(err)
	}
	if err := WriteRemovalCleanup(configPath, pending); err != nil {
		t.Fatal(err)
	}
	if err := RemovalRoutingGate(configPath); err != nil {
		t.Fatalf("pending package unexpectedly gated routing: %v", err)
	}
	inFlight, err := BeginRemovalPackageExecution(configPath)
	if err != nil || inFlight.RoutingRecoveryReleased {
		t.Fatalf("in-flight record=%#v err=%v", inFlight, err)
	}
	if err := RemovalRoutingGate(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
		t.Fatalf("in-flight routing gate error=%v", err)
	}
	verified, err := FinishRemovalPackageExecution(configPath, RemovalExecutionResult{Started: true, CleanupVerified: true})
	if err != nil || verified.Phase != RemovalCleanupPhasePackageVerified {
		t.Fatalf("verified record=%#v err=%v", verified, err)
	}
	if err := RemovalRoutingGate(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
		t.Fatalf("verified routing gate error=%v", err)
	}
	released, err := ReleaseRemovalRoutingGateForRecovery(configPath)
	if err != nil || !released.RoutingRecoveryReleased || RemovalRoutingGate(configPath) != nil {
		t.Fatalf("released routing gate record=%#v gate=%v err=%v", released, RemovalRoutingGate(configPath), err)
	}
	relaunched, err := BeginRemovalPackageExecution(configPath)
	if err != nil || relaunched.RoutingRecoveryReleased {
		t.Fatalf("relaunched record=%#v err=%v", relaunched, err)
	}
	if err := RemovalRoutingGate(configPath); !errors.Is(err, ErrRemovalRoutingGate) {
		t.Fatalf("relaunched routing gate error=%v", err)
	}
}

func useAttestedRemovalBootSession(t *testing.T) string {
	t.Helper()
	previous := removalBootSessionProvider
	boot := strings.Repeat("a", 64)
	removalBootSessionProvider = func() (string, bool, error) {
		return boot, true, nil
	}
	t.Cleanup(func() {
		removalBootSessionProvider = previous
	})
	return boot
}

func TestInFlightRemovalReconciliationRequiresConfirmedAttestedReboot(t *testing.T) {
	currentBoot, attested, err := currentBootSessionID()
	if err != nil || !attested {
		t.Skip("platform does not provide an attested boot session")
	}
	oldBoot := strings.Repeat("0", 64)
	if oldBoot == currentBoot {
		oldBoot = strings.Repeat("1", 64)
	}
	for _, packagePresent := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "present"}[packagePresent], func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "relay.json")
			candidate := removalCleanupCandidate(t)
			request := testRemovalRequest(RemovalModePreserveData)
			intent, err := NewRemovalIntentRecord(candidate, request)
			if err != nil {
				t.Fatal(err)
			}
			pending, err := NewRemovalCleanupRecord(candidate, request, 0)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteRemovalCleanup(configPath, intent); err != nil {
				t.Fatal(err)
			}
			if err := WriteRemovalCleanup(configPath, pending); err != nil {
				t.Fatal(err)
			}
			inFlight, err := BeginRemovalPackageExecution(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := ReconcileRemovalPackageAfterReboot(configPath, false); !errors.Is(err, ErrRemovalConfirmationNeeded) {
				t.Fatalf("unconfirmed reboot reconciliation error=%v", err)
			}
			if _, _, err := ReconcileRemovalPackageAfterReboot(configPath, true); !errors.Is(err, ErrRemovalCleanupUnsafe) {
				t.Fatalf("same-boot reconciliation error=%v", err)
			}
			if err := RemoveRemovalCleanup(configPath); err != nil {
				t.Fatal(err)
			}
			inFlight.ExecutionBootSession = oldBoot
			inFlight.ActiveExecution.BootSession = oldBoot
			if err := WriteRemovalCleanup(configPath, inFlight); err != nil {
				t.Fatal(err)
			}
			if packagePresent {
				if err := os.MkdirAll(candidate.PackageRoot, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			reconciled, absent, err := ReconcileRemovalPackageAfterReboot(configPath, true)
			if err != nil || absent == packagePresent {
				t.Fatalf("reboot reconciliation=%#v absent=%t err=%v", reconciled, absent, err)
			}
			wantPhase := RemovalCleanupPhasePackageVerified
			if packagePresent {
				wantPhase = RemovalCleanupPhasePackagePending
			}
			if reconciled.Phase != wantPhase || packagePresent && (!reconciled.ProcessReconciledAfterReboot || reconciled.ExecutionBootSession != oldBoot) ||
				!packagePresent && (reconciled.ProcessReconciledAfterReboot || reconciled.ExecutionBootSession != oldBoot) {
				t.Fatalf("reconciled phase/evidence=%#v want=%s", reconciled, wantPhase)
			}
		})
	}
}
