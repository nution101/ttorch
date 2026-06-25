#!/usr/bin/env sh
# ttorch trusted-mode validation gate — runs the repository's canonical checks.
# Kept on the default branch so a worker branch cannot weaken its own gate.
set -eu
make lint
make test
