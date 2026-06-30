package asynqmon

import (
	"embed"
	"errors"
	"html/template"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
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

	if code, err := h.serveFile(w, path); err != nil {
		http.Error(w, err.Error(), code)
		return
	}
}

func (h *uiAssetsHandler) indexFilePath() string {
	return filepath.Join(h.staticDirPath, h.indexFileName)
}

func (h *uiAssetsHandler) renderIndexFile(w http.ResponseWriter) error {
	// Note: Replace the default delimiter ("{{") with a custom one
	// since webpack escapes the '{' character when it compiles the index.html file.
	// See the "homepage" field in package.json.
	tmpl, err := template.New(h.indexFileName).Delims("/[[", "]]").ParseFS(h.contents, h.indexFilePath())
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
	return tmpl.Execute(w, data)
}

// serveFile writes file requested at path and returns http status code and error if any.
// If requested path is root, it serves the index file.
// Otherwise, it looks for file requiested in the static content filesystem
// and serves if a file is found.
// If a requested file is not found in the filesystem, it serves the index file to
// make sure when user refreshes the page in SPA things still work.
func (h *uiAssetsHandler) serveFile(w http.ResponseWriter, path string) (code int, err error) {
	if path == "/" || path == "" {
		if err := h.renderIndexFile(w); err != nil {
			return http.StatusInternalServerError, err
		}
		return http.StatusOK, nil
	}
	path = filepath.Join(h.staticDirPath, path)
	bytes, err := h.contents.ReadFile(path)
	if err != nil {
		// If path is error (e.g. file not exist, path is a directory), serve index file.
		var pathErr *fs.PathError
		if errors.As(err, &pathErr) {
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
	if ct := contentTypeByExt(filepath.Ext(path)); ct != "" {
		w.Header().Add("Content-Type", ct)
	} else {
		w.Header().Add("Content-Type", http.DetectContentType(bytes))
	}

	if _, err := w.Write(bytes); err != nil {
		return http.StatusInternalServerError, err
	}
	return http.StatusOK, nil
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
