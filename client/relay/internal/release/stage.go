package release

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	StageSchemaVersion          = 1
	maximumAppArchiveBytes      = 128 << 20
	maximumExtractedBytes       = 512 << 20
	maximumArchiveEntries       = 4096
	maximumArchivePathBytes     = 1024
	maximumCompressionRatio     = 200
	maximumStageRequestDuration = 3 * time.Minute
	stageReceiptFile            = "stage-receipt-v1.json"
	stageArchiveFile            = "OpenCodexRelay.app.zip"
	stageApplicationDirectory   = "OpenCodexRelay.app"
	productionApplicationBundle = "io.github.novelkr.opencodex-relay"
)

var (
	ErrStageInvalidRequest   = errors.New("release stage request is invalid")
	ErrStageInvalidRelease   = errors.New("release stage candidate is invalid")
	ErrStageBusy             = errors.New("another release stage operation is active")
	ErrStageUnsafeFilesystem = errors.New("release staging filesystem is unsafe")
	ErrStageInvalidArchive   = errors.New("release app archive is invalid")
	ErrStageInvalidBundle    = errors.New("staged release app bundle is invalid")
)

type StageRequest struct {
	Channel                UpdateChannel
	CurrentVersion         string
	ReleaseID              int64
	Tag                    string
	ExpectedManifestSHA256 string
	PublicKeyPEM           []byte
}

type StageReceipt struct {
	SchemaVersion     int           `json:"schema_version"`
	ReleaseID         int64         `json:"release_id"`
	Tag               string        `json:"tag"`
	Channel           UpdateChannel `json:"channel"`
	ManifestSHA256    string        `json:"manifest_sha256"`
	AppSHA256         string        `json:"app_sha256"`
	BundleFingerprint string        `json:"bundle_fingerprint"`
	TrustKeyID        string        `json:"trust_key_id"`
	StagingPath       string        `json:"staging_path"`
	VerifiedAt        string        `json:"verified_at"`
}

type BundleValidation struct {
	Fingerprint string
	BuildNumber int
}

type AppBundleValidator interface {
	Validate(
		context.Context,
		string,
		string,
		Artifact,
		int,
		[]byte,
		string,
	) (BundleValidation, error)
}

type StagerConfig struct {
	Checker            *Checker
	RootDirectory      string
	CurrentBuildNumber int
	BundleValidator    AppBundleValidator
	Now                func() time.Time
}

type Stager struct {
	checker            *Checker
	rootDirectory      string
	currentBuildNumber int
	bundleValidator    AppBundleValidator
	now                func() time.Time
}

func NewStager(config StagerConfig) (*Stager, error) {
	if config.Checker == nil || config.BundleValidator == nil || config.Now == nil ||
		!filepath.IsAbs(config.RootDirectory) || config.CurrentBuildNumber < 1 || config.CurrentBuildNumber > 9999 {
		return nil, ErrStageInvalidRequest
	}
	return &Stager{
		checker:            config.Checker,
		rootDirectory:      filepath.Clean(config.RootDirectory),
		currentBuildNumber: config.CurrentBuildNumber,
		bundleValidator:    config.BundleValidator,
		now:                config.Now,
	}, nil
}

func NewProductionStager(updaterVersion string, currentBuildNumber int) (*Stager, error) {
	checker, err := newProductionChecker(updaterVersion, maximumStageRequestDuration)
	if err != nil {
		return nil, err
	}
	root, err := os.UserConfigDir()
	if err != nil || !filepath.IsAbs(root) {
		return nil, ErrStageUnsafeFilesystem
	}
	return NewStager(StagerConfig{
		Checker:            checker,
		RootDirectory:      filepath.Join(root, "OpenCodexRelay", "Updates"),
		CurrentBuildNumber: currentBuildNumber,
		BundleValidator:    systemAppBundleValidator{},
		Now:                time.Now,
	})
}

