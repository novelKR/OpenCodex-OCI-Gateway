// Package release verifies a signed, explicitly versioned relay artifact manifest.
package release

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
)

const (
	CompatibilityRevisionLegacy    = 1
	CompatibilityRevisionDocuments = 2
	CompatibilityRevisionAdHocApp  = 4
	CompatibilityRevisionUpdater   = 5
	ThirdPartyNoticesFile          = "THIRD_PARTY_NOTICES.md"
	ComponentRelay                 = "relay"
	ComponentRelayctl              = "relayctl"
	ComponentMacOSMenuBarBundle    = "macos_menu_bar_bundle"
	SigningModeAdHoc               = "adhoc"
	ChannelStable                  = "stable"
	ChannelPreview                 = "preview"
)

type Manifest struct {
	Version               string     `json:"version"`
	CompatibilityRevision int        `json:"compatibility_revision"`
	Artifacts             []Artifact `json:"artifacts"`
	Documents             []Document `json:"documents,omitempty"`
	Channel               string     `json:"channel,omitempty"`
	MinimumUpdaterVersion string     `json:"minimum_updater_version,omitempty"`
	TrustKeyID            string     `json:"trust_key_id,omitempty"`
}

type Artifact struct {
	OS                  string `json:"os"`
	Arch                string `json:"arch"`
	Component           string `json:"component,omitempty"`
	File                string `json:"file"`
	URL                 string `json:"url"`
	SHA256              string `json:"sha256"`
	BundleID            string `json:"bundle_id,omitempty"`
	SigningMode         string `json:"signing_mode,omitempty"`
	MinimumMacOSVersion string `json:"minimum_macos_version,omitempty"`
	IntegrationProtocol int    `json:"integration_protocol,omitempty"`
	HelperProtocol      int    `json:"helper_protocol,omitempty"`
}

