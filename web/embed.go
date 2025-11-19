package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed index.html app.js styles.css
var content embed.FS

func FS() http.FileSystem {
	sub, _ := fs.Sub(content, ".")
	return http.FS(sub)
}
