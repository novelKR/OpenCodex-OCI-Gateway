package containerruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/runtimemanifest"
)

const (
	appleContainerExecutable   = "/usr/local/bin/container"
	codesignExecutable         = "/usr/bin/codesign"
	pkgutilExecutable          = "/usr/sbin/pkgutil"
	swVersExecutable           = "/usr/bin/sw_vers"
	appleCLIIdentifier         = "com.apple.container.cli"
	appleCLITeamIdentifier     = "UPBK2H6LZM"
	applePackageReceipt        = "com.apple.container-installer"
	imageSourceURL             = "https://github.com/novelKR/OpenCodex-OCI-Gateway"
	appleCLISigningRequirement = `anchor apple generic and identifier "com.apple.container.cli" and certificate 1[field.1.2.840.113635.100.6.2.6] exists and certificate leaf[field.1.2.840.113635.100.6.1.13] exists and certificate leaf[subject.OU] = "UPBK2H6LZM"`
)

const (
	labelOwner        = "io.github.novelkr.opencodex.runtime.owner"
	labelInstallation = "io.github.novelkr.opencodex.runtime.installation"
	labelOperation    = "io.github.novelkr.opencodex.runtime.operation"
	labelManifest     = "io.github.novelkr.opencodex.runtime.manifest"
	labelIndexDigest  = "io.github.novelkr.opencodex.runtime.index-digest"
	labelGeneration   = "io.github.novelkr.opencodex.runtime.state-generation"
)

// runtimeCanaryNetworkName is empty in production builds. The trusted Apple
// Silicon lifecycle job sets it with Go's -X linker flag so the real bundled
// manager can be exercised on the job's owner-labelled, host-only network
// without turning network selection into a user-configurable runtime surface.
var runtimeCanaryNetworkName string

type AppleCLI struct {
	runner          commandRunner
	probePort       func() error
	socketDirectory string
	networkName     string
}

func NewAppleCLI() *AppleCLI {
	return &AppleCLI{
		runner: systemCommandRunner{}, probePort: probeFixedHostPort,
		socketDirectory: DefaultBootstrapSocketDirectory(), networkName: runtimeCanaryNetworkName,
	}
}

func newAppleCLIWithRunner(runner commandRunner) *AppleCLI {
	return &AppleCLI{
		runner: runner, probePort: probeFixedHostPort,
		socketDirectory: DefaultBootstrapSocketDirectory(), networkName: runtimeCanaryNetworkName,
	}
}

func (a *AppleCLI) Capability(ctx context.Context, minimum, installationID string) (Capability, error) {
	result := Capability{Reason: "unsupported_platform", SystemServiceState: "unknown"}
	if a == nil || a.runner == nil || a.probePort == nil || !isSHA256(installationID) {
		return result, nil
	}
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		return result, nil
	}
	if a.networkName != "" && !validRuntimeCanaryNetworkName(a.networkName) {
		result.Reason = "apple_container_network_invalid"
		return result, nil
	}
	macOutput, err := a.runner.Run(ctx, swVersExecutable, []string{"-productVersion"}, nil, 1024)
	if err != nil {
		result.Reason = "macos_version_unavailable"
		return result, nil
	}
	result.MacOSVersion = strings.TrimSpace(string(macOutput.stdout))
	zeroCommandOutput(&macOutput)
	if !numericVersionAtLeast(result.MacOSVersion, "26.0") {
		result.Reason = "macos_version_unsupported"
		return result, nil
	}
	if !protectedExecutable(appleContainerExecutable, string(filepath.Separator), 0) {
		result.Reason = "apple_container_cli_unsafe"
		return result, nil
	}
	verification, err := a.runner.Run(ctx, codesignExecutable, []string{
		"--verify", "--strict", "--verbose=2", "-R=" + appleCLISigningRequirement, appleContainerExecutable,
	}, nil, 8<<10)
	zeroCommandOutput(&verification)
	if err != nil {
		result.Reason = "apple_container_signature_invalid"
		return result, nil
	}
	details, err := a.runner.Run(ctx, codesignExecutable, []string{"-d", "--verbose=4", appleContainerExecutable}, nil, 8<<10)
	identity := append(append([]byte(nil), details.stdout...), details.stderr...)
	if err != nil || !hasExactCodesignIdentity(identity, appleCLIIdentifier, appleCLITeamIdentifier) {
		zeroBytes(identity)
		zeroCommandOutput(&details)
		result.Reason = "apple_container_identity_invalid"
		return result, nil
	}
	zeroBytes(identity)
	zeroCommandOutput(&details)
	versionOutput, err := a.runContainer(ctx, []string{"system", "version", "--format", "json"}, 16<<10)
	if err != nil {
		result.Reason = "apple_container_version_unavailable"
		return result, nil
	}
	cliVersion, versions, valid := extractSystemVersions(versionOutput.stdout)
	zeroCommandOutput(&versionOutput)
	if !valid {
		result.Reason = "apple_container_version_invalid"
		return result, nil
	}
	result.AppleContainerVersion = cliVersion
	for _, version := range versions {
		if compareNumericVersion(version, result.AppleContainerVersion) < 0 {
			result.AppleContainerVersion = version
		}
	}
	required := MinimumAppleContainerVersion
	if minimum != "" && compareNumericVersion(minimum, required) > 0 {
		required = minimum
	}
	if !numericVersionAtLeast(result.AppleContainerVersion, required) {
		result.Reason = "apple_container_version_unsupported"
		return result, nil
	}
	receiptOutput, err := a.runner.Run(ctx, pkgutilExecutable, []string{"--pkg-info-plist", applePackageReceipt}, nil, 16<<10)
	if err != nil {
		result.Reason = "apple_container_receipt_missing"
		return result, nil
	}
	receiptID, receiptVersion, receiptOK := parsePackageReceipt(receiptOutput.stdout)
	zeroCommandOutput(&receiptOutput)
	if !receiptOK || receiptID != applePackageReceipt || receiptVersion != cliVersion || !numericVersionAtLeast(receiptVersion, required) {
		result.Reason = "apple_container_receipt_invalid"
		return result, nil
	}
	filesOutput, err := a.runner.Run(ctx, pkgutilExecutable, []string{"--files", applePackageReceipt}, nil, 16<<10)
	if err != nil || !receiptOwnsCLI(filesOutput.stdout) {
		zeroCommandOutput(&filesOutput)
		result.Reason = "apple_container_receipt_invalid"
		return result, nil
	}
	zeroCommandOutput(&filesOutput)
	statusOutput, err := a.runContainer(ctx, []string{"system", "status", "--format", "json"}, 16<<10)
	if err != nil {
		result.Reason = "apple_container_service_unavailable"
		return result, nil
	}
	result.SystemServiceState = extractServiceState(statusOutput.stdout)
	zeroCommandOutput(&statusOutput)
	if result.SystemServiceState != "running" {
		result.Reason = "apple_container_service_not_running"
		return result, nil
	}
	containerState, err := a.fixedContainerState(ctx, installationID)
	if err != nil {
		result.Reason = "apple_container_conflict_check_failed"
		return result, nil
	}
	if containerState == FixedContainerForeign {
		result.Reason = "apple_container_foreign_container"
		return result, nil
	}
	if containerState == FixedContainerUnknown {
		result.Reason = "apple_container_conflict_check_failed"
		return result, nil
	}
	if containerState == FixedContainerAbsent {
		if listenErr := a.probePort(); listenErr != nil {
			result.Reason = "apple_container_port_unavailable"
			return result, nil
		}
	}
	result.Available = true
	result.Reason = ""
	return result, nil
}

