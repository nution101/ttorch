// Package ttorch embeds the managed content payload (skills, agents, commands,
// and global guidance) that the CLI lays down under the user's home directory.
//
// Keeping the payload embedded in the binary makes installs and updates atomic
// with the binary: a single downloaded executable carries everything it needs.
package ttorch

import "embed"

// Content holds the managed payload tree rooted at "content/".
//
//go:embed all:content
var Content embed.FS
