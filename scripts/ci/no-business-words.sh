#!/usr/bin/env bash
# Reject business-domain words in graph.go, step.go, router.go, state.go,
# store.go, lifecycle.go, and errors.go. Pass an alternate file list to
# exercise the guard locally.
# This WORD_PATTERN is the blacklist's single source of truth; AGENTS.md points
# here instead of maintaining an independent list.

set -euo pipefail

WORD_PATTERN='avatar|team|transfer|delegate|weave|tenant|审批|团队|分身|员工'
default_files=(graph.go step.go router.go state.go store.go lifecycle.go errors.go)
if (($#)); then
  files=("$@")
else
  files=("${default_files[@]}")
fi

if grep -Eni -- "$WORD_PATTERN" "${files[@]}"; then
  echo "no-business-words: business-domain word found in kernel scope" >&2
  exit 1
fi

echo "no-business-words: OK (no blacklisted words found)"