func (s *Stager) Stage(ctx context.Context, request StageRequest) (StageReceipt, error) {
	current, err := ParseSemanticVersion(request.CurrentVersion)
	if err != nil || request.ReleaseID <= 0 || !isLowerHexSHA256(request.ExpectedManifestSHA256) ||
		(request.Channel != UpdateChannelStable && request.Channel != UpdateChannelPreview) {
		return StageReceipt{}, ErrStageInvalidRequest
	}
	target, err := ParseSemanticVersion(request.Tag)
	if err != nil || current.Compare(target) >= 0 ||
		(request.Channel == UpdateChannelStable && target.IsPrerelease()) {
		return StageReceipt{}, ErrStageInvalidRequest
	}
	trustKeyID, err := publicKeyID(request.PublicKeyPEM)
	if err != nil {
		return StageReceipt{}, fmt.Errorf("%w: local release trust key", ErrStageInvalidRequest)
	}
	lock, err := s.acquireLock()
	if err != nil {
		return StageReceipt{}, err
	}
	defer releaseStageLock(lock)
	relocationGuard, err := s.acquireRelocationGuard()
	if err != nil {
		return StageReceipt{}, err
	}
	defer releaseStageLock(relocationGuard)

	verified, exact, appAsset, manifestDigest, status := s.checker.verifyCandidate(
		ctx,
		githubRelease{ID: request.ReleaseID, TagName: request.Tag},
		request.Channel,
		request.PublicKeyPEM,
		trustKeyID,
	)
	if status != "" {
		return StageReceipt{}, fmt.Errorf("%w: %s", ErrStageInvalidRelease, status)
	}
	updaterVersion, updaterErr := ParseSemanticVersion(s.checker.updaterVersion)
	minimumUpdaterVersion, minimumUpdaterErr := ParseSemanticVersion(verified.MinimumUpdaterVersion)
	if exact.ID != request.ReleaseID || exact.TagName != request.Tag ||
		manifestDigest != request.ExpectedManifestSHA256 ||
		verified.CompatibilityRevision != CompatibilityRevisionUpdater ||
		verified.Channel != string(request.Channel) || verified.TrustKeyID != trustKeyID ||
		appAsset.artifact.BundleID != productionApplicationBundle ||
		appAsset.Size > maximumAppArchiveBytes || request.CurrentVersion != s.checker.updaterVersion ||
		updaterErr != nil || minimumUpdaterErr != nil || updaterVersion.Compare(minimumUpdaterVersion) < 0 ||
		!systemVersionAtLeast(s.checker.systemVersion, appAsset.artifact.MinimumMacOSVersion) {
		return StageReceipt{}, ErrStageInvalidRelease
	}

	directoryName := strconv.FormatInt(request.ReleaseID, 10) + "-" + manifestDigest
	finalDirectory := filepath.Join(s.rootDirectory, directoryName)
	finalApp := filepath.Join(finalDirectory, stageApplicationDirectory)
	if _, err := os.Lstat(finalDirectory); err == nil {
		return s.reuseCompletedStage(ctx, request, appAsset.artifact, trustKeyID, finalDirectory, finalApp)
	} else if !os.IsNotExist(err) {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}

	archiveBytes, _, _, status := s.checker.fetch(
		ctx,
		s.checker.assetURL(appAsset.ID),
		maximumAppArchiveBytes,
		"",
	)
	if status != "" || sha256Hex(archiveBytes) != appAsset.manifestDigest {
		return StageReceipt{}, ErrStageInvalidRelease
	}
	temporaryDirectory, err := os.MkdirTemp(s.rootDirectory, "."+directoryName+".partial-")
	if err != nil {
		return StageReceipt{}, err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(temporaryDirectory)
		}
	}()
	if err := os.Chmod(temporaryDirectory, 0o700); err != nil {
		return StageReceipt{}, err
	}
	archivePath := filepath.Join(temporaryDirectory, stageArchiveFile)
	if err := writeOwnerFile(archivePath, archiveBytes, 0o600); err != nil {
		return StageReceipt{}, err
	}
	if err := extractReleaseArchive(archivePath, temporaryDirectory); err != nil {
		return StageReceipt{}, err
	}
	temporaryApp := filepath.Join(temporaryDirectory, stageApplicationDirectory)
	validation, err := s.bundleValidator.Validate(
		ctx,
		temporaryApp,
		request.Tag,
		appAsset.artifact,
		s.currentBuildNumber,
		request.PublicKeyPEM,
		trustKeyID,
	)
	if err != nil {
		return StageReceipt{}, fmt.Errorf("%w: %v", ErrStageInvalidBundle, err)
	}
	receipt := StageReceipt{
		SchemaVersion:     StageSchemaVersion,
		ReleaseID:         request.ReleaseID,
		Tag:               request.Tag,
		Channel:           request.Channel,
		ManifestSHA256:    manifestDigest,
		AppSHA256:         appAsset.manifestDigest,
		BundleFingerprint: validation.Fingerprint,
		TrustKeyID:        trustKeyID,
		StagingPath:       finalApp,
		VerifiedAt:        s.now().UTC().Format(time.RFC3339),
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return StageReceipt{}, err
	}
	encoded = append(encoded, '\n')
	if err := writeOwnerFile(filepath.Join(temporaryDirectory, stageReceiptFile), encoded, 0o600); err != nil {
		return StageReceipt{}, err
	}
	if err := syncDirectory(temporaryDirectory); err != nil {
		return StageReceipt{}, err
	}
	if err := os.Rename(temporaryDirectory, finalDirectory); err != nil {
		return StageReceipt{}, err
	}
	complete = true
	if err := syncDirectory(s.rootDirectory); err != nil {
		return StageReceipt{}, err
	}
	return receipt, nil
}

