// Package skills manages recommended external Agent Skills that ttorch installs
// alongside its own. These are installed on demand via the `skills` npm tool
// (`npx skills add <ref>`); they are not vendored into ttorch. Every recommended
// skill is treated as required: ttorch installs any that are missing before it
// launches a team (see EnsureInstalled), so workers always inherit them.
package skills

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/nution101/ttorch/internal/paths"
)

// Skill is an externally-distributed Agent Skill ttorch installs for every team.
type Skill struct {
	Ref string // "owner/repo" passed to `npx skills add`
	Why string
}

// Recommended returns the skills ttorch installs for any team.
func Recommended() []Skill {
	return []Skill{
		{Ref: "kunchenguid/axi", Why: "guidelines for building token-efficient, agent-native CLI tools"},
		{Ref: "DietrichGebert/ponytail", Why: "keeps workers terse — write the least code that works and flag over-engineering (workers apply it by default)"},
	}
}

// InstallCmd is the command that installs a skill by ref.
func InstallCmd(ref string) []string {
	return []string{"npx", "skills", "add", ref}
}

// NpxAvailable reports whether npx (Node) is on PATH.
func NpxAvailable() bool {
	_, err := exec.LookPath("npx")
	return err == nil
}

// leaf returns the repo portion of an "owner/repo" ref (the directory name the
// skill installs under, e.g. "ponytail" for "DietrichGebert/ponytail").
func leaf(ref string) string {
	if i := strings.LastIndex(ref, "/"); i >= 0 {
		return ref[i+1:]
	}
	return ref
}

// markerPath is where ttorch records that it installed a skill. Recording our own
// marker keeps the install-state check independent of wherever the `skills` CLI
// happens to place files.
func markerPath(p paths.Paths, ref string) string {
	return filepath.Join(p.StateDir(), "skills", strings.ReplaceAll(ref, "/", "_"))
}

// installed reports whether a skill is already present: either laid down where the
// harness loads skills (~/.claude/skills/<repo>), or recorded by a prior install.
func installed(p paths.Paths, s Skill) bool {
	if fi, err := os.Stat(filepath.Join(p.ClaudeSkills(), leaf(s.Ref))); err == nil && fi.IsDir() {
		return true
	}
	if _, err := os.Stat(markerPath(p, s.Ref)); err == nil {
		return true
	}
	return false
}

// mark records that a skill was installed so a later launch skips reinstalling it.
func mark(p paths.Paths, ref string) {
	mp := markerPath(p, ref)
	if err := os.MkdirAll(filepath.Dir(mp), 0o755); err != nil {
		return
	}
	_ = os.WriteFile(mp, []byte(ref+"\n"), 0o644)
}

// skipEnsure reports whether the automatic install-before-launch step is disabled
// via TTORCH_SKIP_SKILL_INSTALL (a falsey escape hatch for tests / offline runs).
func skipEnsure() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TTORCH_SKIP_SKILL_INSTALL"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// EnsureInstalled installs any recommended skill that is not already present, so a
// team never launches without them. It is called before tmux sessions come up (team
// launch and every worker spawn); the per-skill marker makes the common
// already-installed case a cheap set of stat calls.
//
// Mandatory but best-effort: a per-skill install failure, or a missing npx, is
// reported and does NOT block the launch — an orchestration run must not be bricked
// because Node is absent or a registry fetch flaked. Disable with
// TTORCH_SKIP_SKILL_INSTALL=1.
func EnsureInstalled(out io.Writer) {
	if skipEnsure() {
		return
	}
	p := paths.Default()
	var missing []Skill
	for _, s := range Recommended() {
		if !installed(p, s) {
			missing = append(missing, s)
		}
	}
	if len(missing) == 0 {
		return
	}
	if !NpxAvailable() {
		fmt.Fprintln(out, "ttorch: recommended agent skills are not installed and npx (Node.js) is unavailable to install them.")
		fmt.Fprintln(out, "        install Node, then run 'ttorch skills install' — continuing without them for now.")
		return
	}
	fmt.Fprintln(out, "ttorch: installing recommended agent skills before launch…")
	for _, s := range missing {
		cmd := InstallCmd(s.Ref)
		fmt.Fprintf(out, "+ %s\n", strings.Join(cmd, " "))
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Stdout = out
		c.Stderr = out
		if err := c.Run(); err != nil {
			fmt.Fprintf(out, "  install %s failed (continuing): %v\n", s.Ref, err)
			continue
		}
		mark(p, s.Ref)
	}
}

// List prints the recommended skills and how to install them.
func List(out io.Writer) {
	fmt.Fprintln(out, "Recommended agent skills (installed automatically at launch; force now with 'ttorch skills install'):")
	for _, s := range Recommended() {
		fmt.Fprintf(out, "  %-24s %s\n", s.Ref, s.Why)
	}
	if !NpxAvailable() {
		fmt.Fprintln(out, "  (installing these needs npx / Node.js)")
	}
}

// Install installs the recommended skills via `npx skills add`, after consent
// (or immediately when autoYes is set), and records each so the automatic
// install-before-launch step skips them next time.
func Install(out io.Writer, in io.Reader, autoYes bool) error {
	recs := Recommended()
	if !NpxAvailable() {
		return errors.New("npx not found — install Node.js, or run the commands from 'ttorch skills' manually")
	}
	fmt.Fprintln(out, "Will install via npx:")
	for _, s := range recs {
		fmt.Fprintf(out, "  %s — %s\n", s.Ref, s.Why)
	}
	if !autoYes && !confirm(out, in, "Proceed? [Y/n] ") {
		fmt.Fprintln(out, "Skipped.")
		return nil
	}
	p := paths.Default()
	for _, s := range recs {
		cmd := InstallCmd(s.Ref)
		fmt.Fprintf(out, "+ %s\n", strings.Join(cmd, " "))
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Stdout = out
		c.Stderr = out
		if err := c.Run(); err != nil {
			return fmt.Errorf("install %s: %w", s.Ref, err)
		}
		mark(p, s.Ref)
	}
	fmt.Fprintln(out, "Done.")
	return nil
}

func confirm(out io.Writer, in io.Reader, prompt string) bool {
	fmt.Fprint(out, prompt)
	line, _ := bufio.NewReader(in).ReadString('\n')
	s := strings.ToLower(strings.TrimSpace(line))
	return s == "" || s == "y" || s == "yes"
}
