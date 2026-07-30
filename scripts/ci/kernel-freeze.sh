#!/usr/bin/env bash
# Guard the frozen kernel files: graph.go, step.go, router.go, state.go,
# store.go, lifecycle.go, errors.go.
# Usage: scripts/ci/kernel-freeze.sh [base-ref] (default: HEAD~1).
# Kernel changes in <base-ref>...HEAD or the staged index require [kernel-ok]
# in the most recent commit message.

set -euo pipefail

base_ref="${1:-HEAD~1}"
kernel_pattern='^(graph|step|router|state|store|lifecycle|errors)\.go$'

changed_files="$({ git diff --name-only "$base_ref"...HEAD; git diff --cached --name-only; } | sort -u)"
kernel_changes="$(printf '%s\n' "$changed_files" | grep -E "$kernel_pattern" || true)"

if [[ -z "$kernel_changes" ]]; then
  echo "kernel-freeze: OK (no frozen kernel files changed)"
  exit 0
fi

if git log -1 --format=%B | grep -Fq '[kernel-ok]'; then
  echo "kernel-freeze: OK ([kernel-ok] authorizes frozen kernel changes)"
  exit 0
fi

echo "kernel-freeze: frozen kernel files changed without [kernel-ok] in the latest commit message:" >&2
printf '  %s\n' $kernel_changes >&2
echo "Add [kernel-ok] to the authorizing commit message after explicit kernel review." >&2
exit 1
