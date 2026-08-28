//go:build windows

package handoff

import "os/exec"

func configureRemovalProcessGroup(_ *exec.Cmd) {}

func terminateRemovalProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
}

func verifyRemovalProcessGroupTerminated(*exec.Cmd) bool { return true }
