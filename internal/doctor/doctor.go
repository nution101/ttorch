// Package doctor detects the tools ttorch needs (tmux, git, gh, claude) and,
// with consent, installs the missing ones via the platform package manager.
package doctor

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/nution101/ttorch/internal/termtab"
)

// Tool is an external dependency ttorch relies on.
type Tool struct {
	Name     string
	Bin      string
	Why      string
	Required bool
	// Manual, when set, is shown instead of an auto-install (e.g. for claude,
	// which is not distributed through OS package managers).
	Manual string
}

// Tools is the dependency set, in report order.
func Tools() []Tool {
	return []Tool{
		{Name: "tmux", Bin: "tmux", Why: "runs each agent in its own visible session", Required: true},
		{Name: "git", Bin: "git", Why: "worktrees, branches, and delivery", Required: true},
		{Name: "gh", Bin: "gh", Why: "PR creation and merge-status checks", Required: false},
		{Name: "claude", Bin: "claude", Why: "the coding agent ttorch orchestrates", Required: true,
			Manual: "install Claude Code, e.g.  npm install -g @anthropic-ai/claude-code"},
	}
}

// Diagnosis is the result of inspecting the environment.
type Diagnosis struct {
	Found   map[string]string // tool name -> path
	Missing []Tool
	Manager string // detected package manager (empty if none)
	WSL     string // "", "wsl1", or "wsl2"
}

// Diagnose inspects the environment without changing anything.
func Diagnose() Diagnosis {
	d := Diagnosis{Found: map[string]string{}, Manager: detectManager(), WSL: detectWSL()}
	for _, t := range Tools() {
		if p, err := exec.LookPath(t.Bin); err == nil {
			d.Found[t.Name] = p
		} else {
			d.Missing = append(d.Missing, t)
		}
	}
	return d
}

// Run prints the diagnosis and, for installable missing tools, installs them
// after consent (or immediately when autoYes is set).
func Run(out io.Writer, in io.Reader, autoYes bool) error {
	d := Diagnose()

	fmt.Fprintln(out, "ttorch doctor")
	for _, t := range Tools() {
		if p, ok := d.Found[t.Name]; ok {
			fmt.Fprintf(out, "  [ok]      %-7s %s\n", t.Name, p)
		} else {
			tag := "missing"
			if !t.Required {
				tag = "absent "
			}
			fmt.Fprintf(out, "  [%s] %-7s — %s\n", tag, t.Name, t.Why)
		}
	}

	// iTerm2 is an optional macOS convenience: with it present, 'ttorch' opens the
	// team in a single iTerm2 window with clean per-worker tabs. Never required.
	wantITerm := false
	if runtime.GOOS == "darwin" {
		if p := termtab.ITermAppPath(); p != "" {
			fmt.Fprintf(out, "  [ok]      %-7s %s\n", "iTerm2", p)
		} else {
			fmt.Fprintln(out, "  [absent] iTerm2  — clean per-worker tabs; 'ttorch' will open the team in iTerm2 when present")
			if _, ok := itermInstallCmd(d.Manager); ok {
				wantITerm = true
			}
		}
	}

	// codegraph backs the opt-in, default-off worker code-navigation feature. It is never
	// required: report it informationally so enabling without it installed reads as a clean
	// no-op rather than a missing dependency.
	cgPath, cgFound := FindCodegraph()
	reportCodegraph(out, CodegraphEnabled(), cgPath, cgFound)

	switch d.WSL {
	case "wsl2":
		fmt.Fprintln(out, "  runtime: WSL2 (supported)")
	case "wsl1":
		fmt.Fprintln(out, "  runtime: WSL1 detected — upgrade to WSL2 (process/cwd semantics are unreliable on WSL1)")
	}
	if d.Manager == "" {
		fmt.Fprintln(out, "  package manager: none detected")
	} else {
		fmt.Fprintf(out, "  package manager: %s\n", d.Manager)
	}

	// Partition missing tools into installable vs manual.
	var installable []Tool
	for _, t := range d.Missing {
		if t.Manual != "" || d.Manager == "" {
			continue
		}
		if _, ok := installSpec(d.Manager, t.Name); ok {
			installable = append(installable, t)
		}
	}

	for _, t := range d.Missing {
		if t.Manual != "" {
			fmt.Fprintf(out, "  action:  %s  (%s)\n", t.Manual, t.Name)
		}
	}

	fmt.Fprintln(out, "  tip: 'ttorch skills' adds recommended agent skills (e.g. axi)")

	if len(installable) == 0 && !wantITerm {
		fmt.Fprintln(out, "Nothing to auto-install.")
		return nil
	}

	fmt.Fprintln(out, "Will install:")
	for _, t := range installable {
		cmd, _ := installSpec(d.Manager, t.Name)
		fmt.Fprintf(out, "  %s  (%s)\n", strings.Join(cmd, " "), t.Name)
	}
	if wantITerm {
		cmd, _ := itermInstallCmd(d.Manager)
		fmt.Fprintf(out, "  %s  (%s)\n", strings.Join(cmd, " "), "iTerm2")
	}

	if !autoYes && !confirm(out, in, "Install these now? [Y/n] ") {
		fmt.Fprintln(out, "Skipped. Run the commands above yourself, or re-run with --yes.")
		return nil
	}

	for _, t := range installable {
		cmd, _ := installSpec(d.Manager, t.Name)
		fmt.Fprintf(out, "+ %s\n", strings.Join(cmd, " "))
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Stdout = out
		c.Stderr = out
		c.Stdin = os.Stdin
		if err := c.Run(); err != nil {
			return fmt.Errorf("install %s failed: %w", t.Name, err)
		}
	}
	if wantITerm {
		cmd, _ := itermInstallCmd(d.Manager)
		fmt.Fprintf(out, "+ %s\n", strings.Join(cmd, " "))
		c := exec.Command(cmd[0], cmd[1:]...)
		c.Stdout = out
		c.Stderr = out
		c.Stdin = os.Stdin
		if err := c.Run(); err != nil {
			// iTerm2 is optional: a failed install is a note, not a hard error.
			fmt.Fprintf(out, "  iTerm2 install failed (optional): %v\n", err)
		}
	}
	fmt.Fprintln(out, "Done.")
	return nil
}

