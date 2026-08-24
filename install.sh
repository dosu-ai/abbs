#!/bin/sh
set -eu

fail() {
  printf 'abbs installer: %s\n' "$*" >&2
  exit 1
}

for command_name in curl tar; do
  command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done

case "$(uname -s)" in
  Darwin) abbs_os=darwin ;;
  Linux) abbs_os=linux ;;
  *) fail "unsupported operating system: $(uname -s)" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) abbs_arch=amd64 ;;
  arm64|aarch64) abbs_arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

download_base=${ABBS_DOWNLOAD_BASE:-https://github.com/dosu-ai/abbs/releases}
download_base=${download_base%/}
case "$download_base" in
  https://*)
    curl_release() { curl --proto '=https' --proto-redir '=https' "$@"; }
    ;;
  http://127.0.0.1:*|http://localhost:*|http://\[::1\]:*)
    curl_release() { curl --proto '=http,https' --proto-redir '=http,https' "$@"; }
    ;;
  *) fail "ABBS_DOWNLOAD_BASE must use HTTPS (HTTP is allowed only for loopback tests)" ;;
esac

if [ -n "${ABBS_VERSION:-}" ]; then
  abbs_tag=$ABBS_VERSION
  case "$abbs_tag" in
    v*) ;;
    *) abbs_tag="v$abbs_tag" ;;
  esac
else
  abbs_tag=$(curl_release -A abbs-installer -fsSL -o /dev/null -w '%{url_effective}' "$download_base/latest") ||
    fail "could not resolve the latest release"
  abbs_tag=${abbs_tag%%\?*}
  abbs_tag=${abbs_tag%/}
  abbs_tag=${abbs_tag##*/}
fi

abbs_version=${abbs_tag#v}
printf '%s\n' "$abbs_version" | grep -Eq '^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$' ||
  fail "invalid release version: $abbs_tag"

archive_name="abbs_${abbs_version}_${abbs_os}_${abbs_arch}.tar.gz"
release_url="$download_base/download/$abbs_tag"
temporary_dir=$(mktemp -d "${TMPDIR:-/tmp}/abbs-install.XXXXXX") || fail "could not create a temporary directory"
install_temporary=
cleanup() {
  rm -rf "$temporary_dir"
  if [ -n "$install_temporary" ]; then
    rm -f "$install_temporary"
  fi
}
trap cleanup EXIT HUP INT TERM

printf 'Downloading abbs %s for %s/%s...\n' "$abbs_version" "$abbs_os" "$abbs_arch"
curl_release -A abbs-installer -fsSL "$release_url/$archive_name" -o "$temporary_dir/$archive_name" ||
  fail "could not download $archive_name"
curl_release -A abbs-installer -fsSL "$release_url/checksums.txt" -o "$temporary_dir/checksums.txt" ||
  fail "could not download checksums.txt"

awk -v wanted="$archive_name" '
  $2 == wanted || $2 == "*" wanted { print; matches++ }
  END { if (matches != 1) exit 1 }
' "$temporary_dir/checksums.txt" >"$temporary_dir/selected-checksum.txt" ||
  fail "checksums.txt does not contain exactly one entry for $archive_name"

if command -v sha256sum >/dev/null 2>&1; then
  (cd "$temporary_dir" && sha256sum -c selected-checksum.txt >/dev/null) || fail "checksum verification failed"
elif command -v shasum >/dev/null 2>&1; then
  (cd "$temporary_dir" && shasum -a 256 -c selected-checksum.txt >/dev/null) || fail "checksum verification failed"
else
  fail "sha256sum or shasum is required"
fi

tar -xzf "$temporary_dir/$archive_name" -C "$temporary_dir" || fail "could not extract $archive_name"
[ -f "$temporary_dir/abbs" ] || fail "archive does not contain the abbs binary"

install_dir=${ABBS_INSTALL_DIR:-${HOME:?HOME is not set}/.local/bin}
mkdir -p "$install_dir" || fail "could not create $install_dir"
install_temporary="$install_dir/.abbs.install.$$"
umask 022
if command -v install >/dev/null 2>&1; then
  install -m 0755 "$temporary_dir/abbs" "$install_temporary" || fail "could not stage abbs in $install_dir"
else
  cp "$temporary_dir/abbs" "$install_temporary" || fail "could not stage abbs in $install_dir"
  chmod 0755 "$install_temporary" || fail "could not make the staged binary executable"
fi
mv -f "$install_temporary" "$install_dir/abbs" || fail "could not install abbs in $install_dir"
install_temporary=

printf 'Installed abbs %s to %s/abbs\n' "$abbs_version" "$install_dir"
case ":${PATH:-}:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH, then run: abbs --version\n' "$install_dir" ;;
esac
