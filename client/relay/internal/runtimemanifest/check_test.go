package runtimemanifest

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

type runtimeMemoryCache struct {
	mu      sync.Mutex
	entries map[string]CheckCacheEntry
}

func (c *runtimeMemoryCache) Load(_ context.Context, key string) (CheckCacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok, nil
}

func (c *runtimeMemoryCache) Save(_ context.Context, entry CheckCacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[entry.RequestURL] = entry
	return nil
}

type runtimeResponse struct {
	status int
	body   []byte
	header http.Header
}

type runtimeHTTPFixture struct {
	mu        sync.Mutex
	responses map[string]runtimeResponse
	requests  []*http.Request
}

func (fixture *runtimeHTTPFixture) Do(request *http.Request) (*http.Response, error) {
	fixture.mu.Lock()
	fixture.requests = append(fixture.requests, request.Clone(request.Context()))
	response, ok := fixture.responses[request.URL.String()]
	fixture.mu.Unlock()
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Header:     make(http.Header),
			Body:       io.NopCloser(bytes.NewReader(nil)),
			Request:    request,
		}, nil
	}
	header := response.header.Clone()
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode:    response.status,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(response.body)),
		ContentLength: int64(len(response.body)),
		Request:       request,
	}, nil
}

type runtimeCheckFixture struct {
	manifestFixture manifestFixture
	manifest        []byte
	signature       []byte
	release         githubRelease
	http            *runtimeHTTPFixture
	checker         *Checker
}

func newRuntimeCheckFixture(t *testing.T) *runtimeCheckFixture {
	t.Helper()
	signed := newManifestFixture(t)
	manifest, signature := signed.signed(t)
	artifactVersion := signed.manifest.ArtifactVersion
	manifestName := "opencodex-runtime-" + artifactVersion + ".json"
	signatureName := "opencodex-runtime-" + artifactVersion + ".sig"
	no, yes := false, true
	release := githubRelease{
		ID:         42,
		TagName:    RuntimeReleasePrefix + artifactVersion,
		Draft:      &no,
		Prerelease: &no,
		Immutable:  &yes,
		HTMLURL:    "https://github.com/" + ProductionSourceRepo + "/releases/tag/" + RuntimeReleasePrefix + artifactVersion,
		Assets: []githubAsset{
			{ID: 100, Name: manifestName, State: "uploaded", Digest: "sha256:" + ManifestSHA256(manifest), Size: int64(len(manifest))},
			{ID: 101, Name: signatureName, State: "uploaded", Digest: "sha256:" + ManifestSHA256(signature), Size: int64(len(signature))},
		},
	}
	httpFixture := &runtimeHTTPFixture{responses: map[string]runtimeResponse{}}
	cache := &runtimeMemoryCache{entries: map[string]CheckCacheEntry{}}
	checker, err := NewChecker(CheckerConfig{
		HTTPClient: httpFixture,
		Cache:      cache,
		APIBaseURL: "https://api.test",
		Repository: ProductionSourceRepo,
		Now:        func() time.Time { return time.Date(2026, 9, 2, 12, 0, 0, 0, time.UTC) },
		Sleep:      func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture := &runtimeCheckFixture{
		manifestFixture: signed,
		manifest:        manifest,
		signature:       signature,
		release:         release,
		http:            httpFixture,
		checker:         checker,
	}
	fixture.refreshResponses(t)
	return fixture
}

func (fixture *runtimeCheckFixture) refreshResponses(t *testing.T) {
	t.Helper()
	list, err := json.Marshal([]githubRelease{fixture.release})
	if err != nil {
		t.Fatal(err)
	}
	exact, err := json.Marshal(fixture.release)
	if err != nil {
		t.Fatal(err)
	}
	fixture.http.responses = map[string]runtimeResponse{
		"https://api.test/repos/" + ProductionSourceRepo + "/releases?per_page=100&page=1": {
			status: http.StatusOK,
			body:   list,
			header: http.Header{"Etag": []string{`"runtime-v1"`}},
		},
		"https://api.test/repos/" + ProductionSourceRepo + "/releases/42": {
			status: http.StatusOK,
			body:   exact,
		},
		"https://api.test/repos/" + ProductionSourceRepo + "/releases/assets/100": {
			status: http.StatusOK,
			body:   fixture.manifest,
		},
		"https://api.test/repos/" + ProductionSourceRepo + "/releases/assets/101": {
			status: http.StatusOK,
			body:   fixture.signature,
		},
	}
}

func (fixture *runtimeCheckFixture) resign(t *testing.T) {
	t.Helper()
	body, err := json.Marshal(fixture.manifestFixture.manifest)
	if err != nil {
		t.Fatal(err)
	}
	fixture.manifest = body
	fixture.signature = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(fixture.manifestFixture.privateKey, body)))
	fixture.release.Assets[0].Digest = "sha256:" + ManifestSHA256(body)
	fixture.release.Assets[0].Size = int64(len(body))
	fixture.release.Assets[1].Digest = "sha256:" + ManifestSHA256(fixture.signature)
	fixture.release.Assets[1].Size = int64(len(fixture.signature))
	fixture.refreshResponses(t)
}

