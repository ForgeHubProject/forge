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
		path         string
		wantStatus   int
		wantCT       string
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

// The page can be pointed at any pair of revisions, so its header has to say
// which two versions it is showing — a reader who cannot tell whether they are
// looking at uncommitted work or at a historical commit cannot trust the view.
func TestIndexMetaNamesWhatIsCompared(t *testing.T) {
	p := newTestPayload(t)
	p.Compare = "HEAD~1 → the working tree"
	body, _ := io.ReadAll(doGet(t, withCSP(p.handler()), "/").Body)
	if !strings.Contains(string(body), "comparing HEAD~1 → the working tree") {
		t.Errorf("header does not say what is compared; got:\n%s", string(body))
	}

	// With nothing to say, the header stays as it was.
	p.Compare = ""
	body, _ = io.ReadAll(doGet(t, withCSP(p.handler()), "/").Body)
	got := string(body)
	if !strings.Contains(got, "handler: "+p.HandlerID+" · computed locally by forge") {
		t.Errorf("unexpected header without a comparison; got:\n%s", got)
	}
	if strings.Contains(got, "comparing") {
		t.Errorf("header invented a comparison; got:\n%s", got)
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
	// GLB-embedded textures are materialized as blob:/data: URLs by the
	// renderer's loader; both are same-document byte sources, not egress.
	if !strings.Contains(csp, "img-src 'self' data: blob:") || !strings.Contains(csp, "connect-src 'self' data: blob:") {
		t.Fatalf("CSP must allow blob:/data: for embedded textures, got: %q", csp)
	}
	// The policy must never widen to real network egress.
	if strings.Contains(csp, "http:") || strings.Contains(csp, "https:") || strings.Contains(csp, "*") {
		t.Fatalf("CSP unexpectedly permits network sources: %q", csp)
	}
}

// The page must tell the renderer how big each side actually is. A placeholder
// size is not a harmless stub: a renderer deciding whether it can afford to hold
// both versions in memory reads this number, and one that treats a non-positive
// size as "nothing here" silently drops a side forge is really serving.
func TestAppJSReportsRealBlobSizes(t *testing.T) {
	p := newTestPayload(t) // Base "base-bytes" (10), Head "head-bytes" (10)
	body, _ := io.ReadAll(doGet(t, withCSP(p.handler()), "/app.js").Body)
	got := string(body)

	for _, want := range []string{
		`"base":{"url":"/blob/base","size":10}`,
		`"head":{"url":"/blob/head","size":10}`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("app.js missing %s; got:\n%s", want, got)
		}
	}
	if strings.Contains(got, `"size":0`) {
		t.Errorf("app.js still reports a placeholder size:\n%s", got)
	}
}

func TestAppJSSizesMatchServedBytes(t *testing.T) {
	p := newTestPayload(t)
	p.Base = make([]byte, 4096)
	p.Head = make([]byte, 1234)
	h := withCSP(p.handler())

	body, _ := io.ReadAll(doGet(t, h, "/app.js").Body)
	if !strings.Contains(string(body), `"size":4096`) || !strings.Contains(string(body), `"size":1234`) {
		t.Fatalf("declared sizes do not match the payload; got:\n%s", string(body))
	}
	// The declared size must equal what the blob route actually serves, or the
	// renderer budgets against one number and receives another.
	for path, want := range map[string]int{"/blob/base": 4096, "/blob/head": 1234} {
		served, _ := io.ReadAll(doGet(t, h, path).Body)
		if len(served) != want {
			t.Errorf("%s served %d bytes, declared %d", path, len(served), want)
		}
	}
}

// A side with no bytes is a file this change added or deleted. Reporting it as a
// zero-length blob would be indistinguishable from a real but empty version, so
// it is reported as absent instead.
func TestAppJSReportsMissingSideAsNull(t *testing.T) {
	p := newTestPayload(t)
	p.Base = nil
	body, _ := io.ReadAll(doGet(t, withCSP(p.handler()), "/app.js").Body)
	got := string(body)
	if !strings.Contains(got, `"base":null`) {
		t.Errorf("absent base should be null; got:\n%s", got)
	}
	if !strings.Contains(got, `"head":{"url":"/blob/head"`) {
		t.Errorf("present head should still be described; got:\n%s", got)
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

func TestServes3DChunkWhenPresent(t *testing.T) {
	p := newTestPayload(t) // HandlerID == "gltf-scene"
	dir := t.TempDir()
	chunk := filepath.Join(dir, "gltf-scene-3d.js")
	if err := os.WriteFile(chunk, []byte("export default { mount3d(){} };\n"), 0644); err != nil {
		t.Fatal(err)
	}
	p.Renderer3D = chunk
	h := withCSP(p.handler())

	resp := doGet(t, h, "/renderer-gltf-scene-3d.js")
	if resp.StatusCode != 200 {
		t.Fatalf("3D chunk status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "mount3d") {
		t.Fatalf("3D chunk body not served; got %q", string(body))
	}
}

func TestNo3DChunkRouteWhenAbsent(t *testing.T) {
	p := newTestPayload(t) // Renderer3D == ""
	h := withCSP(p.handler())
	if resp := doGet(t, h, "/renderer-gltf-scene-3d.js"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 with no 3D chunk, got %d", resp.StatusCode)
	}
}
