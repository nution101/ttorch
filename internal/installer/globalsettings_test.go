package installer

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/nution101/ttorch/internal/paths"
)

// readSettings reads and decodes the global settings file at path into a map.
func readSettings(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read settings %s: %v", path, err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatalf("settings %s is not a JSON object: %v\n%s", path, err, b)
	}
	return m
}

// seedSettings writes obj as the global settings file, creating ~/.claude first.
func seedSettings(t *testing.T, p paths.Paths, obj map[string]any) {
	t.Helper()
	b, err := json.MarshalIndent(obj, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := atomicWrite(p.GlobalSettingsFile(), append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func TestApplyGlobalSettings_MergeIntoMissingFile(t *testing.T) {
	p := sandbox(t)
	note, err := applyGlobalSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if note == "" {
		t.Fatal("expected a note")
	}
	got := readSettings(t, p.GlobalSettingsFile())
	if got["includeCoAuthoredBy"] != false {
		t.Fatalf("includeCoAuthoredBy not set: %v", got["includeCoAuthoredBy"])
	}
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("hooks not an object: %v", got["hooks"])
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Fatalf("placeholder hook not installed: %v", hooks)
	}
	// Nothing existed, so no backup should be written.
	if _, err := os.Stat(p.GlobalSettingsBackup()); !os.IsNotExist(err) {
		t.Fatal("backup written for a file that never existed")
	}
	// The ledger should record exactly what ttorch owns.
	if _, err := os.Stat(p.GlobalSettingsLedger()); err != nil {
		t.Fatalf("ledger not written: %v", err)
	}
}

func TestApplyGlobalSettings_MergeIntoEmptyFile(t *testing.T) {
	p := sandbox(t)
	if err := atomicWrite(p.GlobalSettingsFile(), []byte("\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, p.GlobalSettingsFile())
	if got["includeCoAuthoredBy"] != false {
		t.Fatalf("merge into empty file failed: %v", got)
	}
}

func TestApplyGlobalSettings_PreservesExistingKeys(t *testing.T) {
	p := sandbox(t)
	seedSettings(t, p, map[string]any{
		"model": "opus",
		"theme": "dark",
		// Developer set includeCoAuthoredBy themselves: ttorch must not flip it.
		"includeCoAuthoredBy": true,
		// Developer has their own hook event under hooks.
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{"matcher": "Bash"}},
		},
	})
	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, p.GlobalSettingsFile())

	if got["model"] != "opus" || got["theme"] != "dark" {
		t.Fatalf("developer top-level keys lost: %v", got)
	}
	if got["includeCoAuthoredBy"] != true {
		t.Fatal("ttorch clobbered a developer-set key")
	}
	hooks := got["hooks"].(map[string]any)
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Fatal("developer hook event was dropped")
	}
	if _, ok := hooks["SessionStart"]; !ok {
		t.Fatal("ttorch hook event not merged alongside developer's")
	}
}

func TestApplyGlobalSettings_Idempotent(t *testing.T) {
	p := sandbox(t)
	note1, err := applyGlobalSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	first := readSettings(t, p.GlobalSettingsFile())

	note2, err := applyGlobalSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	second := readSettings(t, p.GlobalSettingsFile())

	if !reflect.DeepEqual(first, second) {
		t.Fatalf("re-apply changed settings:\n %v\n %v", first, second)
	}
	if note1 == note2 {
		t.Fatalf("expected the second run to report no change (got %q both times)", note2)
	}
	if want := "already up to date"; !strings.Contains(note2, want) {
		t.Fatalf("second note %q does not signal a no-op", note2)
	}
}

func TestApplyGlobalSettings_BackupCreatedOnce(t *testing.T) {
	p := sandbox(t)
	original := map[string]any{"model": "opus"}
	seedSettings(t, p, original)

	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	backup := readSettings(t, p.GlobalSettingsBackup())
	if !reflect.DeepEqual(backup, original) {
		t.Fatalf("backup is not the developer's original: %v", backup)
	}

	// Force a second modification (developer removed the hooks ttorch added).
	cur := readSettings(t, p.GlobalSettingsFile())
	delete(cur, "hooks")
	cur["model"] = "sonnet" // and changed their own value
	seedSettings(t, p, cur)

	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	// The backup must still hold the very first original, not the later state.
	backup2 := readSettings(t, p.GlobalSettingsBackup())
	if !reflect.DeepEqual(backup2, original) {
		t.Fatalf("backup was overwritten on a later modification: %v", backup2)
	}
}

func TestApplyGlobalSettings_InvalidJSONLeftUntouched(t *testing.T) {
	p := sandbox(t)
	bad := []byte("{ not valid json")
	if err := atomicWrite(p.GlobalSettingsFile(), bad); err != nil {
		t.Fatal(err)
	}
	note, err := applyGlobalSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "untouched") {
		t.Fatalf("expected an untouched note, got %q", note)
	}
	got, _ := os.ReadFile(p.GlobalSettingsFile())
	if string(got) != string(bad) {
		t.Fatalf("malformed settings.json was modified: %s", got)
	}
	if _, err := os.Stat(p.GlobalSettingsLedger()); !os.IsNotExist(err) {
		t.Fatal("ledger written despite skipping a malformed file")
	}
}

