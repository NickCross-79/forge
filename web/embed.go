// Package web embeds the built dashboard so `forge serve` is a single binary
// with no second process and no files to deploy alongside it.
//
// The dist directory is a build artifact: in a fresh checkout it holds only a
// .gitkeep placeholder, and `make web` populates it. That placeholder is what
// makes this compile before the frontend has ever been built — go:embed fails
// outright on a missing directory. At runtime the API notices the absent
// index.html and serves a short page explaining how to build it.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Dist returns the built dashboard rooted at the directory containing index.html.
func Dist() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}

// Built reports whether a real dashboard build is embedded.
func Built() bool {
	sub, err := Dist()
	if err != nil {
		return false
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return false
	}
	return true
}