func (s *Stager) reuseCompletedStage(
	ctx context.Context,
	request StageRequest,
	artifact Artifact,
	trustKeyID string,
	finalDirectory string,
	finalApp string,
) (StageReceipt, error) {
	metadata, err := os.Lstat(finalDirectory)
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Mode().Perm()&0o077 != 0 {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}
	receipt, err := readStageReceipt(filepath.Join(finalDirectory, stageReceiptFile))
	if err != nil || receipt.ReleaseID != request.ReleaseID || receipt.Tag != request.Tag ||
		receipt.Channel != request.Channel || receipt.ManifestSHA256 != request.ExpectedManifestSHA256 ||
		receipt.TrustKeyID != trustKeyID || receipt.StagingPath != finalApp || receipt.AppSHA256 != artifact.SHA256 {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}
	archiveDigest, err := hashRegularFile(filepath.Join(finalDirectory, stageArchiveFile), maximumAppArchiveBytes)
	if err != nil || archiveDigest != receipt.AppSHA256 {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}
	validation, err := s.bundleValidator.Validate(
		ctx, finalApp, request.Tag, artifact, s.currentBuildNumber, request.PublicKeyPEM, trustKeyID,
	)
	if err != nil || validation.Fingerprint != receipt.BundleFingerprint {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}
	return receipt, nil
}

func (s *Stager) acquireLock() (*os.File, error) {
	if err := prepareOwnerDirectory(s.rootDirectory); err != nil {
		return nil, err
	}
	return acquireStageFileLock(filepath.Join(s.rootDirectory, ".stage.lock"))
}

func (s *Stager) acquireRelocationGuard() (*os.File, error) {
	supportDirectory := filepath.Dir(s.rootDirectory)
	file, err := acquireStageFileLock(filepath.Join(supportDirectory, "application-relocation.lock"))
	if err != nil {
		return nil, err
	}
	journal := filepath.Join(supportDirectory, "application-relocation.json")
	if _, err := os.Lstat(journal); err == nil || !os.IsNotExist(err) {
		releaseStageLock(file)
		return nil, ErrStageUnsafeFilesystem
	}
	return file, nil
}

func acquireStageFileLock(path string) (*os.File, error) {
	fd, err := syscall.Open(
		path,
		syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC,
		0o600,
	)
	if err != nil {
		return nil, ErrStageUnsafeFilesystem
	}
	file := os.NewFile(uintptr(fd), path)
	metadata, statErr := file.Stat()
	pathMetadata, pathErr := os.Lstat(path)
	if statErr != nil || pathErr != nil || !metadata.Mode().IsRegular() ||
		metadata.Mode().Perm() != 0o600 || pathMetadata.Mode()&os.ModeSymlink != 0 ||
		!os.SameFile(metadata, pathMetadata) {
		file.Close()
		return nil, ErrStageUnsafeFilesystem
	}
	if stat, ok := metadata.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		file.Close()
		return nil, ErrStageUnsafeFilesystem
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrStageBusy
		}
		return nil, ErrStageUnsafeFilesystem
	}
	return file, nil
}

