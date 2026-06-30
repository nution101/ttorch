package profile

import (
	"encoding/json"
	"testing"
)

// decodeServers pulls mcpServers out of a .mcp.json document for assertions.
func decodeServers(t *testing.T, b []byte) map[string]map[string]any {
	t.Helper()
	var doc struct {
		MCPServers map[string]map[string]any `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, b)
	}
	return doc.MCPServers
}

func TestUpsertCodegraphMCPCreatesFile(t *testing.T) {
	out, changed, err := UpsertCodegraphMCP(nil)
	if err != nil || !changed {
		t.Fatalf("nil input: changed=%v err=%v", changed, err)
	}
	servers := decodeServers(t, out)
	cg, ok := servers[CodegraphServerName]
	if !ok {
		t.Fatalf("codegraph server entry missing: %s", out)
	}
	if cg["command"] != "codegraph" || cg["type"] != "stdio" {
		t.Errorf("codegraph entry shape wrong: %v", cg)
	}
	args, _ := cg["args"].([]any)
	if len(args) != 2 || args[0] != "serve" || args[1] != "--mcp" {
		t.Errorf("codegraph args wrong: %v", cg["args"])
	}
}

func TestUpsertCodegraphMCPPreservesExisting(t *testing.T) {
	existing := []byte(`{
  "mcpServers": {
    "other": {"type": "stdio", "command": "other-tool", "args": ["go"]}
  },
  "someTopLevelKey": 42
}`)
	out, changed, err := UpsertCodegraphMCP(existing)
	if err != nil || !changed {
		t.Fatalf("existing input: changed=%v err=%v", changed, err)
	}
	servers := decodeServers(t, out)
	if _, ok := servers["other"]; !ok {
		t.Errorf("pre-existing 'other' server was dropped: %s", out)
	}
	if _, ok := servers[CodegraphServerName]; !ok {
		t.Errorf("codegraph server not added: %s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("output not valid JSON: %v", err)
	}
	if v, ok := doc["someTopLevelKey"]; !ok || v.(float64) != 42 {
		t.Errorf("unrelated top-level key not preserved: %v", doc["someTopLevelKey"])
	}
}

func TestUpsertCodegraphMCPIdempotent(t *testing.T) {
	first, changed, err := UpsertCodegraphMCP(nil)
	if err != nil || !changed {
		t.Fatalf("first upsert: changed=%v err=%v", changed, err)
	}
	second, changed, err := UpsertCodegraphMCP(first)
	if err != nil {
		t.Fatalf("second upsert errored: %v", err)
	}
	if changed {
		t.Errorf("re-upserting an identical codegraph entry must report changed=false")
	}
	if string(second) != string(first) {
		t.Errorf("idempotent upsert must return the input unchanged")
	}
}

func TestUpsertCodegraphMCPInvalidJSONLeftUntouched(t *testing.T) {
	_, changed, err := UpsertCodegraphMCP([]byte("{ this is : not json"))
	if err == nil {
		t.Fatal("an invalid existing .mcp.json must error so the caller leaves it untouched")
	}
	if changed {
		t.Error("invalid input must not report a change")
	}
}
