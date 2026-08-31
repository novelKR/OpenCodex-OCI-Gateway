package release

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	ProductionRepository       = "novelKR/OpenCodex-OCI-Gateway"
	ProductionAPIBaseURL       = "https://api.github.com"
	CheckSchemaVersion         = 1
	maximumGitHubResponseBytes = 1 << 20
	maximumManifestBytes       = 64 << 10
	maximumSignatureBytes      = 4 << 10
	maximumReleasePages        = 5
	maximumReleasesPerPage     = 100
	maximumRequestAttempts     = 3
)

type UpdateChannel string

const (
	UpdateChannelStable  UpdateChannel = ChannelStable
	UpdateChannelPreview UpdateChannel = ChannelPreview
)

type CheckStatus string

const (
	CheckStatusCurrent                  CheckStatus = "current"
	CheckStatusNewerThanSelectedChannel CheckStatus = "newer_than_selected_channel"
	CheckStatusUpdateAvailable          CheckStatus = "update_available"
	CheckStatusOffline                  CheckStatus = "offline"
	CheckStatusRateLimited              CheckStatus = "rate_limited"
	CheckStatusInvalidRelease           CheckStatus = "invalid_release"
	CheckStatusUpdaterTooOld            CheckStatus = "updater_too_old"
	CheckStatusUnsupportedSystem        CheckStatus = "unsupported_system"
)

type ETagCacheState string

const (
	ETagCacheMiss        ETagCacheState = "miss"
	ETagCacheRefreshed   ETagCacheState = "refreshed"
	ETagCacheNotModified ETagCacheState = "not_modified"
	ETagCacheUnavailable ETagCacheState = "unavailable"
)

type CheckRequest struct {
	Channel        UpdateChannel
	CurrentVersion string
	PublicKeyPEM   []byte
}

type CheckResult struct {
	SchemaVersion         int            `json:"schema_version"`
	Status                CheckStatus    `json:"status"`
	Channel               UpdateChannel  `json:"channel"`
	CurrentVersion        string         `json:"current_version"`
	CheckedAt             string         `json:"checked_at"`
	ETagCacheState        ETagCacheState `json:"etag_cache_state"`
	ReleaseID             int64          `json:"release_id,omitempty"`
	Tag                   string         `json:"tag,omitempty"`
	Version               string         `json:"version,omitempty"`
	ReleaseURL            string         `json:"release_url,omitempty"`
	ManifestSHA256        string         `json:"manifest_sha256,omitempty"`
	AppAssetID            int64          `json:"app_asset_id,omitempty"`
	AppSHA256             string         `json:"app_sha256,omitempty"`
	MinimumUpdaterVersion string         `json:"minimum_updater_version,omitempty"`
	MinimumMacOSVersion   string         `json:"minimum_macos_version,omitempty"`
	IntegrationProtocol   int            `json:"integration_protocol,omitempty"`
	HelperProtocol        int            `json:"helper_protocol,omitempty"`
	TrustKeyID            string         `json:"trust_key_id,omitempty"`
}

type CheckCacheEntry struct {
	SchemaVersion int             `json:"schema_version"`
	RequestURL    string          `json:"request_url"`
	ETag          string          `json:"etag"`
	Body          json.RawMessage `json:"body"`
}

type CheckCache interface {
	Load(context.Context, string) (CheckCacheEntry, bool, error)
	Save(context.Context, CheckCacheEntry) error
}

type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

type SleepFunc func(context.Context, time.Duration) error

type CheckerConfig struct {
	HTTPClient     HTTPDoer
	Cache          CheckCache
	APIBaseURL     string
	Repository     string
	UpdaterVersion string
	SystemVersion  string
	Now            func() time.Time
	Sleep          SleepFunc
}

type Checker struct {
	httpClient     HTTPDoer
	cache          CheckCache
	apiBaseURL     string
	repository     string
	updaterVersion string
	systemVersion  string
	now            func() time.Time
	sleep          SleepFunc
}

func NewChecker(config CheckerConfig) (*Checker, error) {
	if config.HTTPClient == nil || config.Cache == nil || config.Now == nil || config.Sleep == nil {
		return nil, errors.New("release checker dependencies are incomplete")
	}
	base, err := url.Parse(config.APIBaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("release checker API base URL is invalid")
	}
	if !validRepository(config.Repository) {
		return nil, errors.New("release checker repository is invalid")
	}
	if _, err := ParseSemanticVersion(config.UpdaterVersion); err != nil {
		return nil, errors.New("release checker updater version is invalid")
	}
	return &Checker{
		httpClient:     config.HTTPClient,
		cache:          config.Cache,
		apiBaseURL:     strings.TrimRight(config.APIBaseURL, "/"),
		repository:     config.Repository,
		updaterVersion: config.UpdaterVersion,
		systemVersion:  config.SystemVersion,
		now:            config.Now,
		sleep:          config.Sleep,
	}, nil
}

