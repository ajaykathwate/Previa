// Package web embeds browser assets in the application binary.
package web

import (
	"embed"
	"net/http"
)

//go:embed static
var assets embed.FS

// StaticHandler serves embedded assets beneath /static/.
func StaticHandler() http.Handler {
	return http.FileServer(http.FS(assets))
}