func (a *AppleCLI) Pull(ctx context.Context, reference string) error {
	if !validExactImageReference(reference) {
		return ErrInvalidRequest
	}
	output, err := a.runContainer(ctx, []string{"image", "pull", "--platform", "linux/arm64", "--progress", "none", reference}, maximumCommandOutputBytes)
	zeroCommandOutput(&output)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func (a *AppleCLI) VerifyImage(ctx context.Context, spec StartSpec, manifest runtimemanifest.Manifest) error {
	if !validExactImageReference(spec.ImageReference) || spec.IndexDigest != manifest.Image.IndexDigest ||
		spec.ImageReference != manifest.Image.Repository+"@"+manifest.Image.IndexDigest {
		return ErrInvalidRequest
	}
	arm64, ok := manifest.ARM64Digest()
	if !ok || arm64 != spec.ARM64Digest {
		return ErrInvalidRequest
	}
	// In Apple Container 1.3.1 inspect commands always emit JSON and do not
	// accept --format. Keep this argument vector pinned to the supported CLI.
	output, err := a.runContainer(ctx, []string{"image", "inspect", spec.ImageReference}, maximumCommandOutputBytes)
	if err != nil {
		return ErrUnavailable
	}
	defer zeroCommandOutput(&output)
	value, err := decodeGenericJSON(output.stdout)
	if err != nil {
		return ErrUnavailable
	}
	expectedLabels := map[string]string{
		"org.opencontainers.image.source":                  imageSourceURL,
		"org.opencontainers.image.version":                 manifest.ArtifactVersion,
		"io.github.novelkr.opencodex.upstream.version":     manifest.Upstream.Version,
		"io.github.novelkr.opencodex.upstream.revision":    manifest.Upstream.Revision,
		"io.github.novelkr.opencodex.public-core.revision": manifest.Source.Revision,
	}
	if !validInspectedRuntimeImage(value, spec, expectedLabels) {
		return ErrUnavailable
	}
	return nil
}

// validInspectedRuntimeImage follows the ImageResource Codable shape emitted
// by Apple Container 1.3.1. Digest and label evidence must come from the same
// unique linux/arm64 variant; unrelated nested strings or a second variant
// cannot be combined into an apparently valid image.
func validInspectedRuntimeImage(value any, spec StartSpec, expectedLabels map[string]string) bool {
	resources, ok := value.([]any)
	if !ok || len(resources) != 1 {
		return false
	}
	resource, ok := resources[0].(map[string]any)
	if !ok {
		return false
	}
	configuration, ok := resource["configuration"].(map[string]any)
	if !ok || stringAtExact(configuration, "name") != spec.ImageReference {
		return false
	}
	descriptor, ok := configuration["descriptor"].(map[string]any)
	if !ok || stringAtExact(descriptor, "digest") != spec.IndexDigest {
		return false
	}
	variants, ok := resource["variants"].([]any)
	if !ok || len(variants) == 0 {
		return false
	}
	matched := false
	for _, item := range variants {
		variant, ok := item.(map[string]any)
		if !ok {
			return false
		}
		platform, ok := variant["platform"].(map[string]any)
		if !ok {
			return false
		}
		if stringAtExact(platform, "os") != "linux" || stringAtExact(platform, "architecture") != "arm64" {
			continue
		}
		if matched || stringAtExact(variant, "digest") != spec.ARM64Digest {
			return false
		}
		imageConfig, ok := variant["config"].(map[string]any)
		if !ok || stringAtExact(imageConfig, "os") != "linux" || stringAtExact(imageConfig, "architecture") != "arm64" {
			return false
		}
		executionConfig, ok := imageConfig["config"].(map[string]any)
		if !ok {
			return false
		}
		labels, ok := executionConfig["Labels"].(map[string]any)
		if !ok {
			return false
		}
		for key, expected := range expectedLabels {
			if stringAtExact(labels, key) != expected {
				return false
			}
		}
		matched = true
	}
	return matched
}

func stringAtExact(value map[string]any, key string) string {
	result, _ := value[key].(string)
	return result
}

func (a *AppleCLI) Start(ctx context.Context, spec StartSpec) (string, error) {
	if a == nil {
		return "", ErrUnavailable
	}
	if err := validateStartSpec(spec, a.socketDirectory); err != nil {
		return "", err
	}
	if a.networkName != "" && !validRuntimeCanaryNetworkName(a.networkName) {
		return "", ErrInvalidRequest
	}
	containerState, err := a.fixedContainerState(ctx, spec.InstallationID)
	if err != nil {
		return "", err
	}
	if containerState != FixedContainerAbsent {
		if containerState == FixedContainerForeign {
			return "", ErrForeignResource
		}
		if containerState == FixedContainerUnknown {
			return "", ErrUnsafeState
		}
		return "", ErrStateChanged
	}
	if a.probePort == nil || a.probePort() != nil {
		return "", ErrForeignResource
	}
	labels := ownedLabels(spec)
	arguments := []string{
		"run", "--detach", "--name", ContainerName, "--platform", "linux/arm64",
		"--uid", strconv.Itoa(os.Geteuid()), "--gid", strconv.Itoa(os.Getegid()),
		"--read-only", "--cap-drop", "ALL", "--init", "--cpus", "2", "--memory", "1G",
		"--tmpfs", "/tmp", "--publish", "127.0.0.1:10210:10100/tcp",
	}
	if a.networkName != "" {
		arguments = append(arguments, "--network", a.networkName)
	}
	for _, key := range []string{labelOwner, labelInstallation, labelOperation, labelManifest, labelIndexDigest, labelGeneration} {
		arguments = append(arguments, "--label", key+"="+labels[key])
	}
	arguments = append(arguments,
		"--mount", "type=bind,source="+spec.StatePath+",target="+GuestStatePath,
		"--mount", "type=bind,source="+spec.SocketPath+",target="+GuestBootstrapSocket,
		spec.ImageReference,
	)
	output, err := a.runContainer(ctx, arguments, maximumCommandOutputBytes)
	zeroCommandOutput(&output)
	if err != nil {
		return "", ErrUnavailable
	}
	return ContainerName, nil
}

func (a *AppleCLI) Stop(ctx context.Context, containerID string, spec StartSpec) error {
	if err := a.verifyOwnership(ctx, containerID, spec); err != nil {
		return err
	}
	output, err := a.runContainer(ctx, []string{"stop", "--time", "15", containerID}, 16<<10)
	zeroCommandOutput(&output)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func (a *AppleCLI) Delete(ctx context.Context, containerID string, spec StartSpec) error {
	if err := a.verifyOwnership(ctx, containerID, spec); err != nil {
		return err
	}
	output, err := a.runContainer(ctx, []string{"delete", containerID}, 16<<10)
	zeroCommandOutput(&output)
	if err != nil {
		return ErrUnavailable
	}
	return nil
}

func (a *AppleCLI) VerifyContainer(ctx context.Context, containerID string, spec StartSpec) error {
	if a == nil {
		return ErrUnavailable
	}
	if containerID != ContainerName || validateReadbackSpec(spec, a.socketDirectory) != nil {
		return ErrInvalidRequest
	}
	value, raw, err := a.inspectContainer(ctx, containerID)
	if err != nil {
		return err
	}
	defer zeroBytes(raw)
	if bytes.Contains(raw, []byte("OPENCODEX_API_AUTH_TOKEN")) || bytes.Contains(raw, []byte("OPENCODEX_ADMIN_AUTH_TOKEN")) ||
		bytes.Contains(raw, []byte("--env")) || bytes.Contains(raw, []byte("--env-file")) {
		return ErrUnsafeState
	}
	container := findContainerObject(value, containerID)
	if container == nil {
		return ErrUnavailable
	}
	labels := mapAt(container, "labels")
	for key, expected := range ownedLabels(spec) {
		if stringAt(labels, key) != expected {
			return ErrForeignResource
		}
	}
	if !validateInspectedRuntimeConfiguration(container, spec) ||
		!validateInspectedRuntimeNetwork(value, containerID, a.networkName) {
		return ErrUnsafeState
	}
	readOnly, readOnlyFound := directBoolAtAny(container, "readOnly", "read_only")
	useInit, useInitFound := directBoolAtAny(container, "useInit", "use_init")
	if !readOnlyFound || !readOnly || !useInitFound || !useInit {
		return ErrUnsafeState
	}
	capDrop, found := directArrayAtAny(container, "capDrop", "cap_drop")
	if !found || len(capDrop) != 1 || !strings.EqualFold(firstStringValue(capDrop[0]), "ALL") {
		return ErrUnsafeState
	}
	if !validateInspectedMounts(container, spec, a.socketDirectory) || !validatePublishedPort(container) {
		return ErrUnsafeState
	}
	return nil
}

// ContainerState is a second, independent inspect performed immediately
// before credentials may cross the fixed loopback port. VerifyContainer owns
// the full static confinement proof; this read binds the current process state
// to the same complete label witness and accepts only the exact `running`
// spelling emitted by Apple Container 1.3.1.
func (a *AppleCLI) ContainerState(ctx context.Context, containerID string, spec StartSpec) (FixedContainerState, error) {
	if a == nil {
		return FixedContainerUnknown, ErrUnavailable
	}
	if containerID != ContainerName || validateReadbackSpec(spec, a.socketDirectory) != nil {
		return FixedContainerUnknown, ErrInvalidRequest
	}
	value, raw, err := a.inspectContainer(ctx, containerID)
	if err != nil {
		return FixedContainerUnknown, err
	}
	defer zeroBytes(raw)
	resource := findContainerResource(value, containerID)
	container := findContainerObject(value, containerID)
	if resource == nil || container == nil {
		return FixedContainerAbsent, nil
	}
	labels := mapAt(container, "labels")
	for key, expected := range ownedLabels(spec) {
		if stringAt(labels, key) != expected {
			return FixedContainerForeign, nil
		}
	}
	return inspectedFixedContainerState(resource), nil
}

func validRuntimeCanaryNetworkName(value string) bool {
	const prefix = "ocx-lifecycle-canary-"
	if !strings.HasPrefix(value, prefix) || len(value) != len(prefix)+12 {
		return false
	}
	return isLowerHex(strings.TrimPrefix(value, prefix), 12)
}

func validateInspectedRuntimeNetwork(value any, identifier, expected string) bool {
	if expected == "" {
		return true
	}
	switch current := value.(type) {
	case map[string]any:
		if stringAt(current, "id") == identifier || stringAt(current, "name") == identifier ||
			stringAt(mapAt(current, "configuration"), "id") == identifier {
			status := mapAt(current, "status")
			networks, found := directArrayAtAny(status, "networks")
			if !found || len(networks) != 1 {
				return false
			}
			network, ok := networks[0].(map[string]any)
			return ok && stringAt(network, "network") == expected
		}
		for _, nested := range current {
			if validateInspectedRuntimeNetwork(nested, identifier, expected) {
				return true
			}
		}
	case []any:
		for _, nested := range current {
			if validateInspectedRuntimeNetwork(nested, identifier, expected) {
				return true
			}
		}
	}
	return false
}

func (a *AppleCLI) VerifyAbsent(ctx context.Context, installationID string) error {
	if !isSHA256(installationID) {
		return ErrInvalidRequest
	}
	containerState, err := a.fixedContainerState(ctx, installationID)
	if err != nil {
		return err
	}
	if containerState == FixedContainerForeign {
		return ErrForeignResource
	}
	if containerState == FixedContainerUnknown {
		return ErrUnsafeState
	}
	if containerState != FixedContainerAbsent {
		return ErrStateChanged
	}
	return nil
}

func (a *AppleCLI) verifyOwnership(ctx context.Context, containerID string, spec StartSpec) error {
	// Mutation authority is the complete start witness, not merely the
	// installation label. This prevents an unrelated fixed-name container that
	// copied the readily inspectable owner/installation labels from being
	// stopped or deleted after its operation, manifest, digest, generation, or
	// confinement profile drifted.
	return a.VerifyContainer(ctx, containerID, spec)
}

func (a *AppleCLI) fixedContainerState(ctx context.Context, installationID string) (FixedContainerState, error) {
	output, err := a.runContainer(ctx, []string{"list", "--all", "--format", "json"}, maximumCommandOutputBytes)
	if err != nil {
		return FixedContainerUnknown, ErrUnavailable
	}
	defer zeroCommandOutput(&output)
	value, err := decodeGenericJSON(output.stdout)
	if err != nil {
		return FixedContainerUnknown, ErrUnavailable
	}
	resource := findContainerResource(value, ContainerName)
	container := findContainerObject(value, ContainerName)
	if resource == nil || container == nil {
		return FixedContainerAbsent, nil
	}
	labels := mapAt(container, "labels")
	owned := stringAt(labels, labelOwner) == "opencodex-relay" && stringAt(labels, labelInstallation) == installationID
	if !owned {
		return FixedContainerForeign, nil
	}
	return inspectedFixedContainerState(resource), nil
}

func inspectedFixedContainerState(container map[string]any) FixedContainerState {
	switch stringAt(mapAt(container, "status"), "state") {
	case "running":
		return FixedContainerRunningOwned
	case "stopped":
		return FixedContainerStoppedOwned
	default:
		return FixedContainerUnknown
	}
}

func (a *AppleCLI) inspectContainer(ctx context.Context, containerID string) (any, []byte, error) {
	if containerID != ContainerName {
		return nil, nil, ErrInvalidRequest
	}
	output, err := a.runContainer(ctx, []string{"inspect", containerID}, maximumCommandOutputBytes)
	zeroBytes(output.stderr)
	if err != nil {
		zeroBytes(output.stdout)
		return nil, nil, ErrUnavailable
	}
	value, err := decodeGenericJSON(output.stdout)
	if err != nil {
		zeroBytes(output.stdout)
		return nil, nil, ErrUnavailable
	}
	return value, output.stdout, nil
}

func (a *AppleCLI) runContainer(ctx context.Context, arguments []string, maximum int64) (commandOutput, error) {
	if a == nil || a.runner == nil {
		return commandOutput{}, ErrUnavailable
	}
	return a.runner.Run(ctx, appleContainerExecutable, append([]string(nil), arguments...), nil, maximum)
}

func probeFixedHostPort() error {
	listener, err := net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(HostServicePort))
	if err != nil {
		return err
	}
	return listener.Close()
}

func validateStartSpec(spec StartSpec, socketDirectory string) error {
	if err := validateReadbackSpec(spec, socketDirectory); err != nil {
		return err
	}
	socketInfo, err := os.Lstat(spec.SocketPath)
	if err != nil || socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 || !ownedByCurrentUser(socketInfo) {
		return ErrUnsafeState
	}
	return nil
}

// validateReadbackSpec authenticates the immutable parts of a start witness
// after the one-shot bootstrap socket has intentionally been removed. An empty
// SocketPath is accepted for crash recovery, but inspect must still report a
// random socket source under the generation's fixed runtime root.
func validateReadbackSpec(spec StartSpec, socketDirectory string) error {
	if !isSHA256(spec.InstallationID) || !isSHA256(spec.OperationID) || !isSHA256(spec.ManifestSHA256) ||
		!validOCIDigest(spec.IndexDigest) || !validOCIDigest(spec.ARM64Digest) || spec.IndexDigest == spec.ARM64Digest ||
		!validExactImageReference(spec.ImageReference) || spec.Generation == 0 ||
		!cleanAbsolute(spec.StatePath) {
		return ErrInvalidRequest
	}
	if spec.ImageReference != runtimemanifest.ProductionImageRepository+"@"+spec.IndexDigest {
		return ErrInvalidRequest
	}
	stateInfo, err := os.Lstat(spec.StatePath)
	if err != nil || !stateInfo.IsDir() || stateInfo.Mode()&os.ModeSymlink != 0 || stateInfo.Mode().Perm()&0o077 != 0 || !ownedByCurrentUser(stateInfo) {
		return ErrUnsafeState
	}
	if filepath.Base(filepath.Dir(spec.StatePath)) != "homes" ||
		filepath.Base(spec.StatePath) != fmt.Sprintf("generation-%04d", spec.Generation) {
		return ErrInvalidRequest
	}
	if spec.SocketPath != "" {
		if !validBootstrapSocketPath(spec.SocketPath, socketDirectory) {
			return ErrInvalidRequest
		}
		if socketInfo, socketErr := os.Lstat(spec.SocketPath); socketErr == nil {
			if socketInfo.Mode()&os.ModeSocket == 0 || socketInfo.Mode().Perm() != 0o600 || !ownedByCurrentUser(socketInfo) {
				return ErrUnsafeState
			}
		} else if !errors.Is(socketErr, os.ErrNotExist) {
			return ErrUnsafeState
		}
	}
	return nil
}

func validBootstrapSocketPath(socketPath, socketDirectory string) bool {
	if !cleanAbsolute(socketPath) || !cleanAbsolute(socketDirectory) || len(socketPath) > 103 {
		return false
	}
	if filepath.Dir(socketPath) != socketDirectory {
		return false
	}
	base := filepath.Base(socketPath)
	if !strings.HasPrefix(base, "b-") {
		return false
	}
	random := strings.TrimPrefix(base, "b-")
	return isLowerHex(random, 32)
}

func ownedLabels(spec StartSpec) map[string]string {
	return map[string]string{
		labelOwner: "opencodex-relay", labelInstallation: spec.InstallationID,
		labelOperation: spec.OperationID, labelManifest: spec.ManifestSHA256,
		labelIndexDigest: spec.IndexDigest, labelGeneration: strconv.FormatUint(spec.Generation, 10),
	}
}

func validExactImageReference(value string) bool {
	prefix := runtimemanifest.ProductionImageRepository + "@"
	return strings.HasPrefix(value, prefix) && validOCIDigest(strings.TrimPrefix(value, prefix))
}

func cleanAbsolute(value string) bool { return filepath.IsAbs(value) && filepath.Clean(value) == value }

func decodeGenericJSON(data []byte) (any, error) {
	if len(data) == 0 || len(data) > maximumCommandOutputBytes || rejectDuplicateJSONKeys(data) != nil {
		return nil, ErrUnavailable
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, ErrUnavailable
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, ErrUnavailable
	}
	return value, nil
}

func extractSystemVersions(data []byte) (string, []string, bool) {
	var rows []struct {
		Version string `json:"version"`
		AppName string `json:"appName"`
	}
	if err := decodeBoundedJSON(data, &rows); err != nil || len(rows) == 0 {
		return "", nil, false
	}
	var cliVersion string
	versions := make([]string, 0, len(rows))
	for _, row := range rows {
		if row.AppName == "" || !validNumericVersion(row.Version) {
			return "", nil, false
		}
		versions = append(versions, row.Version)
		if row.AppName == "container" {
			if cliVersion != "" {
				return "", nil, false
			}
			cliVersion = row.Version
		}
	}
	return cliVersion, versions, cliVersion != ""
}

func hasExactCodesignIdentifier(data []byte, expected string) bool {
	return hasExactCodesignField(data, "Identifier=", expected)
}

func hasExactCodesignIdentity(data []byte, identifier, teamIdentifier string) bool {
	return hasExactCodesignIdentifier(data, identifier) &&
		hasExactCodesignField(data, "TeamIdentifier=", teamIdentifier)
}

func hasExactCodesignField(data []byte, prefix, expected string) bool {
	match, count := "", 0
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, prefix) {
			match = strings.TrimPrefix(line, prefix)
			count++
		}
	}
	return count == 1 && match == expected
}

