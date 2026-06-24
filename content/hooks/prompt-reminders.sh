#!/bin/sh
# prompt-reminders.sh — a Claude Code UserPromptSubmit hook.
#
# It reads the submitted prompt from the hook's JSON payload on stdin and prints
# two kinds of advisory, supplementary context:
#
#   1. A caution note when the prompt mentions a potentially destructive command
#      pattern (recursive deletes, force pushes, dropping tables, and the like).
#   2. Generic, category-based engineering reminders — API, database, testing,
#      version control, security — when the prompt touches one of those areas.
#
# The hook is advisory only. It never blocks a prompt and always exits 0, so a
# problem here can never stop work. It is dependency-free: POSIX shell only, no
# jq/python/etc. Extraction and matching are best-effort by design.
#
# Opt out at runtime by setting TTORCH_NO_PROMPT_REMINDERS to any non-empty value.

# A non-empty opt-out switch disables the hook entirely.
[ -n "$TTORCH_NO_PROMPT_REMINDERS" ] && exit 0

# Read the whole payload. If stdin can't be read, there's nothing to do.
input=$(cat 2>/dev/null) || exit 0

# Pull the user prompt out of the JSON payload using only shell parameter
# expansion (no JSON parser dependency). If there's no recognizable prompt
# field, exit quietly. This is best-effort: a prompt containing an escaped quote
# may be inspected only up to that quote, which is fine for pattern detection.
case "$input" in
	*'"prompt"'*) ;;
	*) exit 0 ;;
esac
rest=${input#*\"prompt\"}   # drop everything up to and including the prompt key
rest=${rest#*:}             # drop up to the value's colon
rest=${rest#*\"}            # drop up to the value's opening quote

# Bound the remaining work before inspecting the value. The longest-suffix removal
# (%%) just below and the case scans further down are O(n^2) under some shells —
# notably bash 3.2, which is macOS /bin/sh — so a large pasted prompt (logs, stack
# traces) would otherwise stall every submission for seconds. 20000 chars is far
# more than advisory pattern matching needs, and cut is a standard POSIX tool.
rest=$(printf '%s' "$rest" | cut -c1-20000 2>/dev/null) || rest=""

prompt=${rest%%\"*}         # keep up to the next quote — the value (best-effort)

[ -n "$prompt" ] || exit 0

# Lowercase once for case-insensitive matching.
lc=$(printf '%s' "$prompt" | tr '[:upper:]' '[:lower:]' 2>/dev/null) || lc=$prompt

cautions=""
reminders=""

add_caution() {
	cautions="${cautions}  - ${1}
"
}

add_reminder() {
	reminders="${reminders}  - ${1}
"
}

# --- Potentially destructive command patterns (advisory caution only) --------

case "$lc" in
	*"rm -rf"* | *"rm -fr"* | *"rm -r -f"* | *"rm -f -r"*)
		add_caution 'Recursive force delete detected — confirm the target path before running.' ;;
esac
case "$lc" in
	*"force push"* | *"force-push"* | *"--force-with-lease"*)
		add_caution 'Force push detected — it can overwrite remote history; verify the branch first.' ;;
	*"push"*)
		# A push plus a force flag anywhere — catches "git push origin main --force"
		# where the flag is separated from the verb. Scoped to prompts mentioning a
		# push so it does not fire on, e.g., "npm install --force".
		case "$lc" in
			*"--force"* | *"git push -f"*)
				add_caution 'Force push detected — it can overwrite remote history; verify the branch first.' ;;
		esac ;;
esac
case "$lc" in
	*"git reset --hard"*)
		add_caution 'Hard reset detected — it discards uncommitted changes; confirm before running.' ;;
esac
case "$lc" in
	*"git clean -f"* | *"git clean -d"* | *"git clean -x"*)
		add_caution 'Forced git clean detected — it permanently removes untracked files; try -n first.' ;;
esac
case "$lc" in
	*"--no-verify"*)
		add_caution '--no-verify bypasses commit/push checks — make sure skipping them is intended.' ;;
esac
case "$lc" in
	*"drop table"* | *"drop database"* | *"drop schema"* | *"truncate table"*)
		add_caution 'Destructive schema statement detected — confirm the target database and a backup exist.' ;;
esac
case "$lc" in
	*"delete from"*)
		add_caution 'Bulk delete detected — confirm the WHERE clause so you do not remove more than intended.' ;;
esac
case "$lc" in
	*"chmod -r 777"* | *"chmod 777"* | *"chmod -r 0777"* | *"chmod 0777"*)
		add_caution 'World-writable permissions detected — prefer the least privilege that works.' ;;
esac
case "$lc" in
	*"mkfs"* | *"dd if="* | *"of=/dev/"* | *"> /dev/sd"* | *"sudo rm"*)
		add_caution 'Low-level or disk-destructive command detected — double-check the target device.' ;;
esac
case "$lc" in
	*"curl "* | *"wget "*)
		case "$lc" in
			*"| sh"* | *"| bash"* | *"|sh"* | *"|bash"*)
				add_caution 'Piping a downloaded script into a shell — review the script before executing it.' ;;
		esac ;;
esac

# --- Generic, category-based engineering reminders ---------------------------
# Short, ambiguous keywords (api, git, table, ...) are space-guarded to avoid
# matching unrelated words (e.g. "capital", "digit", "comfortable").

case "$lc" in
	"api"* | *" api"* | *"/api"* | *"apis"* | *"endpoint"* | *"graphql"* | *"webhook"* | *"rest api"*)
		add_reminder 'API: validate inputs, return clear errors, handle timeouts/retries, and keep auth consistent.' ;;
esac
case "$lc" in
	*"database"* | *"sql"* | *"migration"* | *"schema"* | *" query"* | *"queries"* | *" table"* | *"postgres"* | *"mysql"* | *"sqlite"* | *"mongodb"*)
		add_reminder 'Database: use parameterized queries, keep migrations reversible, and mind indexes and transactions.' ;;
esac
case "$lc" in
	"test"* | *" test"* | *"unit test"* | *"integration test"* | *"coverage"* | *"assert"*)
		add_reminder 'Testing: cover edge cases and failure paths, keep tests deterministic, and run the suite first.' ;;
esac
case "$lc" in
	"git "* | *" git "* | *" git"* | *"commit"* | *"rebase"* | *"cherry-pick"* | *"pull request"* | *" merge"* | *" branch"*)
		add_reminder 'Version control: keep commits focused with clear messages, and confirm the target branch.' ;;
esac
case "$lc" in
	*"password"* | *"secret"* | *"token"* | *"credential"* | *"api key"* | *"apikey"* | *"authenticat"* | *"authoriz"* | *"oauth"* | *"encrypt"* | *"decrypt"* | *"vulnerab"* | *"injection"* | *" xss"* | *"csrf"*)
		add_reminder 'Security: never hardcode secrets, validate and sanitize untrusted input, and apply least privilege.' ;;
esac

# --- Emit. On exit 0, stdout is added to the prompt context. -----------------

if [ -n "$cautions" ] || [ -n "$reminders" ]; then
	printf 'Automated pre-submit checks (advisory):\n'
	if [ -n "$cautions" ]; then
		printf 'Caution — review before running:\n%s' "$cautions"
	fi
	if [ -n "$reminders" ]; then
		printf 'Reminders:\n%s' "$reminders"
	fi
fi

exit 0