func (c *Checker) Check(ctx context.Context, request CheckRequest) (CheckResult, error) {
	current, err := ParseSemanticVersion(request.CurrentVersion)
	if err != nil {
		return CheckResult{}, errors.New("current version must be strict SemVer")
	}
	if request.Channel != UpdateChannelStable && request.Channel != UpdateChannelPreview {
		return CheckResult{}, errors.New("update channel must be stable or preview")
	}
	trustKeyID, err := publicKeyID(request.PublicKeyPEM)
	if err != nil {
		return CheckResult{}, fmt.Errorf("local release trust key is invalid: %w", err)
	}
	result := CheckResult{
		SchemaVersion:  CheckSchemaVersion,
		Status:         CheckStatusCurrent,
		Channel:        request.Channel,
		CurrentVersion: request.CurrentVersion,
		CheckedAt:      c.now().UTC().Format(time.RFC3339),
		ETagCacheState: ETagCacheMiss,
	}
	releases, cacheState, remoteStatus, err := c.listReleases(ctx)
	result.ETagCacheState = cacheState
	if err != nil {
		return CheckResult{}, err
	}
	if remoteStatus != "" {
		result.Status = remoteStatus
		return result, nil
	}
	candidate, candidateVersion, found, valid := selectCandidate(releases, request.Channel)
	if !valid {
		result.Status = CheckStatusInvalidRelease
		return result, nil
	}
	if !found {
		return result, nil
	}
	verified, exact, appAsset, manifestDigest, remoteStatus := c.verifyCandidate(
		ctx, candidate, request.Channel, request.PublicKeyPEM, trustKeyID,
	)
	if remoteStatus != "" {
		result.Status = remoteStatus
		return result, nil
	}
	result.ReleaseID = candidate.ID
	result.Tag = candidate.TagName
	result.Version = candidate.TagName
	result.ReleaseURL = canonicalReleaseURL(c.repository, candidate.TagName)
	result.ManifestSHA256 = manifestDigest
	result.AppAssetID = appAsset.ID
	result.AppSHA256 = appAsset.manifestDigest
	if exact.TagName != candidate.TagName {
		result.Status = CheckStatusInvalidRelease
		return result, nil
	}
	if verified.CompatibilityRevision == CompatibilityRevisionUpdater {
		result.MinimumUpdaterVersion = verified.MinimumUpdaterVersion
		result.MinimumMacOSVersion = appAsset.artifact.MinimumMacOSVersion
		result.IntegrationProtocol = appAsset.artifact.IntegrationProtocol
		result.HelperProtocol = appAsset.artifact.HelperProtocol
		result.TrustKeyID = verified.TrustKeyID
	}
	comparison := current.Compare(candidateVersion)
	if comparison == 0 {
		result.Status = CheckStatusCurrent
		return result, nil
	}
	if comparison > 0 {
		result.Status = CheckStatusNewerThanSelectedChannel
		return result, nil
	}
	if verified.CompatibilityRevision == CompatibilityRevisionUpdater {
		updater, _ := ParseSemanticVersion(c.updaterVersion)
		minimumUpdater, _ := ParseSemanticVersion(verified.MinimumUpdaterVersion)
		if updater.Compare(minimumUpdater) < 0 {
			result.Status = CheckStatusUpdaterTooOld
			return result, nil
		}
		if !systemVersionAtLeast(c.systemVersion, appAsset.artifact.MinimumMacOSVersion) {
			result.Status = CheckStatusUnsupportedSystem
			return result, nil
		}
	}
	result.Status = CheckStatusUpdateAvailable
	return result, nil
}