// protectedExecutable validates every pathname component from trustedRoot to
// the executable without following symlinks. The Apple installer places the
// CLI below the root-owned /usr hierarchy; accepting a user- or group-writable
// parent would create a validation-to-exec replacement window even when the
// file observed by the initial Lstat was signed correctly.
func protectedExecutable(path, trustedRoot string, ownerUID int) bool {
	if !cleanAbsolute(path) || !cleanAbsolute(trustedRoot) || path == trustedRoot ||
		!pathWithin(trustedRoot, path) || ownerUID < 0 {
		return false
	}
	relative, err := filepath.Rel(trustedRoot, path)
	if err != nil || relative == "." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	components := strings.Split(relative, string(filepath.Separator))
	current := trustedRoot
	rootInfo, err := os.Lstat(current)
	rootACL, aclErr := hasExtendedACL(current)
	if err != nil || !protectedPathComponent(rootInfo, ownerUID, false) || aclErr != nil || rootACL {
		return false
	}
	for index, component := range components {
		if component == "" || component == "." || component == ".." {
			return false
		}
		current = filepath.Join(current, component)
		extendedACL, aclErr := hasExtendedACL(current)
		if aclErr != nil || extendedACL {
			return false
		}
		if index == len(components)-1 {
			info, err := os.Lstat(current)
			return err == nil && protectedPathComponent(info, ownerUID, true)
		}
		info, err := os.Lstat(current)
		if err != nil || !protectedPathComponent(info, ownerUID, false) {
			return false
		}
	}
	return false
}

