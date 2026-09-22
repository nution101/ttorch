// Package ttorch embeds the managed content payload (skills, agents, commands,
// and global guidance) that the CLI lays down under the user's home directory.
//
// Keeping the payload embedded in the binary makes installs and updates atomic
// with the binary: a single downloaded executable carries everything it needs.
package ttorch

import (
	"embed"
	"io/fs"
)

// payload holds the managed tree rooted at "content/". Unexported on purpose: see Content.
//
//go:embed all:content
var payload embed.FS

// Content returns the embedded managed payload.
//
// It is a function and not an exported variable, because an exported variable is
// assignable. installer.ApplyEmbedded reads this at call time and walks "content" in
// whatever it gets, so while this was `var Content embed.FS` an init() in any package
// linked into the binary could reassign it and redirect the whole install: five lines and
// one markdown file, all of it in packages the trust gate does not cover, with nothing in
// the diff that the guard matches.
//
// Unexporting the variable closes that at the language level. It is a stronger control than
// scanning source for //go:embed directives, which is what guarded this before: a
// hand-rolled directive parser has to recognise every spelling that produces a usable FS
// (`all:cont*` and `"content"` both work, and both were missed), and that list is not
// bounded. There is now nothing to redirect, so the spelling does not matter.
//
// TestEmbeddedPayloadIsNotAssignable fails if an exported variable comes back.
func Content() fs.FS { return payload }
