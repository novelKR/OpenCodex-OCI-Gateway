//go:build darwin

package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type systemAppBundleValidator struct{}

var stageCDHashPattern = regexp.MustCompile(`(?m)^CDHash=([0-9a-f]{40,128})$`)

func (systemAppBundleValidator) Validate(
	ctx context.Context,
	appPath string,
	tag string,
	artifact Artifact,
	currentBuildNumber int,
	publicKeyPEM []byte,
	trustKeyID string,
) (BundleValidation, error) {
	if artifact.BundleID != productionApplicationBundle || artifact.SigningMode != SigningModeAdHoc {
		return BundleValidation{}, ErrStageInvalidBundle
	}
	paths := map[string]string{
		"app":        appPath,
		"executable": filepath.Join(appPath, "Contents", "MacOS", "OpenCodexRelay"),
		"relay":      filepath.Join(appPath, "Contents", "Library", "Helpers", "opencodex-relay"),
		"relayctl":   filepath.Join(appPath, "Contents", "Library", "Helpers", "opencodex-relayctl"),
		"installer":  filepath.Join(appPath, "Contents", "Library", "Helpers", "OpenCodexRelayHelperInstaller"),
		"helper":     filepath.Join(appPath, "Contents", "Library", "HelperTools", "OpenCodexRelayPrivilegedHelper"),
	}
	for name, candidate := range paths {
		metadata, err := os.Lstat(candidate)
		if err != nil || metadata.Mode()&os.ModeSymlink != 0 {
			return BundleValidation{}, fmt.Errorf("%s identity", name)
		}
		if name == "app" {
			if !metadata.IsDir() {
				return BundleValidation{}, fmt.Errorf("%s is not a directory", name)
			}
		} else if !metadata.Mode().IsRegular() || metadata.Mode().Perm()&0o111 == 0 {
			return BundleValidation{}, fmt.Errorf("%s is not an executable regular file", name)
		}
	}
	allowedExecutables := make(map[string]struct{}, len(paths)-1)
	for name, candidate := range paths {
		if name != "app" {
			allowedExecutables[filepath.Clean(candidate)] = struct{}{}
		}
	}
	if err := filepath.WalkDir(appPath, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return ErrStageInvalidBundle
		}
		metadata, err := entry.Info()
		if err != nil || metadata.Mode()&os.ModeSymlink != 0 || (!metadata.IsDir() && !metadata.Mode().IsRegular()) {
			return fmt.Errorf("unexpected bundle entry identity")
		}
		if metadata.Mode().IsRegular() && metadata.Mode().Perm()&0o111 != 0 {
			if _, allowed := allowedExecutables[filepath.Clean(path)]; !allowed {
				return fmt.Errorf("unexpected executable bundle entry")
			}
		}
		return nil
	}); err != nil {
		return BundleValidation{}, err
	}
	plist := filepath.Join(appPath, "Contents", "Info.plist")
	expectedStrings := map[string]string{
		"CFBundleIdentifier":                        artifact.BundleID,
		"CFBundleShortVersionString":                tag,
		"CFBundleExecutable":                        "OpenCodexRelay",
		"CFBundlePackageType":                       "APPL",
		"LSMinimumSystemVersion":                    artifact.MinimumMacOSVersion,
		"OpenCodexDistributionFlavor":               "production",
		"OpenCodexRuntimeMode":                      "managed",
		"OpenCodexHomebrewGuardBackend":             "manual_admin",
		"OpenCodexHomebrewGuardInstallerExecutable": "OpenCodexRelayHelperInstaller",
		"OpenCodexHomebrewGuardHelperVersion":       tag,
	}
	for key, expected := range expectedStrings {
		actual, err := plistValue(ctx, plist, key)
		if err != nil || actual != expected {
			return BundleValidation{}, fmt.Errorf("plist %s mismatch", key)
		}
	}
	buildValue, err := plistValue(ctx, plist, "CFBundleVersion")
	buildNumber, parseErr := strconv.Atoi(buildValue)
	if err != nil || parseErr != nil || strconv.Itoa(buildNumber) != buildValue ||
		buildNumber < 1 || buildNumber > 9999 || buildNumber <= currentBuildNumber {
		return BundleValidation{}, errors.New("bundle build number is not a newer tracked number")
	}
	if err := runStageCommand(ctx, "/usr/bin/codesign", "--verify", "--deep", "--strict", "--verbose=2", appPath); err != nil {
		return BundleValidation{}, err
	}
	identifiers := map[string]string{
		"app":        productionApplicationBundle,
		"executable": productionApplicationBundle,
		"installer":  "io.github.novelkr.opencodex-relay.homebrew-guard.installer",
		"helper":     "io.github.novelkr.opencodex-relay.homebrew-guard.helper",
	}
	cdHashes := map[string]string{}
	for name, candidate := range paths {
		details, err := stageCommandOutput(ctx, "/usr/bin/codesign", "-dvvv", "--verbose=4", candidate)
		if err != nil || !strings.Contains(details, "Signature=adhoc") ||
			!strings.Contains(details, "TeamIdentifier=not set") ||
			!strings.Contains(details, "flags=0x10002(adhoc,runtime)") {
			return BundleValidation{}, fmt.Errorf("%s signing contract", name)
		}
		match := stageCDHashPattern.FindStringSubmatch(details)
		if len(match) != 2 {
			return BundleValidation{}, fmt.Errorf("%s CDHash", name)
		}
		cdHashes[name] = match[1]
		if expected := identifiers[name]; expected != "" && !strings.Contains(details, "Identifier="+expected+"\n") {
			return BundleValidation{}, fmt.Errorf("%s identifier", name)
		}
		if name == "relay" && !strings.Contains(details, "Identifier=opencodex-relay-") {
			return BundleValidation{}, errors.New("relay identifier")
		}
		if name == "relayctl" && !strings.Contains(details, "Identifier=opencodex-relayctl-") {
			return BundleValidation{}, errors.New("relayctl identifier")
		}
		if name != "app" {
			if err := runStageCommand(ctx, "/usr/bin/codesign", "--verify", "--strict", "--verbose=2", candidate); err != nil {
				return BundleValidation{}, err
			}
		}
		if name != "app" {
			architectures, err := stageCommandOutput(ctx, "/usr/bin/lipo", "-archs", candidate)
			if err != nil || strings.TrimSpace(architectures) != "arm64" {
				return BundleValidation{}, fmt.Errorf("%s architecture", name)
			}
		}
	}
	helperRequirement, err := plistValue(ctx, plist, "OpenCodexHomebrewGuardHelperRequirement")
	if err != nil || helperRequirement != `cdhash H"`+cdHashes["helper"]+`"` {
		return BundleValidation{}, errors.New("helper CDHash binding")
	}
	bundledKey := filepath.Join(appPath, "Contents", "Resources", "ReleaseTrust", "opencodex-relay-release-ed25519.pub")
	metadata, err := os.Lstat(bundledKey)
	if err != nil || !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Size() > 8192 {
		return BundleValidation{}, errors.New("bundled trust key identity")
	}
	keyBytes, err := os.ReadFile(bundledKey)
	if err != nil || !bytes.Equal(keyBytes, publicKeyPEM) {
		return BundleValidation{}, errors.New("bundled trust key differs")
	}
	keyID, err := publicKeyID(keyBytes)
	if err != nil || keyID != trustKeyID {
		return BundleValidation{}, errors.New("bundled trust key fingerprint")
	}
	fingerprint, err := BundleFingerprint(appPath)
	if err != nil {
		return BundleValidation{}, err
	}
	return BundleValidation{Fingerprint: fingerprint, BuildNumber: buildNumber}, nil
}

func plistValue(ctx context.Context, plist, key string) (string, error) {
	output, err := stageCommandOutput(ctx, "/usr/bin/plutil", "-extract", key, "raw", "-o", "-", plist)
	return strings.TrimSpace(output), err
}

func runStageCommand(ctx context.Context, executable string, arguments ...string) error {
	_, err := stageCommandOutput(ctx, executable, arguments...)
	return err
}

func stageCommandOutput(ctx context.Context, executable string, arguments ...string) (string, error) {
	commandContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	command := exec.CommandContext(commandContext, executable, arguments...)
	output, err := command.CombinedOutput()
	if len(output) > 64<<10 {
		return "", ErrStageInvalidBundle
	}
	if err != nil {
		return "", fmt.Errorf("bundle validation command failed")
	}
	return string(output), nil
}
