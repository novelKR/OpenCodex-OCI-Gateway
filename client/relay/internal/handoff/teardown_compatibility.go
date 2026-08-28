package handoff

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

type TeardownCapability string

const (
	TeardownCapabilityNone            TeardownCapability = "none"
	TeardownCapabilityRelayPreserveV1 TeardownCapability = "relay_preserve_v1"
)

type DataCapability string

const (
	DataCapabilityPreserveOnly     DataCapability = "preserve_only"
	DataCapabilitySelectiveTrashV1 DataCapability = "selective_trash_v1"
)

func reviewedDataCapability(capability DataCapability) bool {
	return capability == DataCapabilityPreserveOnly || capability == DataCapabilitySelectiveTrashV1
}

const (
	teardownCompatibilityCompatible         = "compatible"
	teardownCompatibilityUnsupportedPackage = "unsupported_package"
	teardownCompatibilityUnsupportedVersion = "unsupported_version"
	teardownCompatibilityIdentityUnverified = "identity_unverified"
	teardownCompatibilityModuleChanged      = "module_changed"
	teardownCompatibilityClosureChanged     = "package_closure_changed"
	teardownCompatibilityConflict           = "identity_conflict"
)

// teardownAdapterProfile is the extension boundary for Relay-owned package
// teardown support. A new OpenCodex version is admitted by adding one reviewed
// profile and adapter implementation, not by weakening discovery globally.
type teardownAdapterProfile struct {
	packageName           string
	version               string
	artifactVariant       string
	goos                  string
	goarch                string
	registryIntegrity     string
	reviewedClosureSHA256 string
	adapterID             string
	requiredModules       map[string]string
}

type teardownAdapterImplementation struct {
	adapterID  string
	entrypoint string
	sources    map[string][]byte
}

//go:embed adapter/relay_preserve_v1.ts
var relayPreserveV1Adapter []byte

//go:embed adapter/relay_preserve_v1_shim.ts
var relayPreserveV1ShimAdapter []byte

var teardownAdapterImplementations = productionTeardownAdapterImplementations()

func productionTeardownVersions() []string {
	return []string{
		"2.22.0", "2.23.0", "2.24.0", "2.24.1", "2.24.2", "2.25.0", "2.26.0",
		"2.27.0", "2.28.0", "2.29.0", "2.31.0", "2.32.0", "2.32.1", "2.33.0",
	}
}

func teardownAdapterIDForVersion(version string) string {
	return "opencodex_npm_" + strings.ReplaceAll(version, ".", "_") + "_preserve_v1"
}

func productionTeardownAdapterImplementations() []teardownAdapterImplementation {
	versions := productionTeardownVersions()
	implementations := make([]teardownAdapterImplementation, 0, len(versions))
	for _, version := range versions {
		implementations = append(implementations, teardownAdapterImplementation{
			adapterID:  teardownAdapterIDForVersion(version),
			entrypoint: "relay_preserve_v1.ts",
			sources: map[string][]byte{
				"relay_preserve_v1.ts":      relayPreserveV1Adapter,
				"relay_preserve_v1_shim.ts": relayPreserveV1ShimAdapter,
			},
		})
	}
	return implementations
}

type teardownExecutionProof struct {
	adapterID             string
	artifactVariant       string
	profileFingerprint    string
	reviewedClosureSHA256 string
}

func teardownProfileFingerprint(proof *teardownExecutionProof) string {
	if proof == nil || !proof.valid() {
		return ""
	}
	return proof.profileFingerprint
}

func (proof *teardownExecutionProof) valid() bool {
	return proof != nil && safeTeardownToken(proof.adapterID) && safeTeardownToken(proof.artifactVariant) &&
		isFingerprint(proof.profileFingerprint) && isFingerprint(proof.reviewedClosureSHA256)
}

