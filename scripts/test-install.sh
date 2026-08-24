#!/bin/bash
set -euo pipefail

if [[ $# -ne 1 ]]; then
  echo "usage: $0 DIST_DIR" >&2
  exit 2
fi

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dist_dir="$(cd "$1" && pwd)"
shopt -s nullglob
case "$(uname -s)" in
  Darwin) native_os=darwin ;;
  Linux) native_os=linux ;;
  *) echo "unsupported test operating system" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) native_arch=amd64 ;;
  arm64|aarch64) native_arch=arm64 ;;
  *) echo "unsupported test architecture" >&2; exit 1 ;;
esac

native_archives=("$dist_dir"/abbs_*_"$native_os"_"$native_arch".tar.gz)
[[ ${#native_archives[@]} -eq 1 ]] || { echo "could not select one native archive" >&2; exit 1; }
archive_path=${native_archives[0]}
archive_name=$(basename "$archive_path")
version=${archive_name#abbs_}
version=${version%_"${native_os}"_"${native_arch}".tar.gz}
tag="v$version"

test_root=$(mktemp -d "${TMPDIR:-/tmp}/abbs-installer-test.XXXXXX")
server_pid=
cleanup() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
  fi
  rm -rf "$test_root"
}
trap cleanup EXIT

release_dir="$test_root/fixtures/releases/download/$tag"
mkdir -p "$release_dir"
cp "$archive_path" "$release_dir/$archive_name"
cp "$dist_dir/checksums.txt" "$release_dir/checksums.txt"

address_file="$test_root/address"
python3 "$repo_dir/scripts/release-fixture-server.py" "$test_root/fixtures" "$tag" "$address_file" &
server_pid=$!
for _attempt in {1..100}; do
  [[ -s "$address_file" ]] && break
  sleep 0.05
done
[[ -s "$address_file" ]] || { echo "fixture server did not start" >&2; exit 1; }
download_base="$(<"$address_file")/releases"

sh -n "$repo_dir/install.sh"

explicit_dir="$test_root/explicit"
ABBS_VERSION="$tag" ABBS_INSTALL_DIR="$explicit_dir" ABBS_DOWNLOAD_BASE="$download_base" \
  sh "$repo_dir/install.sh"
explicit_output=$("$explicit_dir/abbs" --version)
[[ "$explicit_output" == "abbs $version" ]] || {
  echo "installed binary reported $explicit_output, want abbs $version" >&2
  exit 1
}

latest_dir="$test_root/latest"
ABBS_INSTALL_DIR="$latest_dir" ABBS_DOWNLOAD_BASE="$download_base" sh "$repo_dir/install.sh"
[[ "$("$latest_dir/abbs" --version)" == "$explicit_output" ]]

printf 'tamper' >>"$release_dir/$archive_name"
tampered_dir="$test_root/tampered"
if ABBS_VERSION="$tag" ABBS_INSTALL_DIR="$tampered_dir" ABBS_DOWNLOAD_BASE="$download_base" \
  sh "$repo_dir/install.sh" >"$test_root/tamper.out" 2>&1; then
  echo "installer accepted a checksum mismatch" >&2
  exit 1
fi
[[ ! -e "$tampered_dir/abbs" ]] || { echo "tampered binary was installed" >&2; exit 1; }
grep -q 'checksum verification failed' "$test_root/tamper.out"

if ABBS_VERSION=not-semver ABBS_INSTALL_DIR="$test_root/invalid" ABBS_DOWNLOAD_BASE="$download_base" \
  sh "$repo_dir/install.sh" >"$test_root/invalid.out" 2>&1; then
  echo "installer accepted an invalid version" >&2
  exit 1
fi
grep -q 'invalid release version' "$test_root/invalid.out"

echo "verified install.sh explicit/latest installs, version output, tamper rejection, and error paths"
