//go:build !windows

package handoff

import (
	"errors"
	"os/exec"
	"syscall"
	"time"
)

func configureRemovalProcessGroup(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateRemovalProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
	_ = command.Process.Kill()
}

func verifyRemovalProcessGroupTerminated(command *exec.Cmd) bool {
	if command == nil || command.Process == nil {
		return false
	}
	for attempt := 0; attempt < 40; attempt++ {
		err := syscall.Kill(-command.Process.Pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return true
		}
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return false
}
