---
name: ttorch-validate
description: >
  Run a worker's changes through the repository's own checks (build, vet/lint, tests)
  before delivery, and act on the results. Use when validating or gating a task's work,
  or when the lead asks to verify a worker is green before merging or opening a PR.
metadata:
  managed-by: ttorch
user-invocable: true
---

# Ttorch Validate

Gate a task's work on the repository's own checks before it is delivered.

> **Trust:** `ttorch validate` runs the repository's own build/test/lint commands —
> including any `.ttorch/validate.sh` — on your machine with your credentials. Only run
> it against repositories and worker output you trust. Each check runs under a timeout
> (`TTORCH_VALIDATE_TIMEOUT`, default 10m).

## How to run

`ttorch validate <task-id>` runs the detected checks in the worker's worktree and prints
`PASS`/`FAIL` per check (with output for failures). Checks are auto-detected:

- Go repos: `go build`, `go vet`, `gofmt`, `go test`.
- Node repos: the `build`/`lint`/`test` scripts present in `package.json`.
- Any repo can override detection with a `.ttorch/validate.sh` script (run via `sh`).

## What to do with the result

1. **All green** → report the task as ready for the lead's review/approval. Validation
   does **not** merge or deliver; integration still goes through the lead's approval
   (`ttorch approve` → `ttorch merge-local`, or a PR).
2. **Red** → read the failing output. Have the worker fix the issue (or fix small,
   mechanical problems like formatting yourself), then re-run `ttorch validate <task-id>`
   until it is green. Do not deliver while any check is failing.
3. **No checks detected** → tell the lead; suggest adding `.ttorch/validate.sh` to the
   repo so future tasks can be gated.
