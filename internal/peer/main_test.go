package peer

import (
	"os"
	"testing"

	"github.com/nution101/ttorch/internal/tmuxtest"
)

// TestMain runs the package's tests against a tmux server of their own, so none of them can
// open, read or kill anything on the caller's server.
func TestMain(m *testing.M) { os.Exit(tmuxtest.Run(m)) }
