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
)

var (
	ErrInvalidNativeRestoreSelection = errors.New("invalid OpenCodex native restore selection")
	ErrNativeRestoreCandidateMissing = errors.New("selected OpenCodex native restore installation is no longer discoverable")
	ErrNativeRestoreCandidateChanged = errors.New("selected OpenCodex native restore installation changed")
	ErrNativeRestoreProofUnavailable = errors.New("selected OpenCodex installation has no verified native restore closure")
)

// NativeRestoreSelection carries only the opaque discovery witnesses and the
// already-displayed launcher identity. Runtime paths remain inside the helper
// and are rediscovered from bounded Tier A/B roots before every owner action.
type NativeRestoreSelection struct {
	InstallationID           string
	InstallationFingerprint  string
	NativeRestoreFingerprint string
	Executable               string
	ExecutableSHA256         string
}

// nativeRestoreExecutionProof is intentionally excluded from discovery JSON.
// The UI receives only NativeRestoreFingerprint; the helper rediscovers these
// exact execution inputs instead of accepting paths or runtime hashes from its
// caller.
type nativeRestoreExecutionProof struct {
	cliEntry          string
	cliEntrySHA256    string
	bunExecutable     string
	bunSHA256         string
	packageTreeSHA256 string
}

func discoverNativeRestoreExecutionProof(
	ctx context.Context,
	packageRoot string,
	manifestUID uint32,
) (*nativeRestoreExecutionProof, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The snapshot copier deliberately accepts only the effective user's files.
	// Root-owned or foreign-owner packages remain inspectable but cannot become
	// local-development mutation authority.
	if int(manifestUID) != os.Geteuid() {
		return nil, ErrNativeRestoreProofUnavailable
	}
	cliEntry := filepath.Join(packageRoot, "src", "cli", "index.ts")
	cliBytes, cliInfo, _, err := readDiscoveryRegularFile(cliEntry, maxExecutableBytes)
	if err != nil {
		return nil, ErrNativeRestoreProofUnavailable
	}
	cliUID, ok := ownerUID(cliInfo)
	if !ok || cliUID != manifestUID {
		return nil, ErrNativeRestoreProofUnavailable
	}
	bunExecutable, bunSHA256, ok := discoverBundledBunForNativeRestore(packageRoot, manifestUID)
	if !ok {
		return nil, ErrNativeRestoreProofUnavailable
	}
	packageTreeSHA256, err := stableExecutionTreeFingerprintContext(ctx, packageRoot)
	if err != nil {
		return nil, ErrNativeRestoreProofUnavailable
	}
	if err := validateNativeRestoreTreeOwnership(ctx, packageRoot); err != nil {
		return nil, ErrNativeRestoreProofUnavailable
	}
	cliDigest := sha256.Sum256(cliBytes)
	return &nativeRestoreExecutionProof{
		cliEntry:          cliEntry,
		cliEntrySHA256:    hex.EncodeToString(cliDigest[:]),
		bunExecutable:     bunExecutable,
		bunSHA256:         bunSHA256,
		packageTreeSHA256: packageTreeSHA256,
	}, nil
}

func validateNativeRestoreTreeOwnership(ctx context.Context, packageRoot string) error {
	entries := 0
	return filepath.WalkDir(packageRoot, func(path string, _ fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			return ErrNativeRestoreProofUnavailable
		}
		entries++
		if entries > maxExecutionTreeEntries {
			return ErrNativeRestoreProofUnavailable
		}
		info, err := os.Lstat(path)
		if err != nil || !ownedByEffectiveUser(info) {
			return ErrNativeRestoreProofUnavailable
		}
		return nil
	})
}

func discoverBundledBunForNativeRestore(packageRoot string, manifestUID uint32) (string, string, bool) {
	for _, name := range []string{"bun.exe", "bun"} {
		candidate := filepath.Join(packageRoot, "node_modules", "bun", "bin", name)
		resolved, fingerprint, _, err := verifyDiscoveryExecutable(candidate)
		if err != nil || !pathContainedBy(packageRoot, resolved) {
			continue
		}
		info, err := os.Stat(resolved)
		uid, ok := ownerUID(info)
		if err == nil && ok && uid == manifestUID {
			return resolved, fingerprint, true
		}
	}
	return "", "", false
}

