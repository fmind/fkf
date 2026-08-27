package fkf_test

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const releaseArchive = "fkf_v1.0.0_linux_amd64.tar.gz"

func TestInstallerSelectsEveryPublishedPlatformArchive(t *testing.T) {
	tests := []struct {
		name, system, machine, releaseOS, releaseArch string
	}{
		{name: "Linux amd64", system: "Linux", machine: "x86_64", releaseOS: "linux", releaseArch: "amd64"},
		{name: "Linux arm64", system: "Linux", machine: "aarch64", releaseOS: "linux", releaseArch: "arm64"},
		{name: "macOS amd64", system: "Darwin", machine: "x86_64", releaseOS: "darwin", releaseArch: "amd64"},
		{name: "macOS arm64", system: "Darwin", machine: "arm64", releaseOS: "darwin", releaseArch: "arm64"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newInstallerFixtureForPlatform(
				t, false, test.system, test.machine, test.releaseOS, test.releaseArch,
			)
			output, err := fixture.run()
			if err != nil {
				t.Fatalf("install.sh failed: %v\n%s", err, output)
			}
			log, err := os.ReadFile(fixture.curlLog)
			if err != nil {
				t.Fatal(err)
			}
			want := "https://github.com/fmind/fkf/releases/download/v1.0.0/" + fixture.archiveName
			if !strings.Contains(string(log), want) {
				t.Fatalf("curl log = %q, want %q", log, want)
			}
		})
	}
}

func TestInstallerDownloadsLatestVerifiesAndInstalls(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	output, err := fixture.run()
	if err != nil {
		t.Fatalf("install.sh failed: %v\n%s", err, output)
	}

	installed := filepath.Join(fixture.installDir, "fkf")
	info, err := os.Stat(installed)
	if err != nil {
		t.Fatalf("installed binary: %v", err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("installed binary mode = %o, want executable", info.Mode().Perm())
	}
	version, err := exec.Command(installed, "--version").CombinedOutput()
	if err != nil || !strings.Contains(string(version), "v1.0.0") {
		t.Fatalf("installed --version = %q, %v", version, err)
	}

	log, err := os.ReadFile(fixture.curlLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"https://github.com/fmind/fkf/releases/latest",
		"https://github.com/fmind/fkf/releases/download/v1.0.0/" + releaseArchive,
		"https://github.com/fmind/fkf/releases/download/v1.0.0/checksums.txt",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("curl log = %q, want %q", log, want)
		}
	}
}

func TestInstallerRefusesAChecksumMismatch(t *testing.T) {
	fixture := newInstallerFixture(t, true)
	output, err := fixture.run()
	if err == nil {
		t.Fatalf("install.sh accepted a checksum mismatch:\n%s", output)
	}
	if !strings.Contains(output, "checksum") {
		t.Fatalf("failure = %q, want it to name the checksum", output)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.installDir, "fkf")); !os.IsNotExist(statErr) {
		t.Fatalf("checksum failure installed a binary: %v", statErr)
	}
}

func TestInstallerRefusesAnArchiveWithTheWrongVersion(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	writeReleaseArchive(t, fixture.archive, "v9.9.9")
	fixture.writeChecksum(t)

	output, err := fixture.run()
	if err == nil {
		t.Fatalf("install.sh accepted a binary for the wrong version:\n%s", output)
	}
	if !strings.Contains(output, "version") {
		t.Fatalf("failure = %q, want it to name the version mismatch", output)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.installDir, "fkf")); !os.IsNotExist(statErr) {
		t.Fatalf("version mismatch installed a binary: %v", statErr)
	}
}

func TestInstallerPreservesAnExistingBinaryWhenStagingFails(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	if err := os.MkdirAll(fixture.installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	installed := filepath.Join(fixture.installDir, "fkf")
	const previous = "#!/bin/sh\nprintf 'fkf version v0.9.0\\n'\n"
	if err := os.WriteFile(installed, []byte(previous), 0o755); err != nil {
		t.Fatal(err)
	}
	fixture.env = append(fixture.env, "FKF_TEST_INSTALL_FAIL=1")

	output, err := fixture.run()
	if err == nil {
		t.Fatalf("install.sh accepted a failed staging copy:\n%s", output)
	}
	current, readErr := os.ReadFile(installed)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(current) != previous {
		t.Fatalf("failed install replaced the existing binary with %q", current)
	}
}

func TestInstallerCanRequireReleaseAttestation(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	fixture.env = append(fixture.env, "FKF_VERIFY_ATTESTATION=1")
	output, err := fixture.run()
	if err != nil {
		t.Fatalf("attested install failed: %v\n%s", err, output)
	}
	log, err := os.ReadFile(fixture.ghLog)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"attestation verify", releaseArchive, "--repo fmind/fkf",
		"--signer-workflow fmind/fkf/.github/workflows/cd.yml",
		"--source-ref refs/tags/v1.0.0",
	} {
		if !strings.Contains(string(log), want) {
			t.Errorf("gh log = %q, want %q", log, want)
		}
	}
}

func TestInstallerRefusesARequiredAttestationFailure(t *testing.T) {
	fixture := newInstallerFixture(t, false)
	fixture.env = append(fixture.env, "FKF_VERIFY_ATTESTATION=1", "FKF_TEST_GH_FAIL=1")
	output, err := fixture.run()
	if err == nil {
		t.Fatalf("install.sh accepted a failed attestation:\n%s", output)
	}
	if !strings.Contains(output, "attestation") {
		t.Fatalf("failure = %q, want it to name the attestation", output)
	}
	if _, statErr := os.Stat(filepath.Join(fixture.installDir, "fkf")); !os.IsNotExist(statErr) {
		t.Fatalf("attestation failure installed a binary: %v", statErr)
	}
}

