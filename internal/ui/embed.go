// Package ui embeds the admin dashboard's HTML templates and static assets
// into the binary so the final build is a single self-contained file.
package ui

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:web
var assets embed.FS

// FS returns a sub-fs rooted at web/ (templates/, static/).
func FS() fs.FS {
	sub, err := fs.Sub(assets, "web")
	if err != nil {
		panic(err) // embed invariant: all:web always exists at compile time
	}
	return sub
}

// StaticHandler serves files under /static/* from the embedded FS.
// It is mounted on the admin router.
func StaticHandler() http.Handler {
	sub, err := fs.Sub(assets, "web/static")
	if err != nil {
		panic(err)
	}
	return http.StripPrefix("/admin/static/", http.FileServer(http.FS(sub)))
}

// Templates returns the embedded *template.Template root (templates/*.html).
// Wired up in step 7 when admin pages are introduced.