//go:build darwin

package selfupdate

import "os/exec"

// removeQuarantine clears the com.apple.quarantine xattr so Gatekeeper does not
// block a freshly downloaded binary. Errors are ignored (best effort).
func removeQuarantine(path string) {
	_ = exec.Command("xattr", "-d", "com.apple.quarantine", path).Run()
}
