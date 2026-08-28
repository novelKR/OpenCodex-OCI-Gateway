package handoff

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type recordedRunner struct {
	calls [][]string
	errAt int
}

func (r *recordedRunner) Run(_ context.Context, program string, args ...string) error {
	r.calls = append(r.calls, append([]string{program}, args...))
	if r.errAt > 0 && len(r.calls) == r.errAt {
		return errors.New("runner failure")
	}
	return nil
}

type mutatingRunner struct {
	path  string
	calls [][]string
}

func (r *mutatingRunner) Run(_ context.Context, program string, args ...string) error {
	r.calls = append(r.calls, append([]string{program}, args...))
	if len(r.calls) == 1 {
		return os.WriteFile(r.path, []byte("#!/bin/sh\necho replaced\n"), 0o700)
	}
	return nil
}

func TestExecuteRequiresConfirmationAndRunsOnlyApprovedSequence(t *testing.T) {
	executable := testExecutable(t)
	resolved, _, err := VerifyExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordedRunner{}
	executor := Executor{Platform: "darwin", Runner: runner}
	if _, err := executor.Execute(context.Background(), executable, RetainProxyRemoveShim, false); !errors.Is(err, ErrConfirmationRequired) {
		t.Fatalf("unconfirmed error = %v", err)
	}
	record, err := executor.Execute(context.Background(), executable, RetainProxyRemoveShim, true)
	if err != nil {
		t.Fatal(err)
	}
	if record.Action != RetainProxyRemoveShim || record.Executable != resolved || record.Fingerprint == "" {
		t.Fatalf("record = %#v", record)
	}
	want := [][]string{{resolved, "restore"}, {resolved, "codex-shim", "uninstall"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestExecuteKeepShimRunsOnlyRestore(t *testing.T) {
	executable := testExecutable(t)
	resolved, _, err := VerifyExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordedRunner{}
	record, err := (Executor{Platform: "darwin", Runner: runner}).Execute(
		context.Background(), executable, RetainProxyKeepShim, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.Action != RetainProxyKeepShim {
		t.Fatalf("record action = %q", record.Action)
	}
	want := [][]string{{resolved, "restore"}}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Fatalf("calls = %#v, want %#v", runner.calls, want)
	}
}

func TestExecutorRejectsLegacyUninstallBeforeAnyAction(t *testing.T) {
	runner := &recordedRunner{}
	_, err := (Executor{Platform: "darwin", Runner: runner}).Execute(
		context.Background(), testExecutable(t), Action("uninstall"), true,
	)
	if !errors.Is(err, ErrInvalidAction) {
		t.Fatalf("legacy uninstall error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("legacy uninstall invoked runner: %#v", runner.calls)
	}
}

func TestExecuteStopsAfterRestoreFailure(t *testing.T) {
	executable := testExecutable(t)
	runner := &recordedRunner{errAt: 1}
	_, err := (Executor{Platform: "darwin", Runner: runner}).Execute(context.Background(), executable, RetainProxyRemoveShim, true)
	if err == nil || len(runner.calls) != 1 {
		t.Fatalf("err=%v calls=%#v", err, runner.calls)
	}
}

func TestExecuteExpectedRejectsChangedSelectionBeforeAnyAction(t *testing.T) {
	executable := testExecutable(t)
	_, fingerprint, err := VerifyExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	runner := &recordedRunner{}
	if _, err := (Executor{Platform: "darwin", Runner: runner}).ExecuteExpected(
		context.Background(),
		executable,
		strings.Repeat("0", len(fingerprint)),
		RetainProxyKeepShim,
		true,
	); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("mismatched expected fingerprint error = %v", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("mismatched selection invoked runner: %#v", runner.calls)
	}
}

func TestExecuteExpectedRevalidatesBeforeEveryOpenCodexSubcommand(t *testing.T) {
	executable := testExecutable(t)
	_, fingerprint, err := VerifyExecutable(executable)
	if err != nil {
		t.Fatal(err)
	}
	runner := &mutatingRunner{path: executable}
	_, err = (Executor{Platform: "darwin", Runner: runner}).ExecuteExpected(
		context.Background(),
		executable,
		fingerprint,
		RetainProxyRemoveShim,
		true,
	)
	if !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("replacement between OCX actions error = %v", err)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("replacement ran more than the first OCX command: %#v", runner.calls)
	}
}

func TestVerifyExecutableRejectsWritableFileOrParent(t *testing.T) {
	writableFile := testExecutable(t)
	if err := os.Chmod(writableFile, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyExecutable(writableFile); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("group/world-writable executable error = %v", err)
	}

	directory := t.TempDir()
	writableParent := filepath.Join(directory, "ocx")
	if err := os.WriteFile(writableParent, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatal(err)
	}
	if _, _, err := VerifyExecutable(writableParent); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("group/world-writable parent error = %v", err)
	}
}

func TestPreflightRecordWriteRejectsUnsafeReceiptAndAcceptsOwnerDirectory(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	if err := PreflightRecordWrite(configPath); err != nil {
		t.Fatalf("safe receipt path preflight: %v", err)
	}
	receipt := EnrollmentPath(configPath)
	if err := os.WriteFile(receipt, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PreflightRecordWrite(configPath); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("unsafe receipt preflight error = %v", err)
	}
}

func TestPreflightRelayConfigRequiresOwnerOnlyRegularFile(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "relay.json")
	if err := os.WriteFile(configPath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := PreflightRelayConfig(configPath); err != nil {
		t.Fatalf("safe relay config preflight: %v", err)
	}
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PreflightRelayConfig(configPath); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("broad relay config preflight error = %v", err)
	}
}

func TestReadRecordRejectsChangedExecutable(t *testing.T) {
	executable := testExecutable(t)
	runner := &recordedRunner{}
	record, err := (Executor{Platform: "darwin", Runner: runner}).Execute(context.Background(), executable, RetainProxyKeepShim, true)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "relay.json")
	if err := WriteRecord(configPath, record); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(configPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRecord(configPath); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("changed executable error = %v", err)
	}
}

func TestWriteRecordRejectsLegacyUninstallAction(t *testing.T) {
	executable := testExecutable(t)
	record, err := (Executor{Platform: "darwin", Runner: &recordedRunner{}}).Execute(
		context.Background(), executable, RetainProxyKeepShim, true,
	)
	if err != nil {
		t.Fatal(err)
	}
	record.Action = Action("uninstall")
	if err := WriteRecord(filepath.Join(t.TempDir(), "relay.json"), record); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("legacy record action error = %v", err)
	}
}

func TestExecuteRejectsRelativeAndNonDarwin(t *testing.T) {
	if _, err := (Executor{Platform: "linux"}).Execute(context.Background(), "/bin/true", RetainProxyKeepShim, true); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("platform error = %v", err)
	}
	if _, _, err := VerifyExecutable("relative-ocx"); !errors.Is(err, ErrUnsafeExecutable) {
		t.Fatalf("relative executable error = %v", err)
	}
}

func testExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ocx")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
