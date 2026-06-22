package manifest

import (
	"os"
	"path/filepath"
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

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func actionOf(rep *Report, path string) Action {
	for _, r := range rep.Results {
		if r.Path == path {
			return r.Action
		}
	}
	return Action("<none>")
}

func TestReconcile_AddsNewFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "skills", "a", "SKILL.md")
	desired := map[string][]byte{dest: []byte("v1")}

	next, rep, err := Reconcile(desired, New(""), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(rep, dest); got != Added {
		t.Fatalf("action = %q, want added", got)
	}
	if read(t, dest) != "v1" {
		t.Fatal("file content not written")
	}
	if next.Files[dest] != Sha([]byte("v1")) {
		t.Fatal("manifest did not record sha")
	}
}

func TestReconcile_UnchangedAndUpgrade(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.md")

	// Identical content -> Unchanged.
	write(t, dest, "v1")
	prev := New("")
	prev.Files[dest] = Sha([]byte("v1"))
	_, rep, err := Reconcile(map[string][]byte{dest: []byte("v1")}, prev, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(rep, dest); got != Unchanged {
		t.Fatalf("action = %q, want unchanged", got)
	}

	// Untouched-since-we-wrote-it + new shipped bytes -> Updated.
	_, rep, err = Reconcile(map[string][]byte{dest: []byte("v2")}, prev, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(rep, dest); got != Updated {
		t.Fatalf("action = %q, want updated", got)
	}
	if read(t, dest) != "v2" {
		t.Fatal("expected in-place upgrade to v2")
	}
}

func TestReconcile_NeverClobbersUserEdits(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.md")

	// We originally wrote "v1"; the developer edited it to "mine".
	write(t, dest, "mine")
	prev := New("")
	prev.Files[dest] = Sha([]byte("v1"))

	_, rep, err := Reconcile(map[string][]byte{dest: []byte("v2")}, prev, "0.3.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(rep, dest); got != Conflict {
		t.Fatalf("action = %q, want conflict", got)
	}
	if read(t, dest) != "mine" {
		t.Fatal("user edits were clobbered!")
	}
	if read(t, dest+Suffix) != "v2" {
		t.Fatal("new version not parked as .orcha-new")
	}
}

func TestReconcile_UnmanagedUserFileParked(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "f.md")

	// A file we never tracked already exists (prev has no entry).
	write(t, dest, "user-authored")

	_, rep, err := Reconcile(map[string][]byte{dest: []byte("shipped")}, New(""), "0.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(rep, dest); got != UserFile {
		t.Fatalf("action = %q, want userfile", got)
	}
	if read(t, dest) != "user-authored" {
		t.Fatal("untracked user file was overwritten!")
	}
	if read(t, dest+Suffix) != "shipped" {
		t.Fatal("shipped copy not parked")
	}
}

func TestReconcile_RetiresUntouchedRemovedFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "old.md")
	write(t, dest, "v1")
	prev := New("")
	prev.Files[dest] = Sha([]byte("v1"))

	// Desired no longer includes dest, and it's untouched since we wrote it.
	_, rep, err := Reconcile(map[string][]byte{}, prev, "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(rep, dest); got != Retired {
		t.Fatalf("action = %q, want retired", got)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatal("retired file should have been removed")
	}
}

func TestReconcile_KeepsLocallyModifiedRemovedFile(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "old.md")
	write(t, dest, "i-changed-this")
	prev := New("")
	prev.Files[dest] = Sha([]byte("v1"))

	_, rep, err := Reconcile(map[string][]byte{}, prev, "0.4.0")
	if err != nil {
		t.Fatal(err)
	}
	if got := actionOf(rep, dest); got != Kept {
		t.Fatalf("action = %q, want kept", got)
	}
	if read(t, dest) != "i-changed-this" {
		t.Fatal("locally-modified removed file must be preserved")
	}
}

func TestManifest_SaveLoadRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	m := New("1.2.3")
	m.Files["/a"] = "sha-a"
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	got := Load(path)
	if got.Version != "1.2.3" || got.Files["/a"] != "sha-a" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	// Corrupt manifest -> safe empty (fresh) manifest, never an error.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if c := Load(path); len(c.Files) != 0 {
		t.Fatal("corrupt manifest should load as empty")
	}
}
