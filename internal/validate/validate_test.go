package validate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/ciparity"
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
	os.MkdirAll(filepath.Join(dir, ".ttorch"), 0o755)
	os.WriteFile(filepath.Join(dir, ".ttorch", "validate.sh"), []byte("#!/bin/sh\n"), 0o755)
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

func TestRun_Timeout(t *testing.T) {
	t.Setenv("TTORCH_VALIDATE_TIMEOUT", "300ms")
	results := Run(t.TempDir(), []Step{{Name: "slow", Cmd: []string{"sh", "-c", "sleep 5"}}})
	if len(results) != 1 || results[0].Passed {
		t.Fatalf("slow step should fail by timeout, got %+v", results)
	}
	if !strings.Contains(results[0].Output, "timed out") {
		t.Fatalf("expected a timeout message, got %q", results[0].Output)
	}
}

func TestRun_FmtFailureNamesFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module x\n\ngo 1.22\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "bad.go"), []byte("package x\nfunc  F( ){}\n"), 0o644)

	var fmtStep Step
	for _, s := range Detect(dir) {
		if s.Name == "fmt" {
			fmtStep = s
		}
	}
	if fmtStep.Name == "" {
		t.Fatal("no fmt step detected for a go.mod repo")
	}
	res := Run(dir, []Step{fmtStep})
	if res[0].Passed {
		t.Fatal("fmt should fail on a mis-formatted file")
	}
	if !strings.Contains(res[0].Output, "unformatted") || !strings.Contains(res[0].Output, "bad.go") {
		t.Fatalf("fmt failure should name the unformatted file; got %q", res[0].Output)
	}
}

func TestCIParitySteps_RunsThroughResultPath(t *testing.T) {
	ciSteps := []ciparity.Step{
		{Workflow: "ci.yml", Job: "build", Name: "ok", Run: "exit 0"},
		{Workflow: "ci.yml", Job: "build", Name: "bad", Run: "echo boom; exit 1"},
	}
	steps := CIParitySteps(ciSteps)
	if len(steps) != 2 || steps[0].Name != "ci(ci.yml/build): ok" {
		t.Fatalf("converted steps = %+v", steps)
	}
	results := Run(t.TempDir(), steps)
	if len(results) != 2 || !results[0].Passed || results[1].Passed {
		t.Fatalf("expected ok-pass/bad-fail, got %+v", results)
	}
	if f := Failures(results); len(f) != 1 || !strings.Contains(f[0].Output, "boom") {
		t.Fatalf("failure not captured: %+v", f)
	}
}

func stepNames(steps []Step) []string {
	out := make([]string, len(steps))
	for i, s := range steps {
		out[i] = s.Name
	}
	return out
}
