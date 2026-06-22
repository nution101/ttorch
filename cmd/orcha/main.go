// Command orcha orchestrates a team of Claude Code agents.
//
// When invoked as "cc" (via a symlink), it dispatches to the `cc` subcommand —
// opening a Claude session attached to the orcha team.
package main

import (
	"os"
	"path/filepath"

	"github.com/nution101/orcha/internal/cli"
)

func main() {
	args := os.Args[1:]
	if filepath.Base(os.Args[0]) == "cc" {
		args = append([]string{"cc"}, args...)
	}
	os.Exit(cli.Main(args))
}
