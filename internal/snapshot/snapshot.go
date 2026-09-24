// Package snapshot exports the topology as a single self-contained HTML
// file: the real UI bundle plus a baked-in data snapshot and a small shim
// that answers the UI's API calls from that snapshot. The file needs no
// server — open it from disk, click through every view, hand it to a
// colleague. The UI renders it in "viewer" mode: topology only, no logs.
//
// Both the topology data and the JS bundle are packed (XOR+base64,
// decoded at runtime): a deterrent against casually lifting the data or
// the canvas code out of the file, not cryptography.
package snapshot

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/LogDoc-org/logdoc/internal/graph"
	"github.com/LogDoc-org/logdoc/internal/model"
)

var (
	reJS   = regexp.MustCompile(`src="(/assets/[^"]+\.js)"`)
	reCSS  = regexp.MustCompile(`href="(/assets/[^"]+\.css)"`)
	reFont = regexp.MustCompile(`url\((/fonts/[^)]+)\)`)
)

const obfKey = "ld-snapshot-v2"

// obf packs bytes so they are not plain text in the exported file.
func obf(data []byte) string {
	out := make([]byte, len(data))
	for i, b := range data {
		out[i] = b ^ obfKey[i%len(obfKey)]
	}
	return base64.StdEncoding.EncodeToString(out)
}

// NewHandler — GET /api/v1/topology/snapshot?window=1h&title=WB+Ads
func NewHandler(m *graph.Manager, c *graph.Catalog, dist fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		window := time.Hour
		if v := r.URL.Query().Get("window"); v != "" {
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 || d > 24*time.Hour {
				http.Error(w, `{"error":"invalid window (want 1s..24h)"}`, http.StatusBadRequest)
				return
			}
			window = d
		}
		title := r.URL.Query().Get("title")

		topo, err := m.Topology(r.Context(), model.DefaultTenant, window)
		if err != nil {
			http.Error(w, `{"error":"topology unavailable"}`, http.StatusInternalServerError)
			return
		}
		if topo.Nodes == nil {
			topo.Nodes = []graph.Node{}
		}
		if topo.Edges == nil {
			topo.Edges = []graph.Edge{}
		}
		metas, err := c.List(r.Context(), model.DefaultTenant)
		if err != nil || metas == nil {
			metas = []graph.ServiceMeta{}
		}

		idx, err := fs.ReadFile(dist, "index.html")
		if err != nil {
			http.Error(w, `{"error":"ui assets unavailable"}`, http.StatusInternalServerError)
			return
		}
		jsMatch := reJS.FindSubmatch(idx)
		cssMatch := reCSS.FindSubmatch(idx)
		if jsMatch == nil || cssMatch == nil {
			http.Error(w, `{"error":"ui assets unavailable"}`, http.StatusInternalServerError)
			return
		}
		js, err1 := fs.ReadFile(dist, strings.TrimPrefix(string(jsMatch[1]), "/"))
		css, err2 := fs.ReadFile(dist, strings.TrimPrefix(string(cssMatch[1]), "/"))
		if err1 != nil || err2 != nil {
			http.Error(w, `{"error":"ui assets unavailable"}`, http.StatusInternalServerError)
			return
		}
		// Fonts become data URIs so the file renders identically offline.
		cssStr := reFont.ReplaceAllStringFunc(string(css), func(m string) string {
			path := strings.TrimSuffix(strings.TrimPrefix(m, "url("), ")")
			data, ferr := fs.ReadFile(dist, strings.TrimPrefix(path, "/"))
			if ferr != nil {
				return m
			}
			mime := "font/ttf"
			if strings.HasSuffix(path, ".woff2") {
				mime = "font/woff2"
			}
			return "url(data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(data) + ")"
		})
		css = []byte(cssStr)

		// The header logo becomes a data URI so the file works offline.
		jsStr := string(js)
		if logo, err := fs.ReadFile(dist, "logo.svg"); err == nil {
			uri := "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(logo)
			jsStr = strings.ReplaceAll(jsStr, `"/logo.svg"`, `"`+uri+`"`)
		}

		snap, err := json.Marshal(map[string]any{
			"topology":  topo,
			"catalog":   metas,
			"title":     title,
			"window":    window.String(),
			"generated": time.Now().UTC().Format(time.RFC3339),
		})
		if err != nil {
			http.Error(w, `{"error":"snapshot marshal failed"}`, http.StatusInternalServerError)
			return
		}

		pageTitle := "LogDoc snapshot"
		if title != "" {
			pageTitle = title + " · LogDoc"
		}

		var b strings.Builder
		b.WriteString("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"UTF-8\" />\n")
		b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1.0\" />\n")
		b.WriteString("<meta name=\"generator\" content=\"LogDoc — logdoc.org\" />\n")
		b.WriteString("<title>")
		b.WriteString(htmlEscape(pageTitle))
		b.WriteString("</title>\n<style>\n")
		b.Write(css)
		b.WriteString("\n</style>\n</head>\n<body>\n<div id=\"root\"></div>\n<script>\n")
		b.WriteString("var __D1=\"")
		b.WriteString(obf(snap))
		b.WriteString("\";\nvar __D2=\"")
		b.WriteString(obf([]byte(jsStr)))
		b.WriteString("\";\n")
		b.WriteString(shim)
		b.WriteString("\n</script>\n</body>\n</html>\n")

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Disposition",
			fmt.Sprintf(`attachment; filename="logdoc-snapshot-%s.html"`, time.Now().Format("2006-01-02-1504")))
		_, _ = w.Write([]byte(b.String()))
	})
}

func htmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
	return r.Replace(s)
}

// shim unpacks the data and the UI bundle, answers the UI's API calls from
// the snapshot, and disarms everything that needs a live server.
const shim = `(function () {
  var K = "ld-snapshot-v2";
  function dec(s) {
    var b = atob(s), n = b.length, u = new Uint8Array(n);
    for (var i = 0; i < n; i++) u[i] = b.charCodeAt(i) ^ K.charCodeAt(i % K.length);
    return new TextDecoder("utf-8").decode(u);
  }
  var snap = JSON.parse(dec(__D1));
  window.__SNAP__ = snap;
  var ok = function (obj) {
    return Promise.resolve(new Response(JSON.stringify(obj), {
      status: 200, headers: { "Content-Type": "application/json" },
    }));
  };
  window.fetch = function (url) {
    var u = String(url);
    if (u.indexOf("/api/v1/topology?") >= 0) return ok(snap.topology);
    if (u.indexOf("/api/v1/catalog") >= 0) return ok({ services: snap.catalog });
    if (u.indexOf("/api/v1/deploys") >= 0) return ok({ deploys: [] });
    if (u.indexOf("/api/v1/topology/diff") >= 0)
      return ok({ new_services: [], silent_services: [], new_edges: [], silent_edges: [], error_jumps: [], deploys: [] });
    if (u.indexOf("/api/v1/auth/me") >= 0) return ok({ mode: "open", role: "member" });
    if (u.indexOf("/api/v1/query") >= 0) return ok({ entries: [], count: 0, took_ms: 0 });
    return ok({});
  };
  window.WebSocket = function () { this.close = function () {}; };
  var hp = history.pushState.bind(history), hr = history.replaceState.bind(history);
  history.pushState = function (a, b, c) { try { hp(a, b, c); } catch (e) {} };
  history.replaceState = function (a, b, c) { try { hr(a, b, c); } catch (e) {} };
  try { location.hash = "topology"; } catch (e) {}
  var el = document.createElement("script");
  el.type = "module";
  el.textContent = dec(__D2);
  document.body.appendChild(el);
})();`
