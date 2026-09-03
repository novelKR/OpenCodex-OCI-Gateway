//go:build !darwin && !linux

package containerruntime

import (
	"context"
	"os/exec"
	"time"
)

func prepareOwnedCommand(_ *exec.Cmd) {}

func waitOwnedCommand(
	ctx context.Context,
	command *exec.Cmd,
	waited <-chan error,
	grace time.Duration,
) (error, bool) {
	select {
	case err := <-waited:
		return err, ctx.Err() != nil
	case <-ctx.Done():
		if command.Process != nil {
			_ = command.Process.Kill()
		}
		return <-waited, true
	}
}
