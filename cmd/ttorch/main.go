// Command ttorch orchestrates a team of Claude Code agents.
package main

import (
	"os"

	"github.com/nution101/ttorch/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:]))
}