func inspectTeardownCompatibility(
	ctx context.Context,
	packageRoot string,
	manifest npmPackageManifest,
) (TeardownCapability, DataCapability, string, string, *teardownExecutionProof) {
	if manifest.Name != OpenCodexPackageName {
		return TeardownCapabilityNone, DataCapabilityPreserveOnly, teardownCompatibilityUnsupportedPackage, "", nil
	}
	profiles := matchingTeardownProfiles(manifest.Name, manifest.Version)
	if len(profiles) == 0 {
		return TeardownCapabilityNone, DataCapabilityPreserveOnly, teardownCompatibilityUnsupportedVersion, "", nil
	}
	type compatibleProfile struct {
		profile     teardownAdapterProfile
		fingerprint string
	}
	compatible := make([]compatibleProfile, 0, 1)
	sawModuleChange := false
	sawClosureChange := false
	for _, profile := range profiles {
		if _, ok := teardownAdapterImplementationForID(profile.adapterID); !ok {
			continue
		}
		fingerprint, result := verifyTeardownProfile(ctx, packageRoot, profile)
		if result == teardownCompatibilityCompatible {
			compatible = append(compatible, compatibleProfile{profile: profile, fingerprint: fingerprint})
		} else if result == teardownCompatibilityModuleChanged {
			sawModuleChange = true
		} else if result == teardownCompatibilityClosureChanged {
			sawClosureChange = true
		}
	}
	if len(compatible) == 1 {
		match := compatible[0]
		return TeardownCapabilityRelayPreserveV1, DataCapabilitySelectiveTrashV1,
			teardownCompatibilityCompatible, match.profile.adapterID, &teardownExecutionProof{
				adapterID:             match.profile.adapterID,
				artifactVariant:       match.profile.artifactVariant,
				profileFingerprint:    match.fingerprint,
				reviewedClosureSHA256: match.profile.reviewedClosureSHA256,
			}
	}
	if len(compatible) > 1 {
		return TeardownCapabilityNone, DataCapabilityPreserveOnly, teardownCompatibilityConflict, "", nil
	}
	reason := teardownCompatibilityIdentityUnverified
	if sawClosureChange {
		reason = teardownCompatibilityClosureChanged
	} else if sawModuleChange {
		reason = teardownCompatibilityModuleChanged
	}
	return TeardownCapabilityNone, DataCapabilityPreserveOnly, reason, "", nil
}

func matchingTeardownProfiles(packageName, version string) []teardownAdapterProfile {
	profiles := make([]teardownAdapterProfile, 0, 1)
	for _, profile := range teardownAdapterProfiles {
		if profile.packageName == packageName && profile.version == version &&
			profile.goos == runtime.GOOS && profile.goarch == runtime.GOARCH {
			profiles = append(profiles, profile)
		}
	}
	return profiles
}

func teardownProfileForCandidate(
	adapterID string,
	artifactVariant string,
	packageName string,
	version string,
) (teardownAdapterProfile, teardownAdapterImplementation, bool) {
	var match *teardownAdapterProfile
	for _, profile := range teardownAdapterProfiles {
		if profile.adapterID == adapterID &&
			profile.artifactVariant == artifactVariant &&
			profile.packageName == packageName &&
			profile.version == version &&
			profile.goos == runtime.GOOS && profile.goarch == runtime.GOARCH &&
			validTeardownProfile(profile) {
			if match != nil {
				return teardownAdapterProfile{}, teardownAdapterImplementation{}, false
			}
			selected := profile
			match = &selected
		}
	}
	implementation, ok := teardownAdapterImplementationForID(adapterID)
	if match == nil || !ok {
		return teardownAdapterProfile{}, teardownAdapterImplementation{}, false
	}
	return *match, implementation, true
}

func teardownAdapterImplementationForID(adapterID string) (teardownAdapterImplementation, bool) {
	var match *teardownAdapterImplementation
	for _, implementation := range teardownAdapterImplementations {
		if implementation.adapterID != adapterID {
			continue
		}
		if match != nil || !validTeardownAdapterImplementation(implementation) {
			return teardownAdapterImplementation{}, false
		}
		selected := implementation
		match = &selected
	}
	if match == nil {
		return teardownAdapterImplementation{}, false
	}
	return *match, true
}

