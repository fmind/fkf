package services

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/fmind/fkf/core"
)

const (
	upgradeRepository              = "https://github.com/fmind/fkf"
	upgradeArchiveMaxBytes         = 64 << 20
	upgradeExpandedArchiveMaxBytes = 128 << 20
	upgradeBinaryMaxBytes          = 64 << 20
	upgradeChecksumsMaxBytes       = 1 << 20
	upgradeNetworkTimeout          = 2 * time.Minute
	upgradeVerifyTimeout           = 10 * time.Second
	upgradeOutputMaxBytes          = 4096
)

// UpgradeReport describes the exact executable and release selected by an upgrade.
type UpgradeReport struct {
	Previous string `json:"previous"`
	Current  string `json:"current"`
	Path     string `json:"path"`
	Updated  bool   `json:"updated"`
	// PrecededBy names the different executable an ordinary `fkf` lookup would run instead.
	PrecededBy string `json:"preceded_by,omitempty"`
}

type releaseFetcher interface {
	Latest(context.Context) (string, error)
	Download(context.Context, string, string, int64) error
}

type curlReleaseFetcher struct{}

// Upgrade installs the latest stable release over the executable that launched FKF.
func Upgrade(ctx context.Context, executable string) (*UpgradeReport, error) {
	return upgradeWith(ctx, executable, core.Version, runtime.GOOS, runtime.GOARCH, curlReleaseFetcher{})
}

