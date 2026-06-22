// Package buildinfo exposes version metadata injected at build time via -ldflags.
package buildinfo

import (
	"fmt"
	"runtime/debug"
)

// These are overridden at build time, e.g.:
//
//	go build -ldflags "-X github.com/nution101/ttorch/internal/buildinfo.Version=v0.1.0"
var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)

// CurrentVersion returns the build version, falling back to VCS info from the
// module build when ldflags were not supplied (e.g. `go run`).
func CurrentVersion() string {
	if Version != "dev" && Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "vcs.revision" && s.Value != "" {
				rev := s.Value
				if len(rev) > 12 {
					rev = rev[:12]
				}
				return "dev+" + rev
			}
		}
	}
	return Version
}

// String returns a human-readable one-line build summary.
func String() string {
	return fmt.Sprintf("ttorch %s (commit %s, built %s)", CurrentVersion(), Commit, Date)
}
