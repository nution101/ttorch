package selfupdate

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// ReplaceExecutable atomically replaces the binary at target with newBytes.
//
// It resolves symlinks first so a PATH symlink (e.g. ~/.local/bin/orcha ->
// ~/.orcha/bin/orcha) is followed and the REAL binary is rewritten. The new
// bytes are written to a temp file in the same directory (so os.Rename is an
// atomic same-filesystem swap) with the original permissions preserved.
//
// On macOS, in-place overwrite of a running binary is unreliable, so a failed
// rename is a hard error directing the user to a user-owned install dir. On
// other platforms it falls back to a truncating overwrite.
func ReplaceExecutable(target string, newBytes []byte) error {
	real := target
	if resolved, err := filepath.EvalSymlinks(target); err == nil && resolved != "" {
		real = resolved
	}
	dir := filepath.Dir(real)

	perm := os.FileMode(0o755)
	if fi, err := os.Stat(real); err == nil {
		perm = fi.Mode().Perm()
	}

	tmp, err := os.CreateTemp(dir, ".orcha-update-*")
	if err != nil {
		return fmt.Errorf("create temp in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(newBytes); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}

	if err := os.Rename(tmpName, real); err != nil {
		os.Remove(tmpName)
		if runtime.GOOS == "darwin" {
			return fmt.Errorf("cannot atomically replace %s: %w; reinstall orcha into a user-owned dir (e.g. ~/.orcha/bin)", real, err)
		}
		if werr := os.WriteFile(real, newBytes, perm); werr != nil {
			return werr
		}
	}

	removeQuarantine(real)
	return nil
}
