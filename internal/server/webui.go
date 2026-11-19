package server

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed all:webui/dist
var panelDist embed.FS

var panelAssets, _ = fs.Sub(panelDist, "webui/dist")
var panelAssetsRoot, _ = fs.Sub(panelAssets, "assets")

func (h *Handler) servePanel(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/admin" && r.URL.Path != "/panel" && r.URL.Path != "/panel/" {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(panelAssets, "index.html")
	if err != nil {
		http.Error(w, "panel not built", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

func (h *Handler) servePanelAssets(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=3600")
	http.StripPrefix("/assets/", http.FileServer(http.FS(panelAssetsRoot))).ServeHTTP(w, r)
}
