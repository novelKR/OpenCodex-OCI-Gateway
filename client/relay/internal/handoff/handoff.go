// Package handoff performs only user-confirmed OpenCodex lifecycle handoffs.
// It deliberately invokes the selected upstream owner command instead of
// deleting OpenCodex files itself.
package handoff

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

const (
	schemaVersion      = 1
	maxExecutableBytes = 128 << 20
	maxRecordBytes     = 16 << 10
)

type Action string

const (
	RetainProxyRemoveShim Action = "retain_proxy_remove_shim"
	RetainProxyKeepShim   Action = "retain_proxy_keep_shim"
)

var (
	ErrConfirmationRequired = errors.New("OpenCodex handoff confirmation is required")
	ErrUnsupportedPlatform  = errors.New("OpenCodex handoff is supported only on macOS")
	ErrInvalidAction        = errors.New("invalid OpenCodex handoff action")
	ErrUnsafeExecutable     = errors.New("selected OpenCodex executable is unsafe")
)

// Record contains no OpenCodex configuration or credentials. It binds future
// UI display to the exact executable the user confirmed, rather than PATH.
type Record struct {
	Schema      int    `json:"schema"`
	Executable  string `json:"executable"`
	Fingerprint string `json:"fingerprint"`
	Action      Action `json:"action"`
}

// Runner is injected for tests. Implementations must not retain command
// stdout/stderr because a MenuBar caller may display only bounded safe errors.
type Runner interface {
	Run(context.Context, string, ...string) error
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, program string, args ...string) error {
	command := exec.CommandContext(ctx, program, args...)
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	return command.Run()
}

// verifiedRunner rechecks the selected executable immediately before every
// approved OCX invocation. RetainProxyRemoveShim intentionally makes two
// calls, so one initial hash is not sufficient for the whole sequence.
type verifiedRunner struct {
	delegate Runner
	verify   func() error
}

func (r verifiedRunner) Run(ctx context.Context, program string, args ...string) error {
	if r.delegate == nil || r.verify == nil {
		return ErrUnsafeExecutable
	}
	if err := r.verify(); err != nil {
		return err
	}
	return r.delegate.Run(ctx, program, args...)
}

// Executor carries no ambient PATH lookup. The user-selected executable is
// canonicalized and fingerprinted before it is invoked.
type Executor struct {
	Runner   Runner
	Platform string
}

func (e Executor) Execute(ctx context.Context, executable string, action Action, confirmed bool) (Record, error) {
	return e.ExecuteExpected(ctx, executable, "", action, confirmed)
}

// ExecuteExpected binds an app-selected SHA-256 to the regular executable
// revalidated immediately before an approved lifecycle action. An empty
// expected fingerprint is retained only for narrow unit/offline callers; the
// public MenuBar command requires it.
func (e Executor) ExecuteExpected(ctx context.Context, executable, expectedFingerprint string, action Action, confirmed bool) (Record, error) {
	if !confirmed {
		return Record{}, ErrConfirmationRequired
	}
	if platform := e.Platform; platform != "" && platform != "darwin" {
		return Record{}, ErrUnsupportedPlatform
	}
	if e.Platform == "" && runtime.GOOS != "darwin" {
		return Record{}, ErrUnsupportedPlatform
	}
	if !validAction(action) {
		return Record{}, ErrInvalidAction
	}
	if expectedFingerprint != "" && !isFingerprint(expectedFingerprint) {
		return Record{}, ErrUnsafeExecutable
	}
	resolved, fingerprint, file, err := openVerifiedExecutable(executable)
	if err != nil {
		return Record{}, err
	}
	defer file.Close()
	if expectedFingerprint != "" && subtle.ConstantTimeCompare([]byte(expectedFingerprint), []byte(fingerprint)) != 1 {
		return Record{}, ErrUnsafeExecutable
	}
	runner := e.Runner
	if runner == nil {
		runner = commandRunner{}
	}
	// exec ultimately consumes a pathname rather than this already-open file.
	// Keep the descriptor and rebind it to the pathname before every command.
	// The parent chain is owner-safe, preventing a different local user from
	// swapping the target between this check and exec.
	runner = verifiedRunner{
		delegate: runner,
		verify: func() error {
			return verifyExecutableStillBound(resolved, file, fingerprint)
		},
	}
	err = runAction(ctx, runner, resolved, action)
	if err != nil {
		return Record{}, fmt.Errorf("run approved OpenCodex handoff: %w", err)
	}
	return Record{Schema: schemaVersion, Executable: resolved, Fingerprint: fingerprint, Action: action}, nil
}

