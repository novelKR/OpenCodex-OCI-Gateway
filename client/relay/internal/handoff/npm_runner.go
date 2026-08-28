package handoff

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	maxRemovalJSONOutput = 128 << 10
	maxNPMOutput         = 4 << 10
)

var (
	ErrRemovalCommandFailed       = errors.New("approved OpenCodex removal command failed")
	ErrRemovalOutputInvalid       = errors.New("OpenCodex removal command output is invalid")
	ErrRemovalProcessCleanup      = errors.New("OpenCodex removal process cleanup could not be verified")
	ErrTeardownUnsupported        = errors.New("OpenCodex teardown adapter is unsupported")
	ErrTeardownCandidateChanged   = errors.New("OpenCodex teardown candidate changed")
	ErrTeardownPreflightFailed    = errors.New("OpenCodex teardown preflight failed")
	ErrTeardownRefused            = errors.New("OpenCodex teardown was refused")
	ErrTeardownResultInvalid      = errors.New("OpenCodex teardown result is invalid")
	ErrTeardownVerificationFailed = errors.New("OpenCodex teardown verification failed")
)

// RemovalExecutionResult contains only bounded stdout, a numeric exit code, and
// process-lifecycle proof. CleanupVerified is true only after a started direct
// child has been reaped and its dedicated process group is proven absent.
// Stderr and exec errors are never retained because they can contain paths,
// environment values, or package-manager diagnostics.
type RemovalExecutionResult struct {
	Output          []byte
	ExitCode        int
	Started         bool
	CleanupVerified bool
}

// OpenCodexRemovalRunner exposes only the fixed operations needed by the
// wizard. Callers cannot provide a program, package name, prefix, or extra argv.
type OpenCodexRemovalRunner interface {
	Inventory(context.Context, NPMInstallation) (RemovalExecutionResult, error)
	Preflight(context.Context, NPMInstallation) (RemovalExecutionResult, error)
	Teardown(context.Context, NPMInstallation) (RemovalExecutionResult, error)
	Trash(context.Context, NPMInstallation, []string) (RemovalExecutionResult, error)
	Uninstall(context.Context, NPMInstallation) (RemovalExecutionResult, error)
}

func (r ExactNPMRunner) Preflight(ctx context.Context, candidate NPMInstallation) (RemovalExecutionResult, error) {
	return r.runRelayTeardownAdapter(ctx, candidate, "--preflight", false)
}

type removalProcessRunner interface {
	Run(context.Context, string, []string, []string, int64) (RemovalExecutionResult, error)
}

// ExactNPMRunner invokes the verified Node executable directly. It never uses a
// shell, PATH-resolved node/npm/ocx, sudo, or a caller-supplied package name.
type ExactNPMRunner struct {
	HomeDir                  string
	BeforeOCXMutation        func(context.Context) error
	BeforeUninstallCandidate func(context.Context, NPMInstallation) error
	BeforeUninstall          func(context.Context) error
	process                  removalProcessRunner
}

func (r ExactNPMRunner) Inventory(ctx context.Context, candidate NPMInstallation) (RemovalExecutionResult, error) {
	if candidate.RemovalCapability == RemovalCapabilityHomebrewGuardedNPM {
		return r.runGuardedInventory(ctx, candidate)
	}
	return r.runOCX(ctx, candidate, []string{"data", "inventory", "--json"}, false)
}

func (r ExactNPMRunner) runGuardedInventory(ctx context.Context, candidate NPMInstallation) (RemovalExecutionResult, error) {
	snapshot, err := prepareGuardedInventoryExecutionSnapshot(ctx, candidate)
	if err != nil {
		return RemovalExecutionResult{}, err
	}
	defer snapshot.Close()
	environment, err := removalEnvironment(r.HomeDir)
	if err != nil {
		return RemovalExecutionResult{}, err
	}
	args := []string{
		"--config", snapshot.bunConfigPath,
		"--no-install",
		"--no-orphans",
		"--no-env-file",
		snapshot.cliEntry,
		"data", "inventory", "--json",
	}
	return r.processRunner().Run(ctx, snapshot.bun, args, environment, maxRemovalJSONOutput)
}

