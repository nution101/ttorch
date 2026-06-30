#!/usr/bin/env sh
# ttorch trusted-mode validation gate — runs the repository's canonical checks.
# Kept on the default branch so a worker branch cannot weaken its own gate.
#
# This runs the FAST lane: `make test-fast` (`go test -short`) skips the slow
# internal/orchestrator integration (e2e) tests so the local gate finishes in seconds.
# It is NOT a weaker gate. The FULL suite — including those e2e tests — runs in CI
# (.github/workflows/ci.yml, which installs tmux so the integration tests actually
# execute) on every push and pull request as the required check on the default branch,
# so nothing lands without the full suite having passed there. The fast local lane is a
# turnaround optimization layered on top of — never a replacement for — full validation.
set -eu
make lint
make test-fast
