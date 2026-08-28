package handoff

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type nativeRestoreExecutionSnapshot struct {
	root          string
	packageRoot   string
	bun           string
	cliEntry      string
	bunConfigPath string
}

type teardownAdapterExecutionSnapshot struct {
	nativeRestoreExecutionSnapshot
	adapter string
}

func (s nativeRestoreExecutionSnapshot) Close() {
	if s.root != "" {
		_ = os.RemoveAll(s.root)
	}
}

// prepareNativeRestoreExecutionSnapshot converts a discovery-time restore
// proof into private immutable execution inputs. Unlike automatic removal, it
// copies only the OpenCodex package closure and invokes its bundled Bun
// directly; the env-node launcher and package-manager runtime are not part of
// the execution path.
func prepareNativeRestoreExecutionSnapshot(ctx context.Context, candidate NPMInstallation) (nativeRestoreExecutionSnapshot, error) {
	return preparePackageExecutionSnapshot(ctx, candidate, false)
}

func prepareTeardownAdapterExecutionSnapshot(
	ctx context.Context,
	candidate NPMInstallation,
) (teardownAdapterExecutionSnapshot, error) {
	if candidate.TeardownCapability != TeardownCapabilityRelayPreserveV1 ||
		!reviewedDataCapability(candidate.DataCapability) ||
		candidate.TeardownCompatibility != teardownCompatibilityCompatible ||
		candidate.TeardownAdapterID == "" || candidate.teardownProof == nil ||
		!candidate.teardownProof.valid() || candidate.teardownProof.adapterID != candidate.TeardownAdapterID {
		return teardownAdapterExecutionSnapshot{}, ErrTeardownUnsupported
	}
	profile, adapterImplementation, ok := teardownProfileForCandidate(
		candidate.TeardownAdapterID,
		candidate.teardownProof.artifactVariant,
		OpenCodexPackageName,
		candidate.Version,
	)
	if !ok {
		return teardownAdapterExecutionSnapshot{}, ErrTeardownUnsupported
	}
	snapshot, err := prepareNativeRestoreExecutionSnapshot(ctx, candidate)
	if err != nil {
		if errors.Is(err, ErrNativeRestoreCandidateChanged) || errors.Is(err, ErrRemovalCandidateChanged) {
			return teardownAdapterExecutionSnapshot{}, ErrTeardownCandidateChanged
		}
		return teardownAdapterExecutionSnapshot{}, err
	}
	fail := func(err error) (teardownAdapterExecutionSnapshot, error) {
		snapshot.Close()
		return teardownAdapterExecutionSnapshot{}, err
	}
	fingerprint, result := verifyTeardownProfile(ctx, snapshot.packageRoot, profile)
	if result != teardownCompatibilityCompatible ||
		!constantStringEqual(fingerprint, candidate.teardownProof.profileFingerprint) {
		return fail(ErrTeardownCandidateChanged)
	}
	for name, source := range adapterImplementation.sources {
		adapterPath := filepath.Join(snapshot.root, name)
		file, err := os.OpenFile(adapterPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return fail(ErrTeardownPreflightFailed)
		}
		written, writeErr := file.Write(source)
		closeErr := file.Close()
		if writeErr != nil || closeErr != nil || written != len(source) {
			return fail(ErrTeardownPreflightFailed)
		}
	}
	adapterPath := filepath.Join(snapshot.root, adapterImplementation.entrypoint)
	return teardownAdapterExecutionSnapshot{
		nativeRestoreExecutionSnapshot: snapshot,
		adapter:                        adapterPath,
	}, nil
}

// prepareGuardedInventoryExecutionSnapshot permits only the read-only inventory
// operation before the privileged Homebrew guard is active. All mutation paths
// continue to require the strict removal snapshot.
func prepareGuardedInventoryExecutionSnapshot(ctx context.Context, candidate NPMInstallation) (nativeRestoreExecutionSnapshot, error) {
	if candidate.RemovalCapability != RemovalCapabilityHomebrewGuardedNPM || !candidate.HomebrewGuardRequired {
		return nativeRestoreExecutionSnapshot{}, ErrRemovalManualOnly
	}
	if err := validateAutomaticRemovalCandidateStaticContext(ctx, candidate, os.Geteuid()); err != nil {
		return nativeRestoreExecutionSnapshot{}, err
	}
	return preparePackageExecutionSnapshot(ctx, candidate, true)
}

