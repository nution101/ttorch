package validate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRun_ReportsEachStep(t *testing.T) {
	steps := []Step{
		{Name: "ok", Cmd: []string{"sh", "-c", "exit 0"}},
		{Name: "bad", Cmd: []string{"sh", "-c", "echo boom; exit 1"}},
		{Name: "ok2", Cmd: []string{"sh", "-c", "exit 0"}},
	}
	results := Run(t.TempDir(), steps)
	if len(results) != 3 {
		t.Fatalf("expected all 3 steps to run, got %d", len(results))
	}
	if !results[0].Passed || results[1].Passed || !results[2].Passed {
		t.Fatalf("unexpected pass/fail: %+v", results)
	}
	if results[1].Output != "boom" {
		t.Fatalf("expected captured output, got %q", results[1].Output)
	}
	if f := Failures(results); len(f) != 1 || f[0].Name != "bad" {
		t.Fatalf("Failures wrong: %+v", f)
	}
}

func TestDetect_Go(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	steps := Detect(dir)
	names := stepNames(steps)
	want := []string{"build", "vet", "fmt", "test"}
	if len(names) != len(want) {
		t.Fatalf("go steps = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("go steps = %v, want %v", names, want)
		}
	}
}

func TestDetect_Node(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "package.json"),
		[]byte(`{"scripts":{"test":"jest","lint":"eslint ."}}`), 0o644)
	names := stepNames(Detect(dir))
	// lint and test present; build absent.
	if len(names) != 2 {
		t.Fatalf("node steps = %v, want [lint test]", names)
	}
	if names[0] != "lint" || names[1] != "test" {
		t.Fatalf("node steps = %v, want [lint test]", names)
	}
}

func TestDetect_Override(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".orcha"), 0o755)
	os.WriteFile(filepath.Join(dir, ".orcha", "validate.sh"), []byte("#!/bin/sh\n"), 0o755)
	// Even with a go.mod, the explicit override wins.
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n"), 0o644)
	steps := Detect(dir)
	if len(steps) != 1 || steps[0].Name != "custom" {
		t.Fatalf("override should yield a single custom step, got %+v", steps)
	}
}

func TestDetect_Empty(t *testing.T) {
	if steps := Detect(t.TempDir()); steps != nil {
		t.Fatalf("expected no steps for an unrecognized repo, got %+v", steps)
	}
}

func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}
