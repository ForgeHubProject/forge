package fhr

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
)

// A download reports its progress to stderr and never to stdout. These are
// terminal-facing functions, but the same calls are reached from `forge mcp`,
// where this process's stdout carries the protocol: one line of prose there is a
// parse error that ends the client's session, after the install has already
// happened.
func TestDownloadsAnnounceThemselvesOnStderrOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the installed binary is chmod'ed, which windows has no equivalent for")
	}
	t.Setenv("HOME", t.TempDir())

	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, "payload")
	}))
	defer source.Close()

	m := parseManifest(t, fmt.Sprintf(`
name = "probe"

[formats]
".probe" = { handler = "unit-probe", build = "deadbee" }

[assets.handlers."unit-probe"]
%q = %q

[assets.renderers]
"unit-probe" = %q
`, PlatformKey(), source.URL+"/handler", source.URL+"/renderer.js"))

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w

	_, handlerErr := DownloadHandler(m, "unit-probe", source.URL)
	_, rendererErr := DownloadRenderer(m, "unit-probe", source.URL)

	os.Stdout = saved
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	onStdout, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	if handlerErr != nil || rendererErr != nil {
		t.Fatalf("both downloads must have run: %v / %v", handlerErr, rendererErr)
	}
	if len(onStdout) > 0 {
		t.Fatalf("a download wrote %q to stdout", onStdout)
	}
}
