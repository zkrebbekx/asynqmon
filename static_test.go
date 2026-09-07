package asynqmon

import (
	"net/http/httptest"
	"strings"
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

func TestIndexAssetURLsHonorRootPath(t *testing.T) {
	Convey("Given the embedded index rendered under a RootPath", t, func() {
		h := &uiAssetsHandler{
			rootPath:      "/monitoring",
			contents:      staticContents,
			staticDirPath: "ui/build",
			indexFileName: "index.html",
		}
		rec := httptest.NewRecorder()
		So(h.renderIndexFile(rec), ShouldBeNil)
		body := rec.Body.String()

		Convey("Then every asset URL is prefixed with the root path", func() {
			// The Vite migration once hardcoded /assets/... here, which made
			// library embedding (README RootPath examples) render a blank
			// page: assets resolved outside the PathPrefix router.
			So(body, ShouldContainSubstring, `src="/monitoring/assets/`)
			So(body, ShouldContainSubstring, `href="/monitoring/assets/`)
			So(body, ShouldContainSubstring, `href="/monitoring/favicon.svg"`)
			So(body, ShouldNotContainSubstring, `src="/assets/`)
			So(body, ShouldNotContainSubstring, `href="/assets/`)
			// html/template's JS-string context escapes "/" as "\/".
			So(body, ShouldContainSubstring, `window.ROOT_PATH = "\/monitoring"`)
		})
	})

	Convey("Given the embedded index rendered with no RootPath", t, func() {
		h := &uiAssetsHandler{
			rootPath:      "",
			contents:      staticContents,
			staticDirPath: "ui/build",
			indexFileName: "index.html",
		}
		rec := httptest.NewRecorder()
		So(h.renderIndexFile(rec), ShouldBeNil)
		body := rec.Body.String()

		Convey("Then asset URLs are root-relative with no template residue", func() {
			So(body, ShouldContainSubstring, `src="/assets/`)
			// The dev-fallback guard legitimately contains the literal
			// includes("[[") probe; unrendered actions would carry the
			// full delimiter+field form.
			So(body, ShouldNotContainSubstring, `[[.RootPath`)
		})
	})
}

// Security headers on the SPA and the API (#34). The dashboard has no login
// and renders untrusted task payloads, so every response carries the
// clickjacking, sniffing, referrer, and CSP guards. Requests run
// imperatively before the Convey tree (the repo's goconvey discipline); the
// tree only reads captured results.
func TestSecurityHeadersOnSPAAndAPI(t *testing.T) {
	h := &uiAssetsHandler{
		rootPath:      "",
		contents:      staticContents,
		staticDirPath: "ui/build",
		indexFileName: "index.html",
	}

	indexRec := httptest.NewRecorder()
	h.ServeHTTP(indexRec, httptest.NewRequest("GET", "/", nil))

	assetRec := httptest.NewRecorder()
	h.ServeHTTP(assetRec, httptest.NewRequest("GET", "/favicon.svg", nil))

	// An unmatched /api path is answered by the same handler; it must carry
	// the headers too.
	apiRec := httptest.NewRecorder()
	h.ServeHTTP(apiRec, httptest.NewRequest("GET", "/api/queues", nil))

	// A second index render must not reuse the first nonce.
	secondRec := httptest.NewRecorder()
	h.ServeHTTP(secondRec, httptest.NewRequest("GET", "/", nil))

	nonceOf := func(rec *httptest.ResponseRecorder) string {
		csp := rec.Header().Get("Content-Security-Policy")
		_, after, ok := strings.Cut(csp, "'nonce-")
		if !ok {
			return ""
		}
		v, _, _ := strings.Cut(after, "'")
		return v
	}

	Convey("Given the asynqmon SPA handler (#34)", t, func() {
		for name, rec := range map[string]*httptest.ResponseRecorder{
			"the index page": indexRec, "a static asset": assetRec, "an API path": apiRec,
		} {
			Convey("When "+name+" is served", func() {
				Convey("Then it carries the sniffing and framing guards", func() {
					So(rec.Header().Get("X-Content-Type-Options"), ShouldEqual, "nosniff")
					So(rec.Header().Get("X-Frame-Options"), ShouldEqual, "DENY")
					So(rec.Header().Get("Referrer-Policy"), ShouldEqual, "same-origin")
				})
				Convey("Then it carries a CSP that forbids framing", func() {
					csp := rec.Header().Get("Content-Security-Policy")
					So(csp, ShouldContainSubstring, "default-src 'self'")
					So(csp, ShouldContainSubstring, "frame-ancestors 'none'")
					So(csp, ShouldContainSubstring, "connect-src 'self'")
					So(csp, ShouldContainSubstring, "style-src 'self' 'unsafe-inline'")
				})
			})
		}

		Convey("When the index page is rendered", func() {
			body := indexRec.Body.String()
			nonce := nonceOf(indexRec)

			Convey("Then the CSP allows the inline bootstrap script by nonce", func() {
				So(nonce, ShouldNotBeEmpty)
				So(indexRec.Header().Get("Content-Security-Policy"), ShouldContainSubstring, "script-src 'self' 'nonce-"+nonce+"'")
			})
			Convey("Then the inline script tag carries the same nonce", func() {
				So(body, ShouldContainSubstring, `<script nonce="`+nonce+`">`)
				// No bare inline <script> is left; it would be blocked.
				So(body, ShouldNotContainSubstring, "<script>")
			})
			Convey("Then the module bundle tags are untouched", func() {
				So(body, ShouldContainSubstring, `<script type="module"`)
			})
			Convey("Then the page still serves as HTML that must be revalidated", func() {
				So(indexRec.Code, ShouldEqual, 200)
				So(indexRec.Header().Get("Content-Type"), ShouldContainSubstring, "text/html")
				So(indexRec.Header().Get("Cache-Control"), ShouldEqual, "no-cache")
			})
		})

		Convey("When the index page is rendered twice", func() {
			Convey("Then each response gets a fresh nonce", func() {
				So(nonceOf(secondRec), ShouldNotBeEmpty)
				So(nonceOf(secondRec), ShouldNotEqual, nonceOf(indexRec))
			})
		})
	})
}
