# Codegraph worker code-navigation (opt-in)

ttorch can give each worker an optional **code-navigation** capability backed by
`codegraph` — a code-intelligence CLI that indexes a repository and serves symbol,
caller/callee, definition, and impact queries through an MCP server. A worker that has it
prefers graph-backed navigation over plain text search when finding its way around an
unfamiliar codebase.

This feature is **opt-in and default off**. With it disabled — the default — ttorch behaves
exactly as it does without codegraph: no MCP entry is written, no index is built, and the
worker guidance is unchanged in effect. It is also **evidence-gated**: enabling it without
codegraph installed is a clean no-op (a `ttorch doctor` note), never an error.

## Enabling it

1. Install codegraph so it is on your `PATH` (see the codegraph project's install
   instructions; it is a standalone CLI, not an OS-package-manager dependency of ttorch).
2. Opt in by exporting the switch:

   ```sh
   export TTORCH_CODEGRAPH=1   # 1 / true / yes / on; anything else (or unset) is off
   ```

3. Run `ttorch doctor` to confirm detection. With the feature on and codegraph present you
   will see an `[ok] codegraph …` line; with it on but codegraph absent you get an
   `[absent] codegraph — … worker code-navigation stays off (no error)` note instead.
4. Run `ttorch init` in the repo. When the feature is on **and** codegraph is present,
   `init` will:
   - build the repo's code index (or refresh an existing one),
   - add the `codegraph` MCP server to the repo's `.mcp.json` (merge-safe — any MCP servers
     you already have are preserved; an existing-but-invalid `.mcp.json` is left untouched),
     and
   - git-ignore the `.codegraph/` index directory (a per-checkout build artifact).
5. Commit the resulting `.mcp.json` (and `.gitignore` change) so workers pick it up — the
   same "commit it so workers pick it up" step as `AGENTS.md`. A worker's worktree is a
   checkout of your branch, so a committed `.mcp.json` is what makes Claude Code expose
   `codegraph` to the worker as an MCP tool.

## What the worker sees

When the MCP tool is wired, the worker guidance asks the worker to prefer codegraph for
locating symbols, callers, callees, and definitions, and for impact analysis, before
falling back to text search. When the tool is **not** present, that guidance is inert: the
worker simply navigates as it always has. There is no behavior change when the feature is
off or codegraph is absent.

The `.codegraph/` index is a per-checkout artifact and is git-ignored, so it is not shared
through the repository; codegraph maintains each checkout's index itself.

## Turning it off

Unset `TTORCH_CODEGRAPH` (or set it to `0`). Future `ttorch init` runs stop touching
codegraph. To fully remove the wiring from a repo, delete the `codegraph` entry from
`.mcp.json` (or the file, if it holds nothing else) and run `codegraph uninit` to drop the
`.codegraph/` index.
