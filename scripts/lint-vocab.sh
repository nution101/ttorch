#!/bin/sh
# Fail the build if banned (pirate / role-play) vocabulary appears in source,
# content, or docs. Keeps the project professional. Extend PATTERN as needed.
set -eu

PATTERN='captain|first[ -]?mate|firstmate|crewmate|crewmates|matey|ahoy|shipshape|yarr|nautical'

if command -v git >/dev/null 2>&1 && git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  FILES=$(git ls-files --cached --others --exclude-standard | grep -Ev '^(scripts/lint-vocab\.sh|THIRD_PARTY\.md)$' || true)
else
  FILES=$(find . -type f \( -name '*.go' -o -name '*.md' -o -name '*.sh' \) \
    -not -path './.git/*' -not -path './dist/*' -not -name 'lint-vocab.sh' || true)
fi

[ -z "$FILES" ] && { echo "vocab lint: nothing to check"; exit 0; }

HITS=$(printf '%s\n' "$FILES" | xargs grep -inE "$PATTERN" 2>/dev/null || true)
if [ -n "$HITS" ]; then
  echo "Banned vocabulary found (keep it professional — no pirate terms):"
  echo "$HITS"
  exit 1
fi
echo "vocab lint: clean"
