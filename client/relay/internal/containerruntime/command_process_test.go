//go:build darwin || linux

package containerruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const commandProcessHelperEnvironment = "OPENCODEX_COMMAND_PROCESS_HELPER"
const commandProcessHelperReleaseEnvironment = "OPENCODEX_COMMAND_PROCESS_HELPER_RELEASE"

func TestSystemCommandRunnerCancellationReapsOwnedDescendantsBeforeRetry(t *testing.T) {
	for _, test := range []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
	}{
		{
			name: "user cancellation",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithCancel(context.Background())
			},
		},
		{
			name: "deadline",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 100*time.Millisecond)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			pidPath := t.TempDir() + "/descendant.pid"
			t.Setenv(commandProcessHelperEnvironment, pidPath)
			ctx, cancel := test.context()
			defer cancel()
			type result struct {
				output commandOutput
				err    error
			}
			done := make(chan result, 1)
			go func() {
				output, err := systemCommandRunner{}.Run(ctx, os.Args[0], []string{
					"-test.run=^TestSystemCommandRunnerDescendantHelper$",
				}, nil, 4<<10)
				done <- result{output: output, err: err}
			}()
			pid := waitForFixturePID(t, pidPath)
			cancel()
			select {
			case result := <-done:
				if !errors.Is(result.err, ErrUnavailable) {
					t.Fatalf("canceled command error = %v", result.err)
				}
				if len(result.output.stdout) != 0 || len(result.output.stderr) != 0 {
					t.Fatal("canceled command returned captured output")
				}
			case <-time.After(4 * time.Second):
				t.Fatal("canceled command was not reaped")
			}
			waitForProcessExit(t, pid)

			// Run a narrow retry immediately. The first call is required to keep
			// the lifecycle lease until its complete process group has gone.
			output, err := systemCommandRunner{}.Run(
				context.Background(), "/usr/bin/true", nil, nil, 1024,
			)
			zeroCommandOutput(&output)
			if err != nil {
				t.Fatalf("serialized retry failed: %v", err)
			}
		})
	}
}

func TestSystemCommandRunnerCancellationWaitRaceStillReapsOwnedDescendant(t *testing.T) {
	directory := t.TempDir()
	pidPath := directory + "/descendant.pid"
	releasePath := directory + "/release"
	t.Setenv(commandProcessHelperEnvironment, pidPath)
	t.Setenv(commandProcessHelperReleaseEnvironment, releasePath)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := systemCommandRunner{}.Run(ctx, os.Args[0], []string{
			"-test.run=^TestSystemCommandRunnerDescendantHelper$",
		}, nil, 4<<10)
		done <- err
	}()
	pid := waitForFixturePID(t, pidPath)
	cancel()
	if err := os.WriteFile(releasePath, []byte("release\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrUnavailable) {
			t.Fatalf("canceled command error = %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("canceled command did not return")
	}
	waitForProcessExit(t, pid)
}

func TestSystemCommandRunnerDescendantHelper(t *testing.T) {
	pidPath := os.Getenv(commandProcessHelperEnvironment)
	if pidPath == "" {
		return
	}
	_, _ = os.Stdout.WriteString("fixture-secret\n")
	child := exec.Command("/bin/sh", "-c", `trap '' TERM; echo $$ > "$1"; while :; do sleep 1; done`, "fixture", pidPath)
	child.Stdin = nil
	child.Stdout = io.Discard
	child.Stderr = io.Discard
	if err := child.Start(); err != nil {
		t.Fatalf("start descendant: %v", err)
	}
	if releasePath := os.Getenv(commandProcessHelperReleaseEnvironment); releasePath != "" {
		for {
			if _, err := os.Stat(releasePath); err == nil {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	select {}
}

func waitForFixturePID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, parseErr := strconv.Atoi(strings.TrimSpace(string(data)))
			if parseErr == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("descendant did not become ready")
	return 0
}

func waitForProcessExit(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("descendant process %d survived cancellation", pid)
}
