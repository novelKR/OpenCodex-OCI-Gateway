package handoff

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
)

const (
	OpenCodexPackageName       = "@bitkyc08/opencodex"
	discoverySchemaVersion     = 5
	maxPackageManifestBytes    = 64 << 10
	defaultDiscoveryMaxEntries = 20_000
	defaultDiscoveryMaxDepth   = 10
	maximumDiscoveryCandidates = 128
	maximumDiscoveryCoverage   = 64
	maximumUnmatchedLaunchers  = 128
)

type DiscoveryTier string

const (
	DiscoveryTierA DiscoveryTier = "a"
	DiscoveryTierB DiscoveryTier = "b"
	DiscoveryTierC DiscoveryTier = "c"
)

type DiscoveryManager string

const (
	DiscoveryManagerNPM      DiscoveryManager = "npm"
	DiscoveryManagerHomebrew DiscoveryManager = "homebrew"
	DiscoveryManagerNVM      DiscoveryManager = "nvm"
	DiscoveryManagerFNM      DiscoveryManager = "fnm"
	DiscoveryManagerVolta    DiscoveryManager = "volta"
	DiscoveryManagerASDF     DiscoveryManager = "asdf"
)

type RemovalCapability string

const (
	RemovalCapabilityExactNPM           RemovalCapability = "exact_npm"
	RemovalCapabilityHomebrewGuardedNPM RemovalCapability = "homebrew_guarded_npm"
	RemovalCapabilityVolta              RemovalCapability = "volta"
	RemovalCapabilityManual             RemovalCapability = "manual"
)

// RemovalAuthority is the discovery-time decision about whether a candidate
// may be presented as an automatic-removal selector. It is deliberately
// distinct from RemovalCapability: capability describes the package-manager
// mechanism proved for one installation, while authority also depends on the
// completeness and unambiguity of the bounded discovery run.
type RemovalAuthority string

const (
	RemovalAuthorityAutomatic RemovalAuthority = "automatic"
	RemovalAuthorityManual    RemovalAuthority = "manual"
)

// NativeRestoreCapability is independent from package-removal authority. A
// user-owned Homebrew installation can be unsafe for automatic uninstall while
// still providing a complete OpenCodex package closure that can be copied into
// a private, immutable snapshot for an owner-scoped native restore.
type NativeRestoreCapability string

const (
	NativeRestoreCapabilityVerifiedSnapshot NativeRestoreCapability = "verified_snapshot"
)

// RemovalAuthorityProjection is the only discovery value a caller needs to
// carry into a mutating removal request. It intentionally excludes paths,
// manager commands, hashes, and every other execution detail; the resolver
// rediscovers those details from its own bounded production options.
type RemovalAuthorityProjection struct {
	InstallationID          string           `json:"installation_id"`
	InstallationFingerprint string           `json:"installation_fingerprint"`
	Authority               RemovalAuthority `json:"authority"`
}

var (
	ErrInvalidDiscoveryTier     = errors.New("invalid OpenCodex discovery tier")
	ErrBroadScanConsentRequired = errors.New("broad OpenCodex discovery requires explicit consent")
	ErrUnsupportedBroadScan     = errors.New("broad OpenCodex discovery is supported only on a local macOS volume")
	ErrUnsafeNPMInstallation    = errors.New("OpenCodex npm installation is unsafe")
	ErrDiscoveryLimit           = errors.New("OpenCodex discovery limit reached")
)

type NPMInstallation struct {
	ID                       string                  `json:"id"`
	Tier                     DiscoveryTier           `json:"tier"`
	Source                   string                  `json:"source"`
	Manager                  DiscoveryManager        `json:"manager"`
	Prefix                   string                  `json:"prefix"`
	PackageRoot              string                  `json:"package_root"`
	Version                  string                  `json:"version"`
	Executable               string                  `json:"executable"`
	ExecutableSHA256         string                  `json:"executable_sha256"`
	CLIEntry                 string                  `json:"cli_entry,omitempty"`
	CLIEntrySHA256           string                  `json:"cli_entry_sha256,omitempty"`
	BunExecutable            string                  `json:"bun_executable,omitempty"`
	BunSHA256                string                  `json:"bun_sha256,omitempty"`
	PackageTreeSHA256        string                  `json:"package_tree_sha256,omitempty"`
	NPMTreeSHA256            string                  `json:"npm_tree_sha256,omitempty"`
	Launchers                []string                `json:"launchers"`
	NodeExecutable           string                  `json:"node_executable,omitempty"`
	NodeSHA256               string                  `json:"node_sha256,omitempty"`
	NPMCLI                   string                  `json:"npm_cli,omitempty"`
	NPMCLISHA256             string                  `json:"npm_cli_sha256,omitempty"`
	Confidence               string                  `json:"confidence"`
	RemovalCapability        RemovalCapability       `json:"removal_capability"`
	RemovalAuthority         RemovalAuthority        `json:"removal_authority"`
	HomebrewGuardRequired    bool                    `json:"homebrew_guard_required"`
	TeardownCapability       TeardownCapability      `json:"teardown_capability"`
	DataCapability           DataCapability          `json:"data_capability"`
	TeardownCompatibility    string                  `json:"teardown_compatibility_reason"`
	TeardownAdapterID        string                  `json:"teardown_adapter_id,omitempty"`
	NativeRestoreCapability  NativeRestoreCapability `json:"native_restore_capability,omitempty"`
	NativeRestoreFingerprint string                  `json:"native_restore_fingerprint,omitempty"`
	UserWritable             bool                    `json:"user_writable"`
	RequiresElevation        bool                    `json:"requires_elevation"`
	Fingerprint              string                  `json:"fingerprint"`
	Warnings                 []string                `json:"warnings"`
	packageRootDevice        uint64
	packageRootInode         uint64
	nativeRestoreProof       *nativeRestoreExecutionProof
	teardownProof            *teardownExecutionProof
}

func (candidate NPMInstallation) AuthorityProjection() RemovalAuthorityProjection {
	return RemovalAuthorityProjection{
		InstallationID:          candidate.ID,
		InstallationFingerprint: candidate.Fingerprint,
		Authority:               candidate.RemovalAuthority,
	}
}

type DiscoveryCoverage struct {
	Source string `json:"source"`
	Root   string `json:"root"`
	State  string `json:"state"`
}

type DiscoveryResult struct {
	SchemaVersion     int                 `json:"schema_version"`
	RequestedTier     DiscoveryTier       `json:"requested_tier"`
	BroadScanApproved bool                `json:"broad_scan_approved"`
	Candidates        []NPMInstallation   `json:"candidates"`
	Coverage          []DiscoveryCoverage `json:"coverage"`
	Rejected          int                 `json:"rejected"`
	Truncated         bool                `json:"truncated"`
}

type DiscoveryOptions struct {
	Tier                DiscoveryTier
	RelayConfigPath     string
	HomeDir             string
	PathEnv             string
	GOOS                string
	GOARCH              string
	BroadScanApproved   bool
	BroadRoots          []string
	SkipDefaultPrefixes bool
	MaxEntries          int
	MaxDepth            int
	Getenv              func(string) string
	// HomebrewPrefix is empty for ordinary callers. normalizeDiscoveryOptions
	// fills the reviewed Apple Silicon prefix; tests may inject an owner-only
	// fixture root without granting arbitrary production prefixes authority.
	HomebrewPrefix string
}

type discoverySeed struct {
	packageRoot      string
	requiredLauncher string
	source           string
	manager          DiscoveryManager
	managerRoot      string
	tier             DiscoveryTier
	confidence       string
	homebrewPrefix   string
}

type unmatchedLauncherEvidence struct {
	launcher    string
	fingerprint string
	source      string
	manager     DiscoveryManager
	managerRoot string
	tier        DiscoveryTier
	confidence  string
}

type discoveryCollector struct {
	options                   DiscoveryOptions
	seeds                     []discoverySeed
	unmatchedLaunchers        []unmatchedLauncherEvidence
	coverage                  []DiscoveryCoverage
	entries                   int
	truncated                 bool
	launcherEvidenceTruncated bool
	deviceID                  func(os.FileInfo) (uint64, bool)
}

