package release

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type memoryCheckCache struct {
	mu      sync.Mutex
	entries map[string]CheckCacheEntry
}

func (c *memoryCheckCache) Load(_ context.Context, key string) (CheckCacheEntry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[key]
	return entry, ok, nil
}

func (c *memoryCheckCache) Save(_ context.Context, entry CheckCacheEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[entry.RequestURL] = entry
	return nil
}

type checkFixture struct {
	tag            string
	revision       int
	manifest       []byte
	signature      []byte
	publicKey      []byte
	assets         []githubAsset
	release        githubRelease
	listStatus     int
	listBody       []byte
	exactBody      []byte
	assetBodies    map[int64][]byte
	etag           string
	return304      bool
	requests       []string
	requestHeaders []http.Header
	minimumUpdater string
	minimumSystem  string
	trustKeyID     string
	server         *httptest.Server
	cache          *memoryCheckCache
	checkerVersion string
	checkerSystem  string
	now            time.Time
	sleepDurations []time.Duration
}

func newCheckFixture(t *testing.T, tag string, revision int) *checkFixture {
	t.Helper()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKIXPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	key := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	keyID := digestHex(der)
	appDigest := strings.Repeat("a", 64)
	manifest := Manifest{
		Version:               tag,
		CompatibilityRevision: revision,
		Artifacts: []Artifact{
			{OS: "darwin", Arch: "arm64", Component: ComponentMacOSMenuBarBundle, File: "OpenCodexRelay.app.zip", URL: canonicalAssetURL(ProductionRepository, tag, "OpenCodexRelay.app.zip"), SHA256: appDigest, BundleID: "io.github.novelkr.opencodex-relay", SigningMode: SigningModeAdHoc},
			{OS: "linux", Arch: "amd64", Component: ComponentRelay, File: "opencodex-relay_linux_amd64", URL: "https://example.invalid/opencodex-relay_linux_amd64", SHA256: strings.Repeat("b", 64)},
			{OS: "linux", Arch: "amd64", Component: ComponentRelayctl, File: "opencodex-relayctl_linux_amd64", URL: "https://example.invalid/opencodex-relayctl_linux_amd64", SHA256: strings.Repeat("c", 64)},
			{OS: "linux", Arch: "arm64", Component: ComponentRelay, File: "opencodex-relay_linux_arm64", URL: "https://example.invalid/opencodex-relay_linux_arm64", SHA256: strings.Repeat("d", 64)},
			{OS: "linux", Arch: "arm64", Component: ComponentRelayctl, File: "opencodex-relayctl_linux_arm64", URL: "https://example.invalid/opencodex-relayctl_linux_arm64", SHA256: strings.Repeat("e", 64)},
		},
		Documents: []Document{{File: ThirdPartyNoticesFile, URL: "https://example.invalid/THIRD_PARTY_NOTICES.md", SHA256: strings.Repeat("f", 64)}},
	}
	if revision == CompatibilityRevisionUpdater {
		manifest.Channel = ChannelPreview
		if !strings.Contains(tag, "-") {
			manifest.Channel = ChannelStable
		}
		manifest.MinimumUpdaterVersion = "0.3.8-rc.6"
		manifest.TrustKeyID = keyID
		manifest.Artifacts[0].MinimumMacOSVersion = "26.0"
		manifest.Artifacts[0].IntegrationProtocol = 1
		manifest.Artifacts[0].HelperProtocol = 1
	}
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, manifestBytes)))
	manifestID := int64(100)
	signatureID := int64(101)
	appID := int64(102)
	assets := []githubAsset{
		{ID: appID, Name: "OpenCodexRelay.app.zip", State: "uploaded", Digest: "sha256:" + appDigest, Size: 1000},
		{ID: 103, Name: ThirdPartyNoticesFile, State: "uploaded", Digest: "sha256:" + strings.Repeat("f", 64), Size: 1},
		{ID: manifestID, Name: "manifest-" + tag + ".json", State: "uploaded", Digest: "sha256:" + digestHex(manifestBytes), Size: int64(len(manifestBytes))},
		{ID: signatureID, Name: "manifest-" + tag + ".sig", State: "uploaded", Digest: "sha256:" + digestHex(signature), Size: int64(len(signature))},
		{ID: 104, Name: "opencodex-relay_linux_amd64", State: "uploaded", Digest: "sha256:" + strings.Repeat("b", 64), Size: 1},
		{ID: 105, Name: "opencodex-relay_linux_arm64", State: "uploaded", Digest: "sha256:" + strings.Repeat("d", 64), Size: 1},
		{ID: 106, Name: "opencodex-relayctl_linux_amd64", State: "uploaded", Digest: "sha256:" + strings.Repeat("c", 64), Size: 1},
		{ID: 107, Name: "opencodex-relayctl_linux_arm64", State: "uploaded", Digest: "sha256:" + strings.Repeat("e", 64), Size: 1},
	}
	no := false
	yes := true
	release := githubRelease{ID: 42, TagName: tag, Draft: &no, Prerelease: boolPointer(strings.Contains(tag, "-")), Immutable: &yes, Assets: assets}
	listBody, _ := json.Marshal([]githubRelease{release})
	exactBody, _ := json.Marshal(release)
	return &checkFixture{
		tag:            tag,
		revision:       revision,
		manifest:       manifestBytes,
		signature:      signature,
		publicKey:      key,
		assets:         assets,
		release:        release,
		listStatus:     http.StatusOK,
		listBody:       listBody,
		exactBody:      exactBody,
		assetBodies:    map[int64][]byte{manifestID: manifestBytes, signatureID: signature},
		etag:           `"release-list-v1"`,
		minimumUpdater: manifest.MinimumUpdaterVersion,
		minimumSystem:  manifest.Artifacts[0].MinimumMacOSVersion,
		trustKeyID:     keyID,
		cache:          &memoryCheckCache{entries: map[string]CheckCacheEntry{}},
		checkerVersion: "0.3.8-rc.6",
		checkerSystem:  "26.0",
		now:            time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
}

