package runtimemanifest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/novelKR/OpenCodex-OCI-Gateway/client/relay/internal/release"
)

const (
	ProductionAPIBaseURL       = "https://api.github.com"
	RuntimeReleasePrefix       = "opencodex-runtime-"
	CheckSchemaVersion         = 1
	maximumReleasePages        = 5
	maximumReleasesPerPage     = 100
	maximumGitHubResponseBytes = 1 << 20
	maximumRequestAttempts     = 3
)

type CheckStatus string

const (
	CheckStatusCurrent         CheckStatus = "current"
	CheckStatusUpdateAvailable CheckStatus = "update_available"
	CheckStatusUnavailable     CheckStatus = "unavailable"
	CheckStatusIncompatible    CheckStatus = "incompatible"
	CheckStatusOffline         CheckStatus = "offline"
	CheckStatusRateLimited     CheckStatus = "rate_limited"
	CheckStatusInvalidRelease  CheckStatus = "invalid_release"
)

type ETagCacheState string

const (
	ETagCacheMiss        ETagCacheState = "miss"
	ETagCacheRefreshed   ETagCacheState = "refreshed"
	ETagCacheNotModified ETagCacheState = "not_modified"
	ETagCacheUnavailable ETagCacheState = "unavailable"
)

type CheckRequest struct {
	CurrentArtifactVersion string
	VerifyOptions          VerifyOptions
	PublicKeyPEM           []byte
}

type Candidate struct {
	ReleaseID      int64
	Tag            string
	ReleaseURL     string
	ManifestSHA256 string
	Manifest       Manifest
	ManifestBytes  []byte
	SignatureBytes []byte
}

type CheckResult struct {
	SchemaVersion int            `json:"schema_version"`
	Status        CheckStatus    `json:"status"`
	CheckedAt     string         `json:"checked_at"`
	ETagCache     ETagCacheState `json:"etag_cache_state"`
	Candidate     *Candidate     `json:"-"`
	Reason        string         `json:"reason,omitempty"`
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

type CheckerConfig struct {
	HTTPClient HTTPDoer
	Cache      CheckCache
	APIBaseURL string
	Repository string
	Now        func() time.Time
	Sleep      func(context.Context, time.Duration) error
}

type Checker struct {
	httpClient HTTPDoer
	cache      CheckCache
	apiBaseURL string
	repository string
	now        func() time.Time
	sleep      func(context.Context, time.Duration) error
}

func NewChecker(config CheckerConfig) (*Checker, error) {
	if config.HTTPClient == nil || config.Cache == nil || config.Now == nil || config.Sleep == nil {
		return nil, errors.New("runtime release checker dependencies are incomplete")
	}
	base, err := url.Parse(config.APIBaseURL)
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, errors.New("runtime release API base URL is invalid")
	}
	if config.Repository != ProductionSourceRepo {
		return nil, errors.New("runtime release repository is invalid")
	}
	return &Checker{
		httpClient: config.HTTPClient,
		cache:      config.Cache,
		apiBaseURL: strings.TrimRight(config.APIBaseURL, "/"),
		repository: config.Repository,
		now:        config.Now,
		sleep:      config.Sleep,
	}, nil
}

// Check selects only the separately-prefixed runtime releases. It re-reads the
// exact release by numeric ID before it trusts any asset or manifest field.
func (c *Checker) Check(ctx context.Context, request CheckRequest) (CheckResult, error) {
	result := CheckResult{
		SchemaVersion: CheckSchemaVersion,
		Status:        CheckStatusCurrent,
		CheckedAt:     c.now().UTC().Format(time.RFC3339),
		ETagCache:     ETagCacheMiss,
	}
	if len(request.PublicKeyPEM) == 0 {
		return CheckResult{}, errors.New("runtime release trust key is required")
	}
	if request.CurrentArtifactVersion != "" {
		if _, _, ok := ParseArtifactVersion(request.CurrentArtifactVersion); !ok {
			return CheckResult{}, errors.New("current runtime artifact version is invalid")
		}
	}
	releases, cacheState, remoteStatus, err := c.listReleases(ctx)
	result.ETagCache = cacheState
	if err != nil {
		return CheckResult{}, err
	}
	if remoteStatus != "" {
		result.Status = remoteStatus
		result.Reason = string(remoteStatus)
		return result, nil
	}
	candidate, found, valid := selectRuntimeCandidate(releases)
	if !valid {
		result.Status = CheckStatusInvalidRelease
		result.Reason = "ambiguous_runtime_release"
		return result, nil
	}
	if !found {
		result.Status = CheckStatusUnavailable
		result.Reason = "stable_runtime_manifest_unavailable"
		return result, nil
	}
	verified, status := c.verifyCandidate(ctx, candidate, request.PublicKeyPEM, request.VerifyOptions)
	if verified.ManifestSHA256 != "" {
		result.Candidate = &verified
	}
	if status != "" {
		result.Status = status
		result.Reason = string(status)
		return result, nil
	}
	if request.CurrentArtifactVersion == verified.Manifest.ArtifactVersion {
		return result, nil
	}
	if request.CurrentArtifactVersion != "" {
		comparison, ok := compareArtifactVersions(verified.Manifest.ArtifactVersion, request.CurrentArtifactVersion)
		if !ok || comparison <= 0 {
			result.Status = CheckStatusInvalidRelease
			result.Reason = "runtime_release_not_monotonic"
			result.Candidate = nil
			return result, nil
		}
	}
	result.Status = CheckStatusUpdateAvailable
	return result, nil
}

