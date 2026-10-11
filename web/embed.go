package web

import (
	"embed"
	"net/http"
)

// staticFiles contains the browser client served by Handler.
//
//go:embed index.html styles.css app.js
var staticFiles embed.FS

// Handler returns an http.Handler for the embedded test UI.
func Handler() http.Handler {
	return http.FileServer(http.FS(staticFiles))
}
