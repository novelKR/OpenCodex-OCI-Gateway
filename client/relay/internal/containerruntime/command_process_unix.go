//go:build darwin || linux

package containerruntime

import (
	"context"
	"os/exec"
	"syscall"
	"time"
)

func prepareOwnedCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func waitOwnedCommand(
	ctx context.Context,
	command *exec.Cmd,
	waited <-chan error,
	grace time.Duration,
) (error, bool) {
	select {
	case err := <-waited:
		// Context cancellation and Wait can become ready simultaneously, and a
		// CLI can exit while leaving a descendant in the owned group. Drain that
		// exact group whichever select arm wins before releasing the lifecycle
		// lock to a retry.
		if command.Process != nil && command.Process.Pid > 0 {
			terminateReapedOwnedProcessGroup(command.Process.Pid, grace)
		}
		return err, ctx.Err() != nil
	case <-ctx.Done():
		return terminateOwnedProcessGroup(command, waited, grace), true
	}
}

func terminateReapedOwnedProcessGroup(groupID int, grace time.Duration) {
	if groupID <= 0 || !ownedProcessGroupExists(groupID) {
		return
	}
	_ = syscall.Kill(-groupID, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for ownedProcessGroupExists(groupID) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if ownedProcessGroupExists(groupID) {
		_ = syscall.Kill(-groupID, syscall.SIGKILL)
	}
	// There is deliberately no post-SIGKILL deadline: returning while the
	// owned group is still addressable would release the lifecycle lock and let
	// a retry overlap a descendant from the previous mutation.
	for ownedProcessGroupExists(groupID) {
		time.Sleep(10 * time.Millisecond)
	}
}

func terminateOwnedProcessGroup(command *exec.Cmd, waited <-chan error, grace time.Duration) error {
	if command.Process == nil || command.Process.Pid <= 0 {
		if waited != nil {
			return <-waited
		}
		return nil
	}
	groupID := command.Process.Pid
	// Setpgid above guarantees that the group ID is the exact child PID. Never
	// fall back to the caller's process group or signal a group we did not make.
	_ = syscall.Kill(-groupID, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	var waitErr error
	waitComplete := false
	for time.Now().Before(deadline) {
		if !waitComplete {
			select {
			case waitErr = <-waited:
				waitComplete = true
			default:
			}
		}
		if !ownedProcessGroupExists(groupID) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if ownedProcessGroupExists(groupID) {
		_ = syscall.Kill(-groupID, syscall.SIGKILL)
	}
	if !waitComplete {
		waitErr = <-waited
	}
	// SIGKILL is asynchronous. Do not hand the lifecycle lock to a retry while
	// a descendant from the canceled command is still addressable in the owned
	// group.
	for ownedProcessGroupExists(groupID) {
		time.Sleep(10 * time.Millisecond)
	}
	return waitErr
}

func ownedProcessGroupExists(groupID int) bool {
	err := syscall.Kill(-groupID, 0)
	return err == nil || err == syscall.EPERM
}