func preparePackageExecutionSnapshot(ctx context.Context, candidate NPMInstallation, allowRelaxed bool) (nativeRestoreExecutionSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nativeRestoreExecutionSnapshot{}, err
	}
	if candidate.NativeRestoreCapability != NativeRestoreCapabilityVerifiedSnapshot ||
		candidate.nativeRestoreProof == nil || !candidate.nativeRestoreProof.valid() ||
		!constantStringEqual(candidate.nativeRestoreProof.fingerprint(candidate.Fingerprint), candidate.NativeRestoreFingerprint) {
		return nativeRestoreExecutionSnapshot{}, ErrNativeRestoreProofUnavailable
	}
	if err := verifyNativeRestoreExecutionProofPolicyContext(ctx, candidate, allowRelaxed); err != nil {
		return nativeRestoreExecutionSnapshot{}, err
	}

	temporaryRoot, err := os.MkdirTemp("", "opencodex-relay-native-restore-")
	if err != nil {
		return nativeRestoreExecutionSnapshot{}, ErrNativeRestoreProofUnavailable
	}
	cleanup := func() { _ = os.RemoveAll(temporaryRoot) }
	if err := os.Chmod(temporaryRoot, 0o700); err != nil {
		cleanup()
		return nativeRestoreExecutionSnapshot{}, ErrNativeRestoreProofUnavailable
	}
	resolvedRoot, err := filepath.EvalSymlinks(temporaryRoot)
	if err != nil || !filepath.IsAbs(resolvedRoot) {
		cleanup()
		return nativeRestoreExecutionSnapshot{}, ErrNativeRestoreProofUnavailable
	}
	snapshot := nativeRestoreExecutionSnapshot{
		root:          resolvedRoot,
		packageRoot:   filepath.Join(resolvedRoot, "package"),
		bunConfigPath: filepath.Join(resolvedRoot, "bunfig.toml"),
	}
	fail := func(err error) (nativeRestoreExecutionSnapshot, error) {
		snapshot.Close()
		return nativeRestoreExecutionSnapshot{}, err
	}
	if err := copyRemovalExecutionTree(ctx, candidate.PackageRoot, snapshot.packageRoot); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(ctxErr)
		}
		return fail(ErrNativeRestoreCandidateChanged)
	}
	packageFingerprint, err := stableExecutionTreeFingerprintContext(ctx, snapshot.packageRoot)
	if err != nil || !constantStringEqual(packageFingerprint, candidate.nativeRestoreProof.packageTreeSHA256) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(ctxErr)
		}
		return fail(ErrNativeRestoreCandidateChanged)
	}
	bunRelative, ok := containedRelativePath(candidate.PackageRoot, candidate.nativeRestoreProof.bunExecutable)
	if !ok {
		return fail(ErrNativeRestoreCandidateChanged)
	}
	cliRelative, ok := containedRelativePath(candidate.PackageRoot, candidate.nativeRestoreProof.cliEntry)
	if !ok {
		return fail(ErrNativeRestoreCandidateChanged)
	}
	snapshot.bun = filepath.Join(snapshot.packageRoot, bunRelative)
	snapshot.cliEntry = filepath.Join(snapshot.packageRoot, cliRelative)
	if err := verifyNativeRestoreExecutable(snapshot.bun, candidate.nativeRestoreProof.bunSHA256); err != nil {
		return fail(err)
	}
	cliPayload, _, _, err := readDiscoveryRegularFile(snapshot.cliEntry, maxExecutableBytes)
	if err != nil {
		return fail(ErrNativeRestoreCandidateChanged)
	}
	cliDigest := sha256.Sum256(cliPayload)
	if !constantStringEqual(hex.EncodeToString(cliDigest[:]), candidate.nativeRestoreProof.cliEntrySHA256) {
		return fail(ErrNativeRestoreCandidateChanged)
	}
	if err := os.WriteFile(snapshot.bunConfigPath, []byte("env = false\n[install]\nauto = \"disable\"\n"), 0o600); err != nil {
		return fail(ErrNativeRestoreProofUnavailable)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return snapshot, nil
}

func verifyNativeRestoreExecutionProofContext(ctx context.Context, candidate NPMInstallation) error {
	return verifyNativeRestoreExecutionProofPolicyContext(ctx, candidate, false)
}

