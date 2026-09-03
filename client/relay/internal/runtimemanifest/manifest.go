// Package runtimemanifest verifies the separately signed OpenCodex runtime
// image manifest. It deliberately does not share the Relay application release
// trust root or schema.
package runtimemanifest

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/release"
)

const (
	SchemaVersion             = 1
	ArtifactKind              = "opencodex-runtime-image"
	StableChannel             = "stable"
	ProductionSourceRepo      = "novelKR/OpenCodex-OCI-Gateway"
	ProductionUpstreamRepo    = "lidge-jun/opencodex"
	ProductionImageRepository = "ghcr.io/novelkr/opencodex-runtime"
	NPMPackage                = "@bitkyc08/opencodex"
	SecretDeliveryUDSV1       = "uds-v1"
	MaximumManifestBytes      = 64 << 10
	MaximumSignatureBytes     = 4 << 10
	MaximumPublicKeyBytes     = 8 << 10
)

type Manifest struct {
	Schema          int           `json:"schema"`
	ArtifactKind    string        `json:"artifact_kind"`
	ArtifactVersion string        `json:"artifact_version"`
	ReleaseSequence uint64        `json:"release_sequence"`
	Channel         string        `json:"channel"`
	Source          Source        `json:"source"`
	Upstream        Upstream      `json:"upstream"`
	Image           Image         `json:"image"`
	Compatibility   Compatibility `json:"compatibility"`
	Canary          Canary        `json:"canary"`
	TrustKeyID      string        `json:"trust_key_id"`
}

type Source struct {
	Repository         string `json:"repository"`
	Revision           string `json:"revision"`
	UpstreamLockSHA256 string `json:"upstream_lock_sha256"`
}

type Upstream struct {
	Repository   string `json:"repository"`
	ReleaseID    int64  `json:"release_id"`
	ReleaseTag   string `json:"release_tag"`
	Version      string `json:"version"`
	Revision     string `json:"revision"`
	NPMPackage   string `json:"npm_package"`
	NPMIntegrity string `json:"npm_integrity"`
}

type Image struct {
	Repository  string     `json:"repository"`
	IndexDigest string     `json:"index_digest"`
	Platforms   []Platform `json:"platforms"`
}

type Platform struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Digest string `json:"digest"`
}

type Compatibility struct {
	MinimumRelayVersion   string `json:"minimum_relay_version"`
	MinimumMacOS          string `json:"minimum_macos"`
	MinimumAppleContainer string `json:"minimum_apple_container"`
	ManagementAPIRevision int    `json:"management_api_revision"`
	SecretDelivery        string `json:"secret_delivery"`
	StateFormatRevision   int    `json:"state_format_revision"`
}

type Canary struct {
	SourceRevision     string `json:"source_revision"`
	WorkflowRunID      string `json:"workflow_run_id"`
	WorkflowRunAttempt int    `json:"workflow_run_attempt"`
	Result             string `json:"result"`
}

type VerifyOptions struct {
	HighestSeenSequence   uint64
	RelayVersion          string
	MacOSVersion          string
	AppleContainerVersion string
}

