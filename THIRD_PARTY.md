# Third-party code

orcha's design reuses logic from these MIT-licensed upstream projects. Later milestones
vendor portions of their Go packages into `internal/` with attribution; as of M0, no
third-party source has been vendored yet.

When code is vendored, record it here with the source repository, the commit/version, the
files taken, and the original MIT copyright. Keep upstream copyright notices intact in the
vendored files. Do not let upstream project names or identifiers leak into orcha's package
names, environment variables, on-disk state, or user-facing text.

Planned reuse:

- **treehouse** (MIT) — worktree pool, process termination, git helpers.
- **no-mistakes** (MIT) — self-update engine, global skill install, daemon + IPC,
  harness adapters.
