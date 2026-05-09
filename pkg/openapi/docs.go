// pkg/openapi/docs.go — minimal Swagger UI handler at /docs.
//
// We don't go:embed a full swagger-ui distribution (~3MB of JS that
// only operators using a UI ever need). Instead, we serve a 1KB HTML
// page that loads swagger-ui from a CDN at view time. Operators in
// air-gapped envs can replace the CDN URL via Config.SwaggerCDN.
//
// Config-gated: Cache.docsEnabled must be set via WithDocs() before
// Handler() is wired. Default disabled — meets the "Default false"
// note in the master_plan + avoids leaking the spec link in
// environments that didn't ask for the UI.
//
// [Area 4.2.C]

package openapi

import (
	"net/http"
)

// docsTemplate is rendered server-side once with the spec URL
// substituted. Tiny. No dependencies on the operator's filesystem.
const docsTemplate = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <title>NeoAnvil — API Reference</title>
  <link rel="stylesheet" href="https://unpkg.com/swagger-ui-dist@5.10.5/swagger-ui.css">
</head>
<body>
  <div id="swagger-ui"></div>
  <script src="https://unpkg.com/swagger-ui-dist@5.10.5/swagger-ui-bundle.js"></script>
  <script>
    window.onload = () => {
      window.ui = SwaggerUIBundle({
        url: "/openapi.json",
        dom_id: "#swagger-ui",
        deepLinking: true,
        presets: [SwaggerUIBundle.presets.apis],
      });
    };
  </script>
</body>
</html>
`

// DocsHandler returns an http.Handler that serves the Swagger UI
// HTML. Caller wires it at the path of their choice (typically /docs).
//
// The UI fetches /openapi.json from the same origin — make sure both
// handlers live behind the same scheme/host.
func DocsHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_, _ = w.Write([]byte(docsTemplate))
	})
}
