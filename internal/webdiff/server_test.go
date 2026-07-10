package webdiff

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestPayload(t *testing.T) Payload {
	t.Helper()
	dir := t.TempDir()
	rendererPath := filepath.Join(dir, "gltf-scene.js")
	if err := os.WriteFile(rendererPath, []byte("export default { mount(){} };\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return Payload{
		FilePath:   "models/robot.glb",
		HandlerID:  "gltf-scene",
		Mode:       "diff",
		DiffJSON:   []byte(`{"version":"1.0","format":"gltf-scene","changes":[]}`),
		RendererJS: rendererPath,
		Base:       []byte("base-bytes"),
		Head:       []byte("head-bytes"),
	}
}

func doGet(t *testing.T, h http.Handler, path string) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Result()
}

func TestRoutes(t *testing.T) {
	p := newTestPayload(t)
	h := withCSP(p.handler())

	cases := []struct {
		path        string
		wantStatus  int
		wantCT      string
		wantContains string
	}{
		{"/", 200, "text/html", "models/robot.glb"},
		{"/app.js", 200, "text/javascript", `mode: "diff"`},
		{"/renderer.js", 200, "text/javascript", "export default"},
		{"/diff.json", 200, "application/json", `"format":"gltf-scene"`},
		{"/blob/base", 200, "application/octet-stream", "base-bytes"},
		{"/blob/head", 200, "application/octet-stream", "head-bytes"},
	}
	for _, c := range cases {
		resp := doGet(t, h, c.path)
		if resp.StatusCode != c.wantStatus {
			t.Errorf("%s: status = %d, want %d", c.path, resp.StatusCode, c.wantStatus)
		}
		if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, c.wantCT) {
			t.Errorf("%s: content-type = %q, want contains %q", c.path, ct, c.wantCT)
		}
		body, _ := io.ReadAll(resp.Body)
		if !strings.Contains(string(body), c.wantContains) {
			t.Errorf("%s: body missing %q; got %q", c.path, c.wantContains, string(body))
		}
	}
}

func TestCSPHeaderPresent(t *testing.T) {
	p := newTestPayload(t)
	h := withCSP(p.handler())
	resp := doGet(t, h, "/")
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP not set as expected: %q", csp)
	}
}

func TestUnknownPath404(t *testing.T) {
	p := newTestPayload(t)
	h := withCSP(p.handler())
	if resp := doGet(t, h, "/secret"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", resp.StatusCode)
	}
}

func TestNilBlob404(t *testing.T) {
	p := newTestPayload(t)
	p.Base = nil
	h := withCSP(p.handler())
	if resp := doGet(t, h, "/blob/base"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil blob status = %d, want 404", resp.StatusCode)
	}
}
