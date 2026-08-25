#!/bin/sh
# Install the latest published fkf release without requiring a Go toolchain.
set -eu

repository=https://github.com/fmind/fkf
repository_name=fmind/fkf
version=${FKF_VERSION:-latest}
install_dir=${FKF_INSTALL_DIR:-${HOME:?HOME is required}/.local/bin}
verify_attestation=${FKF_VERIFY_ATTESTATION:-0}

fail() {
	printf 'fkf installer: %s\n' "$*" >&2
	exit 1
}

need() {
	command -v "$1" >/dev/null 2>&1 || fail "required command not found: $1"
}

validate_version() {
	case "$1" in
	v*.*.*) ;;
	*) fail "invalid version $1; expected vMAJOR.MINOR.PATCH" ;;
	esac
	remainder=${1#v}
	major=${remainder%%.*}
	remainder=${remainder#*.}
	minor=${remainder%%.*}
	patch=${remainder#*.}
	for component in "$major" "$minor" "$patch"; do
		case "$component" in
		'' | *[!0-9]*) fail "invalid version $1; expected vMAJOR.MINOR.PATCH" ;;
		esac
	done
}

for command in curl tar awk install mkdir mktemp rm uname; do
	need "$command"
done
case "$verify_attestation" in
0) ;;
1) need gh ;;
*) fail "FKF_VERIFY_ATTESTATION must be 0 or 1" ;;
esac

case "$install_dir" in
/*) ;;
*) fail "FKF_INSTALL_DIR must be an absolute path" ;;
esac

case "$(uname -s)" in
Linux) os=linux ;;
Darwin) os=darwin ;;
*) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
x86_64 | amd64) arch=amd64 ;;
arm64 | aarch64) arch=arm64 ;;
*) fail "unsupported architecture: $(uname -m)" ;;
esac

if [ "$version" = latest ]; then
	version_url=$(curl --proto '=https' --tlsv1.2 -fsSLI -o /dev/null -w '%{url_effective}' "$repository/releases/latest")
	version=${version_url##*/}
fi
validate_version "$version"

temporary_root=${TMPDIR:-/tmp}
case "$temporary_root" in
/*) ;;
*) fail "TMPDIR must be an absolute path" ;;
esac
temporary=$(mktemp -d "$temporary_root/fkf-install.XXXXXX")
case "$temporary" in
"$temporary_root"/fkf-install.*) ;;
*) fail "mktemp returned an unexpected path" ;;
esac
cleanup() {
	rm -rf "$temporary"
}
trap cleanup EXIT HUP INT TERM

archive=fkf_${version}_${os}_${arch}.tar.gz
release_url=$repository/releases/download/$version
curl --proto '=https' --tlsv1.2 -fsSL -o "$temporary/$archive" "$release_url/$archive"
curl --proto '=https' --tlsv1.2 -fsSL -o "$temporary/checksums.txt" "$release_url/checksums.txt"

expected=$(awk -v archive="$archive" '$2 == archive { print $1; found = 1; exit } END { if (!found) exit 1 }' "$temporary/checksums.txt") ||
	fail "release checksum does not name $archive"
case "$expected" in
'' | *[!0-9a-fA-F]*) fail "release checksum for $archive is invalid" ;;
esac
[ "${#expected}" -eq 64 ] || fail "release checksum for $archive is invalid"

if command -v sha256sum >/dev/null 2>&1; then
	actual=$(sha256sum "$temporary/$archive" | awk '{ print $1 }')
elif command -v shasum >/dev/null 2>&1; then
	actual=$(shasum -a 256 "$temporary/$archive" | awk '{ print $1 }')
else
	fail "required checksum command not found: sha256sum or shasum"
fi
[ "$actual" = "$expected" ] || fail "checksum verification failed for $archive"
if [ "$verify_attestation" = 1 ]; then
	gh attestation verify "$temporary/$archive" \
		--repo "$repository_name" \
		--signer-workflow "$repository_name/.github/workflows/cd.yml" \
		--source-ref "refs/tags/$version" >/dev/null ||
		fail "release attestation verification failed for $archive"
fi

# Extract only the expected binary member, never arbitrary paths supplied by an archive.
tar -xzf "$temporary/$archive" -C "$temporary" fkf
observed_version=$("$temporary/fkf" --version)
[ "$observed_version" = "fkf version $version" ] || fail "downloaded binary does not report version $version"
mkdir -p "$install_dir"
install -m 0755 "$temporary/fkf" "$install_dir/fkf"

printf 'Installed %s to %s/fkf\n' "$version" "$install_dir"
case ":${PATH:-}:" in
*":$install_dir:"*) ;;
*) printf 'Add %s to PATH to run fkf.\n' "$install_dir" ;;
esac
