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

// FaviconHandler answers /favicon.ico, which browsers and tools request
// without reading a page, with the 32 px PNG icon. Pages link the SVG icon.
func FaviconHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		icon, err := assets.ReadFile("static/brand/favicon-32.png")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(icon)
	})
}
