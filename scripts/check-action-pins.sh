#!/bin/bash
set -euo pipefail

repo_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
uses_lines=$(grep -RHE '^[[:space:]]*(-[[:space:]]+)?uses:' "$repo_dir/.github/workflows" || true)

if [[ -z "$uses_lines" ]]; then
  echo "no GitHub Actions uses entries found" >&2
  exit 1
fi

failed=0
while IFS= read -r uses_line; do
  uses_ref=${uses_line#*uses:}
  uses_ref=${uses_ref#"${uses_ref%%[![:space:]]*}"}
  uses_ref=${uses_ref%%[[:space:]#]*}
  uses_ref=${uses_ref#\"}
  uses_ref=${uses_ref%\"}
  uses_ref=${uses_ref#\'}
  uses_ref=${uses_ref%\'}

  # Local actions and reusable workflows are immutable with the checkout.
  if [[ "$uses_ref" == ./* ]]; then
    continue
  fi
  if [[ ! "$uses_ref" =~ ^[^@[:space:]]+@[0-9a-f]{40}$ ]]; then
    echo "GitHub Action is not pinned to a full commit SHA: $uses_line" >&2
    failed=1
  fi
done <<<"$uses_lines"
exit "$failed"
