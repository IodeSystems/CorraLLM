// Package ui carries the built dashboard into the binary.
//
// internal/webui serves the SPA from a DIRECTORY, deliberately, so the UI can
// be swapped without rebuilding the daemon. That holds for a slot deploy, where
// the operator has the repo and a build toolchain to hand. It fails completely
// for a binary someone downloaded: --web-root points at ./ui/dist, no such
// directory exists, and every page load 404s — leaving no UI in which to do the
// setup the UI exists to do.
//
// So both. The on-disk root still wins when it has content; this is what serves
// when it does not.
package ui

import (
	"embed"
	"io/fs"
)

// dist is the Vite build output. The `all:` prefix matters: Vite emits no
// dotfiles today, but the default embed pattern silently skips names beginning
// with "." or "_", and a build that quietly ships incomplete is worse than one
// that fails.
//
// ui/dist is gitignored (it is a build artifact), so a committed .gitkeep keeps
// this pattern matching on a clean checkout — //go:embed is a COMPILE error
// when it matches nothing, which would make `go build` depend on having run
// pnpm first. `make dist` builds the UI before the binary, so a release embeds
// the real thing; a bare `go build` embeds only the placeholder, and Handler
// says so rather than serving 404s.
//
//go:embed all:dist
var dist embed.FS

// DistFS returns the embedded dashboard rooted at dist/.
func DistFS() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		// Unreachable: "dist" is embedded above, so the subtree exists.
		return dist
	}
	return sub
}