func (fixture *runtimeCheckFixture) check(t *testing.T, current string, options VerifyOptions) CheckResult {
	t.Helper()
	result, err := fixture.checker.Check(context.Background(), CheckRequest{
		CurrentArtifactVersion: current,
		VerifyOptions:          options,
		PublicKeyPEM:           fixture.manifestFixture.publicKey,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestRuntimeCheckRereadsExactImmutableReleaseAndAssets(t *testing.T) {
	fixture := newRuntimeCheckFixture(t)
	result := fixture.check(t, "2.39.0-r1", VerifyOptions{
		HighestSeenSequence:   6,
		RelayVersion:          "0.3.9",
		MacOSVersion:          "26.5.1",
		AppleContainerVersion: "1.3.1",
	})
	if result.Status != CheckStatusUpdateAvailable || result.Candidate == nil ||
		result.Candidate.ReleaseID != 42 || result.Candidate.ManifestSHA256 != ManifestSHA256(fixture.manifest) ||
		result.ETagCache != ETagCacheRefreshed {
		t.Fatalf("result = %#v", result)
	}
	if len(fixture.http.requests) != 4 {
		t.Fatalf("requests = %d", len(fixture.http.requests))
	}
	if got := fixture.http.requests[2].Header.Get("Accept"); got != "application/octet-stream" {
		t.Fatalf("asset Accept = %q", got)
	}
}

func TestRuntimeCheckWithoutSignedStableManifestIsUnavailableAndCannotResolveDigest(t *testing.T) {
	fixture := newRuntimeCheckFixture(t)
	listURL := "https://api.test/repos/" + ProductionSourceRepo + "/releases?per_page=100&page=1"
	fixture.http.responses[listURL] = runtimeResponse{status: http.StatusOK, body: []byte("[]")}

	result := fixture.check(t, "", VerifyOptions{})
	if result.Status != CheckStatusUnavailable || result.Reason != "stable_runtime_manifest_unavailable" || result.Candidate != nil {
		t.Fatalf("result = %#v", result)
	}
	if _, err := fixture.checker.ResolveExpected(context.Background(), strings.Repeat("a", 64), CheckRequest{
		PublicKeyPEM: fixture.manifestFixture.publicKey,
	}); err == nil || err.Error() != "runtime release candidate changed" {
		t.Fatalf("manual digest resolution error = %v", err)
	}
}

func TestRuntimeCheckRetainsAuthenticatedCandidateWhenLocallyIncompatible(t *testing.T) {
	fixture := newRuntimeCheckFixture(t)
	result := fixture.check(t, "2.39.0-r1", VerifyOptions{
		RelayVersion:          "0.3.8",
		MacOSVersion:          "25.9",
		AppleContainerVersion: "1.2.0",
	})
	if result.Status != CheckStatusIncompatible || result.Candidate == nil ||
		result.Candidate.Manifest.ArtifactVersion != "2.40.0-r1" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRuntimeCheckRejectsReleaseMutationAndRollback(t *testing.T) {
	for name, mutate := range map[string]func(*runtimeCheckFixture){
		"mutable release": func(f *runtimeCheckFixture) {
			no := false
			f.release.Immutable = &no
		},
		"extra asset": func(f *runtimeCheckFixture) {
			f.release.Assets = append(f.release.Assets, githubAsset{ID: 102, Name: "unexpected", State: "uploaded", Digest: "sha256:" + strings.Repeat("a", 64), Size: 1})
		},
		"asset digest": func(f *runtimeCheckFixture) {
			f.release.Assets[0].Digest = "sha256:" + strings.Repeat("0", 64)
		},
		"retagged list identity": func(f *runtimeCheckFixture) {
			f.release.ID = 43
		},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newRuntimeCheckFixture(t)
			mutate(fixture)
			fixture.refreshResponses(t)
			result := fixture.check(t, "2.39.0-r1", VerifyOptions{})
			if result.Status != CheckStatusInvalidRelease || result.Candidate != nil {
				t.Fatalf("result = %#v", result)
			}
		})
	}
	fixture := newRuntimeCheckFixture(t)
	result := fixture.check(t, "2.41.0-r1", VerifyOptions{})
	if result.Status != CheckStatusInvalidRelease || result.Reason != "runtime_release_not_monotonic" {
		t.Fatalf("rollback result = %#v", result)
	}
}

func TestRuntimeCheckRejectsAmbiguousDuplicateArtifactRelease(t *testing.T) {
	fixture := newRuntimeCheckFixture(t)
	duplicate := fixture.release
	duplicate.ID = 43
	list, err := json.Marshal([]githubRelease{fixture.release, duplicate})
	if err != nil {
		t.Fatal(err)
	}
	listURL := "https://api.test/repos/" + ProductionSourceRepo + "/releases?per_page=100&page=1"
	fixture.http.responses[listURL] = runtimeResponse{status: http.StatusOK, body: list}
	result := fixture.check(t, "2.39.0-r1", VerifyOptions{})
	if result.Status != CheckStatusInvalidRelease || result.Reason != "ambiguous_runtime_release" {
		t.Fatalf("result = %#v", result)
	}
}

func TestRuntimeCheckRequiresExactlyTwoStableAssetNames(t *testing.T) {
	fixture := newRuntimeCheckFixture(t)
	fixture.release.Assets[0].Name = "manifest.json"
	fixture.refreshResponses(t)
	result := fixture.check(t, "2.39.0-r1", VerifyOptions{})
	if result.Status != CheckStatusInvalidRelease || result.Candidate != nil {
		t.Fatalf("result = %#v", result)
	}
}
