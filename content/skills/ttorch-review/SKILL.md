---
name: ttorch-review
description: >
  Run the adversarial-review trust gate on a worker's diff: prepare the review inputs,
  fan out independent reviewer subagents (correctness, scope, security — scaled to the
  change size), record a commit-pinned verdict, and — in trusted delivery mode — merge
  without a separate lead approval. Use when gating a task for a trusted-mode or
  --require-verdict merge. Also runs the standalone, advisory security audit that applies
  in EVERY delivery mode.
metadata:
  managed-by: ttorch
user-invocable: true
---

# Ttorch Review

The adversarial-review **trust gate**: let a repository merge a worker's output gated on
parallel AI review (correctness / scope-vs-brief / security-compliance) **plus** the
repo's own build/test/lint — enforced in Go at the merge point, not by convention.

> **When to run.** This full three-dimension **gate** (correctness + scope + security, with
> the hard block) runs after a worker is **green** (`ttorch validate <id>` passes) and the
> repo is in **trusted** delivery mode, or you want to gate one merge in another mode with
> `--require-verdict`. In any non-trusted mode the lead still approves the merge — this gate
> does not replace that. Default to proposing, not delivering. (The advisory **security
> audit** above runs in *every* mode by default and is separate from this gate.)

> **Trust boundary.** In trusted mode the gate authorizes a merge **with no human reading
> the diff**: the boundary moves onto these reviewers and their prompts. Treat it as
> defense in depth, not an unbreakable barrier — opting a repo into trusted mode is the
> lead's decision, recorded per-repo via `ttorch init --mode trusted`.

## Security audit in every mode

The full three-dimension gate above is the **trusted-mode** path. Independently of it, a
**security audit runs in every delivery mode** (`pr` / `local` / `validated` / `trusted`) —
run it **by default** before proposing or delivering any worker. It reuses the same
`ttorch-reviewer-security` agent and the same commit-pinned report mechanism, folding **only**
the security dimension:

1. `ttorch security-review prep <id>` — materializes the same inputs dir as `trust prep`
   (committed `diff.patch`, `brief.md`, `validate.json`, `head.txt`); refuses a dirty worktree.
2. Dispatch the **`ttorch-reviewer-security`** agent over that dir + the commit in `head.txt`.
   It writes `reports/security.json` following the findings contract below.
3. `ttorch security-review record <id>` — folds `reports/security.json` into a commit-pinned verdict
   and prints it. `ttorch security-review show <id>` reprints the latest.

This pass is **advisory and never blocks delivery**: it never mints an approval, never writes
the trust gate's verdict, and never gates a merge. It mirrors how a recorded verdict is
advisory in every non-trusted mode — the lead's `ttorch approve` still governs those merges.
**Surface its findings to the lead;** a `high`/`critical` (or "no review recorded", which
fails closed) is a reason to pause and decide, not an automatic block. The trusted-mode gate
above is **unchanged** — it still hard-blocks on the same findings; this only ADDS a security
pass to the other modes. `ttorch land` prints a non-blocking reminder when no fresh security
audit covers the commit it lands in a non-gated mode.

> **Opt-out.** The security audit is on by default. Skip it for a given task only when the
> lead explicitly says so (e.g. a trivial docs-only change, or the lead is reading the diff
> themselves) — note that you skipped it when you report.

## Test-adequacy (QA) audit (optional)

Independently of the gate and the security audit, an **optional test-adequacy audit** judges
whether a worker's tests are adequate — edge- and failure-path coverage, determinism (no flaky
reliance on time, randomness, network, or order), no vacuous assertions, and adherence to the
repo's testing conventions. It exists to push generated code toward **passing CI on the first
try**. Run it when you want that check — for example before a trusted auto-merge, or on any
change whose tests look thin or flaky. It reuses the same commit-pinned report mechanism as the
gate, folding **only** the QA dimension:

1. `ttorch qa-review prep <id>` — materializes the same inputs dir as `trust prep`
   (committed `diff.patch`, `brief.md`, `validate.json`, `head.txt`); refuses a dirty worktree.
2. Dispatch the **`ttorch-reviewer-qa`** agent over that dir + the commit in `head.txt`.
   It writes `reports/qa.json` following the findings contract below.
