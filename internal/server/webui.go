package server

import (
	"bytes"
	"compress/gzip"
	"embed"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
)

//go:embed all:webui/dist
var panelDist embed.FS

var panelAssets, _ = fs.Sub(panelDist, "webui/dist")

// 运行时 gzip：产物带内容哈希文件名，首次请求压缩一次后常驻内存，
// 之后直接回放缓存（产物换版后哈希变化，缓存键自然失效）。
var panelGzipCache sync.Map // name -> []byte

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
	// Vite 产物文件名带内容哈希，可长期缓存；index.html 之外全部 immutable。
	name := strings.TrimPrefix(r.URL.Path, "/assets/")
	name = path.Clean("/" + name)[1:]
	if name == "" || strings.Contains(name, "..") {
		http.NotFound(w, r)
		return
	}
	data, err := fs.ReadFile(panelAssets, path.Join("assets", name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	header := w.Header()
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		header.Set("Content-Type", ct)
	}
	header.Set("Cache-Control", "public, max-age=31536000, immutable")

	// Accept-Encoding 允许且体积值得压（>512B、文本类）时回放 gzip 缓存。
	if len(data) > 512 && acceptsGzip(r) && compressibleType(header.Get("Content-Type")) {
		if cached, ok := panelGzipCache.Load(name); ok {
			writeGzipped(w, header, cached.([]byte))
			return
		}
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(data); err == nil && zw.Close() == nil {
			gz := buf.Bytes()
			panelGzipCache.Store(name, gz)
			writeGzipped(w, header, gz)
			return
		}
	}
	header.Add("Vary", "Accept-Encoding")
	header.Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func writeGzipped(w http.ResponseWriter, header http.Header, gz []byte) {
	header.Set("Content-Encoding", "gzip")
	header.Add("Vary", "Accept-Encoding")
	header.Set("Content-Length", strconv.Itoa(len(gz)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(gz)
}

func acceptsGzip(r *http.Request) bool {
	for _, part := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		fields := strings.Split(strings.TrimSpace(part), ";")
		enc := strings.TrimSpace(fields[0])
		if enc != "gzip" && enc != "*" {
			continue
		}
		qZero := false
		for _, param := range fields[1:] {
			p := strings.TrimSpace(param)
			if strings.HasPrefix(p, "q=") && (p == "q=0" || p == "q=0.0" || p == "q=0.00" || p == "q=0.000") {
				qZero = true
			}
		}
		if !qZero {
			return true
		}
	}
	return false
}

func compressibleType(ct string) bool {
	return strings.HasPrefix(ct, "text/") ||
		strings.Contains(ct, "javascript") ||
		strings.Contains(ct, "json") ||
		strings.Contains(ct, "svg")
}