func (r ExactNPMRunner) Teardown(ctx context.Context, candidate NPMInstallation) (RemovalExecutionResult, error) {
	return r.runRelayTeardownAdapter(ctx, candidate, "--execute", true)
}

func (r ExactNPMRunner) Trash(ctx context.Context, candidate NPMInstallation, itemIDs []string) (RemovalExecutionResult, error) {
	if len(itemIDs) == 0 || len(itemIDs) > maxRemovalDataItems {
		return RemovalExecutionResult{}, ErrInvalidRemovalRequest
	}
	args := make([]string, 0, 3+len(itemIDs)*2)
	args = append(args, "data", "trash")
	for _, itemID := range itemIDs {
		if !validOpenCodexDataItemID(itemID) {
			return RemovalExecutionResult{}, ErrInvalidRemovalRequest
		}
		args = append(args, "--item", itemID)
	}
	args = append(args, "--json")
	return r.runOCX(ctx, candidate, args, true)
}

func (r ExactNPMRunner) Uninstall(ctx context.Context, candidate NPMInstallation) (RemovalExecutionResult, error) {
	snapshot, err := prepareRemovalExecutionSnapshot(ctx, candidate)
	if err != nil {
		return RemovalExecutionResult{}, err
	}
	defer snapshot.Close()
	environment, err := removalEnvironment(r.HomeDir)
	if err != nil {
		return RemovalExecutionResult{}, err
	}
	if r.BeforeUninstallCandidate != nil {
		if err := r.BeforeUninstallCandidate(ctx, candidate); err != nil {
			return RemovalExecutionResult{}, err
		}
	}
	if r.BeforeUninstall != nil {
		if err := r.BeforeUninstall(ctx); err != nil {
			return RemovalExecutionResult{}, ErrRemovalRoutingChanged
		}
	}
	if err := validateLiveRemovalTargetsContext(ctx, candidate); err != nil {
		return RemovalExecutionResult{}, err
	}
	args := []string{
		snapshot.npmCLI,
		"uninstall",
		"--global",
		"--prefix", candidate.Prefix,
		"--ignore-scripts",
		"--no-audit",
		"--no-fund",
		"--offline",
		OpenCodexPackageName,
	}
	return r.processRunner().Run(ctx, snapshot.node, args, environment, maxNPMOutput)
}

func (r ExactNPMRunner) runOCX(ctx context.Context, candidate NPMInstallation, operation []string, mutating bool) (RemovalExecutionResult, error) {
	snapshot, err := prepareRemovalExecutionSnapshot(ctx, candidate)
	if err != nil {
		return RemovalExecutionResult{}, err
	}
	defer snapshot.Close()
	environment, err := removalEnvironment(r.HomeDir)
	if err != nil {
		return RemovalExecutionResult{}, err
	}
	if mutating && r.BeforeOCXMutation != nil {
		if err := r.BeforeOCXMutation(ctx); err != nil {
			return RemovalExecutionResult{}, ErrRemovalRoutingChanged
		}
	}
	args := make([]string, 0, len(operation)+7)
	args = append(args,
		"--config", snapshot.bunConfigPath,
		"--no-install",
		"--no-orphans",
		"--no-env-file",
		snapshot.cliEntry,
	)
	args = append(args, operation...)
	return r.processRunner().Run(ctx, snapshot.bun, args, environment, maxRemovalJSONOutput)
}