func TestInstallationDocsExposeLatestAndAttestedPaths(t *testing.T) {
	for _, path := range []string{"README.md", "docs/content/docs/getting-started.md"} {
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{
			"github.com/fmind/fkf/cmd/fkf@latest",
			"FKF_VERIFY_ATTESTATION=1 sh",
			"fkf upgrade",
		} {
			if !strings.Contains(string(content), want) {
				t.Errorf("%s lacks documented latest install contract %q", path, want)
			}
		}
	}
}

func TestInstallerArchiveNameMatchesGoReleaser(t *testing.T) {
	config, err := os.ReadFile(".goreleaser.yml")
	if err != nil {
		t.Fatal(err)
	}
	want := `name_template: "{{ .ProjectName }}_{{ .Tag }}_{{ .Os }}_{{ .Arch }}"`
	if !strings.Contains(string(config), want) {
		t.Fatalf(".goreleaser.yml must retain the archive name install.sh downloads: %s", want)
	}
}

type installerFixture struct {
	root        string
	installDir  string
	curlLog     string
	ghLog       string
	archive     string
	archiveName string
	checksums   string
	env         []string
}

func newInstallerFixture(t *testing.T, corruptChecksum bool) installerFixture {
	return newInstallerFixtureForPlatform(t, corruptChecksum, "Linux", "x86_64", "linux", "amd64")
}

func newInstallerFixtureForPlatform(
	t *testing.T, corruptChecksum bool, system, machine, releaseOS, releaseArch string,
) installerFixture {
	t.Helper()
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	temporary := t.TempDir()
	archiveName := fmt.Sprintf("fkf_v1.0.0_%s_%s.tar.gz", releaseOS, releaseArch)
	archive := filepath.Join(temporary, archiveName)
	writeReleaseArchive(t, archive, "v1.0.0")
	data, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if corruptChecksum {
		digest = strings.Repeat("0", sha256.Size*2)
	}
	checksums := filepath.Join(temporary, "checksums.txt")
	if err := os.WriteFile(checksums, []byte(digest+"  "+archiveName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	fakeBin := filepath.Join(temporary, "fake-bin")
	if err := os.MkdirAll(fakeBin, 0o700); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "uname"), `#!/bin/sh
case "${1:-}" in
  -s) printf '%s\n' "$FKF_TEST_UNAME_SYSTEM" ;;
  -m) printf '%s\n' "$FKF_TEST_UNAME_MACHINE" ;;
  *) exit 2 ;;
esac
`)
	writeExecutable(t, filepath.Join(fakeBin, "curl"), `#!/bin/sh
set -eu
out=
url=
while [ "$#" -gt 0 ]; do
  case "$1" in
    -o) out=$2; shift 2 ;;
    -w|--proto) shift 2 ;;
    -*) shift ;;
    *) url=$1; shift ;;
  esac
done
printf '%s\n' "$url" >> "$FKF_TEST_CURL_LOG"
case "$url" in
  https://github.com/fmind/fkf/releases/latest)
    printf '%s' https://github.com/fmind/fkf/releases/tag/v1.0.0
    ;;
  */fkf_v1.0.0_*.tar.gz)
    cp "$FKF_TEST_ARCHIVE" "$out"
    ;;
  */checksums.txt)
    cp "$FKF_TEST_CHECKSUMS" "$out"
    ;;
  *) exit 22 ;;
esac
`)
	realInstall, err := exec.LookPath("install")
	if err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(fakeBin, "install"), `#!/bin/sh
set -eu
if [ "${FKF_TEST_INSTALL_FAIL:-0}" = 1 ]; then
  destination=
  for argument do destination=$argument; done
  printf '%s\n' damaged > "$destination"
  exit 1
fi
exec "$FKF_TEST_REAL_INSTALL" "$@"
`)
	writeExecutable(t, filepath.Join(fakeBin, "gh"), `#!/bin/sh
set -eu
printf '%s\n' "$*" >> "$FKF_TEST_GH_LOG"
[ "${FKF_TEST_GH_FAIL:-0}" != 1 ]
`)

	installDir := filepath.Join(temporary, "installed")
	curlLog := filepath.Join(temporary, "curl.log")
	ghLog := filepath.Join(temporary, "gh.log")
	env := append(os.Environ(),
		"HOME="+filepath.Join(temporary, "home"),
		"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FKF_INSTALL_DIR="+installDir,
		"FKF_TEST_ARCHIVE="+archive,
		"FKF_TEST_CHECKSUMS="+checksums,
		"FKF_TEST_CURL_LOG="+curlLog,
		"FKF_TEST_GH_LOG="+ghLog,
		"FKF_TEST_UNAME_SYSTEM="+system,
		"FKF_TEST_UNAME_MACHINE="+machine,
		"FKF_TEST_REAL_INSTALL="+realInstall,
	)
	return installerFixture{
		root: root, installDir: installDir, curlLog: curlLog, ghLog: ghLog,
		archive: archive, archiveName: archiveName, checksums: checksums, env: env,
	}
}

func (fixture installerFixture) writeChecksum(t *testing.T) {
	t.Helper()
	data, err := os.ReadFile(fixture.archive)
	if err != nil {
		t.Fatal(err)
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(data))
	if err := os.WriteFile(fixture.checksums, []byte(digest+"  "+fixture.archiveName+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fixture installerFixture) run() (string, error) {
	command := exec.Command("sh", filepath.Join(fixture.root, "install.sh"))
	command.Dir = fixture.root
	command.Env = fixture.env
	output, err := command.CombinedOutput()
	return string(output), err
}

func writeReleaseArchive(t *testing.T, path, version string) {
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

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}