// ResolveExpected is the staging CAS boundary: the network is queried again
// and the expected signed-manifest digest must still identify the candidate.
func (c *Checker) ResolveExpected(ctx context.Context, expectedManifestSHA256 string, request CheckRequest) (Candidate, error) {
	if !isLowerHex(expectedManifestSHA256, 64) {
		return Candidate{}, errors.New("expected runtime manifest digest is invalid")
	}
	result, err := c.Check(ctx, request)
	if err != nil {
		return Candidate{}, err
	}
	if result.Status != CheckStatusUpdateAvailable || result.Candidate == nil || result.Candidate.ManifestSHA256 != expectedManifestSHA256 {
		return Candidate{}, errors.New("runtime release candidate changed")
	}
	return *result.Candidate, nil
}

type githubRelease struct {
	ID         int64         `json:"id"`
	TagName    string        `json:"tag_name"`
	Draft      *bool         `json:"draft"`
	Prerelease *bool         `json:"prerelease"`
	Immutable  *bool         `json:"immutable"`
	HTMLURL    string        `json:"html_url"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	State  string `json:"state"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

func (c *Checker) listReleases(ctx context.Context) ([]githubRelease, ETagCacheState, CheckStatus, error) {
	var all []githubRelease
	cacheState := ETagCacheMiss
	for page := 1; page <= maximumReleasePages; page++ {
		requestURL := fmt.Sprintf("%s/repos/%s/releases?per_page=%d&page=%d", c.apiBaseURL, c.repository, maximumReleasesPerPage, page)
		var cached CheckCacheEntry
		cacheFound := false
		etag := ""
		if page == 1 {
			var err error
			cached, cacheFound, err = c.cache.Load(ctx, requestURL)
			if err != nil {
				cacheState = ETagCacheUnavailable
			} else if cacheFound {
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
		} else if page == 1 && boundedETag(responseETag) != "" {
			entry := CheckCacheEntry{SchemaVersion: CheckSchemaVersion, RequestURL: requestURL, ETag: responseETag, Body: append(json.RawMessage(nil), body...)}
			if err := c.cache.Save(ctx, entry); err != nil {
				cacheState = ETagCacheUnavailable
			} else {
				cacheState = ETagCacheRefreshed
			}
		}
		pageReleases, err := decodeReleaseList(body)
		if err != nil {
			return nil, cacheState, CheckStatusInvalidRelease, nil
		}
		all = append(all, pageReleases...)
		if len(pageReleases) < maximumReleasesPerPage {
			return all, cacheState, "", nil
		}
	}
	return nil, cacheState, CheckStatusInvalidRelease, nil
}

func selectRuntimeCandidate(releases []githubRelease) (githubRelease, bool, bool) {
	var selected githubRelease
	selectedVersion := ""
	found := false
	seen := map[string]int64{}
	for _, candidate := range releases {
		if candidate.ID <= 0 || candidate.Draft == nil || candidate.Prerelease == nil || *candidate.Draft || *candidate.Prerelease || !strings.HasPrefix(candidate.TagName, RuntimeReleasePrefix) {
			continue
		}
		artifactVersion := strings.TrimPrefix(candidate.TagName, RuntimeReleasePrefix)
		upstream, _, ok := ParseArtifactVersion(artifactVersion)
		if !ok {
			continue
		}
		parsed, _ := release.ParseSemanticVersion(upstream)
		if parsed.IsPrerelease() {
			continue
		}
		if previous, exists := seen[artifactVersion]; exists && previous != candidate.ID {
			return githubRelease{}, false, false
		}
		seen[artifactVersion] = candidate.ID
		if !found {
			selected, selectedVersion, found = candidate, artifactVersion, true
			continue
		}
		comparison, valid := compareArtifactVersions(artifactVersion, selectedVersion)
		if !valid {
			return githubRelease{}, false, false
		}
		if comparison > 0 {
			selected, selectedVersion = candidate, artifactVersion
		}
	}
	return selected, found, true
}

func compareArtifactVersions(left, right string) (int, bool) {
	leftUpstream, leftRevision, leftOK := ParseArtifactVersion(left)
	rightUpstream, rightRevision, rightOK := ParseArtifactVersion(right)
	if !leftOK || !rightOK {
		return 0, false
	}
	leftVersion, _ := release.ParseSemanticVersion(leftUpstream)
	rightVersion, _ := release.ParseSemanticVersion(rightUpstream)
	if comparison := leftVersion.Compare(rightVersion); comparison != 0 {
		return comparison, true
	}
	switch {
	case leftRevision < rightRevision:
		return -1, true
	case leftRevision > rightRevision:
		return 1, true
	default:
		return 0, true
	}
}

func (c *Checker) verifyCandidate(ctx context.Context, listed githubRelease, publicKey []byte, options VerifyOptions) (Candidate, CheckStatus) {
	exactURL := fmt.Sprintf("%s/repos/%s/releases/%d", c.apiBaseURL, c.repository, listed.ID)
	body, _, _, status := c.fetch(ctx, exactURL, maximumGitHubResponseBytes, "")
	if status != "" {
		return Candidate{}, status
	}
	exact, err := decodeExactRelease(body)
	if err != nil || exact.ID != listed.ID || exact.TagName != listed.TagName || exact.Draft == nil || exact.Prerelease == nil || exact.Immutable == nil || *exact.Draft || *exact.Prerelease || !*exact.Immutable {
		return Candidate{}, CheckStatusInvalidRelease
	}
	artifactVersion := strings.TrimPrefix(exact.TagName, RuntimeReleasePrefix)
	if RuntimeReleasePrefix+artifactVersion != exact.TagName {
		return Candidate{}, CheckStatusInvalidRelease
	}
	manifestName := "opencodex-runtime-" + artifactVersion + ".json"
	signatureName := "opencodex-runtime-" + artifactVersion + ".sig"
	assets, ok := exactRuntimeAssets(exact.Assets, manifestName, signatureName)
	if !ok {
		return Candidate{}, CheckStatusInvalidRelease
	}
	manifestBytes, _, _, status := c.fetch(ctx, c.assetURL(assets[manifestName].ID), MaximumManifestBytes, "")
	if status != "" {
		return Candidate{}, status
	}
	signatureBytes, _, _, status := c.fetch(ctx, c.assetURL(assets[signatureName].ID), MaximumSignatureBytes, "")
	if status != "" {
		return Candidate{}, status
	}
	manifestDigest := ManifestSHA256(manifestBytes)
	if assets[manifestName].Digest != "sha256:"+manifestDigest || assets[signatureName].Digest != "sha256:"+ManifestSHA256(signatureBytes) {
		return Candidate{}, CheckStatusInvalidRelease
	}
	// Authenticate and validate the complete non-local contract first. Keeping
	// the resulting candidate lets the UI identify a genuine stable release
	// even when this Mac cannot run it yet.
	withoutLocal := options
	withoutLocal.RelayVersion = ""
	withoutLocal.MacOSVersion = ""
	withoutLocal.AppleContainerVersion = ""
	verified, err := Verify(manifestBytes, signatureBytes, publicKey, withoutLocal)
	if err != nil {
		return Candidate{}, CheckStatusInvalidRelease
	}
	if verified.ArtifactVersion != artifactVersion {
		return Candidate{}, CheckStatusInvalidRelease
	}
	releaseURL := exact.HTMLURL
	canonical := "https://github.com/" + c.repository + "/releases/tag/" + exact.TagName
	if releaseURL == "" {
		releaseURL = canonical
	}
	if releaseURL != canonical {
		return Candidate{}, CheckStatusInvalidRelease
	}
	result := Candidate{
		ReleaseID:      exact.ID,
		Tag:            exact.TagName,
		ReleaseURL:     releaseURL,
		ManifestSHA256: manifestDigest,
		Manifest:       verified,
		ManifestBytes:  append([]byte(nil), manifestBytes...),
		SignatureBytes: append([]byte(nil), signatureBytes...),
	}
	if _, err := Verify(manifestBytes, signatureBytes, publicKey, options); err != nil {
		return result, CheckStatusIncompatible
	}
	return result, ""
}

func exactRuntimeAssets(assets []githubAsset, manifestName, signatureName string) (map[string]githubAsset, bool) {
	if len(assets) != 2 {
		return nil, false
	}
	expected := map[string]struct{}{manifestName: {}, signatureName: {}}
	result := make(map[string]githubAsset, 2)
	for _, asset := range assets {
		if _, ok := expected[asset.Name]; !ok || asset.ID <= 0 || asset.State != "uploaded" || asset.Size <= 0 || !validOCIDigest(asset.Digest) {
			return nil, false
		}
		if _, duplicate := result[asset.Name]; duplicate {
			return nil, false
		}
		result[asset.Name] = asset
	}
	return result, len(result) == 2
}

func (c *Checker) assetURL(assetID int64) string {
	return fmt.Sprintf("%s/repos/%s/releases/assets/%d", c.apiBaseURL, c.repository, assetID)
}

func (c *Checker) fetch(ctx context.Context, requestURL string, maximum int64, etag string) ([]byte, string, bool, CheckStatus) {
	for attempt := 0; attempt < maximumRequestAttempts; attempt++ {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
		if err != nil {
			return nil, "", false, CheckStatusInvalidRelease
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		request.Header.Set("User-Agent", "OpenCodexRelay-RuntimeUpdater/1")
		if strings.Contains(requestURL, "/releases/assets/") {
			request.Header.Set("Accept", "application/octet-stream")
		}
		if etag != "" {
			request.Header.Set("If-None-Match", etag)
		}
		response, err := c.httpClient.Do(request)
		if err != nil {
			if attempt+1 < maximumRequestAttempts && c.sleep(ctx, time.Duration(1<<attempt)*250*time.Millisecond) == nil {
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
		if limited, delay := runtimeRateLimit(response, c.now()); limited {
			response.Body.Close()
			if delay <= 5*time.Second && attempt+1 < maximumRequestAttempts && c.sleep(ctx, delay) == nil {
				continue
			}
			return nil, "", false, CheckStatusRateLimited
		}
		if response.StatusCode >= 500 && response.StatusCode <= 599 {
			response.Body.Close()
			if attempt+1 < maximumRequestAttempts && c.sleep(ctx, time.Duration(1<<attempt)*250*time.Millisecond) == nil {
				continue
			}
			return nil, "", false, CheckStatusOffline
		}
		if response.StatusCode != http.StatusOK {
			response.Body.Close()
			return nil, "", false, CheckStatusInvalidRelease
		}
		data, tooLarge, readErr := readBounded(response.Body, response.ContentLength, maximum)
		response.Body.Close()
		if readErr != nil {
			return nil, "", false, CheckStatusOffline
		}
		if tooLarge {
			return nil, "", false, CheckStatusInvalidRelease
		}
		return data, boundedETag(response.Header.Get("ETag")), false, ""
	}
	return nil, "", false, CheckStatusOffline
}

func runtimeRateLimit(response *http.Response, now time.Time) (bool, time.Duration) {
	limited := response.StatusCode == http.StatusTooManyRequests || (response.StatusCode == http.StatusForbidden && response.Header.Get("X-RateLimit-Remaining") == "0")
	if !limited {
		return false, 0
	}
	if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds >= 0 {
		return true, time.Duration(seconds) * time.Second
	}
	if reset, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil && reset >= 0 {
		return true, max(0, time.Unix(reset, 0).Sub(now))
	}
	return true, 6 * time.Second
}

func readBounded(reader io.Reader, contentLength, maximum int64) ([]byte, bool, error) {
	if contentLength > maximum {
		return nil, true, nil
	}
	limited := &io.LimitedReader{R: reader, N: maximum + 1}
	data, err := io.ReadAll(limited)
	return data, int64(len(data)) > maximum, err
}

func decodeReleaseList(data []byte) ([]githubRelease, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	// GitHub adds fields over time, so decode through raw objects and strictly
	// decode only our projection rather than treating unrelated additions as a
	// trust failure.
	var raw []json.RawMessage
	decoder = json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&raw); err != nil {
		return nil, err
	}
	if err := requireEOF(decoder); err != nil {
		return nil, err
	}
	result := make([]githubRelease, 0, len(raw))
	for _, item := range raw {
		var value githubRelease
		if err := json.Unmarshal(item, &value); err != nil || value.ID <= 0 || value.TagName == "" || value.Draft == nil || value.Prerelease == nil {
			return nil, errors.New("runtime GitHub release list is incomplete")
		}
		result = append(result, value)
	}
	return result, nil
}

func decodeExactRelease(data []byte) (githubRelease, error) {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return githubRelease{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var value githubRelease
	if err := decoder.Decode(&value); err != nil {
		return githubRelease{}, err
	}
	if err := requireEOF(decoder); err != nil {
		return githubRelease{}, err
	}
	return value, nil
}

func boundedETag(value string) string {
	if value == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n") {
		return ""
	}
	return value
}
