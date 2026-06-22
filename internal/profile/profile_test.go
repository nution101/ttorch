package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func contains(xs []string, sub string) bool {
	for _, x := range xs {
		if strings.Contains(x, sub) {
			return true
		}
	}
	return false
}

func TestDetect_Go(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.22\n")
	write(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	write(t, filepath.Join(dir, "util.go"), "package main\n\nfunc helper() int { return 1 }\n")
	write(t, filepath.Join(dir, "main_test.go"), "package main\n\nimport \"testing\"\n\nfunc TestX(t *testing.T) {}\n")

	p := Detect(dir)
	if p.Stack != "Go" {
		t.Fatalf("stack = %q, want Go", p.Stack)
	}
	if !contains(p.Build, "go build") || !contains(p.Test, "go test") || !contains(p.Lint, "gofmt") {
		t.Fatalf("commands wrong: build=%v test=%v lint=%v", p.Build, p.Test, p.Lint)
	}
	if len(p.Exemplars) == 0 {
		t.Fatal("expected exemplars")
	}
	for _, e := range p.Exemplars {
		if strings.Contains(e, "_test.go") {
			t.Fatalf("exemplars must exclude tests, got %v", p.Exemplars)
		}
	}
}

func TestDetect_Node(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "package.json"), `{"scripts":{"build":"tsc","test":"vitest","lint":"eslint ."}}`)
	write(t, filepath.Join(dir, "src", "index.ts"), "export const x = 1;\n")

	p := Detect(dir)
	if p.Stack != "Node.js" {
		t.Fatalf("stack = %q, want Node.js", p.Stack)
	}
	if !contains(p.Build, "npm run build") || !contains(p.Test, "npm test") || !contains(p.Lint, "npm run lint") {
		t.Fatalf("node commands wrong: %+v", p)
	}
	if !contains(p.Exemplars, "index.ts") {
		t.Fatalf("expected index.ts exemplar, got %v", p.Exemplars)
	}
}

func TestRender(t *testing.T) {
	out := Render(Profile{
		Stack: "Go", Build: []string{"go build ./..."}, Test: []string{"go test ./..."},
		Exemplars: []string{"main.go"}, Notes: []string{"honor .editorconfig"},
	})
	for _, want := range []string{"Stack: Go", "go build ./...", "Match the style of", "honor .editorconfig"} {
		if !strings.Contains(out, want) {
			t.Fatalf("render missing %q in:\n%s", want, out)
		}
	}
}

func TestApply_UpsertPreservesAndIdempotent(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "go.mod"), "module x\n\ngo 1.22\n")
	write(t, filepath.Join(dir, "main.go"), "package main\n\nfunc main() {}\n")
	agents := filepath.Join(dir, "AGENTS.md")
	write(t, agents, "# My rules\n\nuse tabs\n")

	if _, err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(agents)
	if !strings.Contains(string(got), "use tabs") {
		t.Fatal("developer content must be preserved")
	}
	if !strings.Contains(string(got), "Stack: Go") {
		t.Fatal("profile block not written")
	}

	if _, err := Apply(dir); err != nil {
		t.Fatal(err)
	}
	got, _ = os.ReadFile(agents)
	if strings.Count(string(got), markerBegin) != 1 {
		t.Fatal("profile block duplicated on re-apply")
	}
}
