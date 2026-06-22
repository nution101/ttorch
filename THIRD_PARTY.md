# Third-party

## Runtime dependencies

- `github.com/fsnotify/fsnotify` (BSD-3-Clause) — filesystem notifications; the
  supervisor watches the state directory for instant signal detection.
- `golang.org/x/sys` (BSD-3-Clause) — transitive (fsnotify).

## Design credit

orcha's design draws on these MIT-licensed projects. Where their approach is
reimplemented (not copied), they are credited here:

- **treehouse** (MIT) — the reusable worktree-pool model in `internal/worktree`
  (reuse idle clean slots, reset tracked files while keeping untracked caches,
  reap pane process groups on return).
- **no-mistakes** (MIT) — the self-managing single-binary substrate: self-update
  with atomic replace + macOS quarantine handling (`internal/selfupdate`), global
  skill install, and the long-lived supervisor model (`internal/supervisor`).

If upstream source is ever copied verbatim, record it here with the source repo,
commit/version, files taken, and original MIT copyright, and keep the upstream
notice intact in the vendored files. Do not let upstream project names leak into
orcha's package names, environment variables, on-disk state, or user-facing text.
