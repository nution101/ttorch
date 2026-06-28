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
   It writes `security.json` following the findings contract below.
3. `ttorch security-review record <id>` — folds `security.json` into a commit-pinned verdict
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
   It writes `qa.json` following the findings contract below.
3. `ttorch qa-review record <id>` — folds `qa.json` into a commit-pinned advisory verdict and
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
   reviews **only** its dimension, and writes `<dimension>.json` into the inputs dir
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
- A trusted **auto**-merge may not change the gate's own definition. If a worker's diff
  touches `.ttorch/validate.sh` or `AGENTS.md`, the auto-merge is refused and the change
  requires an explicit `ttorch approve` — altering the gate is always a human decision.

## Findings contract

Each reviewer writes exactly `<dimension>.json` in the inputs dir:

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