func protectedPathComponent(info os.FileInfo, ownerUID int, executable bool) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return false
	}
	metadata, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(metadata.Uid) != ownerUID {
		return false
	}
	if executable {
		return info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
	}
	return info.IsDir()
}

func parsePackageReceipt(data []byte) (string, string, bool) {
	if len(data) == 0 || len(data) > 16<<10 {
		return "", "", false
	}
	decoder := xml.NewDecoder(bytes.NewReader(data))
	values := map[string]string{}
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", "", false
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "key" {
			continue
		}
		var key string
		if err := decoder.DecodeElement(&key, &start); err != nil || key == "" {
			return "", "", false
		}
		for {
			next, err := decoder.Token()
			if err != nil {
				return "", "", false
			}
			valueStart, ok := next.(xml.StartElement)
			if !ok {
				continue
			}
			if valueStart.Name.Local != "string" {
				if err := decoder.Skip(); err != nil {
					return "", "", false
				}
				break
			}
			var value string
			if err := decoder.DecodeElement(&value, &valueStart); err != nil {
				return "", "", false
			}
			if _, duplicate := values[key]; duplicate {
				return "", "", false
			}
			values[key] = value
			break
		}
	}
	identifier, idOK := values["pkgid"]
	version, versionOK := values["pkg-version"]
	return identifier, version, idOK && versionOK
}

