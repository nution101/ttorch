// Package skills manages recommended external Agent Skills that ttorch suggests
// installing alongside its own. These are installed on demand via the `skills` npm
// tool (`npx skills add <ref>`); they are not vendored into ttorch.
package skills

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Skill is an externally-distributed Agent Skill ttorch recommends.
type Skill struct {
	Ref string // "owner/repo" passed to `npx skills add`
	Why string
}

// Recommended returns the skills ttorch suggests for any team.
func Recommended() []Skill {
	return []Skill{
		{Ref: "kunchenguid/axi", Why: "guidelines for building token-efficient, agent-native CLI tools"},
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

// List prints the recommended skills and how to install them.
func List(out io.Writer) {
	fmt.Fprintln(out, "Recommended agent skills (install: ttorch skills install):")
	for _, s := range Recommended() {
		fmt.Fprintf(out, "  %-18s %s\n", s.Ref, s.Why)
	}
	if !NpxAvailable() {
		fmt.Fprintln(out, "  (installing these needs npx / Node.js)")
	}
}

// Install installs the recommended skills via `npx skills add`, after consent
// (or immediately when autoYes is set).
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
	for _, s := range recs {
		cmd := InstallCmd(s.Ref)
		fmt.Fprintf(out, "+ %s\n", strings.Join(cmd, " "))
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Stdout = out
		c.Stderr = out
		if err := c.Run(); err != nil {
			return fmt.Errorf("install %s: %w", s.Ref, err)
		}
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
