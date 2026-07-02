package asynqmon

import (
	"bytes"
	"compress/gzip"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
)

// uiAssetsHandler is a http.Handler.
// The path to the static file directory and
// the path to the index file within that static directory are used to
// serve the SPA.
type uiAssetsHandler struct {
	rootPath       string
	contents       embed.FS
	staticDirPath  string
	indexFileName  string
	prometheusAddr string
	readOnly       bool

	// index template is parsed once and reused across requests.
	tmplOnce     sync.Once
	indexTmpl    *template.Template
	indexTmplErr error

	// gzipCache holds gzipped copies of embedded assets, compressed once on
	// first request (the embedded contents never change at runtime).
	gzipCache sync.Map // string -> []byte
}

// ServeHTTP inspects the URL path to locate a file within the static dir
// on the SPA handler.
// If path '/' is requested, it will serve the index file, otherwise it will
// serve the file specified by the URL path.
func (h *uiAssetsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Get the absolute path to prevent directory traversal.
	path, err := filepath.Abs(r.URL.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Get the path relative to the root path.
	if !strings.HasPrefix(path, h.rootPath) {
		http.Error(w, "unexpected path prefix", http.StatusBadRequest)
		return
	}
	path = strings.TrimPrefix(path, h.rootPath)

	// Unmatched API routes must return a JSON 404, not the SPA index page —
	// otherwise the frontend gets HTML with a 200 and fails with a cryptic
	// JSON parse error.
	if path == "/api" || strings.HasPrefix(path, "/api/") {
		writeErrorMsg(w, http.StatusNotFound, "api endpoint not found")
		return
	}

	if code, err := h.serveFile(w, r, path); err != nil {
		http.Error(w, err.Error(), code)
		return
	}
}

func (h *uiAssetsHandler) indexFilePath() string {
	return filepath.Join(h.staticDirPath, h.indexFileName)
}

func (h *uiAssetsHandler) indexTemplate() (*template.Template, error) {
	h.tmplOnce.Do(func() {
		// Note: Replace the default delimiter ("{{") with a custom one
		// since webpack escapes the '{' character when it compiles the index.html file.
		// See the "homepage" field in package.json.
		h.indexTmpl, h.indexTmplErr = template.New(h.indexFileName).
			Delims("/[[", "]]").
			ParseFS(h.contents, h.indexFilePath())
	})
	return h.indexTmpl, h.indexTmplErr
}

func (h *uiAssetsHandler) renderIndexFile(w http.ResponseWriter) error {
	tmpl, err := h.indexTemplate()
	if err != nil {
		return err
	}
	data := struct {
		RootPath       string
		PrometheusAddr string
		ReadOnly       bool
	}{
		RootPath:       h.rootPath,
		PrometheusAddr: h.prometheusAddr,
		ReadOnly:       h.readOnly,
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// The index references content-hashed assets; it must be revalidated on
	// every load so redeploys are picked up immediately.
	w.Header().Set("Cache-Control", "no-cache")
	return tmpl.Execute(w, data)
}

// serveFile writes file requested at path and returns http status code and error if any.
// If requested path is root, it serves the index file.
// Otherwise, it looks for the file requested in the static content filesystem
// and serves it if found.
// Missing extension-less paths are SPA routes and get the index file; missing
// paths with a file extension (e.g. a stale content-hashed chunk after a
// redeploy) get a 404 so browsers don't try to execute HTML as a JS module.
func (h *uiAssetsHandler) serveFile(w http.ResponseWriter, r *http.Request, path string) (code int, err error) {
	if path == "/" || path == "" {
		if err := h.renderIndexFile(w); err != nil {
			return http.StatusInternalServerError, err
		}
		return http.StatusOK, nil
	}
	assetPath := filepath.Join(h.staticDirPath, path)
	contents, err := h.contents.ReadFile(assetPath)
	if err != nil {
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
			if filepath.Ext(path) != "" {
				return http.StatusNotFound, fmt.Errorf("file not found: %s", path)
			}
			if err := h.renderIndexFile(w); err != nil {
				return http.StatusInternalServerError, err
			}
			return http.StatusOK, nil
		}
		return http.StatusInternalServerError, err
	}
	// Set the MIME type explicitly by file extension. http.DetectContentType
	// uses https://mimesniff.spec.whatwg.org/ which, for security reasons, will
	// not recognize text/css or application/javascript from content sniffing —
	// browsers then refuse to apply stylesheets/modules served as text/plain.
	ct := contentTypeByExt(filepath.Ext(assetPath))
	if ct == "" {
		ct = http.DetectContentType(contents)
	}
	w.Header().Set("Content-Type", ct)

	// Vite emits content-hashed filenames under /assets; they can be cached forever.
	if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}

	if compressibleContentType(ct) && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		if gz, gzErr := h.gzipped(assetPath, contents); gzErr == nil {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			contents = gz
		}
	}

	if _, err := w.Write(contents); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
}

// gzipped returns the gzipped bytes for the given embedded asset, compressing
// once and caching the result.
func (h *uiAssetsHandler) gzipped(path string, raw []byte) ([]byte, error) {
	if v, ok := h.gzipCache.Load(path); ok {
		return v.([]byte), nil
	}
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	b := buf.Bytes()
	h.gzipCache.Store(path, b)
	return b, nil
}

// compressibleContentType reports whether the content type benefits from gzip
// (text-based formats; images and woff fonts are already compressed).
func compressibleContentType(ct string) bool {
	switch {
	case strings.HasPrefix(ct, "application/javascript"),
		strings.HasPrefix(ct, "text/css"),
		strings.HasPrefix(ct, "text/html"),
		strings.HasPrefix(ct, "application/json"),
		strings.HasPrefix(ct, "image/svg+xml"),
		strings.HasPrefix(ct, "text/plain"):
		return true
	default:
		return false
	}
}

// contentTypeByExt returns a fixed Content-Type for known web asset extensions,
// or "" to fall back to content sniffing.
func contentTypeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".js", ".mjs":
		return "application/javascript; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".html":
		return "text/html; charset=utf-8"
	case ".json", ".map":
		return "application/json; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".ico":
		return "image/x-icon"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	default:
		return ""
	}
}