func verifyNativeRestoreExecutionProofPolicyContext(ctx context.Context, candidate NPMInstallation, allowRelaxed bool) error {
	proof := candidate.nativeRestoreProof
	if proof == nil || !proof.valid() {
		return ErrNativeRestoreProofUnavailable
	}
	if err := verifyNativeRestoreExecutablePolicy(proof.bunExecutable, proof.bunSHA256, allowRelaxed); err != nil {
		return err
	}
	cliPayload, cliInfo, _, err := readDiscoveryRegularFile(proof.cliEntry, maxExecutableBytes)
	if err != nil || !ownedByEffectiveUser(cliInfo) {
		return ErrNativeRestoreCandidateChanged
	}
	cliDigest := sha256.Sum256(cliPayload)
	if !constantStringEqual(hex.EncodeToString(cliDigest[:]), proof.cliEntrySHA256) {
		return ErrNativeRestoreCandidateChanged
	}
	fingerprintTree := stableExecutionTreeFingerprintContext
	if candidate.RemovalCapability == RemovalCapabilityHomebrewGuardedNPM {
		fingerprintTree = stableExecutionTreeFingerprintWithoutExtendedACLContext
	}
	packageFingerprint, err := fingerprintTree(ctx, candidate.PackageRoot)
	if err != nil || !constantStringEqual(packageFingerprint, proof.packageTreeSHA256) {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrNativeRestoreCandidateChanged
	}
	return nil
}

func verifyNativeRestoreExecutable(path, expectedFingerprint string) error {
	return verifyNativeRestoreExecutablePolicy(path, expectedFingerprint, false)
}

func verifyNativeRestoreExecutablePolicy(path, expectedFingerprint string, allowRelaxed bool) error {
	resolved, fingerprint, err := VerifyExecutable(path)
	if allowRelaxed {
		resolved, fingerprint, _, err = verifyDiscoveryExecutable(path)
	}
	if err != nil || resolved != path || !isFingerprint(expectedFingerprint) ||
		!constantStringEqual(fingerprint, expectedFingerprint) {
		return ErrNativeRestoreCandidateChanged
	}
	return nil
}

type removalExecutionSnapshot struct {
	root          string
	packageRoot   string
	npmRoot       string
	node          string
	bun           string
	cliEntry      string
	npmCLI        string
	bunConfigPath string
}

func (s removalExecutionSnapshot) Close() {
	if s.root != "" {
		_ = os.RemoveAll(s.root)
	}
}

