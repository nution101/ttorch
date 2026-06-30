package profile

import (
	"bytes"
	"encoding/json"
	"strings"
)

// CodegraphServerName is the key, under "mcpServers", that the codegraph MCP server is
// registered as in a Claude Code .mcp.json. ttorch writes this entry so a worker — whose
// worktree is a checkout of the repo's branch — picks codegraph up as a navigation tool
// through Claude Code's project-scoped MCP discovery, exactly the way it picks up AGENTS.md.
const CodegraphServerName = "codegraph"

// codegraphMCPEntry is the canonical codegraph MCP server definition: a stdio server
// launched as `codegraph serve --mcp`. It matches the snippet `codegraph install` writes
// for Claude Code, so a graph already wired by codegraph itself and one wired by ttorch are
// identical.
func codegraphMCPEntry() map[string]any {
	return map[string]any{
		"type":    "stdio",
		"command": "codegraph",
		"args":    []any{"serve", "--mcp"},
	}
}

// UpsertCodegraphMCP returns existing — a Claude Code .mcp.json document, or empty/nil for
// a file that does not exist yet — with the codegraph MCP server added under "mcpServers",
// preserving every other server and every other top-level key. changed is false when the
// codegraph entry is already present and identical, so the caller can skip a needless
// rewrite. It errors only when existing is non-empty but not valid JSON; the caller must
// then leave the file untouched rather than clobber developer content.
func UpsertCodegraphMCP(existing []byte) (out []byte, changed bool, err error) {
	doc := map[string]json.RawMessage{}
	if len(strings.TrimSpace(string(existing))) > 0 {
		if err := json.Unmarshal(existing, &doc); err != nil {
			return nil, false, err
		}
	}
	servers := map[string]json.RawMessage{}
	if raw, ok := doc["mcpServers"]; ok {
		if err := json.Unmarshal(raw, &servers); err != nil {
			return nil, false, err
		}
	}
	entry, err := json.Marshal(codegraphMCPEntry())
	if err != nil {
		return nil, false, err
	}
	if cur, ok := servers[CodegraphServerName]; ok && sameJSON(cur, entry) {
		return existing, false, nil
	}
	servers[CodegraphServerName] = entry
	sraw, err := json.Marshal(servers)
	if err != nil {
		return nil, false, err
	}
	doc["mcpServers"] = sraw
	out, err = json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append(out, '\n'), true, nil
}

// sameJSON reports whether two JSON snippets are semantically equal, normalizing key order
// and whitespace by round-tripping both through a generic decode + canonical re-encode.
func sameJSON(a, b []byte) bool {
	norm := func(x []byte) []byte {
		var v any
		if json.Unmarshal(x, &v) != nil {
			return nil
		}
		n, _ := json.Marshal(v)
		return n
	}
	na, nb := norm(a), norm(b)
	return na != nil && bytes.Equal(na, nb)
}
