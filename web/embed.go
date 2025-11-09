package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html styles.css app.js
var content embed.FS

// FS returns a http.FileSystem that serves the embedded GUI files.
func FS() http.FileSystem {
	sub, _ := fs.Sub(content, ".")
	return http.FS(sub)
}
