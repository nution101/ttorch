---
name: ttorch-review
description: >
  Run the adversarial-review trust gate on a worker's diff: prepare the review inputs,
  fan out three independent reviewer subagents (correctness, scope, security), record a
  commit-pinned verdict, and — in trusted delivery mode — merge without a separate lead
  approval. Use when gating a task for a trusted-mode or --require-verdict merge.
metadata:
  managed-by: ttorch
user-invocable: true
---

# Ttorch Review

The adversarial-review **trust gate**: let a repository merge a worker's output gated on
parallel AI review (correctness / scope-vs-brief / security-compliance) **plus** the
repo's own build/test/lint — enforced in Go at the merge point, not by convention.

> **When to run.** After a worker is **green** (`ttorch validate <id>` passes) and the
> repo is in **trusted** delivery mode, or you want to gate one merge in another mode
> with `--require-verdict`. In any non-trusted mode the lead still approves the merge —
> this gate does not replace that. Default to proposing, not delivering.

> **Trust boundary.** In trusted mode the gate authorizes a merge **with no human reading
> the diff**: the boundary moves onto these reviewers and their prompts. Treat it as
> defense in depth, not an unbreakable barrier — opting a repo into trusted mode is the
> lead's decision, recorded per-repo via `ttorch init --mode trusted`.

## Protocol

1. **Prepare inputs.** `ttorch trust prep <id>` materializes, into the task's review
   inputs dir (the path it prints): `diff.patch` (changes vs the default branch),
   `brief.md` (the task brief, if any), `validate.json` (a fresh build/test/lint run),
   and `head.txt` (the reviewed commit). This step enforces nothing.

2. **Fan out three reviewers — in parallel, one per dimension.** Dispatch the
   `ttorch-reviewer-correctness`, `ttorch-reviewer-scope`, and `ttorch-reviewer-security`
   agents. Give each the inputs dir path and the commit from `head.txt`. Each reads the
   inputs, reviews **only** its dimension, and writes `<dimension>.json` into the inputs
   dir following the findings contract below. Reviewers never edit code.

3. **Record the verdict.** `ttorch trust record <id>` aggregates the three reports into a
   single commit-pinned, time-boxed verdict (Go owns the verdict body, so a missing or
   malformed report **fails closed** to `block`). In **trusted** mode a `pass` verdict
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

- The fresh validate runs the validation definition from the **default branch** (the
  `.ttorch/validate.sh` as it exists there, or the built-in ecosystem checks) — never the
  worker's own copy — so a worker cannot weaken its own gate by editing the script on its
  branch. A repo with no detectable checks fails closed (a hard block).
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
