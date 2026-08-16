package dashboard

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"net/http"
	"strings"

	"github.com/BlueHeisenberg/kenward/internal/lang"
)

// assets holds the whole front end: templates and one stylesheet.
//
// Server-rendered HTML with no build step, and that is a requirement rather than a
// preference. This dashboard is the thing an operator reaches for when something is
// wrong; needing a Node toolchain to change a label on it would mean nobody ever does.
// The stylesheet is a file rather than a <style> block so the content security policy
// can forbid inline styles outright.
//
//go:embed templates/*.html static/style.css
var assets embed.FS

// pages is one parsed template set per page, built once at start.
//
// Per page rather than one set, because every page defines "content" and a single set
// would leave whichever one parsed last defining it for all of them — which fails as a
// wrong page rendered rather than as an error.
var pages = func() map[string]*template.Template {
	names, err := assets.ReadDir("templates")
	if err != nil {
		panic("dashboard: templates missing from the binary: " + err.Error())
	}
	out := map[string]*template.Template{}
	for _, e := range names {
		if e.Name() == "base.html" {
			continue
		}
		t := template.Must(template.New("base.html").Funcs(funcs).ParseFS(assets,
			"templates/base.html", "templates/"+e.Name()))
		out[strings.TrimSuffix(e.Name(), ".html")] = t
	}
	return out
}()

var funcs = template.FuncMap{
	"join": strings.Join,
	// languages is the list kenward's own messages are written in, taken from the
	// catalogue rather than typed into the two templates that quote it. A list in
	// prose drifts from a list in code the first time an eleventh table lands, and
	// the drift is invisible: the page goes on describing a product that has grown
	// past it.
	"languages": func() string {
		names := lang.EnglishNames()
		return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
	},
	"yesno": func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	},
}

// page is what every template is handed.
//
// One shape for every page, so the layout can render the navigation, the flash and the
// error without each handler remembering to supply them.
type page struct {
	Title string
	// Nav marks the current section so the layout can render it as the current page
	// for a screen reader as well as for an eye.
	Nav string
	// CSRF is this session's token. Every form in the layout's care writes it into a
	// hidden field; a page with a form and no token here renders a form that will be
	// refused, which is the failure direction worth having.
	CSRF string
	// Flash is a confirmation of something that just happened.
	Flash string
	// Error is what went wrong, in the operator's terms.
	Error string
	// SignedIn drives the navigation. The login and setup pages have none.
	SignedIn bool
	// Data is the page's own model.
	Data any
}

// render writes a page. A template failure is a 500 with nothing of the half-rendered
// page in it: html/template writes as it goes, so this buffers first rather than
// emitting a truncated document with a status of 200 on it.
func (s *Server) render(w http.ResponseWriter, status int, name string, p page) {
	t, ok := pages[name]
	if !ok {
		s.deps.logger().Error("dashboard", "event", "template_missing", "name", name)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "base.html", p); err != nil {
		s.deps.logger().Error("dashboard", "event", "template_failed", "name", name, "err", err.Error())
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}

// handleCSS serves the one stylesheet.
func (s *Server) handleCSS(w http.ResponseWriter, _ *http.Request) {
	data, err := assets.ReadFile("static/style.css")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	// Overrides the no-store the security wrapper sets: a stylesheet is not a page,
	// and it is embedded in the binary so it only changes when the binary does.
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// redirectWith sends the browser to path with a message, so a POST that succeeded does
// not leave a resubmittable form in the history.
//
// The message travels in the query string, which is safe here for the reason it is not
// safe for a token: it is prose this package wrote, it is not a credential, and the one
// rule about URLs in this dashboard — no secrets in them after setup — is about the
// claim code and the setup token, neither of which is ever redirected with.
func redirectWith(w http.ResponseWriter, r *http.Request, path, flash string) {
	if flash != "" {
		path += "?ok=" + template.URLQueryEscaper(flash)
	}
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// flashOf reads a redirect's message back.
func flashOf(r *http.Request) string { return r.URL.Query().Get("ok") }

// humanBytes is not needed; humanCount renders "1 member" / "3 members" for the
// summaries, because "1 members" reads as a bug in everything else on the page.
func humanCount(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", one)
	}
	return fmt.Sprintf("%d %s", n, many)
}
