package services

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const testUpgradeArchive = "fkf_v1.2.3_linux_amd64.tar.gz"

type fakeReleaseFetcher struct {
	latest    string
	files     map[string]string
	downloads []string
}

func (fetcher *fakeReleaseFetcher) Latest(context.Context) (string, error) {
	return fetcher.latest, nil
}

func (fetcher *fakeReleaseFetcher) Download(_ context.Context, url, destination string, _ int64) error {
	fetcher.downloads = append(fetcher.downloads, url)
	source, ok := fetcher.files[url]
	if !ok {
		return fmt.Errorf("unexpected download %s", url)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	return os.WriteFile(destination, data, 0o600)
}

func TestUpgradeVerifiesAndAtomicallyReplacesTheCurrentExecutable(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v1.2.3", false)
	resolvedExecutable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatal(err)
	}

	report, err := upgradeWith(t.Context(), executable, "v1.2.2", "linux", "amd64", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Updated || report.Previous != "v1.2.2" || report.Current != "v1.2.3" || report.Path != resolvedExecutable {
		t.Fatalf("report = %+v", report)
	}
	wantDownloads := []string{
		"https://github.com/fmind/fkf/releases/download/v1.2.3/" + testUpgradeArchive,
		"https://github.com/fmind/fkf/releases/download/v1.2.3/checksums.txt",
	}
	if strings.Join(fetcher.downloads, "\n") != strings.Join(wantDownloads, "\n") {
		t.Fatalf("downloads = %v, want %v", fetcher.downloads, wantDownloads)
	}
	output, err := exec.Command(executable, "--version").CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "fkf version v1.2.3" {
		t.Fatalf("installed --version = %q, %v", output, err)
	}
}

func TestUpgradeSkipsDownloadsWhenTheExecutableIsCurrent(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v1.2.3", false)

	report, err := upgradeWith(t.Context(), executable, "v1.2.3", "darwin", "arm64", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if report.Updated || len(fetcher.downloads) != 0 {
		t.Fatalf("report = %+v, downloads = %v", report, fetcher.downloads)
	}
}

func TestUpgradeReportsAnotherFKFThatPrecedesItsTargetOnPath(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v1.2.3", false)
	shadowDirectory := t.TempDir()
	shadow := filepath.Join(shadowDirectory, "fkf")
	writeUpgradeExecutable(t, shadow, "v0.1.0")
	t.Setenv("PATH", strings.Join([]string{
		shadowDirectory, filepath.Dir(executable), os.Getenv("PATH"),
	}, string(os.PathListSeparator)))

	report, err := upgradeWith(t.Context(), executable, "v1.2.3", "linux", "amd64", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if report.PrecededBy != shadow {
		t.Fatalf("preceded_by = %q, want %q", report.PrecededBy, shadow)
	}
}

func TestUpgradeReportsAnotherFKFFromRelativePathEntry(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v1.2.3", false)
	shadowDirectory := t.TempDir()
	shadow := filepath.Join(shadowDirectory, "fkf")
	writeUpgradeExecutable(t, shadow, "v0.1.0")
	t.Chdir(shadowDirectory)
	t.Setenv("PATH", strings.Join([]string{
		".", filepath.Dir(executable),
	}, string(os.PathListSeparator)))

	report, err := upgradeWith(t.Context(), executable, "v1.2.3", "linux", "amd64", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if report.PrecededBy != shadow {
		t.Fatalf("preceded_by = %q, want relative PATH candidate %q", report.PrecededBy, shadow)
	}
}

func TestUpgradeDoesNotWarnWhenItsTargetIsFirstOnPath(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v1.2.3", false)
	shadowDirectory := t.TempDir()
	writeUpgradeExecutable(t, filepath.Join(shadowDirectory, "fkf"), "v0.1.0")
	t.Setenv("PATH", strings.Join([]string{
		filepath.Dir(executable), shadowDirectory, os.Getenv("PATH"),
	}, string(os.PathListSeparator)))

	report, err := upgradeWith(t.Context(), executable, "v1.2.3", "linux", "amd64", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if report.PrecededBy != "" {
		t.Fatalf("preceded_by = %q, want no warning for the first PATH entry", report.PrecededBy)
	}
}

func TestCurlReleaseFetcherPreservesHomeAndExplicitlyDisablesCurlConfig(t *testing.T) {
	home := t.TempDir()
	fakeBin := t.TempDir()
	log := filepath.Join(t.TempDir(), "curl.log")
	curl := filepath.Join(fakeBin, "curl")
	writeUpgradeExecutableBody(t, curl, `#!/bin/sh
set -eu
[ "$HOME" = "$FKF_EXPECT_HOME" ] || { printf 'HOME was hidden: %s\n' "$HOME" >&2; exit 91; }
[ "${1:-}" = -q ] || { printf '%s\n' 'curl config was not disabled first' >&2; exit 92; }
printf '%s\n' "$*" >> "$FKF_CURL_LOG"
output=
previous=
for argument do
  if [ "$previous" = -o ] && [ "$argument" != /dev/null ]; then output=$argument; fi
  previous=$argument
done
if [ -n "$output" ]; then : > "$output"; else printf '%s' 'https://github.com/fmind/fkf/releases/tag/v1.2.3'; fi
`)
	t.Setenv("HOME", home)
	t.Setenv("PATH", fakeBin)
	t.Setenv("FKF_EXPECT_HOME", home)
	t.Setenv("FKF_CURL_LOG", log)
	fetcher := curlReleaseFetcher{}

	latest, err := fetcher.Latest(t.Context())
	if err != nil || latest != "v1.2.3" {
		t.Fatalf("Latest() = %q, %v", latest, err)
	}
	destination := filepath.Join(t.TempDir(), "download")
	if err := fetcher.Download(t.Context(), "https://example.test/artifact", destination, 1024); err != nil {
		t.Fatalf("Download() error = %v", err)
	}
	commands, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(string(commands)), "\n") + 1; lines != 2 {
		t.Fatalf("curl commands = %q, want latest and download", commands)
	}
}

func TestUpgradeDoesNotReplaceAnEqualDevelopmentBuild(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v1.2.3", false)
	before, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}

	report, err := upgradeWith(t.Context(), executable, "v1.2.3+dirty", "linux", "amd64", fetcher)
	if err != nil {
		t.Fatal(err)
	}
	if report.Updated || report.Current != "v1.2.3+dirty" || len(fetcher.downloads) != 0 {
		t.Fatalf("report = %+v, downloads = %v", report, fetcher.downloads)
	}
	after, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("equal development build was replaced")
	}
}

func TestUpgradePreservesTheExecutableOnChecksumFailure(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v1.2.3", true)
	before, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := upgradeWith(t.Context(), executable, "v1.2.2", "linux", "amd64", fetcher); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("error = %v, want a checksum refusal", err)
	}
	after, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("checksum failure changed the executable")
	}
}