func runAction(ctx context.Context, runner Runner, executable string, action Action) error {
	switch action {
	case RetainProxyRemoveShim:
		if err := runner.Run(ctx, executable, "restore"); err != nil {
			return err
		}
		return runner.Run(ctx, executable, "codex-shim", "uninstall")
	case RetainProxyKeepShim:
		return runner.Run(ctx, executable, "restore")
	default:
		return ErrInvalidAction
	}
}

func validAction(action Action) bool {
	return action == RetainProxyRemoveShim || action == RetainProxyKeepShim
}

// VerifyExecutable resolves a user-selected path once. A symlink is allowed
// only as an input convenience; its resolved regular executable is what is
// fingerprinted and later recorded.
func VerifyExecutable(path string) (string, string, error) {
	resolved, fingerprint, file, err := openVerifiedExecutable(path)
	if file != nil {
		_ = file.Close()
	}
	return resolved, fingerprint, err
}

func openVerifiedExecutable(path string) (string, string, *os.File, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", nil, ErrUnsafeExecutable
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", "", nil, ErrUnsafeExecutable
	}
	if err := validateExecutableParentChain(filepath.Dir(resolved)); err != nil {
		return "", "", nil, ErrUnsafeExecutable
	}
	pathInfo, err := os.Lstat(resolved)
	if err != nil || !safeExecutableInfo(pathInfo) {
		return "", "", nil, ErrUnsafeExecutable
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", "", nil, ErrUnsafeExecutable
	}
	info, err := file.Stat()
	if err != nil || !safeExecutableInfo(info) || !os.SameFile(pathInfo, info) {
		_ = file.Close()
		return "", "", nil, ErrUnsafeExecutable
	}
	fingerprint, err := fingerprintOpenExecutable(file)
	if err != nil {
		_ = file.Close()
		return "", "", nil, ErrUnsafeExecutable
	}
	return resolved, fingerprint, file, nil
}

func verifyExecutableStillBound(resolved string, file *os.File, expectedFingerprint string) error {
	if err := validateExecutableParentChain(filepath.Dir(resolved)); err != nil {
		return ErrUnsafeExecutable
	}
	pathInfo, err := os.Lstat(resolved)
	if err != nil || !safeExecutableInfo(pathInfo) {
		return ErrUnsafeExecutable
	}
	openedInfo, err := file.Stat()
	if err != nil || !safeExecutableInfo(openedInfo) || !os.SameFile(pathInfo, openedInfo) {
		return ErrUnsafeExecutable
	}
	fingerprint, err := fingerprintOpenExecutable(file)
	if err != nil || subtle.ConstantTimeCompare([]byte(fingerprint), []byte(expectedFingerprint)) != 1 {
		return ErrUnsafeExecutable
	}
	return nil
}