func receiptOwnsCLI(data []byte) bool {
	if len(data) == 0 || len(data) > 16<<10 {
		return false
	}
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		if strings.TrimPrefix(strings.TrimSpace(line), "./") == "bin/container" {
			return true
		}
	}
	return false
}

func extractServiceState(data []byte) string {
	value, err := decodeGenericJSON(data)
	if err != nil {
		return "unknown"
	}
	object, ok := value.(map[string]any)
	if !ok {
		return "unknown"
	}
	required := map[string]bool{
		"status": false, "appRoot": false, "installRoot": false,
		"apiServerVersion": false, "apiServerCommit": false,
		"apiServerBuild": false, "apiServerAppName": false,
	}
	for key, nested := range object {
		if key == "logRoot" {
			if nested != nil {
				if _, ok := nested.(string); !ok {
					return "unknown"
				}
			}
			continue
		}
		if _, expected := required[key]; !expected {
			return "unknown"
		}
		if _, ok := nested.(string); !ok {
			return "unknown"
		}
		required[key] = true
	}
	for _, found := range required {
		if !found {
			return "unknown"
		}
	}
	status := stringAt(object, "status")
	if status != "running" {
		if status == "" {
			return "unknown"
		}
		return "stopped"
	}
	for _, key := range []string{"appRoot", "installRoot", "apiServerVersion", "apiServerCommit", "apiServerBuild", "apiServerAppName"} {
		if stringAt(object, key) == "" {
			return "unknown"
		}
	}
	return "running"
}

