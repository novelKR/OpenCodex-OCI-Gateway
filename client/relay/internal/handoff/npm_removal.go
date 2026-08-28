package handoff

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

var (
	ErrInvalidRemovalSelection = errors.New("invalid OpenCodex removal selection")
	ErrRemovalCandidateMissing = errors.New("selected OpenCodex installation is no longer discoverable")
	ErrRemovalCandidateChanged = errors.New("selected OpenCodex installation changed")
	ErrRemovalManualOnly       = errors.New("selected OpenCodex installation requires manual removal")
	ErrRemovalOutcomeUnknown   = errors.New("OpenCodex package removal could not be verified")
)

// NPMRemovalSelection is the only installation selector accepted by automatic
// removal. Paths, prefixes, managers, and executable names are rediscovered
// from trusted bounded roots instead of being accepted from the caller.
type NPMRemovalSelection struct {
	ID          string `json:"installation_id"`
	Fingerprint string `json:"installation_fingerprint"`
}

// RemovalInstallationResolver pins one selected installation and revalidates
// all removal-critical evidence before each child process boundary.
type RemovalInstallationResolver interface {
	Resolve(context.Context, NPMRemovalSelection) (NPMInstallation, error)
	Revalidate(context.Context, NPMInstallation) error
	VerifyRemoved(NPMInstallation) error
}

// LiveRemovalInstallationValidator is implemented by resolvers that admit a
// read-only discovery proof before a temporary filesystem guard is active.
// Coordinators call it immediately before persisting any mutation intent.
type LiveRemovalInstallationValidator interface {
	ValidateForMutation(context.Context, NPMInstallation) error
}

// DiscoveryRemovalResolver intentionally limits rediscovery to Tier A/B. Tier C
// is useful for explicit inspection, but a broad scan is never removal authority.
type DiscoveryRemovalResolver struct {
	Options      DiscoveryOptions
	EffectiveUID func() int
}

// ProductionDiscoveryOptions returns the common bounded discovery baseline
// for production callers. Authority-sensitive callers must additionally pass
// it through SanitizedRemovalDiscoveryOptions, which removes ambient discovery
// inputs that are appropriate for inspection but not for mutation authority.
func ProductionDiscoveryOptions(relayConfigPath string) (DiscoveryOptions, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return DiscoveryOptions{}, err
	}
	resolvedHome, err := filepath.EvalSymlinks(filepath.Clean(home))
	if err != nil || !filepath.IsAbs(resolvedHome) {
		return DiscoveryOptions{}, ErrUnsafeNPMInstallation
	}
	return DiscoveryOptions{
		Tier:            DiscoveryTierB,
		RelayConfigPath: relayConfigPath,
		HomeDir:         filepath.Clean(resolvedHome),
	}, nil
}

// SanitizedRemovalDiscoveryOptions preserves only bounded, deterministic
// authority inputs. In particular, ambient PATH and version-manager overrides
// can help an operator inspect an installation but never influence a package
// removal selector.
func SanitizedRemovalDiscoveryOptions(options DiscoveryOptions) DiscoveryOptions {
	options.Tier = DiscoveryTierB
	options.BroadScanApproved = false
	options.BroadRoots = nil
	options.PathEnv = ""
	options.Getenv = func(string) string { return "" }
	return options
}

func (r DiscoveryRemovalResolver) Resolve(ctx context.Context, selection NPMRemovalSelection) (NPMInstallation, error) {
	if !validRemovalSelection(selection) {
		return NPMInstallation{}, ErrInvalidRemovalSelection
	}
	options := SanitizedRemovalDiscoveryOptions(r.Options)
	if options.HomeDir == "" {
		production, err := ProductionDiscoveryOptions(options.RelayConfigPath)
		if err != nil {
			return NPMInstallation{}, ErrRemovalCandidateMissing
		}
		options = SanitizedRemovalDiscoveryOptions(production)
	}
	result, err := DiscoverNPMInstallations(ctx, options)
	if err != nil {
		return NPMInstallation{}, ErrRemovalCandidateMissing
	}
	if result.Truncated {
		return NPMInstallation{}, ErrRemovalCandidateMissing
	}
	var matched *NPMInstallation
	for index := range result.Candidates {
		candidate := result.Candidates[index]
		if subtle.ConstantTimeCompare([]byte(candidate.ID), []byte(selection.ID)) != 1 ||
			subtle.ConstantTimeCompare([]byte(candidate.Fingerprint), []byte(selection.Fingerprint)) != 1 {
			continue
		}
		if matched != nil {
			return NPMInstallation{}, ErrRemovalCandidateChanged
		}
		copy := candidate
		matched = &copy
	}
	if matched == nil {
		return NPMInstallation{}, ErrRemovalCandidateMissing
	}
	if matched.RemovalAuthority != RemovalAuthorityAutomatic {
		return NPMInstallation{}, ErrRemovalManualOnly
	}
	uid := os.Geteuid()
	if r.EffectiveUID != nil {
		uid = r.EffectiveUID()
	}
	if err := validateAutomaticRemovalCandidateStaticContext(ctx, *matched, uid); err != nil {
		return NPMInstallation{}, err
	}
	return *matched, nil
}