// prepareRemovalExecutionSnapshot converts the observational discovery proof
// into private immutable execution inputs. The original prefix remains only the
// npm uninstall target; no verified program or module is reopened there.
func prepareRemovalExecutionSnapshot(ctx context.Context, candidate NPMInstallation) (removalExecutionSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return removalExecutionSnapshot{}, err
	}
	if err := verifyExactRemovalExecutable(candidate.NodeExecutable, candidate.NodeSHA256); err != nil {
		return removalExecutionSnapshot{}, err
	}
	if err := verifyExactRemovalExecutable(candidate.BunExecutable, candidate.BunSHA256); err != nil {
		return removalExecutionSnapshot{}, err
	}
	if err := verifyExactRemovalExecutable(candidate.NPMCLI, candidate.NPMCLISHA256); err != nil {
		return removalExecutionSnapshot{}, err
	}
	if err := verifyRemovalExecutionClosureContext(ctx, candidate); err != nil {
		return removalExecutionSnapshot{}, err
	}
	npmRoot, ok := npmRootFromCLI(candidate.NPMCLI)
	if !ok {
		return removalExecutionSnapshot{}, ErrRemovalCandidateChanged
	}

	temporaryRoot, err := os.MkdirTemp("", "opencodex-relay-execution-")
	if err != nil {
		return removalExecutionSnapshot{}, ErrRemovalCommandFailed
	}
	cleanup := func() { _ = os.RemoveAll(temporaryRoot) }
	if err := os.Chmod(temporaryRoot, 0o700); err != nil {
		cleanup()
		return removalExecutionSnapshot{}, ErrRemovalCommandFailed
	}
	resolvedRoot, err := filepath.EvalSymlinks(temporaryRoot)
	if err != nil || !filepath.IsAbs(resolvedRoot) {
		cleanup()
		return removalExecutionSnapshot{}, ErrRemovalCommandFailed
	}
	snapshot := removalExecutionSnapshot{
		root:          resolvedRoot,
		packageRoot:   filepath.Join(resolvedRoot, "package"),
		npmRoot:       filepath.Join(resolvedRoot, "npm"),
		node:          filepath.Join(resolvedRoot, "node"),
		bunConfigPath: filepath.Join(resolvedRoot, "bunfig.toml"),
	}
	fail := func(err error) (removalExecutionSnapshot, error) {
		snapshot.Close()
		return removalExecutionSnapshot{}, err
	}
	if err := copyRemovalExecutionTree(ctx, candidate.PackageRoot, snapshot.packageRoot); err != nil {
		return fail(err)
	}
	if err := copyRemovalExecutionTree(ctx, npmRoot, snapshot.npmRoot); err != nil {
		return fail(err)
	}
	if err := copyRemovalExecutable(ctx, candidate.NodeExecutable, snapshot.node, candidate.NodeSHA256); err != nil {
		return fail(err)
	}
	packageFingerprint, err := stableExecutionTreeFingerprintContext(ctx, snapshot.packageRoot)
	if err != nil || subtle.ConstantTimeCompare([]byte(packageFingerprint), []byte(candidate.PackageTreeSHA256)) != 1 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(ctxErr)
		}
		return fail(ErrRemovalCandidateChanged)
	}
	npmFingerprint, err := stableExecutionTreeFingerprintContext(ctx, snapshot.npmRoot)
	if err != nil || subtle.ConstantTimeCompare([]byte(npmFingerprint), []byte(candidate.NPMTreeSHA256)) != 1 {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return fail(ctxErr)
		}
		return fail(ErrRemovalCandidateChanged)
	}

	bunRelative, ok := containedRelativePath(candidate.PackageRoot, candidate.BunExecutable)
	if !ok {
		return fail(ErrRemovalCandidateChanged)
	}
	cliRelative, ok := containedRelativePath(candidate.PackageRoot, candidate.CLIEntry)
	if !ok {
		return fail(ErrRemovalCandidateChanged)
	}
	npmRelative, ok := containedRelativePath(npmRoot, candidate.NPMCLI)
	if !ok {
		return fail(ErrRemovalCandidateChanged)
	}
	snapshot.bun = filepath.Join(snapshot.packageRoot, bunRelative)
	snapshot.cliEntry = filepath.Join(snapshot.packageRoot, cliRelative)
	snapshot.npmCLI = filepath.Join(snapshot.npmRoot, npmRelative)
	if err := verifyExactRemovalExecutable(snapshot.bun, candidate.BunSHA256); err != nil {
		return fail(err)
	}
	if err := verifyExactRemovalExecutable(snapshot.npmCLI, candidate.NPMCLISHA256); err != nil {
		return fail(err)
	}
	if err := os.WriteFile(snapshot.bunConfigPath, []byte("env = false\n[install]\nauto = \"disable\"\n"), 0o600); err != nil {
		return fail(ErrRemovalCommandFailed)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	return snapshot, nil
}