func (r ExactNPMRunner) runRelayTeardownAdapter(
	ctx context.Context,
	candidate NPMInstallation,
	mode string,
	mutating bool,
) (RemovalExecutionResult, error) {
	if mode != "--preflight" && mode != "--execute" {
		return RemovalExecutionResult{}, ErrTeardownUnsupported
	}
	snapshot, err := prepareTeardownAdapterExecutionSnapshot(ctx, candidate)
	if err != nil {
		return RemovalExecutionResult{}, err
	}
	defer snapshot.Close()
	environment, err := removalEnvironment(r.HomeDir)
	if err != nil {
		return RemovalExecutionResult{}, ErrTeardownPreflightFailed
	}
	if mutating && r.BeforeOCXMutation != nil {
		if err := r.BeforeOCXMutation(ctx); err != nil {
			return RemovalExecutionResult{}, ErrRemovalRoutingChanged
		}
	}
	args := []string{
		"--config", snapshot.bunConfigPath,
		"--no-install",
		"--no-orphans",
		"--no-env-file",
		snapshot.adapter,
		mode,
		"--adapter-id",
		candidate.TeardownAdapterID,
	}
	result, runErr := r.processRunner().Run(ctx, snapshot.bun, args, environment, maxRemovalJSONOutput)
	if runErr == nil {
		return result, nil
	}
	if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) ||
		errors.Is(runErr, ErrRemovalProcessCleanup) {
		return result, runErr
	}
	if mode == "--preflight" {
		return result, ErrTeardownPreflightFailed
	}
	return result, ErrTeardownResultInvalid
}

func (r ExactNPMRunner) processRunner() removalProcessRunner {
	if r.process != nil {
		return r.process
	}
	return boundedRemovalProcess{}
}

func verifyExactRemovalExecutable(path, expectedFingerprint string) error {
	resolved, fingerprint, err := VerifyExecutable(path)
	if err != nil || resolved != path || !isFingerprint(expectedFingerprint) ||
		subtle.ConstantTimeCompare([]byte(fingerprint), []byte(expectedFingerprint)) != 1 {
		return ErrRemovalCandidateChanged
	}
	return nil
}

func removalEnvironment(home string) ([]string, error) {
	if home == "" {
		resolved, err := os.UserHomeDir()
		if err != nil {
			return nil, ErrRemovalCommandFailed
		}
		home = resolved
	}
	home = filepath.Clean(home)
	if !filepath.IsAbs(home) {
		return nil, ErrRemovalCommandFailed
	}
	resolved, err := filepath.EvalSymlinks(home)
	if err != nil || filepath.Clean(resolved) != home {
		return nil, ErrRemovalCommandFailed
	}
	return []string{
		"HOME=" + home,
		"PATH=/usr/bin:/bin",
		"LANG=C",
		"LC_ALL=C",
		"NO_COLOR=1",
		"BUN_FEATURE_FLAG_NO_ORPHANS=1",
		"NPM_CONFIG_USERCONFIG=/dev/null",
		"NPM_CONFIG_GLOBALCONFIG=/dev/null",
		"NPM_CONFIG_IGNORE_SCRIPTS=true",
		"NPM_CONFIG_AUDIT=false",
		"NPM_CONFIG_FUND=false",
		"NPM_CONFIG_UPDATE_NOTIFIER=false",
		"NPM_CONFIG_OFFLINE=true",
	}, nil
}

type boundedRemovalProcess struct{}

type removalReadResult struct {
	payload []byte
	err     error
}

