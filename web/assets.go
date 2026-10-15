// Package web embeds browser assets in the application binary.
package web

import (
	"embed"
	"net/http"
	"strings"
)

//go:embed static
var assets embed.FS

// StaticHandler serves embedded assets beneath /static/, with byte ranges
// for video. Go's built-in type table has no .mp4, and the production image
// has no system table, so the type is set here.
func StaticHandler() http.Handler {
	files := http.FileServer(http.FS(assets))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".mp4") {
			w.Header().Set("Content-Type", "video/mp4")
		}
		files.ServeHTTP(w, r)
	})
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