func copyRemovalExecutionTree(ctx context.Context, sourceRoot, destinationRoot string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if sourceRoot == "" || !filepath.IsAbs(sourceRoot) || filepath.Clean(sourceRoot) != sourceRoot ||
		destinationRoot == "" || !filepath.IsAbs(destinationRoot) || filepath.Clean(destinationRoot) != destinationRoot {
		return ErrRemovalCandidateChanged
	}
	resolvedSource, err := filepath.EvalSymlinks(sourceRoot)
	if err != nil || resolvedSource != sourceRoot {
		return ErrRemovalCandidateChanged
	}
	rootInfo, err := os.Lstat(sourceRoot)
	if err != nil || !safeExecutionDirectory(rootInfo) || !ownedByEffectiveUser(rootInfo) {
		return ErrRemovalCandidateChanged
	}
	if err := os.Mkdir(destinationRoot, rootInfo.Mode().Perm()); err != nil {
		return ErrRemovalCommandFailed
	}
	if err := os.Chmod(destinationRoot, rootInfo.Mode().Perm()); err != nil {
		return ErrRemovalCommandFailed
	}

	entries := 0
	var totalBytes int64
	err = filepath.WalkDir(sourceRoot, func(sourcePath string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return ErrRemovalCandidateChanged
		}
		entries++
		if entries > maxExecutionTreeEntries {
			return ErrRemovalCandidateChanged
		}
		relative, err := filepath.Rel(sourceRoot, sourcePath)
		if err != nil || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return ErrRemovalCandidateChanged
		}
		info, err := os.Lstat(sourcePath)
		if err != nil || !ownedByEffectiveUser(info) {
			return ErrRemovalCandidateChanged
		}
		if relative == "." {
			if !safeExecutionDirectory(info) || info.Mode().Perm() != rootInfo.Mode().Perm() {
				return ErrRemovalCandidateChanged
			}
			return nil
		}
		destinationPath := filepath.Join(destinationRoot, relative)
		switch {
		case info.IsDir():
			if !safeExecutionDirectory(info) {
				return ErrRemovalCandidateChanged
			}
			if err := os.Mkdir(destinationPath, info.Mode().Perm()); err != nil {
				return ErrRemovalCommandFailed
			}
			return os.Chmod(destinationPath, info.Mode().Perm())
		case info.Mode().IsRegular():
			if !safeExecutionRegular(info) || info.Size() < 0 || info.Size() > maxExecutionFileBytes || totalBytes > maxExecutionTreeBytes-info.Size() {
				return ErrRemovalCandidateChanged
			}
			totalBytes += info.Size()
			return copyRemovalRegularFile(ctx, sourcePath, destinationPath, info)
		case info.Mode()&os.ModeSymlink != 0:
			resolvedTarget, err := filepath.EvalSymlinks(sourcePath)
			if err != nil || !pathContainedBy(sourceRoot, resolvedTarget) {
				return ErrRemovalCandidateChanged
			}
			targetRelative, ok := containedRelativePath(sourceRoot, resolvedTarget)
			if !ok {
				return ErrRemovalCandidateChanged
			}
			destinationTarget := filepath.Join(destinationRoot, targetRelative)
			linkTarget, err := filepath.Rel(filepath.Dir(destinationPath), destinationTarget)
			if err != nil || filepath.IsAbs(linkTarget) {
				return ErrRemovalCandidateChanged
			}
			if err := os.Symlink(linkTarget, destinationPath); err != nil {
				return ErrRemovalCommandFailed
			}
			return nil
		default:
			return ErrRemovalCandidateChanged
		}
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		if errors.Is(err, ErrRemovalCommandFailed) {
			return ErrRemovalCommandFailed
		}
		return ErrRemovalCandidateChanged
	}
	return nil
}

func copyRemovalRegularFile(ctx context.Context, sourcePath, destinationPath string, expected os.FileInfo) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		return ErrRemovalCandidateChanged
	}
	openedInfo, statErr := source.Stat()
	if statErr != nil || !safeExecutionRegular(openedInfo) || !ownedByEffectiveUser(openedInfo) || !os.SameFile(expected, openedInfo) {
		_ = source.Close()
		return ErrRemovalCandidateChanged
	}
	destination, err := os.OpenFile(destinationPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, expected.Mode().Perm())
	if err != nil {
		_ = source.Close()
		return ErrRemovalCommandFailed
	}
	if err := destination.Chmod(expected.Mode().Perm()); err != nil {
		_ = source.Close()
		_ = destination.Close()
		return ErrRemovalCommandFailed
	}
	copied, copyErr := copyWithContext(ctx, destination, source, maxExecutionFileBytes+1)
	sourceCloseErr := source.Close()
	destinationCloseErr := destination.Close()
	if copyErr != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrRemovalCandidateChanged
	}
	if sourceCloseErr != nil || destinationCloseErr != nil || copied != expected.Size() {
		return ErrRemovalCandidateChanged
	}
	return nil
}

func copyRemovalExecutable(ctx context.Context, sourcePath, destinationPath, expectedFingerprint string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	info, err := os.Lstat(sourcePath)
	if err != nil || !safeExecutionRegular(info) || !ownedByEffectiveUser(info) || info.Size() < 0 || info.Size() > maxExecutionFileBytes {
		return ErrRemovalCandidateChanged
	}
	if err := copyRemovalRegularFile(ctx, sourcePath, destinationPath, info); err != nil {
		return err
	}
	resolved, fingerprint, err := VerifyExecutable(destinationPath)
	if err != nil || resolved != destinationPath || subtle.ConstantTimeCompare([]byte(fingerprint), []byte(expectedFingerprint)) != 1 {
		return ErrRemovalCandidateChanged
	}
	return nil
}

func containedRelativePath(root, target string) (string, bool) {
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	if filepath.Clean(relative) != relative {
		return "", false
	}
	return relative, true
}

func ownedByEffectiveUser(info os.FileInfo) bool {
	uid, ok := ownerUID(info)
	return ok && int(uid) == os.Geteuid()
}