func findContainerObject(value any, identifier string) map[string]any {
	switch current := value.(type) {
	case map[string]any:
		if stringAt(current, "id") == identifier || stringAt(current, "name") == identifier ||
			stringAt(mapAt(current, "configuration"), "id") == identifier {
			if configuration := mapAt(current, "configuration"); configuration != nil {
				configuration["__top_id"] = stringAt(current, "id")
				return configuration
			}
			return current
		}
		for _, nested := range current {
			if found := findContainerObject(nested, identifier); found != nil {
				return found
			}
		}
	case []any:
		for _, nested := range current {
			if found := findContainerObject(nested, identifier); found != nil {
				return found
			}
		}
	}
	return nil
}

func findContainerResource(value any, identifier string) map[string]any {
	switch current := value.(type) {
	case map[string]any:
		if stringAt(current, "id") == identifier || stringAt(current, "name") == identifier ||
			stringAt(mapAt(current, "configuration"), "id") == identifier {
			return current
		}
		for _, nested := range current {
			if found := findContainerResource(nested, identifier); found != nil {
				return found
			}
		}
	case []any:
		for _, nested := range current {
			if found := findContainerResource(nested, identifier); found != nil {
				return found
			}
		}
	}
	return nil
}

func mapAt(value map[string]any, key string) map[string]any {
	if value == nil {
		return nil
	}
	if result, ok := value[key].(map[string]any); ok {
		return result
	}
	for candidate, nested := range value {
		if strings.EqualFold(candidate, key) {
			result, _ := nested.(map[string]any)
			return result
		}
	}
	return nil
}