func DiscoverNPMInstallations(ctx context.Context, options DiscoveryOptions) (DiscoveryResult, error) {
	options, err := normalizeDiscoveryOptions(options)
	if err != nil {
		return DiscoveryResult{}, err
	}
	collector := discoveryCollector{options: options, deviceID: discoveryDeviceID}
	collector.collectTierA()
	if options.Tier == DiscoveryTierB || options.Tier == DiscoveryTierC {
		collector.collectTierB(ctx)
	}
	if options.Tier == DiscoveryTierC {
		if !options.BroadScanApproved {
			return DiscoveryResult{}, ErrBroadScanConsentRequired
		}
		if options.GOOS != "darwin" {
			return DiscoveryResult{}, ErrUnsupportedBroadScan
		}
		if err := collector.collectTierC(ctx); err != nil && !errors.Is(err, ErrDiscoveryLimit) {
			return DiscoveryResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return DiscoveryResult{}, err
	}

	result := DiscoveryResult{
		SchemaVersion:     discoverySchemaVersion,
		RequestedTier:     options.Tier,
		BroadScanApproved: options.Tier == DiscoveryTierC && options.BroadScanApproved,
		Coverage:          boundedCoverage(collector.coverage),
		Truncated:         collector.truncated,
	}
	validated := make([]NPMInstallation, 0, len(collector.seeds)+len(collector.unmatchedLaunchers))
	for _, seed := range collector.seeds {
		if err := ctx.Err(); err != nil {
			return DiscoveryResult{}, err
		}
		candidate, err := validateNPMInstallation(ctx, seed)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				result.Rejected++
			}
			continue
		}
		validated = append(validated, candidate)
	}
	baseCandidates := append([]NPMInstallation(nil), validated...)
	for _, evidence := range collector.unmatchedLaunchers {
		matchedRoots := make(map[string]struct{})
		for _, candidate := range baseCandidates {
			if _, duplicate := matchedRoots[candidate.PackageRoot]; duplicate ||
				!launcherEvidenceMatchesCandidate(evidence, candidate) {
				continue
			}
			matchedRoots[candidate.PackageRoot] = struct{}{}
			synthetic, err := validateNPMInstallation(ctx, discoverySeed{
				packageRoot:      candidate.PackageRoot,
				requiredLauncher: evidence.launcher,
				source:           evidence.source,
				manager:          evidence.manager,
				managerRoot:      evidence.managerRoot,
				tier:             evidence.tier,
				confidence:       evidence.confidence,
				homebrewPrefix:   collector.options.HomebrewPrefix,
			})
			if err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					result.Rejected++
				}
				continue
			}
			validated = append(validated, synthetic)
		}
	}
	if collector.launcherEvidenceTruncated {
		for index := range validated {
			validated[index].RemovalCapability = RemovalCapabilityManual
			validated[index].Warnings = uniqueSortedStrings(append(validated[index].Warnings, "launcher_evidence_truncated"))
		}
	}
	byRoot := make(map[string][]NPMInstallation)
	for _, candidate := range validated {
		byRoot[candidate.PackageRoot] = append(byRoot[candidate.PackageRoot], candidate)
	}
	for _, candidates := range byRoot {
		result.Candidates = append(result.Candidates, mergeInstallations(candidates))
	}
	sort.Slice(result.Candidates, func(i, j int) bool {
		if result.Candidates[i].Tier != result.Candidates[j].Tier {
			return result.Candidates[i].Tier < result.Candidates[j].Tier
		}
		return result.Candidates[i].PackageRoot < result.Candidates[j].PackageRoot
	})
	if len(result.Candidates) > maximumDiscoveryCandidates {
		result.Candidates = result.Candidates[:maximumDiscoveryCandidates]
		result.Truncated = true
	}
	applyDiscoveryRemovalAuthority(&result)
	return result, nil
}

func applyDiscoveryRemovalAuthority(result *DiscoveryResult) {
	if result == nil {
		return
	}
	// Discovery errors never produce a result. For a returned result, a bound
	// that was reached, rejected evidence, or a refused/truncated coverage root
	// means we cannot prove that the candidate set is complete. Do not turn a
	// partial inspection into deletion authority.
	incomplete := result.Truncated || result.Rejected != 0
	for _, coverage := range result.Coverage {
		if coverage.State != "scanned" && coverage.State != "absent" {
			incomplete = true
			break
		}
	}
	for index := range result.Candidates {
		candidate := &result.Candidates[index]
		candidate.RemovalAuthority = RemovalAuthorityManual
		if !incomplete && automaticRemovalAuthorityCandidate(*candidate) {
			candidate.RemovalAuthority = RemovalAuthorityAutomatic
		}
	}
}

// DiscoverNPMInstallationsWithAuthority preserves the broad ambient result for
// display, then projects automatic-removal authority only from a separate
// sanitized, complete Tier-B pass. Failure of the authority pass is not a
// discovery failure: every displayed candidate simply remains manual.
func DiscoverNPMInstallationsWithAuthority(
	ctx context.Context,
	displayOptions DiscoveryOptions,
	authorityOptions DiscoveryOptions,
) (DiscoveryResult, error) {
	display, err := DiscoverNPMInstallations(ctx, displayOptions)
	if err != nil {
		return DiscoveryResult{}, err
	}
	for index := range display.Candidates {
		display.Candidates[index].RemovalAuthority = RemovalAuthorityManual
	}
	if discoveryResultIncomplete(display) {
		return display, nil
	}
	authority, err := DiscoverNPMInstallations(ctx, SanitizedRemovalDiscoveryOptions(authorityOptions))
	if err != nil || discoveryResultIncomplete(authority) {
		return display, nil
	}
	projectSanitizedRemovalAuthority(&display, authority)
	return display, nil
}

func projectSanitizedRemovalAuthority(display *DiscoveryResult, authority DiscoveryResult) {
	if display == nil || discoveryResultIncomplete(*display) || discoveryResultIncomplete(authority) {
		return
	}
	for displayIndex := range display.Candidates {
		displayCandidate := &display.Candidates[displayIndex]
		if !automaticRemovalAuthorityCandidate(*displayCandidate) {
			displayCandidate.RemovalAuthority = RemovalAuthorityManual
			continue
		}
		exactMatches := 0
		for _, authorityCandidate := range authority.Candidates {
			if authorityCandidate.ID == displayCandidate.ID &&
				authorityCandidate.Fingerprint == displayCandidate.Fingerprint &&
				authorityCandidate.RemovalAuthority == RemovalAuthorityAutomatic {
				exactMatches++
			}
		}
		if exactMatches == 1 {
			displayCandidate.RemovalAuthority = RemovalAuthorityAutomatic
		} else {
			displayCandidate.RemovalAuthority = RemovalAuthorityManual
		}
	}
}

func discoveryResultIncomplete(result DiscoveryResult) bool {
	if result.SchemaVersion != discoverySchemaVersion || result.Truncated || result.Rejected != 0 {
		return true
	}
	for _, coverage := range result.Coverage {
		if coverage.State != "scanned" && coverage.State != "absent" {
			return true
		}
	}
	return false
}

func automaticRemovalAuthorityCandidate(candidate NPMInstallation) bool {
	capabilityEligible := candidate.RemovalCapability == RemovalCapabilityExactNPM ||
		candidate.RemovalCapability == RemovalCapabilityHomebrewGuardedNPM
	if candidate.Tier == DiscoveryTierC || !capabilityEligible ||
		!candidate.UserWritable || candidate.RequiresElevation ||
		candidate.Manager == DiscoveryManagerVolta ||
		candidate.TeardownCapability != TeardownCapabilityRelayPreserveV1 ||
		!reviewedDataCapability(candidate.DataCapability) ||
		candidate.TeardownCompatibility != teardownCompatibilityCompatible ||
		!safeTeardownToken(candidate.TeardownAdapterID) {
		return false
	}
	if candidate.NodeExecutable == "" || candidate.NodeSHA256 == "" ||
		candidate.NPMCLI == "" || candidate.NPMCLISHA256 == "" ||
		candidate.CLIEntry == "" || candidate.CLIEntrySHA256 == "" ||
		candidate.BunExecutable == "" || candidate.BunSHA256 == "" ||
		candidate.PackageTreeSHA256 == "" || candidate.NPMTreeSHA256 == "" {
		return false
	}
	for _, warning := range candidate.Warnings {
		if blockingRemovalWarning(warning) {
			return false
		}
	}
	return true
}