// Verify authenticates the exact bytes before decoding any attacker-controlled
// manifest fields, then applies the strict Runtime schema and compatibility
// policy. Size bounds are admission controls and therefore precede signature
// verification.
func Verify(manifestBytes, signatureBytes, publicKeyPEM []byte, options VerifyOptions) (Manifest, error) {
	if len(manifestBytes) == 0 || len(manifestBytes) > MaximumManifestBytes {
		return Manifest{}, errors.New("runtime manifest size is invalid")
	}
	if len(signatureBytes) == 0 || len(signatureBytes) > MaximumSignatureBytes {
		return Manifest{}, errors.New("runtime manifest signature size is invalid")
	}
	key, keyID, err := ParsePublicKey(publicKeyPEM)
	if err != nil {
		return Manifest{}, err
	}
	signature, err := base64.StdEncoding.Strict().DecodeString(string(trimASCIIWhitespace(signatureBytes)))
	if err != nil || len(signature) != ed25519.SignatureSize {
		return Manifest{}, errors.New("runtime manifest signature is invalid")
	}
	if !ed25519.Verify(key, manifestBytes, signature) {
		return Manifest{}, errors.New("runtime manifest signature is invalid")
	}
	if err := rejectDuplicateJSONKeys(manifestBytes); err != nil {
		return Manifest{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(manifestBytes))
	decoder.DisallowUnknownFields()
	var manifest Manifest
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode runtime manifest: %w", err)
	}
	if err := requireEOF(decoder); err != nil {
		return Manifest{}, err
	}
	if err := validate(manifest, keyID, options); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func VerifyFiles(manifestPath, signaturePath, publicKeyPath string, options VerifyOptions) (Manifest, []byte, error) {
	manifest, err := readBoundedRegular(manifestPath, MaximumManifestBytes, false)
	if err != nil {
		return Manifest{}, nil, err
	}
	signature, err := readBoundedRegular(signaturePath, MaximumSignatureBytes, false)
	if err != nil {
		return Manifest{}, nil, err
	}
	key, err := readBoundedRegular(publicKeyPath, MaximumPublicKeyBytes, false)
	if err != nil {
		return Manifest{}, nil, err
	}
	verified, err := Verify(manifest, signature, key, options)
	return verified, manifest, err
}

func ParsePublicKey(data []byte) (ed25519.PublicKey, string, error) {
	if len(data) == 0 || len(data) > MaximumPublicKeyBytes {
		return nil, "", errors.New("runtime public key size is invalid")
	}
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(trimASCIIWhitespace(rest)) != 0 {
		return nil, "", errors.New("runtime public key is not PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, "", errors.New("runtime public key is invalid")
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, "", errors.New("runtime public key must be Ed25519")
	}
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, "", errors.New("runtime public key is invalid")
	}
	digest := sha256.Sum256(der)
	return key, hex.EncodeToString(digest[:]), nil
}

func ManifestSHA256(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func (m Manifest) ARM64Digest() (string, bool) {
	for _, platform := range m.Image.Platforms {
		if platform.OS == "linux" && platform.Arch == "arm64" {
			return platform.Digest, true
		}
	}
	return "", false
}

func validate(m Manifest, keyID string, options VerifyOptions) error {
	if m.Schema != SchemaVersion || m.ArtifactKind != ArtifactKind || m.Channel != StableChannel {
		return errors.New("runtime manifest identity is invalid")
	}
	upstreamVersion, revision, ok := ParseArtifactVersion(m.ArtifactVersion)
	if !ok || revision < 1 || m.Upstream.Version != upstreamVersion || m.Upstream.ReleaseTag != "v"+upstreamVersion {
		return errors.New("runtime manifest artifact version is invalid")
	}
	if parsed, err := release.ParseSemanticVersion(upstreamVersion); err != nil || parsed.IsPrerelease() {
		return errors.New("runtime manifest upstream version is invalid")
	}
	if m.ReleaseSequence == 0 || m.ReleaseSequence < options.HighestSeenSequence {
		return errors.New("runtime manifest release sequence is invalid")
	}
	if m.Source.Repository != ProductionSourceRepo || !isLowerHex(m.Source.Revision, 40) ||
		!isLowerHex(m.Source.UpstreamLockSHA256, 64) {
		return errors.New("runtime manifest source is invalid")
	}
	if m.Upstream.Repository != ProductionUpstreamRepo || m.Upstream.ReleaseID <= 0 ||
		!isLowerHex(m.Upstream.Revision, 40) || m.Upstream.NPMPackage != NPMPackage ||
		!validNPMIntegrity(m.Upstream.NPMIntegrity) {
		return errors.New("runtime manifest upstream identity is invalid")
	}
	if m.Image.Repository != ProductionImageRepository || !validOCIDigest(m.Image.IndexDigest) {
		return errors.New("runtime manifest image identity is invalid")
	}
	if len(m.Image.Platforms) != 2 {
		return errors.New("runtime manifest requires exactly amd64 and arm64 Linux images")
	}
	expectedArchitectures := [...]string{"amd64", "arm64"}
	for index, platform := range m.Image.Platforms {
		if platform.OS != "linux" || platform.Arch != expectedArchitectures[index] ||
			!validOCIDigest(platform.Digest) || platform.Digest == m.Image.IndexDigest {
			return errors.New("runtime manifest image platform set is invalid")
		}
	}
	if m.Image.Platforms[0].Digest == m.Image.Platforms[1].Digest {
		return errors.New("runtime manifest platform image digests must be distinct")
	}
	if !validCompatibility(m.Compatibility) {
		return errors.New("runtime manifest compatibility is invalid")
	}
	if options.RelayVersion != "" && !versionAtLeast(options.RelayVersion, m.Compatibility.MinimumRelayVersion) {
		return errors.New("runtime manifest requires a newer Relay")
	}
	if options.MacOSVersion != "" && !numericVersionAtLeast(options.MacOSVersion, m.Compatibility.MinimumMacOS) {
		return errors.New("runtime manifest requires a newer macOS")
	}
	if options.AppleContainerVersion != "" && !versionAtLeast(options.AppleContainerVersion, m.Compatibility.MinimumAppleContainer) {
		return errors.New("runtime manifest requires a newer Apple Container")
	}
	workflowRun, err := strconv.ParseUint(m.Canary.WorkflowRunID, 10, 64)
	if m.Canary.SourceRevision != m.Source.Revision || workflowRun == 0 || err != nil ||
		m.Canary.WorkflowRunAttempt < 1 || m.Canary.Result != "passed" {
		return errors.New("runtime manifest canary witness is invalid")
	}
	if m.TrustKeyID != keyID || !isLowerHex(m.TrustKeyID, 64) {
		return errors.New("runtime manifest trust key ID is invalid")
	}
	return nil
}

func ParseArtifactVersion(value string) (string, uint64, bool) {
	marker := strings.LastIndex(value, "-r")
	if marker <= 0 {
		return "", 0, false
	}
	revisionText := value[marker+2:]
	parsedRevision, err := strconv.ParseUint(revisionText, 10, 64)
	if err != nil || parsedRevision < 1 || strconv.FormatUint(parsedRevision, 10) != revisionText {
		return "", 0, false
	}
	version := value[:marker]
	// Keep the Go consumer byte-for-byte aligned with the release builder's
	// strict X.Y.Z-rN contract. ParseSemanticVersion also understands
	// prereleases; artifact versions deliberately do not.
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return "", 0, false
	}
	for _, part := range parts {
		if !validCanonicalNumericIdentifier(part) {
			return "", 0, false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return "", 0, false
		}
	}
	parsed, err := release.ParseSemanticVersion(version)
	if err != nil || parsed.IsPrerelease() {
		return "", 0, false
	}
	return version, parsedRevision, true
}

func validCompatibility(value Compatibility) bool {
	if _, err := release.ParseSemanticVersion(value.MinimumRelayVersion); err != nil {
		return false
	}
	if _, err := release.ParseSemanticVersion(value.MinimumAppleContainer); err != nil {
		return false
	}
	return validNumericVersion(value.MinimumMacOS) && value.ManagementAPIRevision == 1 &&
		value.SecretDelivery == SecretDeliveryUDSV1 && value.StateFormatRevision == 1
}

func versionAtLeast(current, minimum string) bool {
	currentVersion, err := release.ParseSemanticVersion(current)
	if err != nil {
		return false
	}
	minimumVersion, err := release.ParseSemanticVersion(minimum)
	return err == nil && currentVersion.Compare(minimumVersion) >= 0
}

func numericVersionAtLeast(current, minimum string) bool {
	if !validNumericVersion(current) || !validNumericVersion(minimum) {
		return false
	}
	left := numericVersionParts(current)
	right := numericVersionParts(minimum)
	for index := range left {
		if left[index] != right[index] {
			return left[index] > right[index]
		}
	}
	return true
}

func numericVersionParts(value string) [3]uint64 {
	var result [3]uint64
	for index, part := range strings.Split(value, ".") {
		result[index], _ = strconv.ParseUint(part, 10, 32)
	}
	return result
}

func validNumericVersion(value string) bool {
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return false
	}
	for _, part := range parts {
		if !validCanonicalNumericIdentifier(part) {
			return false
		}
		if _, err := strconv.ParseUint(part, 10, 32); err != nil {
			return false
		}
	}
	return true
}

