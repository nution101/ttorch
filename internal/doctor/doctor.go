// Package doctor detects the tools orcha needs (tmux, git, gh, claude) and,
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
)

// Tool is an external dependency orcha relies on.
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
		{Name: "claude", Bin: "claude", Why: "the coding agent orcha orchestrates", Required: true,
			Manual: "install Claude Code, e.g.  npm install -g @anthropic-ai/claude-code"},
	}
}

// Diagnosis is the result of inspecting the environment.
type Diagnosis struct {
	Found   map[string]string // tool name -> path
	Missing []Tool
	Manager string // detected package manager (empty if none)
	WSL     bool
}

// Diagnose inspects the environment without changing anything.
func Diagnose() Diagnosis {
	d := Diagnosis{Found: map[string]string{}, Manager: detectManager(), WSL: isWSL()}
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

	fmt.Fprintln(out, "orcha doctor")
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
	if d.WSL {
		fmt.Fprintln(out, "  runtime: WSL detected (Linux paths)")
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

	if len(installable) == 0 {
		fmt.Fprintln(out, "Nothing to auto-install.")
		return nil
	}

	fmt.Fprintln(out, "Will install:")
	for _, t := range installable {
		cmd, _ := installSpec(d.Manager, t.Name)
		fmt.Fprintf(out, "  %s  (%s)\n", strings.Join(cmd, " "), t.Name)
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

func isWSL() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	b, err := os.ReadFile("/proc/version")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(b)), "microsoft")
}