func confirm(out io.Writer, in io.Reader, prompt string) bool {
	fmt.Fprint(out, prompt)
	r := bufio.NewReader(in)
	line, _ := r.ReadString('\n')
	line = strings.ToLower(strings.TrimSpace(line))
	return line == "" || line == "y" || line == "yes"
}

func detectManager() string {
	candidates := []string{"brew", "apt-get", "dnf", "pacman", "zypper", "apk"}
	for _, m := range candidates {
		if _, err := exec.LookPath(m); err == nil {
			return m
		}
	}
	return ""
}

// installSpec returns the command to install a tool with the given manager.
func installSpec(manager, tool string) ([]string, bool) {
	switch manager {
	case "brew":
		return []string{"brew", "install", tool}, true
	case "apt-get":
		return []string{"sudo", "apt-get", "install", "-y", tool}, true
	case "dnf":
		return []string{"sudo", "dnf", "install", "-y", tool}, true
	case "pacman":
		return []string{"sudo", "pacman", "-S", "--noconfirm", tool}, true
	case "zypper":
		return []string{"sudo", "zypper", "install", "-y", tool}, true
	case "apk":
		return []string{"sudo", "apk", "add", tool}, true
	}
	return nil, false
}

// itermInstallCmd returns the command to install iTerm2 via the given manager.
// iTerm2 is a Homebrew cask, so it is only installable when manager is "brew".
func itermInstallCmd(manager string) ([]string, bool) {
	if manager == "brew" {
		return []string{"brew", "install", "--cask", "iterm2"}, true
	}
	return nil, false
}

// wslKind classifies a /proc/version string as "wsl2", "wsl1", or "" (not WSL).
// WSL2 kernels carry "WSL2" in the version string; older/WSL1 kernels carry
// "Microsoft" without it.
func wslKind(procVersion string) string {
	v := strings.ToLower(procVersion)
	switch {
	case strings.Contains(v, "wsl2"):
		return "wsl2"
	case strings.Contains(v, "microsoft"), strings.Contains(v, "wsl"):
		return "wsl1"
	default:
		return ""
	}
}

func detectWSL() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return ""
	}
	return wslKind(string(b))
}