func (r DiscoveryRemovalResolver) ValidateForMutation(ctx context.Context, candidate NPMInstallation) error {
	uid := os.Geteuid()
	if r.EffectiveUID != nil {
		uid = r.EffectiveUID()
	}
	return validateAutomaticRemovalCandidateContext(ctx, candidate, uid)
}

func (r DiscoveryRemovalResolver) Revalidate(ctx context.Context, expected NPMInstallation) error {
	current, err := r.Resolve(ctx, NPMRemovalSelection{ID: expected.ID, Fingerprint: expected.Fingerprint})
	if err != nil {
		if errors.Is(err, ErrRemovalCandidateMissing) {
			return ErrRemovalCandidateChanged
		}
		return err
	}
	if !sameRemovalCriticalInstallation(expected, current) {
		return ErrRemovalCandidateChanged
	}
	return nil
}

func (DiscoveryRemovalResolver) VerifyRemoved(candidate NPMInstallation) error {
	prefix, ok := prefixFromPackageRoot(candidate.PackageRoot)
	if !ok || prefix != candidate.Prefix {
		return ErrRemovalOutcomeUnknown
	}
	paths := uniqueSortedStrings(append([]string{
		candidate.PackageRoot,
		filepath.Join(prefix, "bin", "ocx"),
		filepath.Join(prefix, "bin", "opencodex"),
	}, candidate.Launchers...))
	for _, path := range paths {
		if _, err := os.Lstat(path); err == nil || !errors.Is(err, os.ErrNotExist) {
			return ErrRemovalOutcomeUnknown
		}
	}
	return nil
}

func validRemovalSelection(selection NPMRemovalSelection) bool {
	return len(selection.ID) == 24 && isLowerHex(selection.ID) && isFingerprint(selection.Fingerprint)
}

func validateAutomaticRemovalCandidate(candidate NPMInstallation, effectiveUID int) error {
	return validateAutomaticRemovalCandidateContext(context.Background(), candidate, effectiveUID)
}