func TestApplyGlobalSettings_NullFileTreatedAsEmpty(t *testing.T) {
	p := sandbox(t)
	if err := atomicWrite(p.GlobalSettingsFile(), []byte("null\n")); err != nil {
		t.Fatal(err)
	}
	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatalf("merging into a `null` settings file should not error: %v", err)
	}
	got := readSettings(t, p.GlobalSettingsFile())
	if got["includeCoAuthoredBy"] != false {
		t.Fatalf("merge into a null settings file failed: %v", got)
	}
}

func TestApplyGlobalSettings_OptOut(t *testing.T) {
	p := sandbox(t)
	t.Setenv(noGlobalHooksEnv, "1")
	note, err := applyGlobalSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, noGlobalHooksEnv) {
		t.Fatalf("expected an opt-out note, got %q", note)
	}
	if _, err := os.Stat(p.GlobalSettingsFile()); !os.IsNotExist(err) {
		t.Fatal("settings.json written while opted out")
	}
}

func TestRemoveGlobalSettings_RemovesOnlyTtorchKeys(t *testing.T) {
	p := sandbox(t)
	seedSettings(t, p, map[string]any{"model": "opus"})
	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	// Developer adds their own key after install.
	cur := readSettings(t, p.GlobalSettingsFile())
	cur["theme"] = "dark"
	seedSettings(t, p, cur)

	note, err := removeGlobalSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(note, "removed") {
		t.Fatalf("expected a removal note, got %q", note)
	}
	got := readSettings(t, p.GlobalSettingsFile())
	if got["model"] != "opus" || got["theme"] != "dark" {
		t.Fatalf("developer keys lost on uninstall: %v", got)
	}
	if _, ok := got["includeCoAuthoredBy"]; ok {
		t.Fatal("ttorch key not removed on uninstall")
	}
	if _, ok := got["hooks"]; ok {
		t.Fatal("emptied hooks object not pruned on uninstall")
	}
	if _, err := os.Stat(p.GlobalSettingsLedger()); !os.IsNotExist(err) {
		t.Fatal("ledger not removed on uninstall")
	}
	// The backup remains for recovery.
	if _, err := os.Stat(p.GlobalSettingsBackup()); err != nil {
		t.Fatal("backup should be kept after uninstall")
	}
}

func TestRemoveGlobalSettings_PreservesDeveloperChangedValue(t *testing.T) {
	p := sandbox(t)
	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	// Developer flips the value ttorch had set.
	cur := readSettings(t, p.GlobalSettingsFile())
	cur["includeCoAuthoredBy"] = true
	seedSettings(t, p, cur)

	if _, err := removeGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, p.GlobalSettingsFile())
	if got["includeCoAuthoredBy"] != true {
		t.Fatalf("uninstall removed a value the developer had changed: %v", got)
	}
}

func TestRemoveGlobalSettings_KeepsDeveloperHookEvent(t *testing.T) {
	p := sandbox(t)
	seedSettings(t, p, map[string]any{
		"hooks": map[string]any{
			"PreToolUse": []any{map[string]any{"matcher": "Bash"}},
		},
	})
	if _, err := applyGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	if _, err := removeGlobalSettings(p); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, p.GlobalSettingsFile())
	hooks, ok := got["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("developer hooks object wrongly pruned: %v", got)
	}
	if _, ok := hooks["PreToolUse"]; !ok {
		t.Fatal("developer hook event lost on uninstall")
	}
	if _, ok := hooks["SessionStart"]; ok {
		t.Fatal("ttorch hook event not removed on uninstall")
	}
}

func TestRemoveGlobalSettings_NoLedgerIsNoOp(t *testing.T) {
	p := sandbox(t)
	seedSettings(t, p, map[string]any{"model": "opus"})
	note, err := removeGlobalSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if note != "" {
		t.Fatalf("expected no note with no ledger, got %q", note)
	}
	got := readSettings(t, p.GlobalSettingsFile())
	if got["model"] != "opus" {
		t.Fatal("uninstall touched settings without a ledger")
	}
}

// TestApply_WiresGlobalSettings exercises the public install/uninstall entry points
// so the wiring (not just the helper) is covered.
func TestApply_WiresGlobalSettings(t *testing.T) {
	p := sandbox(t)
	if _, err := Apply(content("skill-v1"), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	got := readSettings(t, p.GlobalSettingsFile())
	if got["includeCoAuthoredBy"] != false {
		t.Fatalf("install did not merge global settings: %v", got)
	}
	if _, err := os.Stat(p.GlobalSettingsLedger()); err != nil {
		t.Fatalf("install did not write the settings ledger: %v", err)
	}

	if _, err := Uninstall(p, false); err != nil {
		t.Fatal(err)
	}
	got = readSettings(t, p.GlobalSettingsFile())
	if _, ok := got["includeCoAuthoredBy"]; ok {
		t.Fatalf("uninstall left ttorch settings behind: %v", got)
	}
	if _, err := os.Stat(p.GlobalSettingsLedger()); !os.IsNotExist(err) {
		t.Fatal("uninstall left the settings ledger behind")
	}
}
