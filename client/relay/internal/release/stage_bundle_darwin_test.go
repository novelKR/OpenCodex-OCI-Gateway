//go:build darwin

package release

import (
	"context"
	"os"
	"strconv"
	"testing"
)

func TestSystemAppBundleValidatorDryRunFixture(t *testing.T) {
	appPath := os.Getenv("OPENCODEX_STAGE_APP_FIXTURE")
	if appPath == "" {
		t.Skip("release-package dry run supplies the signed app fixture")
	}
	tag := os.Getenv("OPENCODEX_STAGE_TAG")
	keyPath := os.Getenv("OPENCODEX_STAGE_PUBLIC_KEY")
	currentBuild, err := strconv.Atoi(os.Getenv("OPENCODEX_STAGE_CURRENT_BUILD"))
	if err != nil || currentBuild < 1 {
		t.Fatal("invalid dry-run current build")
	}
	key, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	keyID, err := publicKeyID(key)
	if err != nil {
		t.Fatal(err)
	}
	validation, err := (systemAppBundleValidator{}).Validate(
		context.Background(),
		appPath,
		tag,
		Artifact{
			BundleID:            productionApplicationBundle,
			SigningMode:         SigningModeAdHoc,
			MinimumMacOSVersion: "26.0",
			IntegrationProtocol: 1,
			HelperProtocol:      1,
		},
		currentBuild,
		key,
		keyID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if !isLowerHexSHA256(validation.Fingerprint) || validation.BuildNumber <= currentBuild {
		t.Fatalf("validation = %#v", validation)
	}
}