func validateAutomaticRemovalCandidateStaticContext(ctx context.Context, candidate NPMInstallation, effectiveUID int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	capabilityEligible := candidate.RemovalCapability == RemovalCapabilityExactNPM ||
		candidate.RemovalCapability == RemovalCapabilityHomebrewGuardedNPM
	if effectiveUID == 0 || candidate.Tier == DiscoveryTierC ||
		candidate.RemovalAuthority != RemovalAuthorityAutomatic ||
		!capabilityEligible || !candidate.UserWritable || candidate.RequiresElevation ||
		candidate.TeardownCapability != TeardownCapabilityRelayPreserveV1 ||
		!reviewedDataCapability(candidate.DataCapability) ||
		candidate.TeardownCompatibility != teardownCompatibilityCompatible ||
		!safeTeardownToken(candidate.TeardownAdapterID) ||
		candidate.teardownProof == nil || !candidate.teardownProof.valid() ||
		candidate.teardownProof.adapterID != candidate.TeardownAdapterID {
		return ErrRemovalManualOnly
	}
	if candidate.Manager == DiscoveryManagerVolta ||
		candidate.NodeExecutable == "" || candidate.NPMCLI == "" || candidate.CLIEntry == "" || candidate.BunExecutable == "" ||
		!isFingerprint(candidate.NodeSHA256) || !isFingerprint(candidate.NPMCLISHA256) ||
		!isFingerprint(candidate.CLIEntrySHA256) || !isFingerprint(candidate.BunSHA256) ||
		!isFingerprint(candidate.PackageTreeSHA256) || !isFingerprint(candidate.NPMTreeSHA256) {
		return ErrRemovalManualOnly
	}
	for _, warning := range candidate.Warnings {
		if blockingRemovalWarning(warning) {
			return ErrRemovalManualOnly
		}
	}
	rootInfo, err := os.Lstat(candidate.PackageRoot)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return ErrRemovalCandidateChanged
	}
	rootUID, ok := ownerUID(rootInfo)
	if !ok || int(rootUID) != effectiveUID {
		return ErrRemovalManualOnly
	}
	if candidate.RemovalCapability == RemovalCapabilityHomebrewGuardedNPM {
		if !candidate.HomebrewGuardRequired || candidate.Manager != DiscoveryManagerHomebrew ||
			candidate.Prefix == "" || candidate.PackageRoot != packageRootForPrefix(candidate.Prefix) {
			return ErrRemovalManualOnly
		}
		for _, executable := range []struct {
			path string
			hash string
		}{
			{candidate.Executable, candidate.ExecutableSHA256},
			{candidate.BunExecutable, candidate.BunSHA256},
			{candidate.NodeExecutable, candidate.NodeSHA256},
			{candidate.NPMCLI, candidate.NPMCLISHA256},
		} {
			resolved, fingerprint, _, verifyErr := verifyDiscoveryExecutable(executable.path)
			if verifyErr != nil || resolved != executable.path ||
				subtle.ConstantTimeCompare([]byte(fingerprint), []byte(executable.hash)) != 1 {
				return ErrRemovalCandidateChanged
			}
		}
		return verifyRemovalExecutionClosurePolicyContext(ctx, candidate, true)
	}
	if candidate.HomebrewGuardRequired {
		return ErrRemovalManualOnly
	}
	return validateStrictRemovalProofContext(ctx, candidate)
}

func validateAutomaticRemovalCandidateContext(ctx context.Context, candidate NPMInstallation, effectiveUID int) error {
	if err := validateAutomaticRemovalCandidateStaticContext(ctx, candidate, effectiveUID); err != nil {
		return err
	}
	return validateStrictRemovalProofContext(ctx, candidate)
}

func validateStrictRemovalProofContext(ctx context.Context, candidate NPMInstallation) error {
	for _, executable := range []struct {
		path string
		hash string
	}{
		{candidate.Executable, candidate.ExecutableSHA256},
		{candidate.BunExecutable, candidate.BunSHA256},
		{candidate.NodeExecutable, candidate.NodeSHA256},
		{candidate.NPMCLI, candidate.NPMCLISHA256},
	} {
		resolved, fingerprint, verifyErr := VerifyExecutable(executable.path)
		if verifyErr != nil || resolved != executable.path ||
			subtle.ConstantTimeCompare([]byte(fingerprint), []byte(executable.hash)) != 1 {
			return ErrRemovalCandidateChanged
		}
	}
	if err := verifyRemovalExecutionClosureContext(ctx, candidate); err != nil {
		return err
	}
	return nil
}

func verifyRemovalExecutionClosure(candidate NPMInstallation) error {
	return verifyRemovalExecutionClosureContext(context.Background(), candidate)
}

func verifyRemovalExecutionClosureContext(ctx context.Context, candidate NPMInstallation) error {
	return verifyRemovalExecutionClosurePolicyContext(ctx, candidate, false)
}

func verifyRemovalExecutionClosurePolicyContext(ctx context.Context, candidate NPMInstallation, allowRelaxed bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if candidate.CLIEntry != filepath.Join(candidate.PackageRoot, "src", "cli", "index.ts") ||
		!pathContainedBy(candidate.PackageRoot, candidate.BunExecutable) {
		return ErrRemovalCandidateChanged
	}
	cliBytes, _, relaxed, err := readDiscoveryRegularFile(candidate.CLIEntry, maxExecutableBytes)
	if err != nil || (!allowRelaxed && relaxed) {
		return ErrRemovalCandidateChanged
	}
	cliDigest := sha256.Sum256(cliBytes)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(cliDigest[:])), []byte(candidate.CLIEntrySHA256)) != 1 {
		return ErrRemovalCandidateChanged
	}
	fingerprintTree := stableExecutionTreeFingerprintContext
	if candidate.RemovalCapability == RemovalCapabilityHomebrewGuardedNPM {
		fingerprintTree = stableExecutionTreeFingerprintWithoutExtendedACLContext
	}
	packageTree, err := fingerprintTree(ctx, candidate.PackageRoot)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrRemovalCandidateChanged
	}
	if subtle.ConstantTimeCompare([]byte(packageTree), []byte(candidate.PackageTreeSHA256)) != 1 {
		return ErrRemovalCandidateChanged
	}
	npmRoot, ok := npmRootFromCLI(candidate.NPMCLI)
	if !ok {
		return ErrRemovalCandidateChanged
	}
	npmTree, err := fingerprintTree(ctx, npmRoot)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		return ErrRemovalCandidateChanged
	}
	if subtle.ConstantTimeCompare([]byte(npmTree), []byte(candidate.NPMTreeSHA256)) != 1 {
		return ErrRemovalCandidateChanged
	}
	return nil
}