func (boundedRemovalProcess) Run(ctx context.Context, program string, args, environment []string, maximum int64) (RemovalExecutionResult, error) {
	if err := ctx.Err(); err != nil {
		return RemovalExecutionResult{}, err
	}
	if maximum <= 0 || program == "" || !filepath.IsAbs(program) {
		return RemovalExecutionResult{}, ErrRemovalCommandFailed
	}
	workingDirectory, err := os.MkdirTemp("", "opencodex-relay-removal-")
	if err != nil {
		return RemovalExecutionResult{}, ErrRemovalCommandFailed
	}
	defer os.RemoveAll(workingDirectory)
	if err := os.Chmod(workingDirectory, 0o700); err != nil {
		return RemovalExecutionResult{}, ErrRemovalCommandFailed
	}
	resolvedWorkingDirectory, err := filepath.EvalSymlinks(workingDirectory)
	if err != nil || !filepath.IsAbs(resolvedWorkingDirectory) {
		return RemovalExecutionResult{}, ErrRemovalCommandFailed
	}

	command := exec.Command(program, args...)
	command.Dir = resolvedWorkingDirectory
	command.Env = append([]string(nil), environment...)
	command.Stderr = io.Discard
	configureRemovalProcessGroup(command)
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		return RemovalExecutionResult{}, ErrRemovalCommandFailed
	}
	command.Stdout = stdoutWriter
	if err := ctx.Err(); err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return RemovalExecutionResult{}, err
	}
	if err := command.Start(); err != nil {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
		return RemovalExecutionResult{}, ErrRemovalCommandFailed
	}

	result := RemovalExecutionResult{Started: true}
	waitDone := make(chan error, 1)
	go func() { waitDone <- command.Wait() }()
	var waitErr error
	haveWait := false
	cleanupStartedProcess := func() bool {
		terminateRemovalProcessGroup(command)
		_ = stdoutReader.Close()
		if !haveWait {
			waitErr = <-waitDone
			haveWait = true
		}
		result.CleanupVerified = verifyRemovalProcessGroupTerminated(command)
		return result.CleanupVerified
	}
	if err := stdoutWriter.Close(); err != nil {
		if !cleanupStartedProcess() {
			return result, ErrRemovalProcessCleanup
		}
		return result, ErrRemovalCommandFailed
	}
	defer stdoutReader.Close()

	readDone := make(chan removalReadResult, 1)
	go func() {
		payload, readErr := io.ReadAll(io.LimitReader(stdoutReader, maximum+1))
		readDone <- removalReadResult{payload: payload, err: readErr}
	}()

	var read removalReadResult
	var outcomeErr error
	var drainTimer *time.Timer
	var drainDeadline <-chan time.Time
	haveRead := false
	descendantHeldStdout := false
	stopDrainTimer := func() {
		if drainTimer == nil {
			return
		}
		if !drainTimer.Stop() {
			select {
			case <-drainTimer.C:
			default:
			}
		}
		drainTimer = nil
		drainDeadline = nil
	}
	defer stopDrainTimer()

collection:
	for !haveRead || !haveWait {
		select {
		case read = <-readDone:
			haveRead = true
			stopDrainTimer()
			if read.err != nil || int64(len(read.payload)) > maximum {
				outcomeErr = ErrRemovalOutputInvalid
				break collection
			}
		case waitErr = <-waitDone:
			haveWait = true
			if !haveRead && drainTimer == nil {
				// A normal child that has closed stdout drains immediately. A
				// descendant retaining the descriptor gets one short scheduling
				// grace period before the dedicated process group is terminated.
				drainTimer = time.NewTimer(100 * time.Millisecond)
				drainDeadline = drainTimer.C
			}
		case <-drainDeadline:
			descendantHeldStdout = true
			drainDeadline = nil
			terminateRemovalProcessGroup(command)
			_ = stdoutReader.Close()
		case <-ctx.Done():
			outcomeErr = ctx.Err()
			break collection
		}
	}
	stopDrainTimer()
	if !cleanupStartedProcess() {
		return result, ErrRemovalProcessCleanup
	}
	if outcomeErr != nil {
		return result, outcomeErr
	}
	if descendantHeldStdout {
		return result, ErrRemovalOutputInvalid
	}
	result.Output = read.payload
	if waitErr == nil {
		result.ExitCode = 0
		return result, nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) && exitError.ExitCode() >= 0 {
		result.ExitCode = exitError.ExitCode()
		return result, nil
	}
	return result, ErrRemovalCommandFailed
}