func releaseStageLock(file *os.File) {
	if file == nil {
		return
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	_ = file.Close()
}

func prepareOwnerDirectory(directory string) error {
	if !filepath.IsAbs(directory) {
		return ErrStageUnsafeFilesystem
	}
	parent := filepath.Dir(directory)
	if err := ensureOwnerDirectory(parent, true); err != nil {
		return err
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	return ensureOwnerDirectory(directory, false)
}

func ensureOwnerDirectory(directory string, mayCreate bool) error {
	metadata, err := os.Lstat(directory)
	if os.IsNotExist(err) && mayCreate {
		if err := os.Mkdir(directory, 0o700); err != nil {
			return err
		}
		metadata, err = os.Lstat(directory)
	}
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 ||
		metadata.Mode().Perm()&0o077 != 0 {
		return ErrStageUnsafeFilesystem
	}
	if stat, ok := metadata.Sys().(*syscall.Stat_t); !ok || int(stat.Uid) != os.Getuid() {
		return ErrStageUnsafeFilesystem
	}
	return nil
}

func writeOwnerFile(destination string, data []byte, mode os.FileMode) error {
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		file.Close()
		if !complete {
			_ = os.Remove(destination)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

func readStageReceipt(receiptPath string) (StageReceipt, error) {
	metadata, err := os.Lstat(receiptPath)
	if err != nil || !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 ||
		metadata.Mode().Perm()&0o077 != 0 || metadata.Size() < 2 || metadata.Size() > 16<<10 {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}
	body, err := os.ReadFile(receiptPath)
	if err != nil || rejectDuplicateJSONKeys(body) != nil {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var receipt StageReceipt
	if decoder.Decode(&receipt) != nil || requireJSONEOF(decoder) != nil ||
		receipt.SchemaVersion != StageSchemaVersion || receipt.ReleaseID <= 0 ||
		ParseStrictStageReceipt(receipt) != nil {
		return StageReceipt{}, ErrStageUnsafeFilesystem
	}
	return receipt, nil
}

func ParseStrictStageReceipt(receipt StageReceipt) error {
	if _, err := ParseSemanticVersion(receipt.Tag); err != nil {
		return ErrStageInvalidRequest
	}
	if receipt.Channel != UpdateChannelStable && receipt.Channel != UpdateChannelPreview {
		return ErrStageInvalidRequest
	}
	if !isLowerHexSHA256(receipt.ManifestSHA256) || !isLowerHexSHA256(receipt.AppSHA256) ||
		!isLowerHexSHA256(receipt.BundleFingerprint) || !isLowerHexSHA256(receipt.TrustKeyID) ||
		!filepath.IsAbs(receipt.StagingPath) {
		return ErrStageInvalidRequest
	}
	if _, err := time.Parse(time.RFC3339, receipt.VerifiedAt); err != nil {
		return ErrStageInvalidRequest
	}
	return nil
}

func extractReleaseArchive(archivePath, destination string) error {
	cleanDestination := filepath.Clean(destination)
	if !filepath.IsAbs(cleanDestination) || cleanDestination == string(filepath.Separator) {
		return ErrStageInvalidArchive
	}
	destinationPrefix := cleanDestination + string(filepath.Separator)
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return ErrStageInvalidArchive
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > maximumArchiveEntries {
		return ErrStageInvalidArchive
	}
	seen := make(map[string]string, len(reader.File))
	entryKinds := make(map[string]bool, len(reader.File))
	var total uint64
	for _, entry := range reader.File {
		normalized, isDirectory, err := validateArchiveEntry(entry)
		if err != nil {
			return err
		}
		components := strings.Split(normalized, "/")
		for index := range components {
			prefix := strings.Join(components[:index+1], "/")
			folded := strings.ToLower(prefix)
			if existing, exists := seen[folded]; exists && existing != prefix {
				return ErrStageInvalidArchive
			}
			if index != len(components)-1 {
				if prefixIsDirectory, exists := entryKinds[folded]; exists && !prefixIsDirectory {
					return ErrStageInvalidArchive
				}
			}
			if index == len(components)-1 {
				if existing, exists := seen[folded]; exists && existing == prefix {
					return ErrStageInvalidArchive
				}
			} else if _, exists := seen[folded]; !exists {
				seen[folded] = prefix
			}
		}
		seen[strings.ToLower(normalized)] = normalized
		entryKinds[strings.ToLower(normalized)] = isDirectory
		if entry.UncompressedSize64 > maximumExtractedBytes-total {
			return ErrStageInvalidArchive
		}
		total += entry.UncompressedSize64
		target := filepath.Clean(filepath.Join(cleanDestination, filepath.FromSlash(normalized)))
		if !strings.HasPrefix(target, destinationPrefix) {
			return ErrStageInvalidArchive
		}
		if isDirectory {
			if err := os.Mkdir(target, 0o700); err != nil && !os.IsExist(err) {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			return err
		}
		input, err := entry.Open()
		if err != nil {
			return ErrStageInvalidArchive
		}
		mode := entry.Mode().Perm()
		mode &^= 0o022
		if mode == 0 {
			mode = 0o600
		}
		output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			input.Close()
			return err
		}
		written, copyErr := io.Copy(output, io.LimitReader(input, int64(entry.UncompressedSize64)+1))
		closeErr := output.Close()
		input.Close()
		if copyErr != nil || closeErr != nil || written != int64(entry.UncompressedSize64) {
			return ErrStageInvalidArchive
		}
	}
	if existing, ok := seen[strings.ToLower(stageApplicationDirectory)]; !ok || existing != stageApplicationDirectory {
		return ErrStageInvalidArchive
	}
	return nil
}

func validateArchiveEntry(entry *zip.File) (string, bool, error) {
	name := entry.Name
	if name == "" || len(name) > maximumArchivePathBytes || !utf8.ValidString(name) ||
		strings.ContainsRune(name, 0) || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", false, ErrStageInvalidArchive
	}
	for index := 0; index < len(name); index++ {
		if name[index] < 0x20 || name[index] > 0x7e || name[index] == ':' {
			return "", false, ErrStageInvalidArchive
		}
	}
	isDirectory := strings.HasSuffix(name, "/")
	normalized := strings.TrimSuffix(name, "/")
	if normalized == "" || path.Clean(normalized) != normalized {
		return "", false, ErrStageInvalidArchive
	}
	components := strings.Split(normalized, "/")
	if components[0] != stageApplicationDirectory {
		return "", false, ErrStageInvalidArchive
	}
	for _, component := range components {
		if component == "" || component == "." || component == ".." {
			return "", false, ErrStageInvalidArchive
		}
	}
	mode := entry.Mode()
	if mode&(os.ModeSymlink|os.ModeDevice|os.ModeNamedPipe|os.ModeSocket|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return "", false, ErrStageInvalidArchive
	}
	if isDirectory != mode.IsDir() || (!isDirectory && !mode.IsRegular()) {
		return "", false, ErrStageInvalidArchive
	}
	if !isDirectory && entry.UncompressedSize64 > 0 {
		if entry.CompressedSize64 == 0 {
			return "", false, ErrStageInvalidArchive
		}
		quotient := entry.UncompressedSize64 / entry.CompressedSize64
		remainder := entry.UncompressedSize64 % entry.CompressedSize64
		if quotient > maximumCompressionRatio || (quotient == maximumCompressionRatio && remainder != 0) {
			return "", false, ErrStageInvalidArchive
		}
	}
	return normalized, isDirectory, nil
}

func BundleFingerprint(root string) (string, error) {
	metadata, err := os.Lstat(root)
	if err != nil || !metadata.IsDir() || metadata.Mode()&os.ModeSymlink != 0 {
		return "", ErrStageInvalidBundle
	}
	hash := sha256.New()
	err = filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 || (!info.IsDir() && !info.Mode().IsRegular()) {
			return ErrStageInvalidBundle
		}
		relative, err := filepath.Rel(root, current)
		if err != nil {
			return err
		}
		typeByte := byte('f')
		if info.IsDir() {
			typeByte = 'd'
		}
		fmt.Fprintf(hash, "%c\x00%s\x00%04o\x00%d\x00", typeByte, filepath.ToSlash(relative), info.Mode().Perm(), info.Size())
		if info.Mode().IsRegular() {
			file, err := os.Open(current)
			if err != nil {
				return err
			}
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return copyErr
			}
			if closeErr != nil {
				return closeErr
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hashRegularFile(filePath string, maximum int64) (string, error) {
	metadata, err := os.Lstat(filePath)
	if err != nil || !metadata.Mode().IsRegular() || metadata.Mode()&os.ModeSymlink != 0 || metadata.Size() < 1 || metadata.Size() > maximum {
		return "", ErrStageUnsafeFilesystem
	}
	file, err := os.Open(filePath)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	written, err := io.Copy(hash, io.LimitReader(file, maximum+1))
	if err != nil || written != metadata.Size() {
		return "", ErrStageUnsafeFilesystem
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func syncDirectory(directory string) error {
	file, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
