package main

import (
	"html"
	"net/http"
)

// homePage stands in for a product UI. Factory mounts it after /v1, so an
// unknown API path is still a JSON 404. Mount a built SPA here instead (the
// browser-app starter embeds one; @looprig/client and @looprig/react are the
// browser libraries).
func homePage() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>stack example</title>
<body style="font-family:system-ui;max-width:36rem;margin:4rem auto">
<h1>Looprig stack example</h1>
<p>The Factory API is under <code>` + html.EscapeString("/v1") + `</code>. <a href="/dev/login">Sign in (development only)</a>.</p></body>`))
	})
}