func (f *checkFixture) start(t *testing.T) {
	t.Helper()
	f.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		f.requests = append(f.requests, request.URL.RequestURI())
		f.requestHeaders = append(f.requestHeaders, request.Header.Clone())
		switch {
		case strings.Contains(request.URL.Path, "/releases/assets/"):
			var id int64
			if _, err := fmtSscanfPath(request.URL.Path, &id); err != nil {
				http.Error(writer, "bad asset", http.StatusNotFound)
				return
			}
			body, ok := f.assetBodies[id]
			if !ok {
				http.Error(writer, "missing asset", http.StatusNotFound)
				return
			}
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(body)
		case request.URL.Path == "/repos/"+ProductionRepository+"/releases/42":
			writer.Header().Set("Content-Type", "application/json")
			writer.WriteHeader(http.StatusOK)
			_, _ = writer.Write(f.exactBody)
		case request.URL.Path == "/repos/"+ProductionRepository+"/releases":
			if f.return304 && request.Header.Get("If-None-Match") == f.etag {
				writer.WriteHeader(http.StatusNotModified)
				return
			}
			writer.Header().Set("Content-Type", "application/json")
			writer.Header().Set("ETag", f.etag)
			writer.WriteHeader(f.listStatus)
			_, _ = writer.Write(f.listBody)
		default:
			http.Error(writer, "not found", http.StatusNotFound)
		}
	}))
	t.Cleanup(f.server.Close)
}