func fingerprintOpenExecutable(file *os.File) (string, error) {
	if file == nil {
		return "", ErrUnsafeExecutable
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	read, err := io.Copy(hash, io.LimitReader(file, maxExecutableBytes+1))
	if err != nil || read > maxExecutableBytes {
		return "", ErrUnsafeExecutable
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func safeExecutableInfo(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	return ownedByCurrentUserOrRoot(info)
}

func validateExecutableParentChain(directory string) error {
	for {
		info, err := os.Lstat(directory)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || !safeExecutableDirectory(info) {
			return ErrUnsafeExecutable
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return nil
		}
		directory = parent
	}
}

func safeExecutableDirectory(info os.FileInfo) bool {
	if !ownedByCurrentUserOrRoot(info) {
		return false
	}
	permissions := info.Mode().Perm()
	if permissions&0o022 == 0 {
		return true
	}
	// A root-owned sticky directory (notably /tmp) is safe for a child held in
	// an owner-only directory: unrelated users cannot replace the child name.
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0 && info.Mode()&os.ModeSticky != 0
}

func EnrollmentPath(relayConfigPath string) string {
	return filepath.Clean(relayConfigPath) + ".local-opencodex-enrollment.json"
}

func WriteRecord(relayConfigPath string, record Record) error {
	if err := validateRecord(record); err != nil {
		return err
	}
	path := EnrollmentPath(relayConfigPath)
	if err := validateExisting(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create enrollment directory: %w", err)
	}
	payload, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return fmt.Errorf("encode enrollment record: %w", err)
	}
	payload = append(payload, '\n')
	temporary, err := os.CreateTemp(filepath.Dir(path), ".local-opencodex-enrollment.")
	if err != nil {
		return fmt.Errorf("create enrollment record: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("protect enrollment record: %w", err)
	}
	if _, err := temporary.Write(payload); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write enrollment record: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync enrollment record: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close enrollment record: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("publish enrollment record: %w", err)
	}
	if err := syncControlDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync enrollment directory: %w", err)
	}
	return nil
}

// PreflightRecordWrite proves the local receipt path is owner-safe before an
// irreversible OpenCodex lifecycle command is launched. It deliberately does
// not create a file or directory; the normal write remains the transaction's
// only mutation.
func PreflightRecordWrite(relayConfigPath string) error {
	path := EnrollmentPath(relayConfigPath)
	if err := validateExisting(path); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	info, err := os.Lstat(directory)
	// Directory readability does not weaken a 0600 receipt; writability by a
	// different principal would. Permit the normal macOS user-directory 0755
	// shape while rejecting symlinked, foreign, or group/world-writable roots.
	if err != nil || !info.IsDir() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o022 != 0 {
		return ErrUnsafeExecutable
	}
	return nil
}

// PreflightRelayConfig rejects a symlinked, foreign-owned, broad-permission,
// or missing relay config before a handoff can invoke OCX. It is intentionally
// narrow: parsing/semantic validation remains owned by config.Load.
func PreflightRelayConfig(relayConfigPath string) error {
	info, err := os.Lstat(relayConfigPath)
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm() != 0o600 {
		return ErrUnsafeExecutable
	}
	return nil
}

func ReadRecord(relayConfigPath string) (Record, error) {
	path := EnrollmentPath(relayConfigPath)
	if err := validateExisting(path); err != nil {
		return Record{}, err
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	if len(payload) == 0 || len(payload) > maxRecordBytes {
		return Record{}, ErrUnsafeExecutable
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, ErrUnsafeExecutable
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Record{}, ErrUnsafeExecutable
	}
	if err := validateRecord(record); err != nil {
		return Record{}, err
	}
	resolved, fingerprint, err := VerifyExecutable(record.Executable)
	if err != nil || resolved != record.Executable || fingerprint != record.Fingerprint {
		return Record{}, ErrUnsafeExecutable
	}
	return record, nil
}

// RemoveRecord removes only an owner-safe enrollment receipt after the user
// explicitly chooses OpenCodex uninstall. It never removes the selected OCX
// executable or any OpenCodex-owned files.
func RemoveRecord(relayConfigPath string) error {
	path := EnrollmentPath(relayConfigPath)
	if err := validateExisting(path); err != nil {
		return err
	}
	if err := os.Remove(path); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return fmt.Errorf("remove enrollment record: %w", err)
	}
	if err := syncControlDirectory(filepath.Dir(path)); err != nil {
		return fmt.Errorf("sync enrollment directory: %w", err)
	}
	return nil
}

func validateRecord(record Record) error {
	if record.Schema != schemaVersion || !validAction(record.Action) || len(record.Fingerprint) != sha256.Size*2 {
		return ErrUnsafeExecutable
	}
	if _, _, err := VerifyExecutable(record.Executable); err != nil {
		return err
	}
	return nil
}

func validateExisting(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) || info.Mode().Perm()&0o077 != 0 {
		return ErrUnsafeExecutable
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && int(stat.Uid) == os.Getuid()
}

func ownedByCurrentUserOrRoot(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && (int(stat.Uid) == os.Geteuid() || stat.Uid == 0)
}

func isFingerprint(value string) bool {
	return len(value) == sha256.Size*2 && strings.IndexFunc(value, func(character rune) bool {
		return !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f')
	}) == -1
}
