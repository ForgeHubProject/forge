// Package webdiff serves a locally-computed semantic diff to a loopback web
// page, rendered by an FHR renderer bundle. It is the local, zero-server
// equivalent of viewing a diff on ForgeHub: forge computes the StructuredDiff
// with the installed handler, then serves the renderer bundle + diff + blobs
// from 127.0.0.1 so nothing leaves the machine (SPEC-RENDERING.md §3b).
package webdiff

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
)

// Payload is everything the local page needs to render one file's diff.
type Payload struct {
	FilePath   string // repo-relative path being diffed, for display
	HandlerID  string // e.g. "gltf-scene", shown in the header
	Mode       string // "diff" (default) or "merge"
	DiffJSON   []byte // marshaled StructuredDiff
	RendererJS string // path to the installed renderer bundle
	Base       []byte // HEAD blob (may be nil)
	Head       []byte // working-tree blob (may be nil)
}

// Serve starts the loopback server, prints the URL, tries to open a browser,
// and blocks serving requests until the process is interrupted.
func Serve(p Payload, openBrowser bool) error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("starting local server: %w", err)
	}
	url := fmt.Sprintf("http://%s/", ln.Addr().String())

	srv := &http.Server{Handler: withCSP(p.handler())}

	fmt.Printf("forge diff for %s — computed locally, served at:\n\n    %s\n\n", p.FilePath, url)
	fmt.Println("Press Ctrl-C to stop.")
	if openBrowser {
		tryOpen(url)
	}
	return srv.Serve(ln)
}

func (p Payload) handler() http.Handler {
	mode := p.Mode
	if mode == "" {
		mode = "diff"
	}
	mux := http.NewServeMux()

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, indexHTML, htmlEscape(p.FilePath), htmlEscape(p.HandlerID))
	})

	mux.HandleFunc("/app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		fmt.Fprintf(w, appJS, jsString(mode))
	})

	mux.HandleFunc("/renderer.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		http.ServeFile(w, r, p.RendererJS)
	})

	mux.HandleFunc("/diff.json", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(p.DiffJSON)
	})

	mux.HandleFunc("/blob/base", serveBlob(p.Base))
	mux.HandleFunc("/blob/head", serveBlob(p.Head))

	return mux
}

func serveBlob(b []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if b == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write(b)
	}
}

// withCSP blocks any external fetch from the served page: scripts and styles
// come only from this origin (styles allow inline because renderer bundles
// inject a <style> element), and no other resource type is permitted.
func withCSP(next http.Handler) http.Handler {
	const csp = "default-src 'none'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
		"img-src 'self' data:; connect-src 'self'; base-uri 'none'; form-action 'none'"
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func tryOpen(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	// Best effort — headless machines have no browser; the URL is already printed.
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "forge: could not open a browser automatically: %v\n", err)
	}
}
