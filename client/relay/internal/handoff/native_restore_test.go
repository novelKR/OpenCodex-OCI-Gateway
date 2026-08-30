package handoff

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

type nativeRestoreResponse struct {
	payload string
	nonZero bool
	err     error
}

type nativeRestoreCall struct {
	program     string
	arguments   []string
	environment []string
}

type nativeRestoreRunner struct {
	responses []nativeRestoreResponse
	mutate    string
	calls     []nativeRestoreCall
}

func (r *nativeRestoreRunner) RunOutput(_ context.Context, program string, environment []string, args ...string) ([]byte, bool, error) {
	r.calls = append(r.calls, nativeRestoreCall{
		program:     program,
		arguments:   append([]string{}, args...),
		environment: append([]string{}, environment...),
	})
	if r.mutate != "" {
		if err := os.WriteFile(r.mutate, []byte("#!/bin/sh\necho replaced\n"), 0o700); err != nil {
			return nil, false, err
		}
		r.mutate = ""
	}
	if len(r.responses) == 0 {
		return nil, false, errors.New("unexpected native restore invocation")
	}
	response := r.responses[0]
	r.responses = r.responses[1:]
	return []byte(response.payload), response.nonZero, response.err
}

func successfulOwnerProbe() []nativeRestoreResponse {
	return []nativeRestoreResponse{
		{payload: `{"ok":true}`},
		{payload: `{"clientIntegrations":{"codex":false}}`},
	}
}

func nativeRestoreDiscoveryCandidate(t *testing.T, parentWritable bool) (string, string, NPMInstallation) {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, "node")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	if parentWritable {
		if err := os.Chmod(prefix, 0o775); err != nil {
			t.Fatal(err)
		}
	}
	result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
		Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"),
		GOOS: "darwin", GOARCH: "arm64", SkipDefaultPrefixes: true,
		Getenv: func(string) string { return "" },
	})
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("native restore discovery = %#v, %v", result, err)
	}
	candidate := result.Candidates[0]
	if candidate.NativeRestoreCapability != NativeRestoreCapabilityVerifiedSnapshot ||
		candidate.NativeRestoreFingerprint == "" || candidate.nativeRestoreProof == nil {
		t.Fatalf("native restore proof = %#v", candidate)
	}
	if !candidate.nativeRestoreProof.valid() ||
		candidate.nativeRestoreProof.fingerprint(candidate.Fingerprint) != candidate.NativeRestoreFingerprint {
		t.Fatalf(
			"invalid native restore proof valid=%t proof=%#v fingerprint=%q recomputed=%q installation=%q",
			candidate.nativeRestoreProof.valid(), candidate.nativeRestoreProof, candidate.NativeRestoreFingerprint,
			candidate.nativeRestoreProof.fingerprint(candidate.Fingerprint), candidate.Fingerprint,
		)
	}
	return home, prefix, candidate
}

func nativeRestoreFixture(t *testing.T) (NPMInstallation, string, string) {
	t.Helper()
	home, _, candidate := nativeRestoreDiscoveryCandidate(t, false)
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	return candidate, home, filepath.Join(codexHome, "config.toml")
}