func upgradeWith(
	ctx context.Context,
	executable, currentVersion, goos, goarch string,
	fetcher releaseFetcher,
) (*UpgradeReport, error) {
	target, err := releaseTarget(goos, goarch)
	if err != nil {
		return nil, err
	}
	executable, err = validateUpgradeExecutable(executable)
	if err != nil {
		return nil, err
	}
	latest, err := fetcher.Latest(ctx)
	if err != nil {
		return nil, fmt.Errorf("resolve latest FKF release: %w", err)
	}
	if err := validateReleaseVersion(latest); err != nil {
		return nil, err
	}
	report := &UpgradeReport{
		Previous: currentVersion, Current: latest, Path: executable,
		PrecededBy: precedingFKF(executable),
	}
	if versionAtLeast(currentVersion, latest) {
		report.Current = currentVersion
		return report, nil
	}

	temporary, err := os.MkdirTemp("", "fkf-upgrade-*")
	if err != nil {
		return nil, fmt.Errorf("create upgrade directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()

	archiveName := fmt.Sprintf("fkf_%s_%s.tar.gz", latest, target)
	releaseURL := upgradeRepository + "/releases/download/" + latest + "/"
	archivePath := filepath.Join(temporary, archiveName)
	if err := fetcher.Download(ctx, releaseURL+archiveName, archivePath, upgradeArchiveMaxBytes); err != nil {
		return nil, fmt.Errorf("download release archive: %w", err)
	}
	checksumsPath := filepath.Join(temporary, "checksums.txt")
	if err := fetcher.Download(ctx, releaseURL+"checksums.txt", checksumsPath, upgradeChecksumsMaxBytes); err != nil {
		return nil, fmt.Errorf("download release checksums: %w", err)
	}
	artifacts, err := os.OpenRoot(temporary)
	if err != nil {
		return nil, fmt.Errorf("confine downloaded release artifacts: %w", err)
	}
	defer func() { _ = artifacts.Close() }()
	if err := verifyUpgradeChecksum(artifacts, archiveName); err != nil {
		return nil, err
	}
	binary, err := extractUpgradeBinary(artifacts, archiveName)
	if err != nil {
		return nil, err
	}
	candidate := filepath.Join(temporary, "fkf")
	if err := core.WriteFileAtomicMode(candidate, binary, 0o755); err != nil {
		return nil, fmt.Errorf("stage downloaded binary: %w", err)
	}
	output, err := core.RunCLIBounded(ctx, []string{candidate, "--version"}, core.DeclaredCommandDirectory,
		"", upgradeVerifyTimeout, upgradeOutputMaxBytes)
	if err != nil {
		return nil, fmt.Errorf("run downloaded binary: %w", err)
	}
	wantVersion := "fkf version " + latest
	if observed := strings.TrimSpace(output); observed != wantVersion {
		return nil, fmt.Errorf("downloaded binary reports %q, expected %q", observed, wantVersion)
	}
	if err := core.WriteFileAtomicMode(executable, binary, 0o755); err != nil {
		return nil, fmt.Errorf("replace FKF executable: %w", err)
	}
	report.Updated = true
	return report, nil
}

func precedingFKF(target string) string {
	candidate, err := exec.LookPath("fkf")
	if err != nil && !errors.Is(err, exec.ErrDot) {
		return ""
	}
	candidate, err = filepath.Abs(candidate)
	if err != nil {
		return ""
	}
	candidateInfo, err := os.Stat(candidate)
	if err != nil {
		return ""
	}
	targetInfo, err := os.Stat(target)
	if err != nil || os.SameFile(candidateInfo, targetInfo) {
		return ""
	}
	return candidate
}

func (curlReleaseFetcher) Latest(ctx context.Context) (string, error) {
	output, err := core.RunCLIBounded(ctx, []string{
		"curl", "-q", "--proto", "=https", "--tlsv1.2", "-fsSLI",
		"-o", "/dev/null", "-w", "%{url_effective}", upgradeRepository + "/releases/latest",
	}, core.DeclaredCommandDirectory, "", upgradeNetworkTimeout, upgradeOutputMaxBytes)
	if err != nil {
		return "", err
	}
	url := strings.TrimRight(strings.TrimSpace(output), "/")
	version := filepath.Base(url)
	if version == "." || version == "/" || version == "" {
		return "", fmt.Errorf("latest release redirected to an invalid URL")
	}
	return version, nil
}

func (curlReleaseFetcher) Download(ctx context.Context, url, destination string, maximum int64) error {
	_, err := core.RunCLIBounded(ctx, []string{
		"curl", "-q", "--proto", "=https", "--tlsv1.2", "-fsSL",
		"--max-filesize", strconv.FormatInt(maximum, 10), "-o", destination, url,
	}, core.DeclaredCommandDirectory, "", upgradeNetworkTimeout, upgradeOutputMaxBytes)
	return err
}

func releaseTarget(goos, goarch string) (string, error) {
	if goos != "linux" && goos != "darwin" {
		return "", fmt.Errorf("unsupported upgrade operating system %q; install a release manually", goos)
	}
	if goarch != "amd64" && goarch != "arm64" {
		return "", fmt.Errorf("unsupported upgrade architecture %q; install a release manually", goarch)
	}
	return goos + "_" + goarch, nil
}

func validateUpgradeExecutable(executable string) (string, error) {
	if !filepath.IsAbs(executable) {
		return "", fmt.Errorf("current FKF executable path must be absolute")
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(executable))
	if err != nil {
		return "", fmt.Errorf("resolve current FKF executable: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("inspect current FKF executable: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("current FKF executable is not a regular executable file")
	}
	return resolved, nil
}

func validateReleaseVersion(version string) error {
	if strings.ContainsAny(version, "+-") {
		return fmt.Errorf("latest release %q is not vMAJOR.MINOR.PATCH", version)
	}
	if _, ok := parseComparableVersion(version); !ok {
		return fmt.Errorf("latest release %q is not vMAJOR.MINOR.PATCH", version)
	}
	return nil
}

type comparableVersion struct {
	parts      [3]uint64
	prerelease bool
}

func parseComparableVersion(version string) (comparableVersion, bool) {
	withoutBuild, _, _ := strings.Cut(version, "+")
	numbers, prerelease, _ := strings.Cut(withoutBuild, "-")
	parts := strings.Split(strings.TrimPrefix(numbers, "v"), ".")
	if !strings.HasPrefix(numbers, "v") || len(parts) != 3 {
		return comparableVersion{}, false
	}
	parsed := comparableVersion{prerelease: prerelease != ""}
	for index, part := range parts {
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return comparableVersion{}, false
		}
		parsed.parts[index] = value
	}
	return parsed, true
}

func versionAtLeast(current, latest string) bool {
	currentVersion, currentOK := parseComparableVersion(current)
	latestVersion, latestOK := parseComparableVersion(latest)
	if !currentOK || !latestOK {
		return false
	}
	for index := range currentVersion.parts {
		if currentVersion.parts[index] != latestVersion.parts[index] {
			return currentVersion.parts[index] > latestVersion.parts[index]
		}
	}
	return !currentVersion.prerelease || latestVersion.prerelease
}

func verifyUpgradeChecksum(artifacts *os.Root, archiveName string) error {
	checksumsFile, err := artifacts.Open("checksums.txt")
	if err != nil {
		return fmt.Errorf("open release checksums: %w", err)
	}
	checksums, err := readUpgradeFile(checksumsFile, upgradeChecksumsMaxBytes)
	if err != nil {
		return fmt.Errorf("read release checksums: %w", err)
	}
	var expected string
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[1] != archiveName {
			continue
		}
		if expected != "" {
			return fmt.Errorf("release checksum names %s more than once", archiveName)
		}
		expected = fields[0]
	}
	decoded, err := hex.DecodeString(expected)
	if expected == "" || err != nil || len(decoded) != sha256.Size {
		return fmt.Errorf("release checksum for %s is missing or invalid", archiveName)
	}
	archive, err := artifacts.Open(archiveName)
	if err != nil {
		return fmt.Errorf("open release archive: %w", err)
	}
	defer func() { _ = archive.Close() }()
	info, err := archive.Stat()
	if err != nil {
		return fmt.Errorf("inspect release archive: %w", err)
	}
	if info.Size() <= 0 || info.Size() > upgradeArchiveMaxBytes {
		return fmt.Errorf("release archive size %d is outside the allowed range", info.Size())
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, archive); err != nil {
		return fmt.Errorf("hash release archive: %w", err)
	}
	if !strings.EqualFold(hex.EncodeToString(digest.Sum(nil)), expected) {
		return fmt.Errorf("checksum verification failed for %s", archiveName)
	}
	return nil
}

func extractUpgradeBinary(artifacts *os.Root, archiveName string) ([]byte, error) {
	archive, err := artifacts.Open(archiveName)
	if err != nil {
		return nil, fmt.Errorf("open release archive: %w", err)
	}
	defer func() { _ = archive.Close() }()
	compressed, err := gzip.NewReader(archive)
	if err != nil {
		return nil, fmt.Errorf("open compressed release archive: %w", err)
	}
	defer func() { _ = compressed.Close() }()
	reader := tar.NewReader(io.LimitReader(compressed, upgradeExpandedArchiveMaxBytes))
	var binary []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read release archive: %w", err)
		}
		if header.Name != "fkf" {
			continue
		}
		if binary != nil {
			return nil, fmt.Errorf("release archive contains more than one fkf binary")
		}
		if !header.FileInfo().Mode().IsRegular() {
			return nil, fmt.Errorf("release archive fkf member is not a regular file")
		}
		if header.Size <= 0 || header.Size > upgradeBinaryMaxBytes {
			return nil, fmt.Errorf("release archive fkf member size %d is outside the allowed range", header.Size)
		}
		binary, err = io.ReadAll(io.LimitReader(reader, header.Size+1))
		if err != nil {
			return nil, fmt.Errorf("read release binary: %w", err)
		}
		if int64(len(binary)) != header.Size {
			return nil, fmt.Errorf("release archive fkf member is truncated")
		}
	}
	if binary == nil {
		return nil, fmt.Errorf("release archive does not contain fkf")
	}
	return binary, nil
}

func readUpgradeFile(file *os.File, maximum int64) ([]byte, error) {
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("file exceeds %d bytes", maximum)
	}
	return data, nil
}
