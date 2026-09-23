package cli

import (
	"strings"
	"testing"
)

func TestCmdBoardTakesNoArguments(t *testing.T) {
	err := cmdBoard([]string{"extra"})
	if err == nil || !strings.Contains(err.Error(), "usage: ttorch board") {
		t.Fatalf("cmdBoard(extra) = %v, want the usage error", err)
	}
}