func TestNativeRestoreResolverRevalidatesSanitizedTierBSelection(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	prefix := filepath.Join(home, ".nvm", "versions", "node", "v22.0.0")
	writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
	resolver := DiscoveryNativeRestoreResolver{Options: DiscoveryOptions{
		HomeDir: home, GOOS: "darwin", GOARCH: "arm64", SkipDefaultPrefixes: true,
	}}
	result, err := DiscoverNPMInstallations(
		context.Background(),
		SanitizedNativeRestoreDiscoveryOptions(resolver.Options),
	)
	if err != nil || len(result.Candidates) != 1 {
		t.Fatalf("sanitized Tier B discovery = %#v, %v", result, err)
	}
	candidate := result.Candidates[0]
	selection := NativeRestoreSelection{
		InstallationID:           candidate.ID,
		InstallationFingerprint:  candidate.Fingerprint,
		NativeRestoreFingerprint: candidate.NativeRestoreFingerprint,
		Executable:               candidate.Executable,
		ExecutableSHA256:         candidate.ExecutableSHA256,
	}
	resolved, err := resolver.Resolve(context.Background(), selection)
	if err != nil || !sameNativeRestoreExecutionProof(candidate, resolved) {
		t.Fatalf("resolved candidate = %#v, %v", resolved, err)
	}

	drifted := selection
	drifted.NativeRestoreFingerprint = strings.Repeat("f", 64)
	if _, err := resolver.Resolve(context.Background(), drifted); !errors.Is(err, ErrNativeRestoreCandidateMissing) {
		t.Fatalf("selection drift err=%v", err)
	}
	if err := os.WriteFile(candidate.nativeRestoreProof.cliEntry, []byte("console.log('changed')\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := resolver.Revalidate(context.Background(), candidate); !errors.Is(err, ErrNativeRestoreCandidateChanged) {
		t.Fatalf("source drift err=%v", err)
	}
}

func TestNativeRestoreSnapshotRejectsRuntimeAndTreeDrift(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, NPMInstallation)
	}{
		{
			name: "Bun changed",
			mutate: func(t *testing.T, candidate NPMInstallation) {
				writeExecutable(t, candidate.nativeRestoreProof.bunExecutable)
				if err := os.WriteFile(candidate.nativeRestoreProof.bunExecutable, []byte("#!/bin/sh\necho changed\n"), 0o700); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "CLI changed",
			mutate: func(t *testing.T, candidate NPMInstallation) {
				if err := os.WriteFile(candidate.nativeRestoreProof.cliEntry, []byte("console.log('changed')\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "package tree changed",
			mutate: func(t *testing.T, candidate NPMInstallation) {
				if err := os.WriteFile(filepath.Join(candidate.PackageRoot, "drift.txt"), []byte("changed\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, _, _ := nativeRestoreFixture(t)
			test.mutate(t, candidate)
			snapshot, err := prepareNativeRestoreExecutionSnapshot(context.Background(), candidate)
			snapshot.Close()
			if !errors.Is(err, ErrNativeRestoreCandidateChanged) {
				t.Fatalf("runtime drift err=%v", err)
			}
		})
	}
}

func TestNativeRestoreProofRejectsUnsafeTree(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, string, string)
	}{
		{
			name: "external symlink",
			mutate: func(t *testing.T, home, root string) {
				outside := filepath.Join(home, "outside")
				if err := os.WriteFile(outside, []byte("outside\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, filepath.Join(root, "outside-link")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "group writable file",
			mutate: func(t *testing.T, _ string, root string) {
				path := filepath.Join(root, "writable.js")
				if err := os.WriteFile(path, []byte("unsafe\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o660); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			prefix := filepath.Join(home, "node")
			root := writeNPMInstallation(t, prefix, OpenCodexPackageName, "./bin/ocx.mjs")
			test.mutate(t, home, root)
			result, err := DiscoverNPMInstallations(context.Background(), DiscoveryOptions{
				Tier: DiscoveryTierA, HomeDir: home, PathEnv: filepath.Join(prefix, "bin"),
				GOOS: "darwin", GOARCH: "arm64", SkipDefaultPrefixes: true,
				Getenv: func(string) string { return "" },
			})
			if err != nil || len(result.Candidates) != 1 {
				t.Fatalf("unsafe tree discovery = %#v, %v", result, err)
			}
			candidate := result.Candidates[0]
			if candidate.NativeRestoreCapability != "" || candidate.NativeRestoreFingerprint != "" || candidate.nativeRestoreProof != nil {
				t.Fatalf("unsafe tree received restore proof: %#v", candidate)
			}
		})
	}
}

func TestNativeRestoreExecutorAcceptsRoutingSuccessAndBoundsNonRoutingFailure(t *testing.T) {
	candidate, home, configPath := nativeRestoreFixture(t)
	valid := `{"success":true,"message":"ignored","artifacts":{"config":{"state":"ok","changed":true,"action":"owned-fields-stripped","message":"ignored"},"catalog":{"state":"ok"},"history":{"state":"ok"}}}`
	runner := &nativeRestoreRunner{responses: append([]nativeRestoreResponse{{payload: valid}}, successfulOwnerProbe()...)}
	result, err := (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).ExecuteExpected(
		context.Background(), candidate, configPath,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Outcome != NativeRestoreApplied || result.NonRoutingWarning {
		t.Fatalf("result=%#v", result)
	}
	wantArguments := [][]string{{"restore", "--json"}, {"config", "validate", "--json"}, {"config", "show", "--json"}}
	if len(runner.calls) != len(wantArguments) {
		t.Fatalf("calls=%d want=%d", len(runner.calls), len(wantArguments))
	}
	for index, call := range runner.calls {
		if len(call.arguments) < 6 || call.arguments[0] != "--config" || call.arguments[2] != "--no-install" ||
			call.arguments[3] != "--no-orphans" || call.arguments[4] != "--no-env-file" {
			t.Fatalf("call[%d] unbounded Bun args=%v", index, call.arguments)
		}
		if call.program == candidate.nativeRestoreProof.bunExecutable ||
			!reflect.DeepEqual(call.arguments[len(call.arguments)-len(wantArguments[index]):], wantArguments[index]) {
			t.Fatalf("call[%d] program=%q args=%v", index, call.program, call.arguments)
		}
		var codexHome string
		for _, item := range call.environment {
			if strings.HasPrefix(item, "CODEX_HOME=") {
				codexHome = strings.TrimPrefix(item, "CODEX_HOME=")
			}
		}
		if codexHome != filepath.Dir(configPath) {
			t.Fatalf("call[%d] CODEX_HOME=%q", index, codexHome)
		}
	}

	runner = &nativeRestoreRunner{responses: append([]nativeRestoreResponse{{
		payload: `{"success":false,"artifacts":{"config":{"state":"ok","changed":true,"action":"journal-restored"}}}`,
		nonZero: true,
	}}, successfulOwnerProbe()...)}
	result, err = (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).ExecuteExpected(
		context.Background(), candidate, configPath,
	)
	if err != nil || result.Outcome != NativeRestoreApplied || !result.NonRoutingWarning {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestNativeRestoreExecutorClassifiesAlreadyNativeAndRetryableNoMutation(t *testing.T) {
	candidate, home, configPath := nativeRestoreFixture(t)

	runner := &nativeRestoreRunner{responses: append([]nativeRestoreResponse{{
		payload: `{"success":true,"artifacts":{"config":{"state":"skipped","changed":false,"action":"owned-fields-stripped"}}}`,
	}}, successfulOwnerProbe()...)}
	result, err := (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).ExecuteExpected(
		context.Background(), candidate, configPath,
	)
	if err != nil || result.Outcome != NativeRestoreAlreadyNative || len(runner.calls) != 3 {
		t.Fatalf("result=%#v calls=%d err=%v", result, len(runner.calls), err)
	}

	runner = &nativeRestoreRunner{responses: append([]nativeRestoreResponse{{
		payload: `{"success":false,"artifacts":{"config":{"state":"skipped","changed":false,"action":"owned-fields-stripped"}}}`,
		nonZero: true,
	}}, successfulOwnerProbe()...)}
	result, err = (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).ExecuteExpected(
		context.Background(), candidate, configPath,
	)
	if err != nil || result.Outcome != NativeRestoreRetryableNoMutation || !result.NonRoutingWarning || len(runner.calls) != 3 {
		t.Fatalf("result=%#v calls=%d err=%v", result, len(runner.calls), err)
	}
}

func TestNativeRestoreRetryableNoMutationStillRequiresDisabledIntegration(t *testing.T) {
	candidate, home, configPath := nativeRestoreFixture(t)
	runner := &nativeRestoreRunner{responses: []nativeRestoreResponse{
		{payload: `{"success":false,"artifacts":{"config":{"state":"skipped","changed":false,"action":"owned-fields-stripped"}}}`, nonZero: true},
		{payload: `{"ok":true}`},
		{payload: `{"clientIntegrations":{"codex":true}}`},
	}}
	_, err := (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).ExecuteExpected(
		context.Background(), candidate, configPath,
	)
	if !errors.Is(err, ErrNativeRestoreFailed) || len(runner.calls) != 3 {
		t.Fatalf("calls=%d err=%v", len(runner.calls), err)
	}
}

func TestOpenCodexOwnerConfigurationRevisionBindsExactSafeDefaultConfig(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	absent, err := OpenCodexOwnerConfigurationRevision(home)
	if err != nil || !isFingerprint(absent) {
		t.Fatalf("absent revision=%q err=%v", absent, err)
	}
	directory := filepath.Join(home, ".opencodex")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.json")
	if err := os.WriteFile(path, []byte(`{"clientIntegrations":{"codex":false}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := OpenCodexOwnerConfigurationRevision(home)
	if err != nil || !isFingerprint(first) || first == absent {
		t.Fatalf("first revision=%q absent=%q err=%v", first, absent, err)
	}
	if err := os.WriteFile(path, []byte(`{"clientIntegrations":{"codex":true}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := OpenCodexOwnerConfigurationRevision(home)
	if err != nil || !isFingerprint(second) || second == first {
		t.Fatalf("second revision=%q first=%q err=%v", second, first, err)
	}
	if err := os.Chmod(path, 0o622); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenCodexOwnerConfigurationRevision(home); !errors.Is(err, ErrNativeRestoreProofUnavailable) {
		t.Fatalf("unsafe config error=%v", err)
	}
}

func TestNativeRestoreExecutorRejectsStructuredFailureInvalidResultAndUnprovedOwner(t *testing.T) {
	tests := []struct {
		name      string
		responses []nativeRestoreResponse
		want      error
	}{
		{name: "malformed", responses: []nativeRestoreResponse{{payload: `{`}}, want: ErrNativeRestoreOutput},
		{name: "missing artifacts", responses: []nativeRestoreResponse{{payload: `{"success":true}`}}, want: ErrNativeRestoreOutput},
		{name: "structured restore failure", responses: []nativeRestoreResponse{{payload: `{"success":false,"artifacts":{"config":{"state":"failed","changed":false,"action":"failed"}}}`}}, want: ErrNativeRestoreFailed},
		{name: "unknown action", responses: []nativeRestoreResponse{{payload: `{"success":true,"artifacts":{"config":{"state":"ok","changed":true,"action":"other"}}}`}}, want: ErrNativeRestoreOutput},
		{name: "invalid owner configuration", responses: []nativeRestoreResponse{
			{payload: `{"success":true,"artifacts":{"config":{"state":"ok","changed":true,"action":"owned-fields-stripped"}}}`},
			{payload: `{"ok":false}`, nonZero: true},
		}, want: ErrNativeOwnerConfigurationInvalid},
		{name: "owner integration remains enabled", responses: []nativeRestoreResponse{
			{payload: `{"success":true,"artifacts":{"config":{"state":"ok","changed":true,"action":"owned-fields-stripped"}}}`},
			{payload: `{"ok":true}`},
			{payload: `{"clientIntegrations":{"codex":true}}`},
		}, want: ErrNativeRestoreFailed},
		{name: "owner result unavailable", responses: []nativeRestoreResponse{
			{payload: `{"success":true,"artifacts":{"config":{"state":"ok","changed":true,"action":"owned-fields-stripped"}}}`},
			{payload: `{`},
		}, want: ErrNativeRestoreOutput},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, home, configPath := nativeRestoreFixture(t)
			runner := &nativeRestoreRunner{responses: test.responses}
			_, err := (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).ExecuteExpected(
				context.Background(), candidate, configPath,
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("err=%v want=%v", err, test.want)
			}
		})
	}
}

func TestNativeRestoreExecutorInspectsOwnerWithoutExposingConfiguration(t *testing.T) {
	tests := []struct {
		name      string
		responses []nativeRestoreResponse
		want      NativeOwnerInspection
	}{
		{
			name: "valid enabled",
			responses: []nativeRestoreResponse{
				{payload: `{"ok":true}`},
				{payload: `{"clientIntegrations":{"codex":true},"secret":"discarded"}`},
			},
			want: NativeOwnerInspection{SchemaVersion: 1, Owner: "opencodex", Configuration: NativeOwnerConfigurationValid, Integration: NativeOwnerIntegrationEnabled, Reason: NativeOwnerReady},
		},
		{
			name: "valid disabled",
			responses: []nativeRestoreResponse{
				{payload: `{"ok":true}`},
				{payload: `{"clientIntegrations":{"codex":false}}`},
			},
			want: NativeOwnerInspection{SchemaVersion: 1, Owner: "opencodex", Configuration: NativeOwnerConfigurationValid, Integration: NativeOwnerIntegrationDisabled, Reason: NativeOwnerReady},
		},
		{
			name:      "invalid",
			responses: []nativeRestoreResponse{{payload: `{"ok":false}`, nonZero: true}},
			want:      NativeOwnerInspection{SchemaVersion: 1, Owner: "opencodex", Configuration: NativeOwnerConfigurationInvalid, Integration: NativeOwnerIntegrationUnknown, Reason: NativeOwnerConfigurationInvalidReason},
		},
		{
			name:      "unavailable",
			responses: []nativeRestoreResponse{{payload: `{`}},
			want:      NativeOwnerInspection{SchemaVersion: 1, Owner: "opencodex", Configuration: NativeOwnerConfigurationUnavailable, Integration: NativeOwnerIntegrationUnknown, Reason: NativeOwnerProbeUnavailable},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate, home, configPath := nativeRestoreFixture(t)
			runner := &nativeRestoreRunner{responses: test.responses}
			inspection, err := (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).InspectExpected(
				context.Background(), candidate, configPath,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(inspection, test.want) {
				t.Fatalf("inspection=%#v want=%#v", inspection, test.want)
			}
		})
	}
}

func TestNativeRestoreExecutorUsesImmutableSnapshotAfterSourceMutation(t *testing.T) {
	candidate, home, configPath := nativeRestoreFixture(t)
	runner := &nativeRestoreRunner{
		responses: append([]nativeRestoreResponse{{payload: `{"success":true,"artifacts":{"config":{"state":"ok","changed":true,"action":"owned-fields-stripped"}}}`}}, successfulOwnerProbe()...),
		mutate:    candidate.nativeRestoreProof.bunExecutable,
	}
	result, err := (NativeRestoreExecutor{Platform: "darwin", Runner: runner, HomeDir: home}).ExecuteExpected(
		context.Background(), candidate, configPath,
	)
	if err != nil || result.Outcome != NativeRestoreApplied {
		t.Fatalf("snapshot result=%#v err=%v", result, err)
	}
	for _, call := range runner.calls {
		if call.program == candidate.nativeRestoreProof.bunExecutable {
			t.Fatalf("source Bun executed directly: %#v", call)
		}
	}
}

func TestBoundedNativeRestoreRunnerRejectsOversizedOutput(t *testing.T) {
	payload := strings.Repeat("x", maxNativeRestoreOutputBytes+1)
	_, _, err := (boundedCommandOutputRunner{}).RunOutput(
		context.Background(), "/bin/sh", os.Environ(), "-c", "printf '%s' \"$1\"", "sh", payload,
	)
	if !errors.Is(err, ErrNativeRestoreOutput) {
		t.Fatalf("oversized output err=%v", err)
	}
}

func TestBoundedNativeRestoreRunnerCancellationIsBounded(t *testing.T) {
	const helperEnvironment = "OPENCODEX_NATIVE_RESTORE_CANCELLED_HELPER"
	if os.Getenv(helperEnvironment) == "sleep" {
		time.Sleep(5 * time.Second)
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, _, err = (boundedCommandOutputRunner{}).RunOutput(
		ctx,
		executable,
		environmentWith(os.Environ(), helperEnvironment, "sleep"),
		"-test.run=TestBoundedNativeRestoreRunnerCancellationIsBounded",
	)
	if !errors.Is(err, ErrNativeRestoreOutput) || time.Since(started) >= 2*time.Second {
		t.Fatalf("cancelled native restore err=%v elapsed=%s", err, time.Since(started))
	}
}

func TestNativeRestoreEnvironmentDropsHostileCallerOverrides(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codexHome := filepath.Join(home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NODE_OPTIONS", "--require=/tmp/hostile.js")
	t.Setenv("OPENCODEX_BUN_PATH", "/tmp/hostile-bun")
	t.Setenv("PATH", "/tmp/hostile-path")
	environment, err := nativeRestoreEnvironment(home, codexHome)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"HOME":       home,
		"PATH":       "/usr/bin:/bin",
		"CODEX_HOME": codexHome,
	}
	for _, item := range environment {
		name, value, ok := strings.Cut(item, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", item)
		}
		if name == "NODE_OPTIONS" || name == "OPENCODEX_BUN_PATH" {
			t.Fatalf("hostile environment survived: %q", item)
		}
		if expected, exists := want[name]; exists && value != expected {
			t.Fatalf("%s=%q want %q", name, value, expected)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("missing environment entries: %#v", want)
	}
}

func TestBoundedNativeRestoreRunnerTerminatesDescendantHoldingStdout(t *testing.T) {
	const helperEnvironment = "OPENCODEX_NATIVE_RESTORE_DESCENDANT_HELPER"
	mode := os.Getenv(helperEnvironment)
	marker := os.Getenv("OPENCODEX_NATIVE_RESTORE_DESCENDANT_MARKER")
	if mode == "parent" {
		executable, err := os.Executable()
		if err != nil {
			os.Exit(91)
		}
		child := exec.Command(executable, "-test.run=TestBoundedNativeRestoreRunnerTerminatesDescendantHoldingStdout")
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
	environment = environmentWith(environment, "OPENCODEX_NATIVE_RESTORE_DESCENDANT_MARKER", marker)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	started := time.Now()
	_, _, err = (boundedCommandOutputRunner{}).RunOutput(
		ctx,
		executable,
		environment,
		"-test.run=TestBoundedNativeRestoreRunnerTerminatesDescendantHoldingStdout",
	)
	elapsed := time.Since(started)
	if !errors.Is(err, ErrNativeRestoreOutput) || elapsed >= 2*time.Second {
		t.Fatalf("descendant native restore err=%v elapsed=%s", err, elapsed)
	}
	time.Sleep(1800 * time.Millisecond)
	if _, err := os.Lstat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("native restore descendant survived process-group cleanup: %v", err)
	}
}