func TestUpgradePreservesTheExecutableWhenTheArchiveReportsAnotherVersion(t *testing.T) {
	executable, fetcher := upgradeFixture(t, "v9.9.9", false)
	before, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := upgradeWith(t.Context(), executable, "v1.2.2", "linux", "amd64", fetcher); err == nil || !strings.Contains(err.Error(), "reports") {
		t.Fatalf("error = %v, want a version refusal", err)
	}
	after, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("version failure changed the executable")
	}
}

func upgradeFixture(t *testing.T, archiveVersion string, corruptChecksum bool) (string, *fakeReleaseFetcher) {
	t.Helper()
	temporary := t.TempDir()
	executable := filepath.Join(temporary, "fkf")
	writeUpgradeExecutable(t, executable, "v1.2.2")
	archive := filepath.Join(temporary, testUpgradeArchive)
	writeUpgradeArchive(t, archive, archiveVersion)
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if corruptChecksum {
		digest = strings.Repeat("0", sha256.Size*2)
	}
	checksums := filepath.Join(temporary, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(digest+"  "+testUpgradeArchive+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	release := "https://github.com/fmind/fkf/releases/download/v1.2.3/"
	return executable, &fakeReleaseFetcher{
		latest: "v1.2.3",
		files: map[string]string{
			release + testUpgradeArchive: archive,
			release + "checksums.txt":    checksums,
		},
	}
}

func writeUpgradeArchive(t *testing.T, path, version string) {
	t.Helper()
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	binary := []byte("#!/bin/sh\nprintf '%s\\n' 'fkf version " + version + "'\n")
	if err := tarWriter.WriteHeader(&tar.Header{Name: "fkf", Mode: 0o755, Size: int64(len(binary))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeUpgradeExecutable(t *testing.T, path, version string) {
	t.Helper()
	body := []byte("#!/bin/sh\nprintf '%s\\n' 'fkf version " + version + "'\n")
	writeUpgradeExecutableBody(t, path, string(body))
}

func writeUpgradeExecutableBody(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}