type Document struct {
	File   string `json:"file"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
}

func Verify(manifestBytes, signatureBytes, publicKeyPEM []byte) (Manifest, error) {
	key, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return Manifest{}, err
	}
	signature, err := base64.StdEncoding.DecodeString(string(trimSpace(signatureBytes)))
	if err != nil {
		return Manifest{}, fmt.Errorf("decode manifest signature: %w", err)
	}
	if !ed25519.Verify(key, manifestBytes, signature) {
		return Manifest{}, errors.New("release manifest signature is invalid")
	}
	if err := rejectDuplicateJSONKeys(manifestBytes); err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(manifestBytes)))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode release manifest: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("release manifest contains trailing JSON")
	}
	if manifest.Version == "" || len(manifest.Artifacts) == 0 {
		return Manifest{}, errors.New("release manifest is incomplete")
	}
	if manifest.CompatibilityRevision != CompatibilityRevisionLegacy &&
		manifest.CompatibilityRevision != CompatibilityRevisionDocuments &&
		manifest.CompatibilityRevision != CompatibilityRevisionAdHocApp &&
		manifest.CompatibilityRevision != CompatibilityRevisionUpdater {
		return Manifest{}, fmt.Errorf("unsupported release manifest compatibility revision: %d", manifest.CompatibilityRevision)
	}
	if manifest.CompatibilityRevision == CompatibilityRevisionUpdater {
		expectedChannel, ok := channelForVersion(manifest.Version)
		if !ok || manifest.Channel != expectedChannel || !validStrictSemVer(manifest.MinimumUpdaterVersion) ||
			!isLowerHexSHA256(manifest.TrustKeyID) {
			return Manifest{}, errors.New("release updater manifest metadata is invalid")
		}
	} else if manifest.Channel != "" || manifest.MinimumUpdaterVersion != "" || manifest.TrustKeyID != "" {
		return Manifest{}, errors.New("legacy release manifest must not contain updater metadata")
	}
	seenArtifacts := make(map[string]struct{}, len(manifest.Artifacts))
	for _, artifact := range manifest.Artifacts {
		if artifact.OS == "" || artifact.Arch == "" || artifact.File == "" ||
			!validHTTPSURL(artifact.URL) || !isLowerHexSHA256(artifact.SHA256) {
			return Manifest{}, errors.New("release manifest contains an incomplete artifact")
		}
		if manifest.CompatibilityRevision == CompatibilityRevisionAdHocApp ||
			manifest.CompatibilityRevision == CompatibilityRevisionUpdater {
			if err := validateMenuBarArtifact(artifact, manifest.CompatibilityRevision); err != nil {
				return Manifest{}, err
			}
			key := artifact.OS + "/" + artifact.Arch + "/" + artifact.Component
			if _, found := seenArtifacts[key]; found {
				return Manifest{}, errors.New("release manifest contains a duplicate artifact component")
			}
			seenArtifacts[key] = struct{}{}
		} else if artifact.Component != "" || artifact.BundleID != "" || artifact.SigningMode != "" ||
			artifact.MinimumMacOSVersion != "" || artifact.IntegrationProtocol != 0 || artifact.HelperProtocol != 0 {
			return Manifest{}, errors.New("legacy release manifest must not contain menu bar component metadata")
		}
	}
	seenDocuments := make(map[string]struct{}, len(manifest.Documents))
	for _, document := range manifest.Documents {
		if document.File == "" || !validHTTPSURL(document.URL) || !isLowerHexSHA256(document.SHA256) {
			return Manifest{}, errors.New("release manifest contains an incomplete document")
		}
		if _, found := seenDocuments[document.File]; found {
			return Manifest{}, errors.New("release manifest contains a duplicate document")
		}
		seenDocuments[document.File] = struct{}{}
	}
	if manifest.CompatibilityRevision >= CompatibilityRevisionDocuments {
		if len(manifest.Documents) != 1 || manifest.Documents[0].File != ThirdPartyNoticesFile {
			return Manifest{}, errors.New("release manifest requires the third-party notices document")
		}
	}
	if manifest.CompatibilityRevision == CompatibilityRevisionAdHocApp ||
		manifest.CompatibilityRevision == CompatibilityRevisionUpdater {
		if err := validateMenuBarArtifactSet(manifest.Artifacts); err != nil {
			return Manifest{}, err
		}
	}
	return manifest, nil
}

func VerifyFiles(manifestPath, signaturePath, publicKeyPath string) (Manifest, error) {
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return Manifest{}, err
	}
	signature, err := os.ReadFile(signaturePath)
	if err != nil {
		return Manifest{}, err
	}
	key, err := os.ReadFile(publicKeyPath)
	if err != nil {
		return Manifest{}, err
	}
	return Verify(manifest, signature, key)
}

func (m Manifest) Select(goos, goarch string) (Artifact, error) {
	if m.CompatibilityRevision == CompatibilityRevisionAdHocApp ||
		m.CompatibilityRevision == CompatibilityRevisionUpdater {
		return Artifact{}, fmt.Errorf("release %s uses component-aware artifacts; select an explicit component", m.Version)
	}
	for _, artifact := range m.Artifacts {
		if artifact.OS == goos && artifact.Arch == goarch {
			return artifact, nil
		}
	}
	return Artifact{}, fmt.Errorf("release %s has no artifact for %s/%s", m.Version, goos, goarch)
}

// SelectComponent resolves one explicitly named component-aware artifact. Keeping the
// component in the signed tuple prevents a menu-bar bundle from being selected
// where a loopback relay binary is required.
func (m Manifest) SelectComponent(goos, goarch, component string) (Artifact, error) {
	if m.CompatibilityRevision != CompatibilityRevisionAdHocApp &&
		m.CompatibilityRevision != CompatibilityRevisionUpdater {
		return Artifact{}, fmt.Errorf("release %s does not use component-aware artifacts", m.Version)
	}
	for _, artifact := range m.Artifacts {
		if artifact.OS == goos && artifact.Arch == goarch && artifact.Component == component {
			return artifact, nil
		}
	}
	return Artifact{}, fmt.Errorf("release %s has no %s artifact for %s/%s", m.Version, component, goos, goarch)
}

func validateMenuBarArtifact(artifact Artifact, revision int) error {
	switch artifact.Component {
	case ComponentRelay, ComponentRelayctl:
		if artifact.OS != "linux" || (artifact.Arch != "amd64" && artifact.Arch != "arm64") ||
			artifact.BundleID != "" || artifact.SigningMode != "" || artifact.MinimumMacOSVersion != "" ||
			artifact.IntegrationProtocol != 0 || artifact.HelperProtocol != 0 {
			return errors.New("relay binary artifact must not contain app signing metadata")
		}
	case ComponentMacOSMenuBarBundle:
		if artifact.OS != "darwin" || artifact.Arch != "arm64" ||
			!hasSuffix(artifact.File, ".app.zip") || !validBundleID(artifact.BundleID) ||
			artifact.SigningMode != SigningModeAdHoc {
			return errors.New("release manifest contains an invalid macOS menu bar bundle")
		}
		if revision == CompatibilityRevisionUpdater {
			if !validNumericVersion(artifact.MinimumMacOSVersion) ||
				artifact.IntegrationProtocol <= 0 || artifact.HelperProtocol <= 0 {
				return errors.New("release updater manifest contains invalid macOS compatibility metadata")
			}
		} else if artifact.MinimumMacOSVersion != "" || artifact.IntegrationProtocol != 0 || artifact.HelperProtocol != 0 {
			return errors.New("revision 4 release must not contain updater compatibility metadata")
		}
	default:
		return errors.New("release manifest contains an unsupported artifact component")
	}
	return nil
}

func validateMenuBarArtifactSet(artifacts []Artifact) error {
	required := map[string]struct{}{}
	for _, target := range [][2]string{{"linux", "amd64"}, {"linux", "arm64"}} {
		for _, component := range []string{ComponentRelay, ComponentRelayctl} {
			required[target[0]+"/"+target[1]+"/"+component] = struct{}{}
		}
	}
	required["darwin/arm64/"+ComponentMacOSMenuBarBundle] = struct{}{}
	for _, artifact := range artifacts {
		key := artifact.OS + "/" + artifact.Arch + "/" + artifact.Component
		if _, wanted := required[key]; !wanted {
			return errors.New("release manifest contains an unexpected component-aware artifact")
		}
		delete(required, key)
	}
	if len(required) != 0 {
		return errors.New("release manifest is missing a required component-aware artifact")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("decode release manifest: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("release manifest contains trailing JSON")
	}
	return nil
}

func scanJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := map[string]struct{}{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("release manifest object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("release manifest contains duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim('}') {
			return errors.New("release manifest object is not terminated")
		}
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		closing, err := decoder.Token()
		if err != nil || closing != json.Delim(']') {
			return errors.New("release manifest array is not terminated")
		}
	default:
		return errors.New("release manifest contains an unexpected delimiter")
	}
	return nil
}

func channelForVersion(value string) (string, bool) {
	version, err := ParseSemanticVersion(value)
	if err != nil {
		return "", false
	}
	if version.IsPrerelease() {
		return ChannelPreview, true
	}
	return ChannelStable, true
}

func validStrictSemVer(value string) bool {
	_, err := ParseSemanticVersion(value)
	return err == nil
}

func validNumericVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if !validNumericIdentifier(part) {
			return false
		}
	}
	return true
}

func validNumericIdentifier(value string) bool {
	if value == "" || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func parsePublicKey(data []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(trimSpace(rest)) != 0 {
		return nil, errors.New("release public key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse release public key: %w", err)
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("release public key must be Ed25519")
	}
	return key, nil
}

func trimSpace(value []byte) []byte {
	start, end := 0, len(value)
	for start < end && (value[start] == ' ' || value[start] == '\n' || value[start] == '\r' || value[start] == '\t') {
		start++
	}
	for end > start && (value[end-1] == ' ' || value[end-1] == '\n' || value[end-1] == '\r' || value[end-1] == '\t') {
		end--
	}
	return value[start:end]
}

func validHTTPSURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

func hasSuffix(value, suffix string) bool {
	if len(value) < len(suffix) {
		return false
	}
	return value[len(value)-len(suffix):] == suffix
}

func validBundleID(value string) bool {
	if len(value) < 3 || len(value) > 255 {
		return false
	}
	lastDot := -1
	for index, char := range value {
		if char == '.' {
			if index == 0 || index == len(value)-1 || lastDot == index-1 {
				return false
			}
			lastDot = index
			continue
		}
		if (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') &&
			(char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return lastDot > 0
}
