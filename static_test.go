package asynqmon

import (
	"net/http/httptest"
	"testing"

	. "github.com/smartystreets/goconvey/convey"
)

func TestContentTypeByExt(t *testing.T) {
	Convey("Given the static asset MIME resolver", t, func() {

		Convey("When asked for web asset extensions", func() {
			Convey("Then .js maps to application/javascript", func() {
				So(contentTypeByExt(".js"), ShouldEqual, "application/javascript; charset=utf-8")
			})
			Convey("Then .css maps to text/css (not text/plain)", func() {
				So(contentTypeByExt(".css"), ShouldEqual, "text/css; charset=utf-8")
			})
			Convey("Then .svg maps to image/svg+xml", func() {
				So(contentTypeByExt(".svg"), ShouldEqual, "image/svg+xml")
			})
			Convey("Then .woff2 maps to font/woff2", func() {
				So(contentTypeByExt(".woff2"), ShouldEqual, "font/woff2")
			})
		})

		Convey("When the extension is uppercase", func() {
			Convey("Then it is matched case-insensitively", func() {
				So(contentTypeByExt(".CSS"), ShouldEqual, "text/css; charset=utf-8")
			})
		})

		Convey("When the extension is unknown", func() {
			Convey("Then it returns empty so the caller falls back to sniffing", func() {
				So(contentTypeByExt(".bin"), ShouldEqual, "")
				So(contentTypeByExt(""), ShouldEqual, "")
			})
		})
	})
}

func TestServeFileContentType(t *testing.T) {
	Convey("Given a uiAssetsHandler over the embedded build", t, func() {
		h := &uiAssetsHandler{
			rootPath:      "",
			contents:      staticContents,
			staticDirPath: "ui/build",
			indexFileName: "index.html",
		}
		req := httptest.NewRequest("GET", "/", nil)

		Convey("When serving an embedded .svg asset", func() {
			rec := httptest.NewRecorder()
			code, err := h.serveFile(rec, req, "/favicon.svg")

			Convey("Then it succeeds with the SVG content type", func() {
				So(err, ShouldBeNil)
				So(code, ShouldEqual, 200)
				So(rec.Header().Get("Content-Type"), ShouldEqual, "image/svg+xml")
			})
		})

		Convey("When serving the root path", func() {
			rec := httptest.NewRecorder()
			code, err := h.serveFile(rec, req, "/")

			Convey("Then it renders the index template with no-cache", func() {
				So(err, ShouldBeNil)
				So(code, ShouldEqual, 200)
				So(rec.Body.Len(), ShouldBeGreaterThan, 0)
				So(rec.Header().Get("Cache-Control"), ShouldEqual, "no-cache")
			})
		})

		Convey("When serving an extension-less path that does not exist", func() {
			rec := httptest.NewRecorder()
			code, err := h.serveFile(rec, req, "/queues/default/tasks")

			Convey("Then it falls back to the index file (SPA routing)", func() {
				So(err, ShouldBeNil)
				So(code, ShouldEqual, 200)
				So(rec.Body.Len(), ShouldBeGreaterThan, 0)
			})
		})

		Convey("When serving a missing file with an extension (e.g. a stale hashed chunk)", func() {
			rec := httptest.NewRecorder()
			code, err := h.serveFile(rec, req, "/does-not-exist.css")

			Convey("Then it returns 404 instead of the index page", func() {
				So(err, ShouldNotBeNil)
				So(code, ShouldEqual, 404)
			})
		})

		Convey("When the client accepts gzip for a compressible asset", func() {
			gzReq := httptest.NewRequest("GET", "/", nil)
			gzReq.Header.Set("Accept-Encoding", "gzip, deflate, br")
			rec := httptest.NewRecorder()
			code, err := h.serveFile(rec, gzReq, "/favicon.svg")

			Convey("Then the response is gzip-encoded with Vary set", func() {
				So(err, ShouldBeNil)
				So(code, ShouldEqual, 200)
				So(rec.Header().Get("Content-Encoding"), ShouldEqual, "gzip")
				So(rec.Header().Get("Vary"), ShouldEqual, "Accept-Encoding")
			})
		})
	})
}

func TestServeHTTPAPINotFound(t *testing.T) {
	Convey("Given a uiAssetsHandler mounted as the NotFoundHandler", t, func() {
		h := &uiAssetsHandler{
			rootPath:      "",
			contents:      staticContents,
			staticDirPath: "ui/build",
			indexFileName: "index.html",
		}

		Convey("When an unmatched /api path is requested", func() {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest("GET", "/api/nope", nil)
			h.ServeHTTP(rec, req)

			Convey("Then it returns a JSON 404, not the SPA index", func() {
				So(rec.Code, ShouldEqual, 404)
				So(rec.Header().Get("Content-Type"), ShouldStartWith, "application/json")
			})
		})
	})
}