func validateLiveRemovalTargetsContext(ctx context.Context, candidate NPMInstallation) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rootInfo, err := os.Lstat(candidate.PackageRoot)
	if err != nil || !safeExecutionDirectory(rootInfo) || !ownedByEffectiveUser(rootInfo) {
		return ErrRemovalCandidateChanged
	}
	device, inode, ok := discoveryFileIdentity(rootInfo)
	if !ok || device != candidate.packageRootDevice || inode != candidate.packageRootInode {
		return ErrRemovalCandidateChanged
	}
	prefix, ok := prefixFromPackageRoot(candidate.PackageRoot)
	if !ok || prefix != candidate.Prefix {
		return ErrRemovalCandidateChanged
	}
	if err := verifyExactRemovalExecutable(candidate.Executable, candidate.ExecutableSHA256); err != nil {
		return err
	}
	for _, name := range []string{"ocx", "opencodex"} {
		launcher := filepath.Join(prefix, "bin", name)
		if stringSliceContains(candidate.Launchers, launcher) {
			resolved, fingerprint, _, verifyErr := verifyDiscoveryExecutable(launcher)
			if verifyErr != nil || resolved != candidate.Executable ||
				subtle.ConstantTimeCompare([]byte(fingerprint), []byte(candidate.ExecutableSHA256)) != 1 {
				return ErrRemovalCandidateChanged
			}
			continue
		}
		if _, err := os.Lstat(launcher); !errors.Is(err, os.ErrNotExist) {
			return ErrRemovalCandidateChanged
		}
	}
	return ctx.Err()
}

func stringSliceContains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func sameRemovalCriticalInstallation(left, right NPMInstallation) bool {
	return left.ID == right.ID &&
		left.Manager == right.Manager &&
		left.Prefix == right.Prefix &&
		left.PackageRoot == right.PackageRoot &&
		left.Executable == right.Executable &&
		left.ExecutableSHA256 == right.ExecutableSHA256 &&
		left.CLIEntry == right.CLIEntry &&
		left.CLIEntrySHA256 == right.CLIEntrySHA256 &&
		left.BunExecutable == right.BunExecutable &&
		left.BunSHA256 == right.BunSHA256 &&
		left.PackageTreeSHA256 == right.PackageTreeSHA256 &&
		left.NPMTreeSHA256 == right.NPMTreeSHA256 &&
		left.NodeExecutable == right.NodeExecutable &&
		left.NodeSHA256 == right.NodeSHA256 &&
		left.NPMCLI == right.NPMCLI &&
		left.NPMCLISHA256 == right.NPMCLISHA256 &&
		left.RemovalCapability == right.RemovalCapability &&
		left.RemovalAuthority == right.RemovalAuthority &&
		left.HomebrewGuardRequired == right.HomebrewGuardRequired &&
		left.TeardownCapability == right.TeardownCapability &&
		left.DataCapability == right.DataCapability &&
		left.TeardownCompatibility == right.TeardownCompatibility &&
		left.TeardownAdapterID == right.TeardownAdapterID &&
		sameTeardownProofValues(left.teardownProof, right.teardownProof) &&
		left.UserWritable == right.UserWritable &&
		left.RequiresElevation == right.RequiresElevation &&
		left.Fingerprint == right.Fingerprint &&
		left.packageRootDevice == right.packageRootDevice &&
		left.packageRootInode == right.packageRootInode &&
		reflect.DeepEqual(left.Launchers, right.Launchers) &&
		reflect.DeepEqual(left.Warnings, right.Warnings)
}

func isLowerHex(value string) bool {
	return value != "" && strings.IndexFunc(value, func(character rune) bool {
		return !(character >= '0' && character <= '9' || character >= 'a' && character <= 'f')
	}) == -1
}
