// Package themes embeds the built-in base theme that every site theme extends.
package themes

import (
	"embed"
	"io/fs"
)

//go:embed all:base
var embedded embed.FS

// Base returns the base theme's file system (layouts/, partials/, templates/, blocks/, assets/).
func Base() fs.FS {
	sub, err := fs.Sub(embedded, "base")
	if err != nil {
		panic(err)
	}
	return sub
}