func stringAt(value map[string]any, key string) string {
	if value == nil {
		return ""
	}
	if result, ok := value[key].(string); ok {
		return result
	}
	for candidate, nested := range value {
		if strings.EqualFold(candidate, key) {
			result, _ := nested.(string)
			return result
		}
	}
	return ""
}

func directBoolAtAny(value map[string]any, keys ...string) (bool, bool) {
	for key, nested := range value {
		for _, candidate := range keys {
			if strings.EqualFold(key, candidate) {
				result, ok := nested.(bool)
				return result, ok
			}
		}
	}
	return false, false
}

func directArrayAtAny(value map[string]any, keys ...string) ([]any, bool) {
	for key, nested := range value {
		for _, candidate := range keys {
			if strings.EqualFold(key, candidate) {
				result, ok := nested.([]any)
				return result, ok
			}
		}
	}
	return nil, false
}

func validateInspectedRuntimeConfiguration(container map[string]any, spec StartSpec) bool {
	image := mapAt(container, "image")
	descriptor := mapAt(image, "descriptor")
	if image == nil || descriptor == nil || stringAt(image, "reference") != spec.ImageReference ||
		stringAt(descriptor, "digest") != spec.IndexDigest {
		return false
	}
	platform := mapAt(container, "platform")
	if platform == nil || stringAt(platform, "os") != "linux" ||
		firstString(platform, "architecture", "arch") != "arm64" {
		return false
	}
	resources := mapAt(container, "resources")
	if resources == nil || unsignedAt(resources, "cpus") != 2 ||
		unsignedAt(resources, "memoryInBytes", "memory_in_bytes") != 1<<30 {
		return false
	}
	process := mapAt(container, "initProcess")
	user := mapAt(mapAt(process, "user"), "id")
	if process == nil || user == nil || unsignedAt(user, "uid") != uint64(os.Geteuid()) ||
		unsignedAt(user, "gid") != uint64(os.Getegid()) {
		return false
	}
	terminal, terminalFound := directBoolAtAny(process, "terminal")
	groups, groupsFound := directArrayAtAny(process, "supplementalGroups", "supplemental_groups")
	capAdd, capAddFound := directArrayAtAny(container, "capAdd", "cap_add")
	publishedSockets, socketsFound := directArrayAtAny(container, "publishedSockets", "published_sockets")
	ssh, sshFound := directBoolAtAny(container, "ssh")
	rosetta, rosettaFound := directBoolAtAny(container, "rosetta")
	virtualization, virtualizationFound := directBoolAtAny(container, "virtualization")
	return terminalFound && !terminal && groupsFound && len(groups) == 0 &&
		capAddFound && len(capAdd) == 0 && socketsFound && len(publishedSockets) == 0 &&
		sshFound && !ssh && rosettaFound && !rosetta && virtualizationFound && !virtualization &&
		stringAt(container, "runtimeHandler") == "container-runtime-linux"
}

