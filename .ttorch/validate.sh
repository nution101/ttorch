#!/usr/bin/env sh
# ttorch trusted-mode validation gate — runs the repository's canonical checks.
# Kept on the default branch so a worker branch cannot weaken its own gate.
#
# This runs the FAST lane: `make test-fast` (`go test -short`) skips the slow
# internal/orchestrator integration (e2e) tests so the local gate finishes in seconds.
# The FULL suite, including those e2e tests, runs in CI (.github/workflows/ci.yml, which
# installs tmux so the integration tests actually execute). Note what that does and does
# not cover: ci.yml fires on push to main and on pull_request, so a worker branch with no
# open PR gets no CI run at all. Deferring the gate's own proofs to CI therefore deferred
# them to nothing, which is why test-gate runs here rather than being left to CI.
set -eu
make lint
make test-fast
# The fast lane skips every end-to-end gate attack: they all reach deliveryHarness, which
# calls skipIfShort. Run them explicitly, or the gate never executes the proofs that justify
# it. See the test-gate target for the full reasoning.
make test-gate