func validCanonicalNumericIdentifier(value string) bool {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return true
}

func validNPMIntegrity(value string) bool {
	if !strings.HasPrefix(value, "sha512-") {
		return false
	}
	digest, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(value, "sha512-"))
	return err == nil && len(digest) == sha512DigestSize
}

const sha512DigestSize = 64

func validOCIDigest(value string) bool {
	return strings.HasPrefix(value, "sha256:") && isLowerHex(strings.TrimPrefix(value, "sha256:"), 64)
}

func isLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func readBoundedRegular(path string, maximum int64, ownerOnly bool) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("runtime trust input path must be absolute")
	}
	metadata, err := os.Lstat(path)
	if err != nil || !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 ||
		metadata.Size() < 1 || metadata.Size() > maximum || (ownerOnly && metadata.Mode().Perm()&0o077 != 0) {
		return nil, errors.New("runtime trust input is not a bounded regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, errors.New("runtime trust input cannot be read")
	}
	defer file.Close()
	limited := &io.LimitedReader{R: file, N: maximum + 1}
	data, err := io.ReadAll(limited)
	if err != nil || len(data) == 0 || int64(len(data)) > maximum {
		return nil, errors.New("runtime trust input cannot be read safely")
	}
	return data, nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := scanJSONValue(decoder); err != nil {
		return fmt.Errorf("decode runtime manifest: %w", err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errors.New("runtime manifest contains trailing JSON")
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
				return errors.New("runtime manifest object key is not a string")
			}
			if _, exists := seen[key]; exists {
				return fmt.Errorf("runtime manifest contains duplicate JSON key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		for decoder.More() {
			if err := scanJSONValue(decoder); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	default:
		return errors.New("runtime manifest contains invalid JSON delimiter")
	}
}

func requireEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("runtime manifest contains trailing JSON")
	}
	return nil
}

func trimASCIIWhitespace(value []byte) []byte {
	return bytes.Trim(value, " \t\r\n")
}
