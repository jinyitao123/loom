# Contributing to Loom

Thanks for your interest. Loom is intentionally a **minimal** agent kernel — the
most valuable contributions keep it that way.

## Read first

`AGENTS.md` defines the rules that govern every change, human or AI:

- **Layered dependency**: kernel (root) → `contract/` → `stdlib/` →
  `provider/` / `pgstore/` → `cmd/loom`. Layer N imports only N-1 or below.
- **No new primitives**: everything is composed from the five existing ones
  (`State` · `Step` · `Router` · `Graph` · `Store`).
- **No business vocabulary** in the kernel — it stays a generic engine.

Changes that break the layering are the wrong change, however useful they look.

## Before you commit (must match CI)

```bash
go build ./...
go test ./...
go test -race ./cmd/loom/
gofmt -l .          # must print nothing
go vet ./...
golangci-lint run   # config in .golangci.yml
scripts/ci/api-surface.sh        # exported API must match baseline
scripts/ci/no-business-words.sh  # kernel vocabulary guard
```

- New code needs tests. Black-box tests go in `tests/`; white-box tests that
  need kernel internals stay at the root next to the kernel files.
- Changing the frozen kernel files (`graph.go`, `step.go`, `router.go`,
  `state.go`, `store.go`, `lifecycle.go`, `errors.go`) requires `[kernel-ok]`
  in the commit message after explicit review.
- Intentional exported-API changes: `scripts/ci/api-surface.sh --update` in the
  same commit.
- The CLI wire contract (`loom run --format stream-json`) is stable: add events
  additively, never repurpose existing ones, and update `docs/` in the same PR.

## What makes a good PR

One focused change, a clear rationale, and tests that prove it. If you're
planning something large (new stdlib capability, new provider), open an issue
first so we can check it fits the kernel's minimalism before you write code.
