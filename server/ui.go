package main

import (
	_ "embed"
	"net/http"
)

//go:embed web/index.html
var uiHTML []byte

// serveUI serves the embedded dashboard page. It is intentionally
// exempt from token auth: the page shell contains no secrets, and the
// browser attaches the X-Auth-Token header (entered in the UI) to the
// data fetches it makes to /clients, /jobs, and /health.
func serveUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(uiHTML)
}
