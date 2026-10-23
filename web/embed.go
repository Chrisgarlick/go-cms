// Package web embeds the built admin assets (web/dist). Run `make assets`
// (esbuild) after changing web/src; CI checks dist is up to date.
package web

import (
	"embed"
	"io/fs"
)

//go:embed dist
var dist embed.FS

func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		panic(err)
	}
	return sub
}