func (f *checkFixture) checker(t *testing.T) *Checker {
	t.Helper()
	checker, err := NewChecker(CheckerConfig{
		HTTPClient:     f.server.Client(),
		Cache:          f.cache,
		APIBaseURL:     f.server.URL,
		Repository:     ProductionRepository,
		UpdaterVersion: f.checkerVersion,
		SystemVersion:  f.checkerSystem,
		Now:            func() time.Time { return f.now },
		Sleep: func(_ context.Context, duration time.Duration) error {
			f.sleepDurations = append(f.sleepDurations, duration)
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return checker
}

func (f *checkFixture) check(t *testing.T, current string, channel UpdateChannel) CheckResult {
	t.Helper()
	result, err := f.checker(t).Check(context.Background(), CheckRequest{Channel: channel, CurrentVersion: current, PublicKeyPEM: f.publicKey})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestCheckVerifiesRevisionFourUpdate(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
	fixture.start(t)
	result := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
	if result.Status != CheckStatusUpdateAvailable || result.ReleaseID != 42 || result.Tag != fixture.tag ||
		result.ReleaseURL != canonicalReleaseURL(ProductionRepository, fixture.tag) || result.AppAssetID != 102 ||
		result.ManifestSHA256 != digestHex(fixture.manifest) || result.AppSHA256 != strings.Repeat("a", 64) ||
		result.MinimumUpdaterVersion != "" || result.ETagCacheState != ETagCacheRefreshed {
		t.Fatalf("result = %#v", result)
	}
	if len(fixture.requests) != 4 {
		t.Fatalf("requests = %#v", fixture.requests)
	}
}

func TestCheckChannelAndDowngradeStatuses(t *testing.T) {
	t.Run("stable hides prerelease", func(t *testing.T) {
		fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
		fixture.start(t)
		result := fixture.check(t, "0.3.7", UpdateChannelStable)
		if result.Status != CheckStatusCurrent || result.ReleaseID != 0 || len(fixture.requests) != 1 {
			t.Fatalf("result=%#v requests=%#v", result, fixture.requests)
		}
	})
	t.Run("never downgrades", func(t *testing.T) {
		fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
		fixture.start(t)
		result := fixture.check(t, "0.3.8", UpdateChannelPreview)
		if result.Status != CheckStatusNewerThanSelectedChannel {
			t.Fatalf("result = %#v", result)
		}
	})
	t.Run("never downgrades because an older candidate is incompatible", func(t *testing.T) {
		fixture := newCheckFixture(t, "0.3.8-rc.7", CompatibilityRevisionUpdater)
		fixture.checkerVersion = "0.3.8-rc.5"
		fixture.checkerSystem = "25.0"
		fixture.start(t)
		result := fixture.check(t, "0.3.8", UpdateChannelPreview)
		if result.Status != CheckStatusNewerThanSelectedChannel {
			t.Fatalf("result = %#v", result)
		}
	})
}

func TestCheckUsesETagCacheOnNotModified(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
	fixture.start(t)
	first := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
	fixture.return304 = true
	second := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
	if first.ETagCacheState != ETagCacheRefreshed || second.ETagCacheState != ETagCacheNotModified ||
		fixture.requestHeaders[4].Get("If-None-Match") != fixture.etag {
		t.Fatalf("first=%#v second=%#v headers=%#v", first, second, fixture.requestHeaders)
	}
}

func TestCheckRevisionFiveCompatibilityStatuses(t *testing.T) {
	tests := []struct {
		name    string
		updater string
		system  string
		status  CheckStatus
	}{
		{name: "available", updater: "0.3.8-rc.6", system: "26.0", status: CheckStatusUpdateAvailable},
		{name: "updater too old", updater: "0.3.8-rc.5", system: "26.0", status: CheckStatusUpdaterTooOld},
		{name: "unsupported system", updater: "0.3.8-rc.6", system: "25.9", status: CheckStatusUnsupportedSystem},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckFixture(t, "0.3.8-rc.7", CompatibilityRevisionUpdater)
			fixture.checkerVersion = test.updater
			fixture.checkerSystem = test.system
			fixture.start(t)
			result := fixture.check(t, "0.3.8-rc.6", UpdateChannelPreview)
			if result.Status != test.status || result.MinimumUpdaterVersion != "0.3.8-rc.6" ||
				result.MinimumMacOSVersion != "26.0" || result.TrustKeyID != fixture.trustKeyID {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCheckReturnsExpectedRemoteStatusesAsData(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
		headers    http.Header
		want       CheckStatus
	}{
		{name: "rate limit 429", statusCode: http.StatusTooManyRequests, headers: http.Header{"Retry-After": []string{"3"}}, want: CheckStatusRateLimited},
		{name: "rate limit 403", statusCode: http.StatusForbidden, headers: http.Header{"X-RateLimit-Remaining": []string{"0"}, "X-RateLimit-Reset": []string{"100"}}, want: CheckStatusRateLimited},
		{name: "invalid response", statusCode: http.StatusNotFound, want: CheckStatusInvalidRelease},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
			fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				for key, values := range test.headers {
					writer.Header()[key] = values
				}
				writer.WriteHeader(test.statusCode)
			}))
			t.Cleanup(fixture.server.Close)
			result := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
			if result.Status != test.want {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCheckRejectsUntrustedReleaseMetadataAndPayloads(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*checkFixture)
	}{
		{name: "mutable release", mutate: func(f *checkFixture) { no := false; f.release.Immutable = &no; f.refreshExact() }},
		{name: "prerelease mismatch", mutate: func(f *checkFixture) { no := false; f.release.Prerelease = &no; f.refreshExact() }},
		{name: "missing asset", mutate: func(f *checkFixture) { f.release.Assets = f.release.Assets[:7]; f.refreshExact() }},
		{name: "duplicate asset", mutate: func(f *checkFixture) { f.release.Assets[7] = f.release.Assets[6]; f.refreshExact() }},
		{name: "manifest digest mismatch", mutate: func(f *checkFixture) {
			f.release.Assets[2].Digest = "sha256:" + strings.Repeat("0", 64)
			f.refreshExact()
		}},
		{name: "signature mismatch", mutate: func(f *checkFixture) {
			f.assetBodies[101] = []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
			test.mutate(fixture)
			fixture.start(t)
			result := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
			if result.Status != CheckStatusInvalidRelease || result.ReleaseID != 0 || result.ManifestSHA256 != "" {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCheckRejectsOversizedAndTruncatedJSON(t *testing.T) {
	for name, body := range map[string][]byte{
		"oversized": append([]byte("["), make([]byte, maximumGitHubResponseBytes+1)...),
		"truncated": []byte(`[{"id":42`),
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
			fixture.listBody = body
			fixture.start(t)
			result := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
			if result.Status != CheckStatusInvalidRelease {
				t.Fatalf("result = %#v", result)
			}
		})
	}
}

func TestCheckRetriesServerFailureWithBoundedBackoff(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
	attempts := 0
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts < 3 {
			writer.Header().Set("Retry-After", "2")
			writer.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`[]`))
	}))
	t.Cleanup(fixture.server.Close)
	result := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
	if result.Status != CheckStatusCurrent || attempts != 3 || len(fixture.sleepDurations) != 2 ||
		fixture.sleepDurations[0] != 2*time.Second || fixture.sleepDurations[1] != 2*time.Second {
		t.Fatalf("result=%#v attempts=%d delays=%v", result, attempts, fixture.sleepDurations)
	}
}

func TestCheckReportsExhaustedServerFailureAsOffline(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.6", CompatibilityRevisionAdHocApp)
	attempts := 0
	fixture.server = httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts++
		writer.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(fixture.server.Close)
	result := fixture.check(t, "0.3.8-rc.5", UpdateChannelPreview)
	if result.Status != CheckStatusOffline || attempts != maximumRequestAttempts {
		t.Fatalf("result=%#v attempts=%d", result, attempts)
	}
}

func TestRateLimitHeadersDriveOnlyBoundedRetries(t *testing.T) {
	now := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	retrySeconds := make(http.Header)
	retrySeconds.Set("Retry-After", "3")
	retryDate := make(http.Header)
	retryDate.Set("Retry-After", now.Add(4*time.Second).Format(http.TimeFormat))
	rateReset := make(http.Header)
	rateReset.Set("X-RateLimit-Remaining", "0")
	rateReset.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(5*time.Second).Unix(), 10))
	for name, test := range map[string]struct {
		header http.Header
		want   time.Duration
	}{
		"retry after seconds": {header: retrySeconds, want: 3 * time.Second},
		"retry after date":    {header: retryDate, want: 4 * time.Second},
		"rate reset":          {header: rateReset, want: 5 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			statusCode := http.StatusTooManyRequests
			if test.header.Get("X-RateLimit-Remaining") == "0" {
				statusCode = http.StatusForbidden
			}
			delay, limited, retryable := rateLimitDelay(&http.Response{StatusCode: statusCode, Header: test.header}, now)
			if !limited || !retryable || delay != test.want {
				t.Fatalf("delay=%v limited=%v retryable=%v", delay, limited, retryable)
			}
		})
	}
	longDelay, limited, retryable := rateLimitDelay(
		&http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"120"}}},
		now,
	)
	if !limited || !retryable || longDelay != 120*time.Second {
		t.Fatalf("long delay=%v limited=%v retryable=%v", longDelay, limited, retryable)
	}
}