func firstStringValue(value any) string {
	result, _ := value.(string)
	return result
}

func validateInspectedMounts(container map[string]any, spec StartSpec, socketDirectory string) bool {
	var mounts []any
	mounts, found := directArrayAtAny(container, "mounts")
	// Apple Container 1.3.1 persists both --mount and --tmpfs inputs in the
	// ContainerConfiguration.mounts array. Require the exact /tmp tmpfs used by
	// Start in addition to the two expected bind mounts; any missing, duplicate,
	// host-backed, or unknown mount remains fail-closed.
	if !found || len(mounts) != 3 {
		return false
	}
	expected := map[string]string{GuestStatePath: spec.StatePath}
	bootstrapFound := false
	tmpfsFound := false
	for _, item := range mounts {
		mount, ok := item.(map[string]any)
		if !ok {
			return false
		}
		target := firstString(mount, "target", "destination")
		source := firstString(mount, "source")
		kind := strings.ToLower(firstString(mount, "type", "kind"))
		if target == "/tmp" {
			validTmpfs := kind == "tmpfs" && (source == "" || strings.EqualFold(source, "tmpfs")) ||
				kind == "" && strings.EqualFold(source, "tmpfs")
			if tmpfsFound || !validTmpfs {
				return false
			}
			tmpfsFound = true
			continue
		}
		if target == GuestBootstrapSocket {
			if bootstrapFound || !validBootstrapSocketPath(source, socketDirectory) ||
				spec.SocketPath != "" && source != spec.SocketPath {
				return false
			}
			bootstrapFound = true
			continue
		}
		if expected[target] != source || target != GuestStatePath {
			return false
		}
		delete(expected, target)
	}
	return len(expected) == 0 && bootstrapFound && tmpfsFound
}

func validatePublishedPort(container map[string]any) bool {
	var ports []any
	ports, found := directArrayAtAny(container, "publishedPorts", "published_ports")
	if !found || len(ports) != 1 {
		return false
	}
	port, ok := ports[0].(map[string]any)
	if !ok {
		return false
	}
	host := firstString(port, "hostAddress", "host_address", "hostIP", "host_ip")
	count := integerAt(port, "count")
	return host == "127.0.0.1" && integerAt(port, "hostPort", "host_port") == HostServicePort &&
		integerAt(port, "containerPort", "container_port") == GuestServicePort &&
		strings.EqualFold(firstString(port, "proto", "protocol"), "tcp") && count == 1
}

func firstString(value map[string]any, keys ...string) string {
	for _, key := range keys {
		if result := stringAt(value, key); result != "" {
			return result
		}
	}
	return ""
}

func integerAt(value map[string]any, keys ...string) int {
	for _, key := range keys {
		for candidate, nested := range value {
			if !strings.EqualFold(candidate, key) {
				continue
			}
			switch number := nested.(type) {
			case json.Number:
				parsed, _ := strconv.Atoi(number.String())
				return parsed
			case float64:
				return int(number)
			}
		}
	}
	return 0
}

func unsignedAt(value map[string]any, keys ...string) uint64 {
	for _, key := range keys {
		for candidate, nested := range value {
			if !strings.EqualFold(candidate, key) {
				continue
			}
			switch number := nested.(type) {
			case json.Number:
				parsed, err := strconv.ParseUint(number.String(), 10, 64)
				if err == nil {
					return parsed
				}
			case float64:
				if number >= 0 && number == float64(uint64(number)) {
					return uint64(number)
				}
			}
			return 0
		}
	}
	return 0
}

func validNumericVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) < 2 || len(parts) > 4 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}

func numericVersionAtLeast(actual, minimum string) bool {
	return compareNumericVersion(actual, minimum) >= 0
}

func compareNumericVersion(left, right string) int {
	if !validNumericVersion(left) || !validNumericVersion(right) {
		return -1
	}
	l, r := strings.Split(left, "."), strings.Split(right, ".")
	for index := 0; index < max(len(l), len(r)); index++ {
		lv, rv := uint64(0), uint64(0)
		if index < len(l) {
			lv, _ = strconv.ParseUint(l[index], 10, 32)
		}
		if index < len(r) {
			rv, _ = strconv.ParseUint(r[index], 10, 32)
		}
		if lv < rv {
			return -1
		}
		if lv > rv {
			return 1
		}
	}
	return 0
}

func zeroCommandOutput(output *commandOutput) {
	if output == nil {
		return
	}
	zeroBytes(output.stdout)
	zeroBytes(output.stderr)
}
