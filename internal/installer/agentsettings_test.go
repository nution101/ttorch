package installer

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	ttorch "github.com/nution101/ttorch"
	"github.com/nution101/ttorch/internal/manifest"
)

const (
	agentHead = "---\nname: ttorch-reviewer-security\ndescription: >\n  Adversarial reviewer.\n" +
		"tools: Read, Grep, Bash\nmetadata:\n  managed-by: ttorch\n"
	agentV1 = agentHead + "---\n\nYou are the reviewer. Write reports/old.json.\n"
	agentV2 = agentHead + "---\n\nYou are the reviewer. Write reports/security.json.\n"
)

func withAgent(agent string) fstest.MapFS {
	fs := content("skill-v1")
	fs["content/agents/ttorch-reviewer-security.md"] = &fstest.MapFile{Data: []byte(agent)}
	return fs
}

func agentPath(t *testing.T, root string) string {
	t.Helper()
	return filepath.Join(root, "ttorch-reviewer-security.md")
}

// setSettings inserts model/effort lines just before the closing frontmatter delimiter.
func setSettings(s, lines string) string {
	i := strings.Index(s[4:], "\n---\n") + 4
	return s[:i+1] + lines + s[i+1:]
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func noParked(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p + manifest.Suffix); !os.IsNotExist(err) {
		t.Fatalf("unexpected %s (err=%v)", filepath.Base(p)+manifest.Suffix, err)
	}
}

func actionFor(rep *manifest.Report, p string) (manifest.Action, string) {
	for _, r := range rep.Results {
		if r.Path == p {
			return r.Action, r.Note
		}
	}
	return "<none>", ""
}

// assertConflict checks today's behaviour: the user's file is kept byte-for-byte and the
// shipped version is parked beside it.
func assertConflict(t *testing.T, rep *manifest.Report, dest, userFile, shipped string) string {
	t.Helper()
	act, note := actionFor(rep, dest)
	if act != manifest.Conflict {
		t.Fatalf("action = %q, want conflict", act)
	}
	if got := read(t, dest); got != userFile {
		t.Fatalf("user file changed:\n%s", got)
	}
	if got := read(t, dest+manifest.Suffix); got != shipped {
		t.Fatalf(".ttorch-new = %q, want the shipped version", got)
	}
	return note
}

