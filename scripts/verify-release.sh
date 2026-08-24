#!/bin/bash
set -euo pipefail

if [[ $# -lt 1 || $# -gt 2 ]]; then
  echo "usage: $0 DIST_DIR [--signed]" >&2
  exit 2
fi

dist_dir="$(cd "$1" && pwd)"
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
    sha256sum --check checksums.txt
  else
    shasum -a 256 --check checksums.txt
  fi
)

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

echo "verified six archives, checksums, archive contents, and per-archive SBOMs"
