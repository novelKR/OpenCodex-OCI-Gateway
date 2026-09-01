package release

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fixtureBundleValidator struct {
	calls int
}

func (v *fixtureBundleValidator) Validate(
	_ context.Context,
	appPath string,
	tag string,
	artifact Artifact,
	currentBuildNumber int,
	_ []byte,
	trustKeyID string,
) (BundleValidation, error) {
	v.calls++
	if filepath.Base(appPath) != stageApplicationDirectory || tag != "0.3.8-rc.7" ||
		artifact.BundleID != productionApplicationBundle || currentBuildNumber != 1000 || !isLowerHexSHA256(trustKeyID) {
		return BundleValidation{}, ErrStageInvalidBundle
	}
	fingerprint, err := BundleFingerprint(appPath)
	return BundleValidation{Fingerprint: fingerprint, BuildNumber: 1001}, err
}

func TestStageReverifiesDownloadsAndReusesCompleteCandidate(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.7", CompatibilityRevisionUpdater)
	archive := validStageArchive(t)
	refreshStageFixture(t, fixture, archive)
	fixture.start(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "Updates")
	validator := &fixtureBundleValidator{}
	stager, err := NewStager(StagerConfig{
		Checker:            fixture.checker(t),
		RootDirectory:      root,
		CurrentBuildNumber: 1000,
		BundleValidator:    validator,
		Now:                func() time.Time { return fixture.now },
	})
	if err != nil {
		t.Fatal(err)
	}
	request := StageRequest{
		Channel:                UpdateChannelPreview,
		CurrentVersion:         "0.3.8-rc.6",
		ReleaseID:              42,
		Tag:                    fixture.tag,
		ExpectedManifestSHA256: digestHex(fixture.manifest),
		PublicKeyPEM:           fixture.publicKey,
	}
	first, err := stager.Stage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if first.SchemaVersion != StageSchemaVersion || first.AppSHA256 != digestHex(archive) ||
		first.ManifestSHA256 != request.ExpectedManifestSHA256 || first.TrustKeyID != fixture.trustKeyID ||
		first.StagingPath != filepath.Join(root, "42-"+request.ExpectedManifestSHA256, stageApplicationDirectory) ||
		!isLowerHexSHA256(first.BundleFingerprint) || validator.calls != 1 {
		t.Fatalf("first receipt=%#v validator_calls=%d", first, validator.calls)
	}
	metadata, err := os.Lstat(root)
	if err != nil || !metadata.IsDir() || metadata.Mode().Perm()&0o077 != 0 {
		t.Fatalf("staging root mode=%v err=%v", metadata, err)
	}
	second, err := stager.Stage(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if second != first || validator.calls != 2 {
		t.Fatalf("reused receipt=%#v calls=%d", second, validator.calls)
	}
	appDownloads := 0
	for _, requestPath := range fixture.requests {
		if strings.HasSuffix(requestPath, "/releases/assets/102") {
			appDownloads++
		}
	}
	if appDownloads != 1 {
		t.Fatalf("app downloads = %d, requests=%#v", appDownloads, fixture.requests)
	}
}

func TestStageRejectsStaleOrUntrustedSelectionBeforeAppDownload(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.7", CompatibilityRevisionUpdater)
	archive := validStageArchive(t)
	refreshStageFixture(t, fixture, archive)
	fixture.start(t)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	stager, err := NewStager(StagerConfig{
		Checker:            fixture.checker(t),
		RootDirectory:      filepath.Join(parent, "Updates"),
		CurrentBuildNumber: 1000,
		BundleValidator:    &fixtureBundleValidator{},
		Now:                time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	request := StageRequest{
		Channel:                UpdateChannelPreview,
		CurrentVersion:         "0.3.8-rc.6",
		ReleaseID:              42,
		Tag:                    fixture.tag,
		ExpectedManifestSHA256: strings.Repeat("0", 64),
		PublicKeyPEM:           fixture.publicKey,
	}
	if _, err := stager.Stage(context.Background(), request); !errors.Is(err, ErrStageInvalidRelease) {
		t.Fatalf("error = %v", err)
	}
	for _, requestPath := range fixture.requests {
		if strings.HasSuffix(requestPath, "/releases/assets/102") {
			t.Fatalf("app downloaded for stale selection: %#v", fixture.requests)
		}
	}
}

func TestStageLockRejectsConcurrentWriter(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.7", CompatibilityRevisionUpdater)
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	stager, err := NewStager(StagerConfig{
		Checker:            fixture.checkerWithoutServer(t),
		RootDirectory:      filepath.Join(parent, "Updates"),
		CurrentBuildNumber: 1000,
		BundleValidator:    &fixtureBundleValidator{},
		Now:                time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	lock, err := stager.acquireLock()
	if err != nil {
		t.Fatal(err)
	}
	defer releaseStageLock(lock)
	if _, err := stager.acquireLock(); !errors.Is(err, ErrStageBusy) {
		t.Fatalf("second lock error = %v", err)
	}
}

func TestProductionStagerUsesDownloadSizedRequestTimeout(t *testing.T) {
	stager, err := NewProductionStager("0.3.8-rc.7", 1001)
	if err != nil {
		t.Fatal(err)
	}
	client, ok := stager.checker.httpClient.(*http.Client)
	if !ok || client.Timeout != maximumStageRequestDuration {
		t.Fatalf("production stage HTTP client = %#v", stager.checker.httpClient)
	}
}

func TestStageRejectsRelocationRecoveryBeforeNetwork(t *testing.T) {
	fixture := newCheckFixture(t, "0.3.8-rc.7", CompatibilityRevisionUpdater)
	support := t.TempDir()
	if err := os.Chmod(support, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(support, "application-relocation.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	stager, err := NewStager(StagerConfig{
		Checker:            fixture.checkerWithoutServer(t),
		RootDirectory:      filepath.Join(support, "Updates"),
		CurrentBuildNumber: 1000,
		BundleValidator:    &fixtureBundleValidator{},
		Now:                time.Now,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = stager.Stage(context.Background(), StageRequest{
		Channel:                UpdateChannelPreview,
		CurrentVersion:         "0.3.8-rc.6",
		ReleaseID:              42,
		Tag:                    fixture.tag,
		ExpectedManifestSHA256: digestHex(fixture.manifest),
		PublicKeyPEM:           fixture.publicKey,
	})
	if !errors.Is(err, ErrStageUnsafeFilesystem) {
		t.Fatalf("error = %v", err)
	}
}

func TestStageLockRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "stage.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := acquireStageFileLock(link); !errors.Is(err, ErrStageUnsafeFilesystem) {
		t.Fatalf("error = %v", err)
	}
}

func TestExtractReleaseArchiveRejectsUnsafeEntries(t *testing.T) {
	tests := map[string][]stageArchiveEntry{
		"absolute":        {{name: "/OpenCodexRelay.app/Contents/Info.plist", mode: 0o600, body: "x"}},
		"parent":          {{name: "OpenCodexRelay.app/../escape", mode: 0o600, body: "x"}},
		"empty component": {{name: "OpenCodexRelay.app//escape", mode: 0o600, body: "x"}},
		"multiple roots":  {{name: "OpenCodexRelay.app/", mode: os.ModeDir | 0o700}, {name: "Other.app/file", mode: 0o600, body: "x"}},
		"symlink":         {{name: "OpenCodexRelay.app/", mode: os.ModeDir | 0o700}, {name: "OpenCodexRelay.app/link", mode: os.ModeSymlink | 0o777, body: "target"}},
		"case collision":  {{name: "OpenCodexRelay.app/", mode: os.ModeDir | 0o700}, {name: "OpenCodexRelay.app/File", mode: 0o600, body: "a"}, {name: "OpenCodexRelay.app/file", mode: 0o600, body: "b"}},
		"exact duplicate": {{name: "OpenCodexRelay.app/", mode: os.ModeDir | 0o700}, {name: "OpenCodexRelay.app/file", mode: 0o600, body: "a"}, {name: "OpenCodexRelay.app/file", mode: 0o600, body: "b"}},
		"file as parent":  {{name: "OpenCodexRelay.app/", mode: os.ModeDir | 0o700}, {name: "OpenCodexRelay.app/file", mode: 0o600, body: "a"}, {name: "OpenCodexRelay.app/file/child", mode: 0o600, body: "b"}},
		"non ASCII path":  {{name: "OpenCodexRelay.app/Contents/한글", mode: 0o600, body: "x"}},
	}
	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			archive := archiveWithEntries(t, entries)
			destination := t.TempDir()
			archivePath := filepath.Join(destination, "candidate.zip")
			if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := extractReleaseArchive(archivePath, destination); !errors.Is(err, ErrStageInvalidArchive) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestExtractReleaseArchiveRequiresAbsoluteBoundedDestination(t *testing.T) {
	archive := validStageArchive(t)
	archivePath := filepath.Join(t.TempDir(), "candidate.zip")
	if err := os.WriteFile(archivePath, archive, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, destination := range []string{"relative", string(filepath.Separator)} {
		if err := extractReleaseArchive(archivePath, destination); !errors.Is(err, ErrStageInvalidArchive) {
			t.Fatalf("destination %q error = %v", destination, err)
		}
	}
}

func TestValidateArchiveEntryRejectsExcessiveCompressionRatioAndPathLength(t *testing.T) {
	for name, entry := range map[string]*zip.File{
		"compression ratio": {
			FileHeader: zip.FileHeader{
				Name:               "OpenCodexRelay.app/Contents/payload",
				CompressedSize64:   1,
				UncompressedSize64: maximumCompressionRatio + 1,
			},
		},
		"path length": {
			FileHeader: zip.FileHeader{
				Name: strings.Repeat("a", maximumArchivePathBytes+1),
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			entry.SetMode(0o600)
			if _, _, err := validateArchiveEntry(entry); !errors.Is(err, ErrStageInvalidArchive) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestReadStageReceiptRejectsUnknownDuplicateAndTamperedFields(t *testing.T) {
	directory := t.TempDir()
	base := `{"schema_version":1,"release_id":42,"tag":"0.3.8-rc.7","channel":"preview","manifest_sha256":"` + strings.Repeat("a", 64) + `","app_sha256":"` + strings.Repeat("b", 64) + `","bundle_fingerprint":"` + strings.Repeat("c", 64) + `","trust_key_id":"` + strings.Repeat("d", 64) + `","staging_path":"/tmp/OpenCodexRelay.app","verified_at":"2026-09-01T00:00:00Z"}`
	for name, body := range map[string]string{
		"unknown":   strings.Replace(base, `"schema_version":1`, `"schema_version":1,"extra":true`, 1),
		"duplicate": strings.Replace(base, `"release_id":42`, `"release_id":42,"release_id":43`, 1),
		"bad hash":  strings.Replace(base, strings.Repeat("a", 64), "A"+strings.Repeat("a", 63), 1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(directory, strings.ReplaceAll(name, " ", "-")+".json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readStageReceipt(path); !errors.Is(err, ErrStageUnsafeFilesystem) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestSharedStageReceiptFixtureDecodesStrictly(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "testdata", "release-update", "stage-ready-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rejectDuplicateJSONKeys(body); err != nil {
		t.Fatal(err)
	}
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	var receipt StageReceipt
	if err := decoder.Decode(&receipt); err != nil {
		t.Fatal(err)
	}
	if err := requireJSONEOF(decoder); err != nil || ParseStrictStageReceipt(receipt) != nil {
		t.Fatalf("fixture validation error = %v", err)
	}
	if receipt.SchemaVersion != 1 || receipt.ReleaseID != 42 || receipt.Tag != "0.3.8-rc.7" ||
		receipt.Channel != UpdateChannelPreview || !strings.HasSuffix(receipt.StagingPath, "/OpenCodexRelay.app") {
		t.Fatalf("fixture = %#v", receipt)
	}
}

func TestExtractReleaseArchiveDryRunFixture(t *testing.T) {
	archivePath := os.Getenv("OPENCODEX_STAGE_ARCHIVE_FIXTURE")
	if archivePath == "" {
		t.Skip("release-package dry run supplies the signed app archive")
	}
	destination := t.TempDir()
	if err := extractReleaseArchive(archivePath, destination); err != nil {
		t.Fatal(err)
	}
	metadata, err := os.Lstat(filepath.Join(destination, stageApplicationDirectory))
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("extracted app identity error = %v", err)
	}
}

func refreshStageFixture(t *testing.T, fixture *checkFixture, archive []byte) {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(fixture.manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Artifacts[0].SHA256 = digestHex(archive)
	body, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(fixture.privateKey, body)))
	fixture.manifest = body
	fixture.signature = signature
	fixture.assetBodies[100] = body
	fixture.assetBodies[101] = signature
	fixture.assetBodies[102] = archive
	fixture.release.Assets[0].Digest = "sha256:" + digestHex(archive)
	fixture.release.Assets[0].Size = int64(len(archive))
	fixture.release.Assets[2].Digest = "sha256:" + digestHex(body)
	fixture.release.Assets[2].Size = int64(len(body))
	fixture.release.Assets[3].Digest = "sha256:" + digestHex(signature)
	fixture.release.Assets[3].Size = int64(len(signature))
	fixture.refreshExact()
}

func (f *checkFixture) checkerWithoutServer(t *testing.T) *Checker {
	t.Helper()
	checker, err := NewChecker(CheckerConfig{
		HTTPClient:     failingHTTPDoer{},
		Cache:          f.cache,
		APIBaseURL:     "https://example.test",
		Repository:     ProductionRepository,
		UpdaterVersion: f.checkerVersion,
		SystemVersion:  f.checkerSystem,
		Now:            time.Now,
		Sleep:          func(context.Context, time.Duration) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return checker
}

type failingHTTPDoer struct{}

func (failingHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("unused")
}

type stageArchiveEntry struct {
	name string
	mode os.FileMode
	body string
}

func validStageArchive(t *testing.T) []byte {
	t.Helper()
	return archiveWithEntries(t, []stageArchiveEntry{
		{name: "OpenCodexRelay.app/", mode: os.ModeDir | 0o700},
		{name: "OpenCodexRelay.app/Contents/", mode: os.ModeDir | 0o700},
		{name: "OpenCodexRelay.app/Contents/Info.plist", mode: 0o600, body: "fixture"},
	})
}

func archiveWithEntries(t *testing.T, entries []stageArchiveEntry) []byte {
	t.Helper()
	var output strings.Builder
	writer := zip.NewWriter(stringWriter{&output})
	for _, entry := range entries {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		file, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(entry.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return []byte(output.String())
}

type stringWriter struct{ builder *strings.Builder }

func (writer stringWriter) Write(value []byte) (int, error) {
	return writer.builder.WriteString(string(value))
}