3. `ttorch qa-review record <id>` — folds `reports/qa.json` into a commit-pinned advisory verdict and
   prints it. `ttorch qa-review show <id>` reprints the latest.

This pass is **advisory and never blocks delivery**: it never mints an approval, never writes
the trust gate's verdict, and never gates a merge — exactly like the security audit, and
distinct from the trusted-mode three-dimension gate (correctness / scope / security), which is
**unchanged**. **Surface its findings to the lead;** a `high`/`critical` test-adequacy gap (or
"no review recorded", which fails closed) is a reason to ask the worker for stronger tests, not
an automatic block.

## Protocol

1. **Prepare inputs.** `ttorch trust prep <id>` materializes, into the task's review
   inputs dir (the path it prints): `diff.patch` (the **committed** diff vs the default
   branch), `brief.md` (the task brief, if any), `validate.json` (a fresh validate of the
   committed commit), and `head.txt` (the reviewed commit). It **refuses a dirty
   worktree** and reads only committed objects, so the reviewers see exactly the commit
   that will fast-forward — a worker cannot show a benign working tree while a different
   commit merges. Commit (or discard) all changes before prep.

2. **Fan out the reviewers for this diff — in parallel, one per dimension.** The gate
   **scales the reviewer set to the change size**, so spawn exactly the dimensions `trust
   prep` named (it prints them, and records them in `reviewers.json` in the inputs dir):

   - **Substantial / code change → all three:** `ttorch-reviewer-correctness`,
     `ttorch-reviewer-scope`, and `ttorch-reviewer-security`. This is also the default
     whenever the diff is anything but cleanly docs-only or trivial.
   - **Docs-only change** (every changed file is inert prose — `.md`, `.txt`, a
     LICENSE-style file) **→ correctness + scope.** Prose has no executable surface, so the
     security reviewer is dropped — for documentation only, never for code.
   - **Trivial change** (one small code file, within the gate's line budget) **→
     correctness + security.** A tiny single-file change has little room for scope creep, so
     scope is dropped — but it is still code, so **security is kept**.

   Give each the inputs dir path and the commit from `head.txt`. Each reads the inputs,
   reviews **only** its dimension, and writes `reports/<dimension>.json` into the inputs dir
   following the findings contract below. Reviewers never edit code, and they **trust the
   green `validate.json`** that prep staged rather than re-running the suite themselves —
   review is a static read of the diff (a green `validate.json` already proves the repo's
   build/lint and full test suite pass at the pinned commit), with at most one targeted check, only
   to probe a specific gap a reviewer names.

   > **Why scale, and why it stays safe.** The full pass is wasted on a one-line README
   > fix. Reducing the set keeps the gate fast on low-risk diffs while never under-reviewing
   > a real one: **security review is dropped only for a diff with no code at all**, and
   > anything the gate cannot cleanly classify as docs-only or trivial falls back to the
   > full three-dimension pass. `trust record` then aggregates against exactly the prepared
   > set, and a missing record fails safe to all three.

3. **Record the verdict.** `ttorch trust record <id>` aggregates the prepared reports into
   a single commit-pinned, time-boxed verdict (Go owns the verdict body, so a missing or
   malformed report for any required dimension **fails closed** to `block`). In **trusted** mode a `pass` verdict
   over a still-green worktree auto-mints the approval token; every other mode leaves the
   verdict advisory.

4. **Merge.**
   - **Trusted mode:** `ttorch merge-local <id>` — the gate applies automatically and
     re-checks the verdict + a fresh validate before fast-forwarding. No `ttorch approve`.
   - **Any other mode:** the lead runs `ttorch approve <id>`, then
     `ttorch merge-local <id> --require-verdict` opts that one merge into the same gate.

The merge **re-checks everything**, commit-pinned: a clean worktree (so the reviewed
state is exactly the committed HEAD that merges), a passing unexpired verdict, a fresh
green validate, and `verdict.ReviewedSHA == worker HEAD`. Any commit landing after review
invalidates the verdict — re-prep, re-review, re-record.

**The gate is worker-proof by design:**

- The fresh validate runs the validation definition from the **default branch**, against
  an immutable checkout of the committed sha — never the worker's own copy — so a worker
  cannot weaken its own gate by editing the script on its branch. A trusted **auto**-merge
  **requires** a `.ttorch/validate.sh` on the default branch: without it the gate would
  fall back to ecosystem detection (`go.mod`/`package.json`) on the worker's checkout,
  which the worker controls, so the auto path is refused and a human `ttorch approve` is
  required instead. A repo with no detectable checks fails closed (a hard block).
- **No GATED merge changes the gate's own definition unless the approval says so by name.**
  The covered set is:

  | Covered | Why it is the gate |
  |---|---|
  | `.ttorch/**` | `validate.sh` is what "green" means, and `learnings.jsonl` is rendered verbatim into `AGENTS.md` by `ttorch learn` at every delivery |
  | `AGENTS.md` | whether the gate runs at all (`projectinit.ReadMode`) |
  | `CLAUDE.md` | a **symlink** to `AGENTS.md`; replacing it with a real file reports only `CLAUDE.md`, and it is the instruction file every session loads, manager included |
  | `AGENTS.md` / `CLAUDE.md` **at any depth** | a nested one loads on demand for its directory, so it is an instruction file too |
  | `.claude/**`, `.mcp.json` | project-level agent config: a landed `.claude/agents/ttorch-reviewer-security.md` **replaces** the security reviewer, with no build and no install |
  | `go.mod`, `go.sum` | a `replace` redirects what `make test-fast` compiles — including `golang.org/x/text`, which the guard's own path folding now depends on |
  | `docs/install.sh`, `docs/install.ps1` | README tells users to pipe these into a shell; a merge changes those bytes with no build and no release step |
  | `content/**` | everything `content.go` embeds and `installer.desiredFiles` lays down under `~/.claude`: the skills (**this file included**), all the agent definitions, `commands/ttorch.md` (the `/ttorch` entry point), `assets/AGENTS.global.md` (merged into the global `AGENTS.md` every session reads) and `hooks/prompt-reminders.sh` (**runs on every prompt**) |
  | `internal/review/**` | the findings contract, the severity-to-block rule, and the classifier that picks which reviewers run |
  | `internal/approval/**` | the approval token the `--allow-gate-change` scope rides on |
  | `internal/validate/**` | what counts as a passing check |
  | `internal/projectinit/**` | parses `AGENTS.md` into the delivery mode and the auto-mint staleness bound |
  | `internal/orchestrator/{gate,merge,validate,validatecache}.go` | the Go code that resolves, enforces and caches the decision |
  | *(measured cost of each entry)* | see the generated table in `docs/ARCHITECTURE.md` (gate-cost block); this file does not restate it, because a second copy here is what let the figures diverge |
  | `.github/workflows/**` | the full suite: `.ttorch/validate.sh` runs lint, the fast lane and the gate's proofs (`make test-gate`), and defers the rest to CI by name. The gate runs the default branch's copy of that script, never the branch under review's, so a change to it takes effect only after it lands |
  | `Makefile` | `.ttorch/validate.sh` does nothing but run `make lint`, `make test-fast` and `make test-gate` |
  | `go.work`, `go.work.sum` | auto-discovered via `GOWORK`; `replace` directives there override `go.mod`, so a committed one redirects what `go test` compiles |
  | `vendor/**` | a consistent `vendor/` makes the toolchain build from it instead of the module cache |
  | `content.go`, `internal/installer/**` | decide which embedded file becomes which installed file. `content.go` is a separate exact entry — the `content/` prefix does **not** match it, since the two share no prefix relationship |
  | `internal/orchestrator/audit.go` | the merge record a trusted merge refuses to proceed without |
  | `internal/skills/**` | `Recommended()` → `npx skills add <ref>` → `~/.claude/skills`, run before every team launch and every worker spawn: third-party code into the directory `content/skills/` is covered to protect, with no ttorch build in between |

  `content/` is the whole tree, not a list of subtrees. `desiredFiles` walks it rather than
  naming files, so an enumerated subset kept missing installed ones — the last version covered
  7 of the 42 embedded files. A landed edit to any of them changes what the gate does on the
  next run for every repo on the machine. The Go entries take effect one step later — they change the next
  binary, after a build and an install — which is a real difference but a thin one, since the
  maintainer self-updates routinely.

  If a worker's diff touches any of these, an auto-merge is refused outright, and a plain
  `ttorch approve` is refused too — the lead must run
  `ttorch approve <id> --allow-gate-change`, and the merge's audit line then names the file
  that changed.

  **Matching on the name alone is not enough, so the guard does not rely on it.** Paths are
  compared under `fsIdentityKey` — NFD, Unicode FULL case folding, then SimpleFold
  orbit-minimum — which is the relation APFS implements, verified by creating the files and
  reading them back. Three weaker versions were exploitable: `strings.ToLower` missed U+017F
  (`agentſ.md` → `AGENTS.md`), `unicode.SimpleFold` missed the multi-rune full folds
  (`Makeﬁle` → `Makefile`, the Makefile `.ttorch/validate.sh` executes), and NFD+fold+NFD
  missed the 172 Cherokee runes where `cases.Fold` swaps the two cases instead of picking one.

  Beyond the name, the guard **refuses outright** any newly introduced tree entry that
  collides with a different entry in the same tree, and any changed path containing a control
  character. Entry means blob **or directory**: a blob named `.github/workflowſ` deletes the
  whole `.github/workflows/` directory from the checkout, and because that breaks no build
  the validate would otherwise go green against a checkout that is not the tree. Both
  refusals are blocking — `--allow-gate-change` does not clear them, because a diff with two
  entries resolving to one path has no single well-defined checkout and there is nothing for
  an approval to be an approval of. Only collisions the diff *introduces* are reported, so a
  repo that already contains a colliding pair can still land the rename that fixes it.

  A third refusal is blocking for the same reason: **a newly introduced symlink or gitlink
  standing at, or above, covered ground.** `.claude -> docs/payload` committed alongside
  `docs/payload/agents/ttorch-reviewer-security.md` replaces the project-level security
  reviewer in a fresh clone while reporting two paths that a reader cannot connect, and
  repointing the link later moves the bytes again with no diff at `.claude` at all. The
  collision check cannot backstop it: a link is one entry, not a colliding pair. Approving
  it would mean approving contents the diff never shows, so there is no flag for it. Note
  the two bounds. *Newly introduced* only, because this repo's own `CLAUDE.md` is a
  committed symlink and repointing an existing link stays flaggable through the name match.
  *At or above covered ground* only, so a link shadowing nothing covered still merges; any
  directory symlink can introduce files git never lists by path, and refusing all of them
  would fire on ordinary layout.

- **The covered set is NOT the same in every repo.** Part of it is ttorch's own source, and
  in your repo those paths mean something else. `content/` is the clearest: here it is
  ttorch's embedded payload, but it is also where Hugo, Next and most CMS layouts keep
  articles, so gating on it would demand `--allow-gate-change` for every ordinary change.
  `internal/review/` and `internal/validate/` are ordinary Go package names.

  `resolveGateScope` decides from the repository, not its name: a Go `//go:embed` rooted at
  `content`, read from the BASE commit's raw blob so a worker's branch cannot change the
  answer, and so no `.gitattributes` can change what git reports about it. It fails closed,
  treating an unreadable repo as ttorch's own source.

  ```gate-ttorch-source-only
  content/
  content.go
  docs/install.sh
  docs/install.ps1
  internal/review/
  internal/approval/
  internal/validate/
  internal/projectinit/
  internal/installer/
  internal/skills/
  internal/worktree/
  internal/orchestrator/gate.go
  internal/orchestrator/merge.go
  internal/orchestrator/validate.go
  internal/orchestrator/validatecache.go
  internal/orchestrator/audit.go
  internal/orchestrator/gateattacks_test.go
  internal/orchestrator/gateconfig_test.go
  ```

  Everything else in the covered table above is universal and applies wherever ttorch gates:
  `.ttorch/**`, `AGENTS.md`, `CLAUDE.md` and both by basename, `.claude/**`, `.mcp.json`,
  `.gitattributes`, `Makefile`, the `go.*` files, `vendor/**` and `.github/workflows/**`.
  `TestNotCoveredListIsHonest` reconciles both fences against the live matcher in both
  scopes, so this split cannot drift from the code.

- **Paths this gate does NOT cover**, as a machine-readable list so the claim can be checked
  rather than trusted. `TestNotCoveredListIsHonest` reconciles every entry below against the
  live matcher and fails if any of them is in fact covered, so this block cannot quietly
  describe a state the code left behind.

  ```gate-not-covered
  cmd/ttorch/
  internal/cli/
  internal/db/
  internal/scheduler/
  internal/harness/
  internal/selfupdate/
  internal/manifest/
  internal/orchestrator/spawn.go
  internal/orchestrator/landqueue.go
  internal/orchestrator/autostart.go
  internal/orchestrator/overlap.go
  internal/orchestrator/orchestrator.go
  ```

  `internal/cli/` and `internal/db/` are left out on cost. What covering any entry above would
  cost is in the generated table in `docs/ARCHITECTURE.md` (gate-cost block), and this file
  does not restate or rank it. `TestGateCostFiguresMatchTheDoc` checks that the block matches
  the matcher. Its check for hand-written figures elsewhere is partial: it catches a number
  written as a fraction of the corpus or a percentage, and "N commits" written on one line,
  and misses a commit count split from its noun across lines, one with words between the
  number and its noun, one spelled out, and a comparison with no number. Treat a cost claim
  you find anywhere else as unverified. `internal/worktree/` used to be in this list and is
  now COVERED: it holds `ChangedFiles`, the only input to the guard, plus the raw blob read
  the scope resolves from, so an incomplete list or a desynced read there makes the guard
  match nothing at all.

- **What that claim does NOT cover**, stated so nobody reads it as wider than it is:
  - Gated means trusted mode or `--require-verdict`. A `local`/`validated` merge without
    `--require-verdict` does not run this check at all, so it still merges a gate-definition
    change on a plain approval with nothing in the audit naming it — including an `AGENTS.md`
    change, which is what sets the delivery mode, so that path can flip a repo into trusted
    (auto-merge) unaudited.
  - `~/.claude/agents/ttorch-reviewer-*.md` and `~/.claude/skills/` as they exist ON DISK. The
    guard sees a diff; these files live outside any repo and anything running as the lead can
    overwrite them directly. No diff-channel guard can ever see that. Covering the repo copies
    under `content/` narrows the channel; it does not close it. This bullet used to be the
    whole story for `~/.claude/skills/`, which was an understatement: `internal/skills/` is a
    diff-channel route into that directory and is now covered.
  - **The rest of `internal/orchestrator/`** — `spawn.go`, `landqueue.go`, `autostart.go`,
    `overlap.go` and the others. A deliberate, measured exclusion; the last row of the
    generated table in `docs/ARCHITECTURE.md` is what it rests on. A flag that fires on most
    commits stops being a signal.
    `TestGateConfigCoversTheDecidingCode` fails if a deciding function moves out of those
    five, so the narrower list cannot decay into false coverage unnoticed.
  - `internal/cli/` wires the `--allow-gate-change` flag but is not covered, and
    `internal/db/` holds the verdict row the merge trusts for `Overall == pass`. Both are cost
    judgements rather than oversights; the figures are in the generated table in
    `docs/ARCHITECTURE.md`. `internal/cli/` is the one that matters, because it also used to
    choose the tree the installer walked.
  - Filesystems whose folding rules differ from Unicode's. `fsIdentityKey` is measured
    against **APFS only**, by `TestFSIdentityKeySweep` creating both files and reading one
    back, which beats reading the Unicode tables but says nothing about any other
    filesystem. An earlier version of this line claimed NTFS too; that was never measured,
    and Win32 path normalization strips a trailing dot and a trailing space, so `AGENTS.md.`
    would open the covered file on Windows while folding to a distinct key here. That last
    point is documented Win32 behaviour, not something measured: `AGENTS.md` and
    `AGENTS.md.` were confirmed to be two separate files on APFS, which is the half that
    can be checked from here. Anyone gating a repo on Windows should treat the name
    match as unproven; the collision check and the control-character refusal do not depend
    on the fold.
  - Substitutions that never produce two entries in one tree. The collision check detects two
    entries resolving to one path; that is a narrower claim than "any substitution is
    observable", which this document wrongly made for two rounds. Replacing the `CLAUDE.md`
    symlink with a real file is one such case — same path, new blob and mode, no pair to
    collide — so the covered-set entry is the only thing standing there, not a second line.
  - A symlink is matched by its OWN path, never by what it resolves to. `CLAUDE.md` is the
    only one in this repo and it is covered; `TestTreeHasNoUncoveredSymlinks` fails if another
    appears outside the set, because each would be the same trick.
  - Everything here is inside the `gated` branch of the merge, so a `local`/`validated` merge
    without `--require-verdict` gets none of it — not the name match, not the collision
    refusal, not the control-character refusal. Same pre-existing hole as the first bullet.
  - **Coordination note for step 6:** `go.mod` and `go.sum` ARE now covered by this branch.
    Step 6's not-covered list still names them; that line should go when the two land
    together, the same way the fold is coordinated.
  - Git will not tell you. `git clone` warns about a collision; `git worktree add --detach` —
    what the gate uses to build the checkout it validates — exits 0 with nothing on stderr and
    silently drops the losing entry. The gate's own collision check is load-bearing, not a
    second opinion.
  - Untrusted text on the lead's terminal is escaped, not sanitized away. `worktree.warnf`
    escapes control bytes before printing git's stderr, so an ANSI sequence in a committed
    filename or `.gitattributes` is shown rather than executed; the text itself still reaches
    the screen and can still say whatever the worker wants it to say.
  - The anchor tests ask a narrow question. `TestGateConfigCoversTheDecidingCode` catches a
    listed deciding function MOVING into an uncovered file, and
    `TestOrchestratorFilesAreClassified` catches a NEW file nobody classified. Neither notices
    a new deciding function added inside a file already judged non-deciding. That stays a
    review responsibility.
  - The covered set answers TWO questions, not one: what decides how a change is reviewed or
    validated, and what a merge publishes directly to users (the two installers, and only
    those). Anything outside both is not covered however alarming it looks.
  - The flag is a boolean, so the cheapest way to defeat the guard is habit. A lead who
    passes the flag without reading has given exactly the same authorization as one who
    read. What survives that is the audit line, which names the file either way. Making the
    flag take the expected paths, so a bare `--allow-gate-change` stops working, is the
    obvious next step and is not done here.
  - **The input set is the part that keeps being wrong.** Five separate bypasses here were
    defects in the list of paths handed to the matcher, not in the matcher: no case folding,
    then lowercasing instead of folding, then single-rune instead of full folding, then blobs
    without the directories they imply, then renames reporting only their destination. If you
    are auditing this guard, check what reaches it before checking what it does with what
    reaches it — is every path present, is it spelled the way the matcher matches, and is it
    the committed tree rather than the working one.
  - None of it is a barrier. Anything running as the lead can write the approval token with
    the `allow-gate-change` scope already in it. What the guard removes is the silent skip.

## Findings contract

Each reviewer writes exactly `reports/<dimension>.json` — the `reports/` subdirectory of the
inputs dir, which prep creates. Reports are named by dimension and the control files are not,
so they live in separate namespaces and a dimension can never name a control file:

```json
{
  "dimension": "correctness",
  "reviewedSha": "<the full sha from head.txt, verbatim>",
  "findings": [
    {
      "dimension": "correctness",
      "severity": "high",
      "reviewer": "ttorch-reviewer-correctness",
      "summary": "one-line, specific, file:line where possible"
    }
  ]
}
```

- A clean review is `"findings": []`.
- Severity is one of `low | medium | high | critical`. **Any `high` or `critical` blocks
  the merge;** `low`/`medium` are advisory. An unknown or empty severity is treated as
  blocking — always use one of the four.
- `reviewedSha` MUST equal the commit in `head.txt`. A report pinned to any other commit
  is rejected as stale.

## Fail closed

- If the diff is too large to review fully, **do not shallow-pass.** Scope it with
  `ttorch review-diff <id> --stat`, review what you can, and raise a `high` finding so the
  gate blocks pending a smaller change or a human read.
- A reviewer that is unsure whether something is a real problem records it (security
  biases to `high` on uncertainty). The gate is meant to stop bad merges, not wave them
  through.
- Never edit the diff to "fix" a finding — report it; the worker fixes and re-review runs.