func TestApply_ModelEffortOnlyEditUpdatesInPlace(t *testing.T) {
	p := sandbox(t)
	if _, err := apply(withAgent(agentV1), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	dest := agentPath(t, p.ClaudeAgents())
	writeFile(t, dest, setSettings(agentV1, "model: opus\neffort: xhigh\n"))

	res, err := apply(withAgent(agentV2), p, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if act, _ := actionFor(res.Report, dest); act != manifest.Updated {
		t.Fatalf("action = %q, want updated", act)
	}
	if want := setSettings(agentV2, "model: opus\neffort: xhigh\n"); read(t, dest) != want {
		t.Fatalf("file = %q\nwant %q", read(t, dest), want)
	}
	noParked(t, dest)
	if c := res.Report.Conflicts(); len(c) != 0 {
		t.Fatalf("unexpected conflicts: %+v", c)
	}
}

func TestApply_ShippedModelReplacedAndRemovedByUser(t *testing.T) {
	v1 := setSettings(agentV1, "model: sonnet\neffort: high\n")
	v2 := setSettings(agentV2, "model: haiku\neffort: high\n")

	t.Run("replaced", func(t *testing.T) {
		p := sandbox(t)
		if _, err := apply(withAgent(v1), p, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		dest := agentPath(t, p.ClaudeAgents())
		writeFile(t, dest, strings.Replace(v1, "model: sonnet", `model: "claude-opus-5"`, 1))
		if _, err := apply(withAgent(v2), p, "0.2.0"); err != nil {
			t.Fatal(err)
		}
		if want := strings.Replace(v2, "model: haiku", "model: claude-opus-5", 1); read(t, dest) != want {
			t.Fatalf("file = %q\nwant %q", read(t, dest), want)
		}
		noParked(t, dest)
	})

	t.Run("removed", func(t *testing.T) {
		p := sandbox(t)
		if _, err := apply(withAgent(v1), p, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		dest := agentPath(t, p.ClaudeAgents())
		writeFile(t, dest, strings.Replace(v1, "effort: high\n", "", 1))
		if _, err := apply(withAgent(v2), p, "0.2.0"); err != nil {
			t.Fatal(err)
		}
		if want := strings.Replace(v2, "effort: high\n", "", 1); read(t, dest) != want {
			t.Fatalf("file = %q\nwant %q", read(t, dest), want)
		}
		noParked(t, dest)
	})
}

// Only the settings the user changed are carried; the others take the new version's value.
func TestApply_UnchangedSettingTakesNewDefault(t *testing.T) {
	t.Run("new shipped model", func(t *testing.T) {
		v1 := setSettings(agentV1, "model: sonnet\n")
		v2 := setSettings(agentV2, "model: haiku\n")
		p := sandbox(t)
		if _, err := apply(withAgent(v1), p, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		dest := agentPath(t, p.ClaudeAgents())
		writeFile(t, dest, setSettings(v1, "effort: max\n"))
		if _, err := apply(withAgent(v2), p, "0.2.0"); err != nil {
			t.Fatal(err)
		}
		if want := setSettings(v2, "effort: max\n"); read(t, dest) != want {
			t.Fatalf("file = %q\nwant %q", read(t, dest), want)
		}
	})

	t.Run("newly shipped effort", func(t *testing.T) {
		v2 := setSettings(agentV2, "effort: high\n")
		p := sandbox(t)
		if _, err := apply(withAgent(agentV1), p, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		dest := agentPath(t, p.ClaudeAgents())
		writeFile(t, dest, setSettings(agentV1, "model: opus\n"))
		if _, err := apply(withAgent(v2), p, "0.2.0"); err != nil {
			t.Fatal(err)
		}
		if want := setSettings(agentV2, "effort: high\nmodel: opus\n"); read(t, dest) != want {
			t.Fatalf("file = %q\nwant %q", read(t, dest), want)
		}
	})
}

func TestApply_ModelEditPlusBodyEditStillConflicts(t *testing.T) {
	p := sandbox(t)
	if _, err := apply(withAgent(agentV1), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	dest := agentPath(t, p.ClaudeAgents())
	mine := setSettings(agentV1, "model: opus\n") + "Also check the lockfile.\n"
	writeFile(t, dest, mine)

	res, err := apply(withAgent(agentV2), p, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	assertConflict(t, res.Report, dest, mine, agentV2)
}

func TestApply_ChangedFrontmatterKeyStillConflicts(t *testing.T) {
	for name, edit := range map[string]func(string) string{
		"tools": func(s string) string { return strings.Replace(s, "tools: Read, Grep, Bash", "tools: Read", 1) },
		"tools plus model": func(s string) string {
			return setSettings(strings.Replace(s, "Bash", "Bash, Write", 1), "model: opus\n")
		},
		"name":        func(s string) string { return strings.Replace(s, "name: ttorch-reviewer-security", "name: mine", 1) },
		"description": func(s string) string { return strings.Replace(s, "Adversarial reviewer.", "Lenient reviewer.", 1) },
		"added key":   func(s string) string { return setSettings(s, "permissionMode: bypassPermissions\n") },
		"indented model": func(s string) string {
			return strings.Replace(s, "  managed-by: ttorch\n", "  managed-by: ttorch\n  model: opus\n", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := sandbox(t)
			if _, err := apply(withAgent(agentV1), p, "0.1.0"); err != nil {
				t.Fatal(err)
			}
			dest := agentPath(t, p.ClaudeAgents())
			mine := edit(agentV1)
			if mine == agentV1 {
				t.Fatal("edit did not change the fixture")
			}
			writeFile(t, dest, mine)
			res, err := apply(withAgent(agentV2), p, "0.2.0")
			if err != nil {
				t.Fatal(err)
			}
			assertConflict(t, res.Report, dest, mine, agentV2)
		})
	}
}

func TestApply_InvalidSettingIsNotCarried(t *testing.T) {
	for name, tc := range map[string]struct{ lines, why string }{
		"bracketed model": {"model: opus[1m]\n", `model "opus[1m]"`},
		"unknown alias":   {"model: opuss\n", `model "opuss"`},
		"model comment":   {"model: opus # mine\n", `model "opus # mine"`},
		"empty model":     {"model:\n", `model ""`},
		"unknown effort":  {"model: opus\neffort: turbo\n", `effort "turbo"`},
		"duplicate model": {"model: opus\nmodel: sonnet\n", "model is set more than once"},
	} {
		t.Run(name, func(t *testing.T) {
			p := sandbox(t)
			if _, err := apply(withAgent(agentV1), p, "0.1.0"); err != nil {
				t.Fatal(err)
			}
			dest := agentPath(t, p.ClaudeAgents())
			mine := setSettings(agentV1, tc.lines)
			writeFile(t, dest, mine)
			res, err := apply(withAgent(agentV2), p, "0.2.0")
			if err != nil {
				t.Fatal(err)
			}
			note := assertConflict(t, res.Report, dest, mine, agentV2)
			if !strings.Contains(note, tc.why) || !strings.Contains(note, "not carried") {
				t.Fatalf("note %q does not say why (%s)", note, tc.why)
			}
		})
	}
}

func TestApply_ModelOutsideFrontmatterIsContent(t *testing.T) {
	t.Run("no frontmatter", func(t *testing.T) {
		v1 := "You are the reviewer.\n"
		p := sandbox(t)
		if _, err := apply(withAgent(v1), p, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		dest := agentPath(t, p.ClaudeAgents())
		mine := "model: opus\n" + v1
		writeFile(t, dest, mine)
		res, err := apply(withAgent("You are the new reviewer.\n"), p, "0.2.0")
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, res.Report, dest, mine, "You are the new reviewer.\n")
	})

	t.Run("model line in body", func(t *testing.T) {
		v1 := agentV1 + "model: sonnet\n"
		v2 := agentV2 + "model: sonnet\n"
		p := sandbox(t)
		if _, err := apply(withAgent(v1), p, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		dest := agentPath(t, p.ClaudeAgents())
		mine := strings.Replace(v1, "model: sonnet", "model: opus", 1)
		writeFile(t, dest, mine)
		res, err := apply(withAgent(v2), p, "0.2.0")
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, res.Report, dest, mine, v2)
	})

	t.Run("unterminated frontmatter", func(t *testing.T) {
		v1 := strings.TrimSuffix(agentHead, "\n") + "\n"
		p := sandbox(t)
		if _, err := apply(withAgent(v1), p, "0.1.0"); err != nil {
			t.Fatal(err)
		}
		dest := agentPath(t, p.ClaudeAgents())
		mine := v1 + "model: opus\n"
		writeFile(t, dest, mine)
		res, err := apply(withAgent(v1+"extra: 1\n"), p, "0.2.0")
		if err != nil {
			t.Fatal(err)
		}
		assertConflict(t, res.Report, dest, mine, v1+"extra: 1\n")
	})
}

// Only agents are covered: a model: key in a skill's frontmatter is still content.
func TestApply_SkillFrontmatterNotCovered(t *testing.T) {
	skill1 := "---\nname: s\n---\nv1\n"
	skill2 := "---\nname: s\n---\nv2\n"
	p := sandbox(t)
	if _, err := apply(content(skill1), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(p.ClaudeSkills(), "ttorch-manager", "SKILL.md")
	mine := "---\nname: s\nmodel: opus\n---\nv1\n"
	writeFile(t, dest, mine)
	res, err := apply(content(skill2), p, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	assertConflict(t, res.Report, dest, mine, skill2)
}

func TestApply_CarriedSettingsRoundTrip(t *testing.T) {
	p := sandbox(t)
	if _, err := apply(withAgent(agentV1), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	dest := agentPath(t, p.ClaudeAgents())
	writeFile(t, dest, setSettings(agentV1, "model: opus\neffort: xhigh\n"))

	if _, err := apply(withAgent(agentV2), p, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	after := read(t, dest)

	res, err := apply(withAgent(agentV2), p, "0.2.0")
	if err != nil {
		t.Fatal(err)
	}
	if act, _ := actionFor(res.Report, dest); act != manifest.Unchanged {
		t.Fatalf("second update action = %q, want unchanged", act)
	}
	if read(t, dest) != after {
		t.Fatal("second update rewrote the file")
	}
	noParked(t, dest)
	if res.Report.Count(manifest.Updated) != 0 || len(res.Report.Conflicts()) != 0 {
		t.Fatalf("second update was not a no-op: %+v", res.Report.Results)
	}
}

// A ledger written before normalized hashes were recorded only has the whole-file sha of
// what was last shipped. When the shipped settings did not change, that is still enough.
func TestApply_LegacyLedgerWithoutNormalizedHash(t *testing.T) {
	p := sandbox(t)
	if _, err := apply(withAgent(agentV1), p, "0.1.0"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p.ManifestFile())
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	delete(raw, "normalized")
	b, _ = json.Marshal(raw)
	writeFile(t, p.ManifestFile(), string(b))

	dest := agentPath(t, p.ClaudeAgents())
	writeFile(t, dest, setSettings(agentV1, "model: opus\n"))
	if _, err := apply(withAgent(agentV2), p, "0.2.0"); err != nil {
		t.Fatal(err)
	}
	if want := setSettings(agentV2, "model: opus\n"); read(t, dest) != want {
		t.Fatalf("file = %q\nwant %q", read(t, dest), want)
	}
	noParked(t, dest)
}

// Every shipped agent's settings must parse and validate, or the ledger records no settings
// for it and a user's model/effort edit to that agent falls back to .ttorch-new.
func TestShippedAgentSettingsAreValid(t *testing.T) {
	a := agentSettings{dir: "/agents"}
	n := 0
	err := fs.WalkDir(ttorch.Content(), embedRoot+"/agents", func(fp string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fs.ReadFile(ttorch.Content(), fp)
		if err != nil {
			return err
		}
		if _, err := a.Values("/agents/x.md", b); err != nil {
			t.Errorf("%s: %v", fp, err)
		}
		n++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("no shipped agents found, so this test would pass vacuously")
	}
}
