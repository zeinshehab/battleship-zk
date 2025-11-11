package web

import (
	"embed"
	"io/fs"
	"net/http"
)

var content embed.FS

func FS() http.FileSystem {
	sub, _ := fs.Sub(content, ".")
	return http.FS(sub)
}