func validTeardownAdapterImplementation(implementation teardownAdapterImplementation) bool {
	if !safeTeardownToken(implementation.adapterID) || implementation.entrypoint == "" ||
		len(implementation.sources) == 0 || len(implementation.sources) > 8 {
		return false
	}
	total := 0
	for name, source := range implementation.sources {
		if name == "" || filepath.Base(name) != name || filepath.Clean(name) != name ||
			(!strings.HasSuffix(name, ".ts") && !strings.HasSuffix(name, ".js")) || len(source) == 0 {
			return false
		}
		total += len(source)
		if total > maxExecutableBytes {
			return false
		}
	}
	_, ok := implementation.sources[implementation.entrypoint]
	return ok
}

func verifyTeardownProfile(ctx context.Context, packageRoot string, profile teardownAdapterProfile) (string, string) {
	if err := ctx.Err(); err != nil || !validTeardownProfile(profile) || packageRoot == "" ||
		!filepath.IsAbs(packageRoot) || filepath.Clean(packageRoot) != packageRoot {
		return "", teardownCompatibilityIdentityUnverified
	}
	paths := make([]string, 0, len(profile.requiredModules))
	for relative := range profile.requiredModules {
		paths = append(paths, relative)
	}
	sort.Strings(paths)
	hash := sha256.New()
	writeField := func(value string) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	writeField("relay-teardown-profile-v2")
	writeField(profile.packageName)
	writeField(profile.version)
	writeField(profile.artifactVariant)
	writeField(profile.goos)
	writeField(profile.goarch)
	writeField(profile.registryIntegrity)
	writeField(profile.reviewedClosureSHA256)
	writeField(profile.adapterID)
	for _, relative := range paths {
		if err := ctx.Err(); err != nil {
			return "", teardownCompatibilityIdentityUnverified
		}
		payload, _, _, err := readDiscoveryRegularFile(filepath.Join(packageRoot, filepath.FromSlash(relative)), maxExecutableBytes)
		if err != nil {
			return "", teardownCompatibilityIdentityUnverified
		}
		digest := sha256.Sum256(payload)
		actual := hex.EncodeToString(digest[:])
		if !constantStringEqual(actual, profile.requiredModules[relative]) {
			return "", teardownCompatibilityModuleChanged
		}
		writeField(relative)
		writeField(actual)
	}
	closureDigest, err := stableReviewedPackageClosureDigest(ctx, packageRoot)
	if err != nil {
		return "", teardownCompatibilityIdentityUnverified
	}
	if !constantStringEqual(closureDigest, profile.reviewedClosureSHA256) {
		return "", teardownCompatibilityClosureChanged
	}
	return hex.EncodeToString(hash.Sum(nil)), teardownCompatibilityCompatible
}

func validTeardownProfile(profile teardownAdapterProfile) bool {
	if profile.packageName != OpenCodexPackageName || profile.version == "" || len(profile.version) > 128 ||
		!safeTeardownToken(profile.artifactVariant) || !safeTeardownToken(profile.goos) || !safeTeardownToken(profile.goarch) ||
		!validRegistryIntegrity(profile.registryIntegrity) || !isFingerprint(profile.reviewedClosureSHA256) ||
		!safeTeardownToken(profile.adapterID) || len(profile.requiredModules) == 0 || len(profile.requiredModules) > 64 {
		return false
	}
	for relative, digest := range profile.requiredModules {
		clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative)))
		if relative == "" || clean != relative || filepath.IsAbs(relative) || relative == "." || relative == ".." ||
			strings.HasPrefix(relative, "../") || !isFingerprint(digest) {
			return false
		}
	}
	return true
}

func validRegistryIntegrity(value string) bool {
	if !strings.HasPrefix(value, "sha512-") || len(value) <= len("sha512-") || len(value) > 192 {
		return false
	}
	for _, character := range value[len("sha512-"):] {
		if (character < 'A' || character > 'Z') && (character < 'a' || character > 'z') &&
			(character < '0' || character > '9') && character != '+' && character != '/' && character != '=' {
			return false
		}
	}
	return true
}

func safeTeardownToken(value string) bool {
	if value == "" || len(value) > 64 {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '_' && character != '-' {
			return false
		}
	}
	return true
}
