package containerruntime

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
)

const maximumCommandOutputBytes = 64 << 10

type commandOutput struct {
	stdout []byte
	stderr []byte
}

type commandRunner interface {
	Run(context.Context, string, []string, io.Reader, int64) (commandOutput, error)
}

type systemCommandRunner struct{}

func (systemCommandRunner) Run(ctx context.Context, executable string, arguments []string, stdin io.Reader, maximum int64) (commandOutput, error) {
	if executable == "" || maximum <= 0 || maximum > maximumCommandOutputBytes {
		return commandOutput{}, ErrInvalidRequest
	}
	command := exec.CommandContext(ctx, executable, arguments...)
	command.Stdin = stdin
	stdout := &limitedCommandBuffer{remaining: maximum}
	stderr := &limitedCommandBuffer{remaining: maximum}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	if stdout.exceeded || stderr.exceeded {
		zeroBytes(stdout.buffer.Bytes())
		zeroBytes(stderr.buffer.Bytes())
		return commandOutput{}, ErrUnavailable
	}
	result := commandOutput{
		stdout: append([]byte(nil), stdout.buffer.Bytes()...),
		stderr: append([]byte(nil), stderr.buffer.Bytes()...),
	}
	if err != nil {
		zeroBytes(result.stdout)
		zeroBytes(result.stderr)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.Canceled) {
			return commandOutput{}, ErrUnavailable
		}
		return commandOutput{}, ErrUnavailable
	}
	return result, nil
}

type limitedCommandBuffer struct {
	buffer    bytes.Buffer
	remaining int64
	exceeded  bool
}

func (b *limitedCommandBuffer) Write(value []byte) (int, error) {
	original := len(value)
	if int64(len(value)) > b.remaining {
		value = value[:max(0, int(b.remaining))]
		b.exceeded = true
	}
	if len(value) != 0 {
		_, _ = b.buffer.Write(value)
		b.remaining -= int64(len(value))
	}
	return original, nil
}