func blockingRemovalWarning(warning string) bool {
	return warning == "writable_parent_chain" || warning == "exact_npm_pair_unavailable" ||
		warning == "execution_closure_unavailable" || warning == "extended_acl" || warning == "external_launcher_requires_manual_removal" ||
		strings.HasPrefix(warning, "launcher_mismatch_") || warning == "launcher_evidence_truncated" ||
		warning == "package_identity_conflict"
}

func homebrewCandidateHasExtendedACL(prefix string, paths ...string) (bool, error) {
	prefix = filepath.Clean(prefix)
	if prefix == "" || !filepath.IsAbs(prefix) {
		return false, ErrUnsafeNPMInstallation
	}
	seen := make(map[string]struct{})
	for _, path := range paths {
		path = filepath.Clean(path)
		if path == "" || !filepath.IsAbs(path) || !pathContainedBy(prefix, path) {
			return false, ErrUnsafeNPMInstallation
		}
		current := path
		for {
			if _, ok := seen[current]; !ok {
				present, err := hasExtendedACL(current)
				if err != nil {
					return false, err
				}
				if present {
					return true, nil
				}
				seen[current] = struct{}{}
			}
			if current == prefix {
				break
			}
			parent := filepath.Dir(current)
			if parent == current || !pathContainedBy(prefix, parent) {
				return false, ErrUnsafeNPMInstallation
			}
			current = parent
		}
	}
	return false, nil
}

func normalizeDiscoveryOptions(options DiscoveryOptions) (DiscoveryOptions, error) {
	if options.Tier != DiscoveryTierA && options.Tier != DiscoveryTierB && options.Tier != DiscoveryTierC {
		return DiscoveryOptions{}, ErrInvalidDiscoveryTier
	}
	if options.Getenv == nil {
		options.Getenv = os.Getenv
	}
	if options.HomeDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return DiscoveryOptions{}, err
		}
		options.HomeDir = home
	}
	options.HomeDir = filepath.Clean(options.HomeDir)
	if !filepath.IsAbs(options.HomeDir) {
		return DiscoveryOptions{}, ErrUnsafeNPMInstallation
	}
	resolvedHome, err := filepath.EvalSymlinks(options.HomeDir)
	if err != nil || !filepath.IsAbs(resolvedHome) {
		return DiscoveryOptions{}, ErrUnsafeNPMInstallation
	}
	options.HomeDir = filepath.Clean(resolvedHome)
	if options.PathEnv == "" {
		options.PathEnv = options.Getenv("PATH")
	}
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	if options.HomebrewPrefix == "" && !options.SkipDefaultPrefixes && options.GOOS == "darwin" && options.GOARCH == "arm64" {
		options.HomebrewPrefix = "/opt/homebrew"
	}
	if options.HomebrewPrefix != "" {
		prefix := filepath.Clean(options.HomebrewPrefix)
		resolved, err := filepath.EvalSymlinks(prefix)
		if options.GOOS != "darwin" || options.GOARCH != "arm64" || !filepath.IsAbs(prefix) ||
			err != nil || resolved != prefix {
			return DiscoveryOptions{}, ErrUnsafeNPMInstallation
		}
		options.HomebrewPrefix = prefix
	}
	if options.MaxEntries <= 0 || options.MaxEntries > defaultDiscoveryMaxEntries {
		options.MaxEntries = defaultDiscoveryMaxEntries
	}
	if options.MaxDepth <= 0 || options.MaxDepth > defaultDiscoveryMaxDepth {
		options.MaxDepth = defaultDiscoveryMaxDepth
	}
	return options, nil
}

func (c *discoveryCollector) collectTierA() {
	if c.options.RelayConfigPath != "" {
		if record, err := ReadRecord(c.options.RelayConfigPath); err == nil {
			c.addLauncherSeed(record.Executable, "enrollment", c.managerForPath(record.Executable), "", DiscoveryTierA, "high")
		}
	}
	for _, directory := range filepath.SplitList(c.options.PathEnv) {
		if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
			continue
		}
		for _, name := range []string{"ocx", "opencodex"} {
			c.addLauncherSeed(filepath.Join(directory, name), "path", c.managerForPath(directory), "", DiscoveryTierA, "high")
		}
	}
	if !c.options.SkipDefaultPrefixes {
		prefix := "/usr/local"
		if c.options.GOOS == "darwin" && c.options.GOARCH == "arm64" {
			prefix = "/opt/homebrew"
		}
		c.addPrefixSeed(prefix, "native_prefix", c.managerForPrefix(prefix), "", DiscoveryTierA, "high")
	}
}

func (c *discoveryCollector) collectTierB(ctx context.Context) {
	if !c.options.SkipDefaultPrefixes {
		for _, prefix := range []string{"/opt/homebrew", "/usr/local", "/usr"} {
			c.addPrefixSeed(prefix, "trusted_prefix", c.managerForPrefix(prefix), "", DiscoveryTierB, "trusted")
		}
	}

	type managerRoot struct {
		manager DiscoveryManager
		root    string
		source  string
	}
	xdg := c.options.Getenv("XDG_CONFIG_HOME")
	if xdg == "" {
		xdg = filepath.Join(c.options.HomeDir, ".config")
	}
	nvmRoot := c.options.Getenv("NVM_DIR")
	if nvmRoot == "" {
		nvmRoot = filepath.Join(c.options.HomeDir, ".nvm")
	}
	fnmRoot := c.options.Getenv("FNM_DIR")
	if fnmRoot == "" {
		fnmRoot = filepath.Join(c.options.HomeDir, "Library", "Application Support", "fnm")
	}
	voltaRoot := c.options.Getenv("VOLTA_HOME")
	if voltaRoot == "" {
		voltaRoot = filepath.Join(c.options.HomeDir, ".volta")
	}
	asdfRoot := c.options.Getenv("ASDF_DATA_DIR")
	if asdfRoot == "" {
		asdfRoot = filepath.Join(c.options.HomeDir, ".asdf")
	}
	roots := []managerRoot{
		{DiscoveryManagerNVM, nvmRoot, "nvm"},
		{DiscoveryManagerNVM, filepath.Join(xdg, "nvm"), "nvm_xdg"},
		{DiscoveryManagerFNM, fnmRoot, "fnm"},
		{DiscoveryManagerVolta, voltaRoot, "volta"},
		{DiscoveryManagerASDF, asdfRoot, "asdf"},
	}
	seen := map[string]struct{}{}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return
		}
		if root.root == "" || !filepath.IsAbs(root.root) {
			continue
		}
		requestedRoot := filepath.Clean(root.root)
		scanRoot := requestedRoot
		if canonical, ok := canonicalExistingDirectory(requestedRoot); ok {
			scanRoot = canonical
		}
		if _, ok := seen[scanRoot]; ok {
			continue
		}
		seen[scanRoot] = struct{}{}
		if !allowedTierBManagerRoot(scanRoot, c.options.HomeDir) {
			c.coverage = append(c.coverage, DiscoveryCoverage{Source: root.source, Root: requestedRoot, State: "refused"})
			continue
		}
		_ = c.scanRoot(ctx, scanRoot, root.source, root.manager, scanRoot, DiscoveryTierB, "trusted")
	}
}

