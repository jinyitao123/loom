#!/usr/bin/env bash
# Compare the exported contract and stdlib API list with api-surface.txt.
# For an intentional API change, regenerate it with: api-surface.sh --update
# Use --baseline <path> to compare against a temporary baseline during testing.

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "$script_dir/../.." && pwd)"
baseline="$script_dir/api-surface.txt"
update=false

while (($#)); do
  case "$1" in
    --update)
      update=true
      shift
      ;;
    --baseline)
      if (($# < 2)); then
        echo "api-surface: --baseline requires a path" >&2
        exit 2
      fi
      baseline="$2"
      shift 2
      ;;
    *)
      echo "Usage: $0 [--update] [--baseline path]" >&2
      exit 2
      ;;
  esac
done

current="$(mktemp)"
trap 'rm -f "$current"' EXIT

cd "$repo_root"
{
  echo '# contract'
  go doc -short ./contract
  echo '# stdlib'
  go doc -short ./stdlib
} >"$current"

if [[ "$update" == true ]]; then
  cp "$current" "$baseline"
  echo "api-surface: updated ${baseline#$repo_root/}"
  exit 0
fi

if [[ ! -f "$baseline" ]]; then
  echo "api-surface: baseline not found: $baseline" >&2
  echo "Run $0 --update for an intentional baseline creation." >&2
  exit 1
fi

if ! diff -u "$baseline" "$current"; then
  echo "api-surface: exported API differs from baseline" >&2
  echo "For an intentional API change, run $0 --update." >&2
  exit 1
fi

echo "api-surface: OK (exported API matches baseline)"