func TestProductionRedirectAllowlistRejectsCredentialAndForeignHosts(t *testing.T) {
	for _, raw := range []string{
		"http://api.github.com/repos/example/releases",
		"https://user@example.com/file",
		"https://example.com/file",
	} {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if allowedProductionRedirect(target) {
			t.Fatalf("unsafe redirect accepted: %s", raw)
		}
	}
	for _, raw := range []string{
		"https://api.github.com/repos/novelKR/OpenCodex-OCI-Gateway/releases",
		"https://release-assets.githubusercontent.com/file?token=temporary",
	} {
		target, err := url.Parse(raw)
		if err != nil {
			t.Fatal(err)
		}
		if !allowedProductionRedirect(target) {
			t.Fatalf("production redirect rejected: %s", raw)
		}
	}
}

func TestSharedCheckResultFixture(t *testing.T) {
	path := filepath.Join("..", "..", "testdata", "release-update", "check-update-available-v1.json")
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var result CheckResult
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != CheckSchemaVersion || result.Status != CheckStatusUpdateAvailable ||
		result.Channel != UpdateChannelPreview || result.ReleaseID != 42 || result.Tag != "0.3.8-rc.6" {
		t.Fatalf("fixture = %#v", result)
	}
}

func (f *checkFixture) refreshExact() {
	f.exactBody, _ = json.Marshal(f.release)
}

func boolPointer(value bool) *bool { return &value }

func digestHex(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

func fmtSscanfPath(path string, id *int64) (int, error) {
	const marker = "/releases/assets/"
	index := strings.LastIndex(path, marker)
	if index < 0 {
		return 0, io.EOF
	}
	return fmt.Sscanf(path[index+len(marker):], "%d", id)
}