func (p *nativeRestoreExecutionProof) fingerprint(installationFingerprint string) string {
	if p == nil || !isFingerprint(installationFingerprint) || !p.valid() {
		return ""
	}
	hash := sha256.New()
	for _, value := range []string{
		"opencodex-native-restore-proof-v1",
		installationFingerprint,
		p.cliEntry,
		p.cliEntrySHA256,
		p.bunExecutable,
		p.bunSHA256,
		p.packageTreeSHA256,
	} {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func (p *nativeRestoreExecutionProof) valid() bool {
	return p != nil && filepath.IsAbs(p.cliEntry) && filepath.Clean(p.cliEntry) == p.cliEntry &&
		filepath.IsAbs(p.bunExecutable) && filepath.Clean(p.bunExecutable) == p.bunExecutable &&
		isFingerprint(p.cliEntrySHA256) && isFingerprint(p.bunSHA256) && isFingerprint(p.packageTreeSHA256)
}

// DiscoveryNativeRestoreResolver is separate from removal authority. It
// accepts manual package-removal candidates only when their package runtime can
// be pinned into a verified private snapshot.
type DiscoveryNativeRestoreResolver struct {
	Options DiscoveryOptions
}

func SanitizedNativeRestoreDiscoveryOptions(options DiscoveryOptions) DiscoveryOptions {
	options.Tier = DiscoveryTierB
	options.BroadScanApproved = false
	options.BroadRoots = nil
	options.PathEnv = ""
	options.Getenv = func(string) string { return "" }
	return options
}

func (r DiscoveryNativeRestoreResolver) Resolve(ctx context.Context, selection NativeRestoreSelection) (NPMInstallation, error) {
	if !validNativeRestoreSelection(selection) {
		return NPMInstallation{}, ErrInvalidNativeRestoreSelection
	}
	options := SanitizedNativeRestoreDiscoveryOptions(r.Options)
	if options.HomeDir == "" {
		production, err := ProductionDiscoveryOptions(options.RelayConfigPath)
		if err != nil {
			return NPMInstallation{}, ErrNativeRestoreCandidateMissing
		}
		options = SanitizedNativeRestoreDiscoveryOptions(production)
	}
	result, err := DiscoverNPMInstallations(ctx, options)
	if err != nil || discoveryResultIncomplete(result) {
		return NPMInstallation{}, ErrNativeRestoreCandidateMissing
	}
	var matched *NPMInstallation
	for index := range result.Candidates {
		candidate := result.Candidates[index]
		if !constantStringEqual(candidate.ID, selection.InstallationID) ||
			!constantStringEqual(candidate.Fingerprint, selection.InstallationFingerprint) ||
			!constantStringEqual(candidate.NativeRestoreFingerprint, selection.NativeRestoreFingerprint) {
			continue
		}
		if matched != nil {
			return NPMInstallation{}, ErrNativeRestoreCandidateChanged
		}
		copy := candidate
		matched = &copy
	}
	if matched == nil {
		return NPMInstallation{}, ErrNativeRestoreCandidateMissing
	}
	if matched.NativeRestoreCapability != NativeRestoreCapabilityVerifiedSnapshot ||
		matched.nativeRestoreProof == nil || !matched.nativeRestoreProof.valid() {
		return NPMInstallation{}, ErrNativeRestoreProofUnavailable
	}
	if !constantStringEqual(matched.Executable, selection.Executable) ||
		!constantStringEqual(matched.ExecutableSHA256, selection.ExecutableSHA256) {
		return NPMInstallation{}, ErrNativeRestoreCandidateChanged
	}
	if !constantStringEqual(
		matched.nativeRestoreProof.fingerprint(matched.Fingerprint),
		matched.NativeRestoreFingerprint,
	) {
		return NPMInstallation{}, ErrNativeRestoreCandidateChanged
	}
	return *matched, nil
}

func (r DiscoveryNativeRestoreResolver) Revalidate(ctx context.Context, expected NPMInstallation) error {
	selection := NativeRestoreSelection{
		InstallationID:           expected.ID,
		InstallationFingerprint:  expected.Fingerprint,
		NativeRestoreFingerprint: expected.NativeRestoreFingerprint,
		Executable:               expected.Executable,
		ExecutableSHA256:         expected.ExecutableSHA256,
	}
	current, err := r.Resolve(ctx, selection)
	if err != nil {
		return ErrNativeRestoreCandidateChanged
	}
	if !sameNativeRestoreExecutionProof(expected, current) {
		return ErrNativeRestoreCandidateChanged
	}
	return nil
}

func validNativeRestoreSelection(selection NativeRestoreSelection) bool {
	return len(selection.InstallationID) == 24 && isLowerHex(selection.InstallationID) &&
		isFingerprint(selection.InstallationFingerprint) && isFingerprint(selection.NativeRestoreFingerprint) &&
		filepath.IsAbs(selection.Executable) && filepath.Clean(selection.Executable) == selection.Executable &&
		isFingerprint(selection.ExecutableSHA256)
}

func sameNativeRestoreExecutionProof(left, right NPMInstallation) bool {
	if left.nativeRestoreProof == nil || right.nativeRestoreProof == nil {
		return false
	}
	return constantStringEqual(left.ID, right.ID) &&
		constantStringEqual(left.Fingerprint, right.Fingerprint) &&
		constantStringEqual(left.NativeRestoreFingerprint, right.NativeRestoreFingerprint) &&
		constantStringEqual(left.Executable, right.Executable) &&
		constantStringEqual(left.ExecutableSHA256, right.ExecutableSHA256) &&
		constantStringEqual(left.nativeRestoreProof.cliEntry, right.nativeRestoreProof.cliEntry) &&
		constantStringEqual(left.nativeRestoreProof.cliEntrySHA256, right.nativeRestoreProof.cliEntrySHA256) &&
		constantStringEqual(left.nativeRestoreProof.bunExecutable, right.nativeRestoreProof.bunExecutable) &&
		constantStringEqual(left.nativeRestoreProof.bunSHA256, right.nativeRestoreProof.bunSHA256) &&
		constantStringEqual(left.nativeRestoreProof.packageTreeSHA256, right.nativeRestoreProof.packageTreeSHA256)
}

func sameNativeRestoreProofValues(left, right *nativeRestoreExecutionProof) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return constantStringEqual(left.cliEntry, right.cliEntry) &&
		constantStringEqual(left.cliEntrySHA256, right.cliEntrySHA256) &&
		constantStringEqual(left.bunExecutable, right.bunExecutable) &&
		constantStringEqual(left.bunSHA256, right.bunSHA256) &&
		constantStringEqual(left.packageTreeSHA256, right.packageTreeSHA256)
}

func constantStringEqual(left, right string) bool {
	return len(left) == len(right) && subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}