func (c *discoveryCollector) collectTierC(ctx context.Context) error {
	roots := c.options.BroadRoots
	if len(roots) == 0 {
		roots = []string{c.options.HomeDir, "/opt", "/usr/local"}
	}
	seen := map[string]struct{}{}
	for _, root := range roots {
		if err := ctx.Err(); err != nil {
			return err
		}
		requestedRoot := filepath.Clean(root)
		clean, ok := canonicalExistingDirectory(requestedRoot)
		if !ok {
			c.coverage = append(c.coverage, DiscoveryCoverage{Source: "broad", Root: requestedRoot, State: "refused"})
			continue
		}
		if _, ok := seen[clean]; ok {
			continue
		}
		seen[clean] = struct{}{}
		if !allowedBroadRoot(clean, c.options.HomeDir) || !isLocalVolume(clean) {
			c.coverage = append(c.coverage, DiscoveryCoverage{Source: "broad", Root: clean, State: "refused"})
			continue
		}
		if err := c.scanRoot(ctx, clean, "broad", c.managerForPath(clean), "", DiscoveryTierC, "broad"); err != nil {
			if errors.Is(err, ErrDiscoveryLimit) {
				c.truncated = true
				return err
			}
			return err
		}
	}
	return nil
}

func (c *discoveryCollector) addLauncherSeed(launcher, source string, manager DiscoveryManager, managerRoot string, tier DiscoveryTier, confidence string) {
	if launcher == "" || !filepath.IsAbs(launcher) || filepath.Clean(launcher) != launcher {
		return
	}
	canonicalLauncher, ok := canonicalLeafPath(launcher)
	if !ok {
		return
	}
	resolved, fingerprint, _, err := verifyDiscoveryExecutable(canonicalLauncher)
	if err != nil {
		return
	}
	root, ok := packageRootFromBinTarget(resolved)
	if !ok {
		if len(c.unmatchedLaunchers) >= maximumUnmatchedLaunchers {
			c.truncated = true
			c.launcherEvidenceTruncated = true
			return
		}
		c.unmatchedLaunchers = append(c.unmatchedLaunchers, unmatchedLauncherEvidence{
			launcher:    canonicalLauncher,
			fingerprint: fingerprint,
			source:      source,
			manager:     manager,
			managerRoot: managerRoot,
			tier:        tier,
			confidence:  confidence,
		})
		return
	}
	c.seeds = append(c.seeds, discoverySeed{
		packageRoot:      root,
		requiredLauncher: canonicalLauncher,
		source:           source,
		manager:          manager,
		managerRoot:      managerRoot,
		tier:             tier,
		confidence:       confidence,
		homebrewPrefix:   c.options.HomebrewPrefix,
	})
}

func (c *discoveryCollector) addPrefixSeed(prefix, source string, manager DiscoveryManager, managerRoot string, tier DiscoveryTier, confidence string) {
	if prefix == "" || !filepath.IsAbs(prefix) {
		return
	}
	root := packageRootForPrefix(filepath.Clean(prefix))
	if _, err := os.Lstat(root); err != nil {
		return
	}
	c.seeds = append(c.seeds, discoverySeed{
		packageRoot:    root,
		source:         source,
		manager:        manager,
		managerRoot:    managerRoot,
		tier:           tier,
		confidence:     confidence,
		homebrewPrefix: c.options.HomebrewPrefix,
	})
	c.coverage = append(c.coverage, DiscoveryCoverage{Source: source, Root: filepath.Clean(prefix), State: "scanned"})
}

func (c *discoveryCollector) scanRoot(ctx context.Context, root, source string, manager DiscoveryManager, managerRoot string, tier DiscoveryTier, confidence string) error {
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return ErrUnsafeNPMInstallation
	}
	requestedRoot := root
	info, err := os.Lstat(requestedRoot)
	if errors.Is(err, os.ErrNotExist) {
		c.coverage = append(c.coverage, DiscoveryCoverage{Source: source, Root: requestedRoot, State: "absent"})
		return nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		c.coverage = append(c.coverage, DiscoveryCoverage{Source: source, Root: requestedRoot, State: "refused"})
		return nil
	}
	resolvedRoot, err := filepath.EvalSymlinks(requestedRoot)
	if err != nil || !filepath.IsAbs(resolvedRoot) || filepath.Clean(resolvedRoot) != resolvedRoot || resolvedRoot != requestedRoot {
		c.coverage = append(c.coverage, DiscoveryCoverage{Source: source, Root: requestedRoot, State: "refused"})
		return nil
	}
	deviceID := c.deviceID
	if deviceID == nil {
		deviceID = discoveryDeviceID
	}
	rootDevice, ok := deviceID(info)
	if !ok {
		c.coverage = append(c.coverage, DiscoveryCoverage{Source: source, Root: requestedRoot, State: "refused"})
		return nil
	}
	root = resolvedRoot
	state := "scanned"
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entryInfo, infoErr := entry.Info()
		if infoErr != nil {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		entryDevice, ok := deviceID(entryInfo)
		if !ok || entryDevice != rootDevice {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		c.entries++
		if c.entries > c.options.MaxEntries {
			c.truncated = true
			return ErrDiscoveryLimit
		}
		relative, relErr := filepath.Rel(root, path)
		if relErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return filepath.SkipDir
		}
		depth := 0
		if relative != "." {
			depth = strings.Count(relative, string(filepath.Separator)) + 1
		}
		if entry.IsDir() {
			if entry.Type()&os.ModeSymlink != 0 || depth > c.options.MaxDepth || skippedDiscoveryDirectory(entry.Name(), path) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 || entry.Name() != "package.json" {
			return nil
		}
		packageRoot := filepath.Dir(path)
		if !hasOpenCodexPackageSuffix(packageRoot) {
			return nil
		}
		c.seeds = append(c.seeds, discoverySeed{
			packageRoot:    packageRoot,
			source:         source,
			manager:        manager,
			managerRoot:    managerRoot,
			tier:           tier,
			confidence:     confidence,
			homebrewPrefix: c.options.HomebrewPrefix,
		})
		return nil
	})
	if errors.Is(err, ErrDiscoveryLimit) {
		state = "truncated"
	}
	c.coverage = append(c.coverage, DiscoveryCoverage{Source: source, Root: root, State: state})
	return err
}

