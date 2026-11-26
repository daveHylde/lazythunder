package main

import (
	"embed"
	"io/fs"
)

//go:embed all:addon
var addonFiles embed.FS

// addonFS is the add-on's directory, manifest.json at its root.
func addonFS() fs.FS {
	sub, err := fs.Sub(addonFiles, "addon")
	if err != nil {
		panic(err) // "addon" is a constant path known to exist at build time
	}
	return sub
}
