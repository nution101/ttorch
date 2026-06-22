//go:build !darwin

package selfupdate

// removeQuarantine is a no-op outside macOS.
func removeQuarantine(path string) {}