func validateNPMInstallation(ctx context.Context, seed discoverySeed) (NPMInstallation, error) {
	if err := ctx.Err(); err != nil {
		return NPMInstallation{}, err
	}
	root := filepath.Clean(seed.packageRoot)
	if !filepath.IsAbs(root) || !hasOpenCodexPackageSuffix(root) {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return NPMInstallation{}, err
	}
	parentPermissionsRelaxed, parentErr := validateDiscoveryParentChain(root)
	if resolvedRoot != root || parentErr != nil {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	rootDevice, rootInode, ok := discoveryFileIdentity(rootInfo)
	if !ok {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}

	manifestPath := filepath.Join(root, "package.json")
	manifestBytes, manifestInfo, manifestRelaxed, err := readDiscoveryRegularFile(manifestPath, maxPackageManifestBytes)
	if err != nil {
		return NPMInstallation{}, err
	}
	parentPermissionsRelaxed = parentPermissionsRelaxed || manifestRelaxed
	manifest, bins, err := decodeOpenCodexManifest(manifestBytes)
	if err != nil {
		return NPMInstallation{}, err
	}
	targetRelative := bins["ocx"]
	if bins["opencodex"] != targetRelative {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	targetPath := filepath.Join(root, filepath.FromSlash(targetRelative))
	resolvedTarget, targetFingerprint, targetFile, targetRelaxed, err := openDiscoveryExecutable(targetPath)
	if err != nil {
		return NPMInstallation{}, err
	}
	parentPermissionsRelaxed = parentPermissionsRelaxed || targetRelaxed
	targetInfo, statErr := targetFile.Stat()
	_ = targetFile.Close()
	if statErr != nil || !pathContainedBy(root, resolvedTarget) {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}

	prefix, ok := prefixFromPackageRoot(root)
	if !ok {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	warnings := make([]string, 0, 6)
	launchers := make([]string, 0, 2)
	fixedLauncherStates := make([]string, 0, 2)
	fixedLauncherConflict := false
	externalLauncher := false
	for _, name := range []string{"ocx", "opencodex"} {
		launcher := filepath.Join(prefix, "bin", name)
		launcherInfo, lstatErr := os.Lstat(launcher)
		if errors.Is(lstatErr, os.ErrNotExist) {
			warnings = append(warnings, "launcher_missing_"+name)
			fixedLauncherStates = append(fixedLauncherStates, "fixed_launcher:"+name+":absent")
			continue
		}
		if lstatErr != nil {
			warnings = append(warnings, "launcher_mismatch_"+name)
			fixedLauncherStates = append(fixedLauncherStates, "fixed_launcher:"+name+":occupied_unreadable")
			fixedLauncherConflict = true
			continue
		}
		resolved, launcherFingerprint, launcherRelaxed, verifyErr := verifyDiscoveryExecutable(launcher)
		if verifyErr != nil {
			state := "fixed_launcher:" + name + ":occupied_unsafe:" + strconv.FormatUint(uint64(launcherInfo.Mode()), 10)
			if launcherUID, ok := ownerUID(launcherInfo); ok {
				state += ":" + strconv.FormatUint(uint64(launcherUID), 10)
			}
			warnings = append(warnings, "launcher_mismatch_"+name)
			fixedLauncherStates = append(fixedLauncherStates, state)
			fixedLauncherConflict = true
			continue
		}
		if resolved != resolvedTarget {
			warnings = append(warnings, "launcher_mismatch_"+name)
			fixedLauncherStates = append(fixedLauncherStates, "fixed_launcher:"+name+":mismatched:"+resolved+":"+launcherFingerprint)
			fixedLauncherConflict = true
			continue
		}
		parentPermissionsRelaxed = parentPermissionsRelaxed || launcherRelaxed
		fixedLauncherStates = append(fixedLauncherStates, "fixed_launcher:"+name+":matched:"+launcherFingerprint)
		launchers = append(launchers, launcher)
	}
	if seed.requiredLauncher != "" {
		resolved, launcherFingerprint, launcherRelaxed, verifyErr := verifyDiscoveryExecutable(seed.requiredLauncher)
		if verifyErr != nil || launcherFingerprint != targetFingerprint || !sameDiscoveryExecutableFile(resolved, resolvedTarget) {
			return NPMInstallation{}, ErrUnsafeNPMInstallation
		}
		parentPermissionsRelaxed = parentPermissionsRelaxed || launcherRelaxed
		if seed.requiredLauncher != resolvedTarget {
			fixedOCX := filepath.Join(prefix, "bin", "ocx")
			fixedOpenCodex := filepath.Join(prefix, "bin", "opencodex")
			if seed.requiredLauncher != fixedOCX && seed.requiredLauncher != fixedOpenCodex {
				externalLauncher = true
			}
			launchers = append(launchers, seed.requiredLauncher)
		}
	}
	launchers = uniqueSortedStrings(launchers)

	rootUID, ok := ownerUID(rootInfo)
	if !ok {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	manifestUID, ok := ownerUID(manifestInfo)
	if !ok || manifestUID != rootUID {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	targetUID, ok := ownerUID(targetInfo)
	if !ok || targetUID != manifestUID {
		return NPMInstallation{}, ErrUnsafeNPMInstallation
	}
	userWritable := manifestUID == uint32(os.Geteuid()) && rootInfo.Mode().Perm()&0o200 != 0
	requiresElevation := manifestUID == 0 && os.Geteuid() != 0

	homebrewGuarded := seed.manager == DiscoveryManagerHomebrew &&
		seed.homebrewPrefix != "" && prefix == seed.homebrewPrefix &&
		root == packageRootForPrefix(seed.homebrewPrefix) && userWritable && !requiresElevation
	nodePath, nodeHash, npmCLI, npmHash, pairUID, pairOK := verifyNodeNPMPair(prefix)
	if homebrewGuarded {
		nodePath, nodeHash, npmCLI, npmHash, pairUID, pairOK = verifyDiscoveryNodeNPMPair(prefix)
	}
	capability := RemovalCapabilityManual
	if pairOK && pairUID == manifestUID && !requiresElevation {
		if homebrewGuarded {
			capability = RemovalCapabilityHomebrewGuardedNPM
			warnings = append(warnings, "homebrew_guard_required")
		} else {
			capability = RemovalCapabilityExactNPM
		}
	} else {
		warnings = append(warnings, "exact_npm_pair_unavailable")
	}
	if seed.manager == DiscoveryManagerVolta {
		if voltaNode, voltaNodeHash, voltaNPM, voltaNPMHash, voltaUID, ok := verifyVoltaPair(seed.managerRoot); ok && voltaUID == manifestUID && !requiresElevation {
			nodePath, nodeHash, npmCLI, npmHash = voltaNode, voltaNodeHash, voltaNPM, voltaNPMHash
			capability = RemovalCapabilityVolta
		} else {
			warnings = append(warnings, "volta_pair_unavailable")
		}
	}
	if fixedLauncherConflict {
		capability = RemovalCapabilityManual
	}
	if parentPermissionsRelaxed && capability != RemovalCapabilityHomebrewGuardedNPM {
		warnings = append(warnings, "writable_parent_chain")
		capability = RemovalCapabilityManual
	}
	if externalLauncher {
		warnings = append(warnings, "external_launcher_requires_manual_removal")
		capability = RemovalCapabilityManual
	}

	var cliEntry, cliEntryHash, bunPath, bunHash, packageTreeHash, npmTreeHash string
	if capability == RemovalCapabilityExactNPM || capability == RemovalCapabilityHomebrewGuardedNPM {
		allowRelaxed := capability == RemovalCapabilityHomebrewGuardedNPM
		cliEntry = filepath.Join(root, "src", "cli", "index.ts")
		cliBytes, cliInfo, cliRelaxed, cliErr := readDiscoveryRegularFile(cliEntry, maxExecutableBytes)
		bunCandidate, bunCandidateHash, bunOK := discoverBundledBunForDiscovery(root, allowRelaxed)
		npmRoot, npmRootOK := npmRootFromCLI(npmCLI)
		packageClosure, packageErr := stableExecutionTreeFingerprintContext(ctx, root)
		npmClosure, npmErr := stableExecutionTreeFingerprintContext(ctx, npmRoot)
		if homebrewGuarded {
			packageClosure, packageErr = stableExecutionTreeFingerprintWithoutExtendedACLContext(ctx, root)
			npmClosure, npmErr = stableExecutionTreeFingerprintWithoutExtendedACLContext(ctx, npmRoot)
		}
		cliUID, cliUIDOK := ownerUID(cliInfo)
		bunInfo, bunStatErr := os.Stat(bunCandidate)
		bunUID, bunUIDOK := ownerUID(bunInfo)
		if cliErr != nil || (!allowRelaxed && cliRelaxed) || !cliUIDOK || cliUID != manifestUID ||
			!bunOK || bunStatErr != nil || !bunUIDOK || bunUID != manifestUID ||
			!npmRootOK || packageErr != nil || npmErr != nil {
			if errors.Is(packageErr, errExecutionTreeExtendedACL) || errors.Is(npmErr, errExecutionTreeExtendedACL) {
				warnings = append(warnings, "extended_acl")
			} else {
				warnings = append(warnings, "execution_closure_unavailable")
			}
			capability = RemovalCapabilityManual
			cliEntry, cliEntryHash, bunPath, bunHash, packageTreeHash, npmTreeHash = "", "", "", "", "", ""
		} else {
			cliDigest := sha256.Sum256(cliBytes)
			cliEntryHash = hex.EncodeToString(cliDigest[:])
			bunPath, bunHash = bunCandidate, bunCandidateHash
			packageTreeHash, npmTreeHash = packageClosure, npmClosure
		}
	}

	if capability == RemovalCapabilityHomebrewGuardedNPM {
		aclPresent, aclErr := homebrewCandidateHasExtendedACL(
			prefix,
			append([]string{root, resolvedTarget, cliEntry, bunPath, nodePath, npmCLI}, launchers...)...,
		)
		if aclErr != nil || aclPresent {
			warnings = append(warnings, "extended_acl")
			capability = RemovalCapabilityManual
		}
	}

	teardownCapability, dataCapability, teardownCompatibility, teardownAdapterID, teardownProof :=
		inspectTeardownCompatibility(ctx, root, manifest)
	if teardownCapability != TeardownCapabilityRelayPreserveV1 {
		warnings = append(warnings, "teardown_"+teardownCompatibility)
		capability = RemovalCapabilityManual
	}

	// Native restore authority is deliberately computed independently from
	// automatic package removal. In particular, a Homebrew prefix may have a
	// writable parent chain while the package tree itself is safe to fingerprint
	// and copy into a private execution snapshot.
	nativeRestoreProof, _ := discoverNativeRestoreExecutionProof(ctx, root, manifestUID)

	fingerprintHash := sha256.New()
	writeFingerprintField := func(value string) {
		_, _ = fingerprintHash.Write([]byte(value))
		_, _ = fingerprintHash.Write([]byte{0})
	}
	// Bind every executable and permission fact needed for automatic removal.
	// The candidate ID/fingerprint is only a stale-selection witness; removal
	// still rediscovers and revalidates immediately before every child process.
	for _, value := range []string{
		"opencodex-npm-removal-proof-v6",
		string(seed.manager),
		root,
		prefix,
		strconv.FormatUint(rootDevice, 10),
		strconv.FormatUint(rootInode, 10),
		strconv.FormatUint(uint64(rootUID), 10),
		strconv.FormatUint(uint64(rootInfo.Mode()), 10),
		strconv.FormatUint(uint64(manifestUID), 10),
		strconv.FormatUint(uint64(manifestInfo.Mode()), 10),
		resolvedTarget,
		targetFingerprint,
		cliEntry,
		cliEntryHash,
		bunPath,
		bunHash,
		packageTreeHash,
		npmTreeHash,
		strconv.FormatUint(uint64(targetUID), 10),
		strconv.FormatUint(uint64(targetInfo.Mode()), 10),
		nodePath,
		nodeHash,
		npmCLI,
		npmHash,
		strconv.FormatUint(uint64(pairUID), 10),
		string(capability),
		string(teardownCapability),
		string(dataCapability),
		teardownCompatibility,
		teardownAdapterID,
		teardownProfileFingerprint(teardownProof),
		strconv.FormatBool(userWritable),
		strconv.FormatBool(requiresElevation),
		strconv.FormatBool(homebrewGuarded),
		strconv.FormatBool(externalLauncher),
		strconv.FormatBool(fixedLauncherConflict),
	} {
		writeFingerprintField(value)
	}
	for _, state := range fixedLauncherStates {
		writeFingerprintField(state)
	}
	for _, launcher := range launchers {
		writeFingerprintField("launcher:" + launcher)
	}
	_, _ = fingerprintHash.Write(manifestBytes)
	fingerprint := hex.EncodeToString(fingerprintHash.Sum(nil))
	idHash := sha256.Sum256([]byte(string(seed.manager) + "\x00" + root + "\x00" + fingerprint))
	nativeRestoreCapability := NativeRestoreCapability("")
	nativeRestoreFingerprint := ""
	if nativeRestoreProof != nil {
		nativeRestoreFingerprint = nativeRestoreProof.fingerprint(fingerprint)
		if nativeRestoreFingerprint != "" {
			nativeRestoreCapability = NativeRestoreCapabilityVerifiedSnapshot
		}
	}

	return NPMInstallation{
		ID:                       hex.EncodeToString(idHash[:12]),
		Tier:                     seed.tier,
		Source:                   seed.source,
		Manager:                  seed.manager,
		Prefix:                   prefix,
		PackageRoot:              root,
		Version:                  manifest.Version,
		Executable:               resolvedTarget,
		ExecutableSHA256:         targetFingerprint,
		CLIEntry:                 cliEntry,
		CLIEntrySHA256:           cliEntryHash,
		BunExecutable:            bunPath,
		BunSHA256:                bunHash,
		PackageTreeSHA256:        packageTreeHash,
		NPMTreeSHA256:            npmTreeHash,
		Launchers:                launchers,
		NodeExecutable:           nodePath,
		NodeSHA256:               nodeHash,
		NPMCLI:                   npmCLI,
		NPMCLISHA256:             npmHash,
		Confidence:               seed.confidence,
		RemovalCapability:        capability,
		HomebrewGuardRequired:    capability == RemovalCapabilityHomebrewGuardedNPM,
		TeardownCapability:       teardownCapability,
		DataCapability:           dataCapability,
		TeardownCompatibility:    teardownCompatibility,
		TeardownAdapterID:        teardownAdapterID,
		NativeRestoreCapability:  nativeRestoreCapability,
		NativeRestoreFingerprint: nativeRestoreFingerprint,
		UserWritable:             userWritable,
		RequiresElevation:        requiresElevation,
		Fingerprint:              fingerprint,
		Warnings:                 uniqueSortedStrings(warnings),
		packageRootDevice:        rootDevice,
		packageRootInode:         rootInode,
		nativeRestoreProof:       nativeRestoreProof,
		teardownProof:            teardownProof,
	}, nil
}

type npmPackageManifest struct {
	Name    string          `json:"name"`
	Version string          `json:"version"`
	Bin     json.RawMessage `json:"bin"`
}

func decodeOpenCodexManifest(payload []byte) (npmPackageManifest, map[string]string, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var manifest npmPackageManifest
	if err := decoder.Decode(&manifest); err != nil {
		return npmPackageManifest{}, nil, ErrUnsafeNPMInstallation
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return npmPackageManifest{}, nil, ErrUnsafeNPMInstallation
	}
	if manifest.Name != OpenCodexPackageName || manifest.Version == "" || len(manifest.Version) > 128 || len(manifest.Bin) == 0 {
		return npmPackageManifest{}, nil, ErrUnsafeNPMInstallation
	}
	var bins map[string]string
	if err := json.Unmarshal(manifest.Bin, &bins); err != nil || len(bins) != 2 {
		return npmPackageManifest{}, nil, ErrUnsafeNPMInstallation
	}
	for _, name := range []string{"ocx", "opencodex"} {
		value, ok := bins[name]
		if !ok {
			return npmPackageManifest{}, nil, ErrUnsafeNPMInstallation
		}
		normalized, ok := normalizePackageRelativePath(value)
		if !ok {
			return npmPackageManifest{}, nil, ErrUnsafeNPMInstallation
		}
		bins[name] = normalized
	}
	if bins["ocx"] != bins["opencodex"] {
		return npmPackageManifest{}, nil, ErrUnsafeNPMInstallation
	}
	return manifest, bins, nil
}

func normalizePackageRelativePath(value string) (string, bool) {
	if value == "" || filepath.IsAbs(value) || strings.HasPrefix(value, "/") || strings.Contains(value, "\\") || strings.ContainsRune(value, '\x00') {
		return "", false
	}
	if strings.HasPrefix(value, "./") {
		value = strings.TrimPrefix(value, "./")
	}
	if value == "" {
		return "", false
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return "", false
		}
	}
	clean := filepath.ToSlash(filepath.Clean(filepath.FromSlash(value)))
	if clean != value || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", false
	}
	return clean, true
}

func readDiscoveryRegularFile(path string, maximum int64) ([]byte, os.FileInfo, bool, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, nil, false, ErrUnsafeNPMInstallation
	}
	relaxed, err := validateDiscoveryParentChain(filepath.Dir(path))
	if err != nil {
		return nil, nil, false, ErrUnsafeNPMInstallation
	}
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, false, err
	}
	if !safeRegularInfo(pathInfo) {
		return nil, nil, false, ErrUnsafeNPMInstallation
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, false, ErrUnsafeNPMInstallation
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !safeRegularInfo(info) || !os.SameFile(pathInfo, info) {
		return nil, nil, false, ErrUnsafeNPMInstallation
	}
	payload, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil || int64(len(payload)) > maximum {
		return nil, nil, false, ErrUnsafeNPMInstallation
	}
	return payload, info, relaxed, nil
}

func verifyDiscoveryExecutable(path string) (string, string, bool, error) {
	resolved, fingerprint, file, relaxed, err := openDiscoveryExecutable(path)
	if file != nil {
		_ = file.Close()
	}
	return resolved, fingerprint, relaxed, err
}

func openDiscoveryExecutable(path string) (string, string, *os.File, bool, error) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	pathRelaxed, err := validateDiscoveryParentChain(filepath.Dir(path))
	if err != nil {
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(resolved) || filepath.Clean(resolved) != resolved {
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	targetRelaxed, err := validateDiscoveryParentChain(filepath.Dir(resolved))
	if err != nil {
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	pathInfo, err := os.Stat(path)
	if err != nil || !safeExecutableInfo(pathInfo) {
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	resolvedInfo, err := os.Lstat(resolved)
	if err != nil || !safeExecutableInfo(resolvedInfo) {
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	file, err := os.Open(resolved)
	if err != nil {
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	info, err := file.Stat()
	if err != nil || !safeExecutableInfo(info) || !os.SameFile(pathInfo, info) || !os.SameFile(resolvedInfo, info) {
		_ = file.Close()
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	fingerprint, err := fingerprintOpenExecutable(file)
	if err != nil {
		_ = file.Close()
		return "", "", nil, false, ErrUnsafeNPMInstallation
	}
	return resolved, fingerprint, file, pathRelaxed || targetRelaxed, nil
}

func safeRegularInfo(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular() && info.Mode().Perm()&0o022 == 0 && ownedByCurrentUserOrRoot(info)
}

func validateDiscoveryParentChain(directory string) (bool, error) {
	if directory == "" || !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return false, ErrUnsafeNPMInstallation
	}
	currentUID := uint32(os.Geteuid())
	relaxed := false
	for {
		info, err := os.Lstat(directory)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return false, ErrUnsafeNPMInstallation
		}
		uid, ok := ownerUID(info)
		if !ok || (uid != currentUID && uid != 0) {
			return false, ErrUnsafeNPMInstallation
		}
		permissions := info.Mode().Perm()
		if permissions&0o022 != 0 {
			if uid == 0 && info.Mode()&os.ModeSticky != 0 {
				// Preserve the strict validator's root-owned sticky-directory exception.
			} else if permissions&0o002 == 0 && permissions&0o020 != 0 && uid == currentUID && currentUID != 0 {
				relaxed = true
			} else {
				return false, ErrUnsafeNPMInstallation
			}
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return relaxed, nil
		}
		directory = parent
	}
}

func verifyNodeNPMPair(prefix string) (string, string, string, string, uint32, bool) {
	node, nodeHash, err := VerifyExecutable(filepath.Join(prefix, "bin", "node"))
	if err != nil {
		return "", "", "", "", 0, false
	}
	npmCLI, npmHash, err := VerifyExecutable(filepath.Join(prefix, "bin", "npm"))
	if err != nil {
		return "", "", "", "", 0, false
	}
	nodeInfo, err := os.Stat(node)
	if err != nil {
		return "", "", "", "", 0, false
	}
	npmInfo, err := os.Stat(npmCLI)
	if err != nil {
		return "", "", "", "", 0, false
	}
	nodeUID, nodeOK := ownerUID(nodeInfo)
	npmUID, npmOK := ownerUID(npmInfo)
	if !nodeOK || !npmOK || nodeUID != npmUID {
		return "", "", "", "", 0, false
	}
	return node, nodeHash, npmCLI, npmHash, nodeUID, true
}

func verifyDiscoveryNodeNPMPair(prefix string) (string, string, string, string, uint32, bool) {
	node, nodeHash, _, err := verifyDiscoveryExecutable(filepath.Join(prefix, "bin", "node"))
	if err != nil || !pathContainedBy(prefix, node) {
		return "", "", "", "", 0, false
	}
	npmCLI, npmHash, _, err := verifyDiscoveryExecutable(filepath.Join(prefix, "bin", "npm"))
	if err != nil || !pathContainedBy(prefix, npmCLI) {
		return "", "", "", "", 0, false
	}
	nodeInfo, err := os.Stat(node)
	if err != nil {
		return "", "", "", "", 0, false
	}
	npmInfo, err := os.Stat(npmCLI)
	if err != nil {
		return "", "", "", "", 0, false
	}
	nodeUID, nodeOK := ownerUID(nodeInfo)
	npmUID, npmOK := ownerUID(npmInfo)
	if !nodeOK || !npmOK || nodeUID != npmUID {
		return "", "", "", "", 0, false
	}
	return node, nodeHash, npmCLI, npmHash, nodeUID, true
}

func verifyVoltaPair(root string) (string, string, string, string, uint32, bool) {
	if root == "" || !filepath.IsAbs(root) {
		return "", "", "", "", 0, false
	}
	return verifyNodeNPMPair(filepath.Clean(root))
}

func packageRootForPrefix(prefix string) string {
	return filepath.Join(prefix, "lib", "node_modules", "@bitkyc08", "opencodex")
}

func packageRootFromBinTarget(target string) (string, bool) {
	current := filepath.Dir(target)
	for range 5 {
		if hasOpenCodexPackageSuffix(current) {
			return current, true
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", false
}

func prefixFromPackageRoot(root string) (string, bool) {
	if !hasOpenCodexPackageSuffix(root) {
		return "", false
	}
	scope := filepath.Dir(root)
	nodeModules := filepath.Dir(scope)
	lib := filepath.Dir(nodeModules)
	if filepath.Base(scope) != "@bitkyc08" || filepath.Base(nodeModules) != "node_modules" || filepath.Base(lib) != "lib" {
		return "", false
	}
	prefix := filepath.Dir(lib)
	return prefix, filepath.IsAbs(prefix) && filepath.Clean(prefix) == prefix
}

func hasOpenCodexPackageSuffix(root string) bool {
	return filepath.Base(root) == "opencodex" && filepath.Base(filepath.Dir(root)) == "@bitkyc08" && filepath.Base(filepath.Dir(filepath.Dir(root))) == "node_modules"
}

func pathContainedBy(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func ownerUID(info os.FileInfo) (uint32, bool) {
	if info == nil {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return stat.Uid, true
}

func discoveryFileIdentity(info os.FileInfo) (uint64, uint64, bool) {
	if info == nil {
		return 0, 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return uint64(stat.Dev), uint64(stat.Ino), true
}

func discoveryDeviceID(info os.FileInfo) (uint64, bool) {
	device, _, ok := discoveryFileIdentity(info)
	return device, ok
}

func (c *discoveryCollector) managerForPrefix(prefix string) DiscoveryManager {
	clean := filepath.Clean(prefix)
	if c.options.HomebrewPrefix != "" && clean == c.options.HomebrewPrefix {
		return DiscoveryManagerHomebrew
	}
	return managerForPrefix(clean)
}

func (c *discoveryCollector) managerForPath(path string) DiscoveryManager {
	clean := filepath.Clean(path)
	if c.options.HomebrewPrefix != "" &&
		(clean == c.options.HomebrewPrefix || pathContainedBy(c.options.HomebrewPrefix, clean)) {
		return DiscoveryManagerHomebrew
	}
	return managerForPath(clean)
}
func managerForPrefix(prefix string) DiscoveryManager {
	if prefix == "/opt/homebrew" || prefix == "/usr/local" {
		return DiscoveryManagerHomebrew
	}
	return DiscoveryManagerNPM
}

func managerForPath(path string) DiscoveryManager {
	value := filepath.ToSlash(path)
	switch {
	case strings.Contains(value, "/.nvm/") || strings.Contains(value, "/nvm/"):
		return DiscoveryManagerNVM
	case strings.Contains(value, "/fnm/") || strings.Contains(value, "/Application Support/fnm/"):
		return DiscoveryManagerFNM
	case strings.Contains(value, "/.volta/"):
		return DiscoveryManagerVolta
	case strings.Contains(value, "/.asdf/"):
		return DiscoveryManagerASDF
	case strings.HasPrefix(value, "/opt/homebrew/") || strings.HasPrefix(value, "/usr/local/"):
		return DiscoveryManagerHomebrew
	default:
		return DiscoveryManagerNPM
	}
}

func canonicalExistingDirectory(path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", false
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || !filepath.IsAbs(resolved) {
		return "", false
	}
	return filepath.Clean(resolved), true
}

func launcherEvidenceMatchesCandidate(evidence unmatchedLauncherEvidence, candidate NPMInstallation) bool {
	resolved, fingerprint, _, err := verifyDiscoveryExecutable(evidence.launcher)
	return err == nil && fingerprint == evidence.fingerprint && fingerprint == candidate.ExecutableSHA256 &&
		sameDiscoveryExecutableFile(resolved, candidate.Executable)
}

func sameDiscoveryExecutableFile(left, right string) bool {
	if left == "" || right == "" || !filepath.IsAbs(left) || !filepath.IsAbs(right) {
		return false
	}
	leftFile, err := os.Open(left)
	if err != nil {
		return false
	}
	defer leftFile.Close()
	rightFile, err := os.Open(right)
	if err != nil {
		return false
	}
	defer rightFile.Close()
	leftInfo, leftErr := leftFile.Stat()
	rightInfo, rightErr := rightFile.Stat()
	return leftErr == nil && rightErr == nil && safeRegularInfo(leftInfo) && safeRegularInfo(rightInfo) && os.SameFile(leftInfo, rightInfo)
}

func canonicalLeafPath(path string) (string, bool) {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", false
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil || !filepath.IsAbs(parent) {
		return "", false
	}
	canonical := filepath.Join(parent, filepath.Base(path))
	if _, err := os.Lstat(canonical); err != nil {
		return "", false
	}
	return canonical, true
}

func allowedTierBManagerRoot(root, home string) bool {
	return filepath.IsAbs(root) && filepath.Clean(root) == root && root != home && pathContainedBy(home, root)
}

func allowedBroadRoot(root, home string) bool {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || root == "/" || root == "/System" || root == "/Users" || root == "/Volumes" {
		return false
	}
	if pathContainedBy(home, root) {
		return true
	}
	return root == "/opt" || pathContainedBy("/opt", root) || root == "/usr/local" || pathContainedBy("/usr/local", root)
}

func skippedDiscoveryDirectory(name, path string) bool {
	if name == ".Trash" || name == ".git" || name == "CloudStorage" || name == "Backups.backupdb" || name == ".MobileBackups" {
		return true
	}
	value := filepath.ToSlash(path)
	return strings.Contains(value, "/Library/CloudStorage/") || strings.Contains(value, "/.timemachine/")
}

func mergeInstallations(values []NPMInstallation) NPMInstallation {
	if len(values) == 0 {
		return NPMInstallation{}
	}
	values = append([]NPMInstallation(nil), values...)
	sort.Slice(values, func(i, j int) bool {
		if tierRank(values[i].Tier) != tierRank(values[j].Tier) {
			return tierRank(values[i].Tier) < tierRank(values[j].Tier)
		}
		if values[i].Source != values[j].Source {
			return values[i].Source < values[j].Source
		}
		if values[i].Manager != values[j].Manager {
			return values[i].Manager < values[j].Manager
		}
		return values[i].Fingerprint < values[j].Fingerprint
	})
	merged := values[0]
	proofs := make([]string, 0, len(values))
	launchers := make([]string, 0, len(values)*2)
	warnings := make([]string, 0, len(values)*2)
	capability := merged.RemovalCapability
	userWritable := true
	requiresElevation := false
	nativeRestoreProof := merged.nativeRestoreProof
	nativeRestoreProofConsistent := nativeRestoreProof != nil
	teardownProof := merged.teardownProof
	teardownProofConsistent := teardownProof != nil
	for _, candidate := range values {
		proofs = append(proofs, candidate.Fingerprint)
		launchers = append(launchers, candidate.Launchers...)
		warnings = append(warnings, candidate.Warnings...)
		userWritable = userWritable && candidate.UserWritable
		requiresElevation = requiresElevation || candidate.RequiresElevation
		if candidate.RemovalCapability == RemovalCapabilityManual || candidate.RemovalCapability != capability {
			capability = RemovalCapabilityManual
		}
		if candidate.packageRootDevice != merged.packageRootDevice || candidate.packageRootInode != merged.packageRootInode {
			warnings = append(warnings, "package_identity_conflict")
			capability = RemovalCapabilityManual
		}
		if !sameNativeRestoreProofValues(nativeRestoreProof, candidate.nativeRestoreProof) {
			nativeRestoreProofConsistent = false
		}
		if candidate.TeardownCapability != merged.TeardownCapability ||
			candidate.DataCapability != merged.DataCapability ||
			candidate.TeardownCompatibility != merged.TeardownCompatibility ||
			candidate.TeardownAdapterID != merged.TeardownAdapterID ||
			!sameTeardownProofValues(teardownProof, candidate.teardownProof) {
			teardownProofConsistent = false
			capability = RemovalCapabilityManual
			merged.TeardownCapability = TeardownCapabilityNone
			merged.TeardownCompatibility = teardownCompatibilityConflict
			merged.TeardownAdapterID = ""
		}
	}
	proofs = uniqueSortedStrings(proofs)
	merged.Launchers = uniqueSortedStrings(launchers)
	merged.Warnings = uniqueSortedStrings(warnings)
	merged.RemovalCapability = capability
	merged.UserWritable = userWritable
	merged.RequiresElevation = requiresElevation

	hash := sha256.New()
	writeField := func(value string) {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	writeField("opencodex-npm-aggregate-proof-v2")
	writeField(merged.PackageRoot)
	for _, proof := range proofs {
		writeField("candidate:" + proof)
	}
	for _, launcher := range merged.Launchers {
		writeField("launcher:" + launcher)
	}
	for _, warning := range merged.Warnings {
		writeField("warning:" + warning)
	}
	writeField("capability:" + string(merged.RemovalCapability))
	writeField("teardown-capability:" + string(merged.TeardownCapability))
	writeField("data-capability:" + string(merged.DataCapability))
	writeField("teardown-compatibility:" + merged.TeardownCompatibility)
	writeField("teardown-adapter:" + merged.TeardownAdapterID)
	writeField("teardown-profile:" + teardownProfileFingerprint(teardownProof))
	writeField(strconv.FormatBool(merged.UserWritable))
	writeField(strconv.FormatBool(merged.RequiresElevation))
	merged.Fingerprint = hex.EncodeToString(hash.Sum(nil))
	idHash := sha256.Sum256([]byte("opencodex-npm-aggregate-id-v2\x00" + merged.PackageRoot + "\x00" + merged.Fingerprint))
	merged.ID = hex.EncodeToString(idHash[:12])
	merged.NativeRestoreCapability = ""
	merged.NativeRestoreFingerprint = ""
	merged.nativeRestoreProof = nil
	if nativeRestoreProofConsistent && nativeRestoreProof != nil {
		if restoreFingerprint := nativeRestoreProof.fingerprint(merged.Fingerprint); restoreFingerprint != "" {
			merged.NativeRestoreCapability = NativeRestoreCapabilityVerifiedSnapshot
			merged.NativeRestoreFingerprint = restoreFingerprint
			merged.nativeRestoreProof = nativeRestoreProof
		}
	}
	merged.teardownProof = nil
	if teardownProofConsistent && teardownProof != nil && merged.TeardownCapability == TeardownCapabilityRelayPreserveV1 {
		merged.teardownProof = teardownProof
	}
	return merged
}

func sameTeardownProofValues(left, right *teardownExecutionProof) bool {
	if left == nil || right == nil {
		return left == right
	}
	return left.adapterID == right.adapterID &&
		left.artifactVariant == right.artifactVariant &&
		left.profileFingerprint == right.profileFingerprint &&
		left.reviewedClosureSHA256 == right.reviewedClosureSHA256
}

func tierRank(tier DiscoveryTier) int {
	switch tier {
	case DiscoveryTierA:
		return 0
	case DiscoveryTierB:
		return 1
	default:
		return 2
	}
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func boundedCoverage(values []DiscoveryCoverage) []DiscoveryCoverage {
	if len(values) > maximumDiscoveryCoverage {
		values = values[:maximumDiscoveryCoverage]
	}
	return values
}
