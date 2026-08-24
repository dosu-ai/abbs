#!/bin/bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 DIST_DIR [--signed]" >&2
  exit 2
fi

dist_dir="$(cd "$1" && pwd)"
repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
require_signature="${2:-}"
shopt -s nullglob

archives=("$dist_dir"/abbs_*.tar.gz "$dist_dir"/abbs_*.zip)
if [[ ${#archives[@]} -ne 6 ]]; then
  echo "expected exactly six release archives, found ${#archives[@]}" >&2
  printf '  %s\n' "${archives[@]}" >&2
  exit 1
fi

for platform in darwin linux windows; do
  for architecture in amd64 arm64; do
    extension=tar.gz
    [[ "$platform" == windows ]] && extension=zip
    matches=("$dist_dir"/abbs_*_"$platform"_"$architecture"."$extension")
    if [[ ${#matches[@]} -ne 1 ]]; then
      echo "expected one ${platform}/${architecture} ${extension} archive, found ${#matches[@]}" >&2
      exit 1
    fi
  done
done

(
  cd "$dist_dir"
  if command -v sha256sum >/dev/null 2>&1; then
    awk '$2 != "install.sh" && $2 != "install.ps1"' checksums.txt | sha256sum --check -
  else
    awk '$2 != "install.sh" && $2 != "install.ps1"' checksums.txt | shasum -a 256 --check -
  fi
)

for installer in install.sh install.ps1; do
  expected_hash=$(awk -v name="$installer" '$2 == name { print $1 }' "$dist_dir/checksums.txt")
  expected_count=$(awk -v name="$installer" '$2 == name { count++ } END { print count + 0 }' "$dist_dir/checksums.txt")
  if [[ "$expected_count" -ne 1 || ! "$expected_hash" =~ ^[0-9a-f]{64}$ ]]; then
    echo "expected exactly one SHA-256 checksum for $installer" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    actual_hash=$(sha256sum "$repo_dir/$installer")
  else
    actual_hash=$(shasum -a 256 "$repo_dir/$installer")
  fi
  actual_hash=${actual_hash%%[[:space:]]*}
  if [[ "$actual_hash" != "$expected_hash" ]]; then
    echo "$installer does not match checksums.txt" >&2
    exit 1
  fi
done

for archive in "${archives[@]}"; do
  if [[ "$archive" == *.zip ]]; then
    listing=$(unzip -Z1 "$archive")
    binary=abbs.exe
  else
    listing=$(tar -tzf "$archive")
    binary=abbs
  fi
  for required_file in "$binary" README.md LICENSE; do
    if ! grep -Eq "(^|/)${required_file}$" <<<"$listing"; then
      echo "$(basename "$archive") is missing $required_file" >&2
      exit 1
    fi
  done
done

sboms=("$dist_dir"/abbs_*.sbom.json)
if [[ ${#sboms[@]} -ne 6 ]]; then
  echo "expected one SBOM per archive, found ${#sboms[@]}" >&2
  exit 1
fi

if [[ "$require_signature" == "--signed" && ! -s "$dist_dir/checksums.txt.sigstore.json" ]]; then
  echo "missing keyless Cosign bundle for checksums.txt" >&2
  exit 1
fi

echo "verified six archives, checksums, installers, archive contents, and per-archive SBOMs"