type githubRelease struct {
	ID         int64         `json:"id"`
	TagName    string        `json:"tag_name"`
	Draft      *bool         `json:"draft"`
	Prerelease *bool         `json:"prerelease"`
	Immutable  *bool         `json:"immutable"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type verifiedAppAsset struct {
	githubAsset
	manifestDigest string
	artifact       Artifact
}

func (c *Checker) listReleases(ctx context.Context) ([]githubRelease, ETagCacheState, CheckStatus, error) {
	var releases []githubRelease
	cacheState := ETagCacheMiss
	for page := 1; page <= maximumReleasePages; page++ {
		requestURL := fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d", c.apiBaseURL, c.repository, maximumReleasesPerPage, page)
		etag := ""
		var cached CheckCacheEntry
		cacheFound := false
		if page == 1 {
			var err error
			cached, cacheFound, err = c.cache.Load(ctx, requestURL)
			if err != nil {
				return nil, cacheState, "", fmt.Errorf("load release ETag cache: %w", err)
			}
			if cacheFound {
				etag = cached.ETag
			}
		}
		body, responseETag, notModified, status := c.fetch(ctx, requestURL, maximumGitHubResponseBytes, etag)
		if status != "" {
			return nil, cacheState, status, nil
		}
		if notModified {
			if !cacheFound {
				return nil, cacheState, CheckStatusInvalidRelease, nil
			}
			body = cached.Body
			cacheState = ETagCacheNotModified
		} else if page == 1 {
			if responseETag != "" {
				entry := CheckCacheEntry{
					SchemaVersion: CheckSchemaVersion,
					RequestURL:    requestURL,
					ETag:          responseETag,
					Body:          append(json.RawMessage(nil), body...),
				}
				if err := c.cache.Save(ctx, entry); err != nil {
					cacheState = ETagCacheUnavailable
				} else {
					cacheState = ETagCacheRefreshed
				}
			}
		}
		pageReleases, err := decodeReleaseList(body)
		if err != nil {
			return nil, cacheState, CheckStatusInvalidRelease, nil
		}
		releases = append(releases, pageReleases...)
		if len(pageReleases) < maximumReleasesPerPage {
			return releases, cacheState, "", nil
		}
	}
	return nil, cacheState, CheckStatusInvalidRelease, nil
}

func selectCandidate(releases []githubRelease, channel UpdateChannel) (githubRelease, SemanticVersion, bool, bool) {
	var selected githubRelease
	var selectedVersion SemanticVersion
	found := false
	seen := map[string]int64{}
	for _, release := range releases {
		if release.ID <= 0 || release.TagName == "" || release.Draft == nil || *release.Draft {
			continue
		}
		version, err := ParseSemanticVersion(release.TagName)
		if err != nil {
			continue
		}
		if channel == UpdateChannelStable && version.IsPrerelease() {
			continue
		}
		if previousID, duplicate := seen[version.String()]; duplicate && previousID != release.ID {
			return githubRelease{}, SemanticVersion{}, false, false
		}
		seen[version.String()] = release.ID
		if !found || version.Compare(selectedVersion) > 0 {
			selected = release
			selectedVersion = version
			found = true
		}
	}
	return selected, selectedVersion, found, true
}

func (c *Checker) verifyCandidate(
	ctx context.Context,
	candidate githubRelease,
	channel UpdateChannel,
	publicKeyPEM []byte,
	trustKeyID string,
) (Manifest, githubRelease, verifiedAppAsset, string, CheckStatus) {
	exactURL := fmt.Sprintf("%s/repos/%s/releases/%d", c.apiBaseURL, c.repository, candidate.ID)
	body, _, _, status := c.fetch(ctx, exactURL, maximumGitHubResponseBytes, "")
	if status != "" {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", status
	}
	exact, err := decodeExactRelease(body)
	if err != nil || exact.ID != candidate.ID || exact.TagName != candidate.TagName ||
		exact.Draft == nil || *exact.Draft || exact.Prerelease == nil || exact.Immutable == nil || !*exact.Immutable {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	version, err := ParseSemanticVersion(exact.TagName)
	if err != nil || *exact.Prerelease != version.IsPrerelease() ||
		(channel == UpdateChannelStable && version.IsPrerelease()) {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	assets, ok := exactAssetSet(exact.TagName, exact.Assets)
	if !ok {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	manifestAsset := assets["manifest-"+exact.TagName+".json"]
	signatureAsset := assets["manifest-"+exact.TagName+".sig"]
	manifestBytes, _, _, status := c.fetch(
		ctx, c.assetURL(manifestAsset.ID), maximumManifestBytes, "",
	)
	if status != "" {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", status
	}
	signatureBytes, _, _, status := c.fetch(
		ctx, c.assetURL(signatureAsset.ID), maximumSignatureBytes, "",
	)
	if status != "" {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", status
	}
	manifestDigest := sha256Hex(manifestBytes)
	if manifestAsset.Digest != "sha256:"+manifestDigest ||
		signatureAsset.Digest != "sha256:"+sha256Hex(signatureBytes) {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	verified, err := Verify(manifestBytes, signatureBytes, publicKeyPEM)
	if err != nil || verified.Version != exact.TagName {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	appArtifact, err := verified.SelectComponent("darwin", "arm64", ComponentMacOSMenuBarBundle)
	if err != nil || appArtifact.URL != canonicalAssetURL(c.repository, exact.TagName, "OpenCodexRelay.app.zip") {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	if verified.CompatibilityRevision == CompatibilityRevisionUpdater {
		if verified.TrustKeyID != trustKeyID {
			return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
		}
	} else if verified.CompatibilityRevision != CompatibilityRevisionAdHocApp {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	appAsset := assets["OpenCodexRelay.app.zip"]
	if appAsset.Digest != "sha256:"+appArtifact.SHA256 {
		return Manifest{}, githubRelease{}, verifiedAppAsset{}, "", CheckStatusInvalidRelease
	}
	return verified, exact, verifiedAppAsset{
		githubAsset:    appAsset,
		manifestDigest: appArtifact.SHA256,
		artifact:       appArtifact,
	}, manifestDigest, ""
}

func exactAssetSet(tag string, assets []githubAsset) (map[string]githubAsset, bool) {
	expected := map[string]struct{}{
		"OpenCodexRelay.app.zip":         {},
		"THIRD_PARTY_NOTICES.md":         {},
		"manifest-" + tag + ".json":      {},
		"manifest-" + tag + ".sig":       {},
		"opencodex-relay_linux_amd64":    {},
		"opencodex-relay_linux_arm64":    {},
		"opencodex-relayctl_linux_amd64": {},
		"opencodex-relayctl_linux_arm64": {},
	}
	if len(assets) != len(expected) {
		return nil, false
	}
	result := make(map[string]githubAsset, len(assets))
	for _, asset := range assets {
		if asset.ID <= 0 || asset.State != "uploaded" || asset.Size <= 0 ||
			!strings.HasPrefix(asset.Digest, "sha256:") || !isLowerHexSHA256(strings.TrimPrefix(asset.Digest, "sha256:")) {
			return nil, false
		}
		if _, exists := expected[asset.Name]; !exists {
			return nil, false
		}
		if _, duplicate := result[asset.Name]; duplicate {
			return nil, false
		}
		result[asset.Name] = asset
	}
	return result, len(result) == len(expected)
}

func (c *Checker) assetURL(assetID int64) string {
	return fmt.Sprintf("%s/repos/%s/releases/assets/%d", c.apiBaseURL, c.repository, assetID)
}

func (c *Checker) fetch(
	ctx context.Context,
	requestURL string,
	maximumBytes int64,
	etag string,
) ([]byte, string, bool, CheckStatus) {
	for attempt := 0; attempt < maximumRequestAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return nil, "", false, CheckStatusInvalidRelease
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("User-Agent", "OpenCodexRelay-Updater/1")
		if strings.Contains(requestURL, "/releases/assets/") {
			request.Header.Set("Accept", "application/octet-stream")
		}
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			if attempt+1 < maximumRequestAttempts {
				if !c.waitForRetry(ctx, c.responseBackoff(nil, attempt)) {
					return nil, "", false, CheckStatusOffline
				}
				continue
			}
			return nil, "", false, CheckStatusOffline
		}
		if response.Body == nil {
			return nil, "", false, CheckStatusInvalidRelease
		}
		if response.StatusCode == http.StatusNotModified {
			response.Body.Close()
			return nil, boundedETag(response.Header.Get("ETag")), true, ""
		}
		if delay, rateLimited, retryable := rateLimitDelay(response, c.now()); rateLimited {
			response.Body.Close()
			if retryable && delay <= 5*time.Second && attempt+1 < maximumRequestAttempts {
				if !c.waitForRetry(ctx, delay) {
					return nil, "", false, CheckStatusOffline
				}
				continue
			}
			return nil, "", false, CheckStatusRateLimited
		}
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			if attempt+1 < maximumRequestAttempts {
				delay := c.responseBackoff(response, attempt)
				response.Body.Close()
				if !c.waitForRetry(ctx, delay) {
					return nil, "", false, CheckStatusOffline
				}
				continue
			}
			response.Body.Close()
			return nil, "", false, CheckStatusOffline
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, "", false, CheckStatusInvalidRelease
		}
		body, oversized, readErr := readBounded(response.Body, response.ContentLength, maximumBytes)
		response.Body.Close()
		if readErr != nil {
			return nil, "", false, CheckStatusOffline
		}
		if oversized {
			return nil, "", false, CheckStatusInvalidRelease
		}
		return body, boundedETag(response.Header.Get("ETag")), false, ""
	}
	return nil, "", false, CheckStatusOffline
}

func (c *Checker) waitForRetry(ctx context.Context, delay time.Duration) bool {
	return c.sleep(ctx, delay) == nil
}

func (c *Checker) responseBackoff(response *http.Response, attempt int) time.Duration {
	if response != nil {
		if delay, ok := parseRetryAfter(response.Header.Get("Retry-After"), c.now()); ok {
			return min(delay, 5*time.Second)
		}
	}
	return time.Duration(1<<attempt) * 250 * time.Millisecond
}

func parseRetryAfter(value string, now time.Time) (time.Duration, bool) {
	if value == "" || len(value) > 128 {
		return 0, false
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second, true
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	return max(0, when.Sub(now)), true
}

func rateLimitDelay(response *http.Response, now time.Time) (time.Duration, bool, bool) {
	limited := response.StatusCode == http.StatusTooManyRequests ||
		(response.StatusCode == http.StatusForbidden && response.Header.Get("X-RateLimit-Remaining") == "0")
	if !limited {
		return 0, false, false
	}
	if delay, ok := parseRetryAfter(response.Header.Get("Retry-After"), now); ok {
		return delay, true, true
	}
	reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err == nil && reset >= 0 {
		return max(0, time.Unix(reset, 0).Sub(now)), true, true
	}
	return 0, true, false
}

func readBounded(reader io.Reader, contentLength, maximumBytes int64) ([]byte, bool, error) {
	if contentLength > maximumBytes {
		return nil, true, nil
	}
	limited := &io.LimitedReader{R: reader, N: maximumBytes + 1}
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, false, err
	}
	return body, int64(len(body)) > maximumBytes, nil
}

func decodeReleaseList(body []byte) ([]githubRelease, error) {
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var releases []githubRelease
	if err := decoder.Decode(&releases); err != nil {
		return nil, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	for _, release := range releases {
		if release.ID <= 0 || release.TagName == "" || release.Draft == nil || release.Prerelease == nil {
			return nil, errors.New("GitHub release list is incomplete")
		}
	}
	return releases, nil
}

func decodeExactRelease(body []byte) (githubRelease, error) {
	if err := rejectDuplicateJSONKeys(body); err != nil {
		return githubRelease{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	var release githubRelease
	if err := decoder.Decode(&release); err != nil {
		return githubRelease{}, err
	}
	if err := requireJSONEOF(decoder); err != nil {
		return githubRelease{}, err
	}
	return release, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("JSON response contains trailing data")
	}
	return nil
}

func publicKeyID(publicKeyPEM []byte) (string, error) {
	key, err := parsePublicKey(publicKeyPEM)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	return sha256Hex(der), nil
}

func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func canonicalReleaseURL(repository, tag string) string {
	return "https://github.com/" + repository + "/releases/tag/" + tag
}

func canonicalAssetURL(repository, tag, name string) string {
	return "https://github.com/" + repository + "/releases/download/" + tag + "/" + name
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || len(part) > 100 {
			return false
		}
		for _, character := range part {
			if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
				(character < '0' || character > '9') && character != '-' && character != '_' && character != '.' {
				return false
			}
		}
	}
	return true
}

func boundedETag(value string) string {
	if len(value) > 256 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}

func systemVersionAtLeast(current, minimum string) bool {
	currentParts, currentOK := numericVersionParts(current)
	minimumParts, minimumOK := numericVersionParts(minimum)
	if !currentOK || !minimumOK {
		return false
	}
	for index := range currentParts {
		if currentParts[index] != minimumParts[index] {
			return currentParts[index] > minimumParts[index]
		}
	}
	return true
}

func numericVersionParts(value string) ([3]int, bool) {
	var result [3]int
	parts := strings.Split(value, ".")
	if len(parts) < 1 || len(parts) > 3 {
		return result, false
	}
	for index, part := range parts {
		if !validNumericIdentifier(part) || len(part) > 6 {
			return result, false
		}
		parsed, err := strconv.Atoi(part)
		if err != nil {
			return result, false
		}
		result[index] = parsed
	}
	return result, true
}
