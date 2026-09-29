package fhr

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

// fakePreviewHandler writes a handler binary that answers info with the given
// preview declaration, answers preview with a fixed GLB-shaped blob ("glTF" in
// base64), and logs every subcommand it is run with.
func fakePreviewHandler(t *testing.T, previewDecl string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "forge-handler-obj")
	log = filepath.Join(dir, "calls.log")
	info := `{"id":"obj","formats":[".obj"],"protocol":"1.0"}`
	if previewDecl != "" {
		info = `{"id":"obj","formats":[".obj"],"protocol":"1.0","preview":"` + previewDecl + `"}`
	}
	script := "#!/bin/sh\necho \"$1\" >> '" + log + "'\ncat > /dev/null\ncase \"$1\" in\n" +
		"  info) echo '" + info + "' ;;\n" +
		"  preview) echo '{\"mediaType\":\"model/gltf-binary\",\"blob\":\"Z2xURg==\"}' ;;\n" +
		"  *) echo \"unknown subcommand: $1\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func calls(t *testing.T, log string) []string {
	t.Helper()
	b, err := os.ReadFile(log)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(b))
}

// A handler binary that declares a preview in its info gets one asked of it,
// and its info is asked once however many times the previewer is wanted.
func TestPreviewerWhenDeclared(t *testing.T) {
	bin, log := fakePreviewHandler(t, "model/gltf-binary")
	h := NewSubprocessHandler(nil, bin, InstalledMeta{ID: "obj", Formats: []string{".obj"}})

	var provider handler.PreviewerProvider = h
	p, ok := provider.Previewer()
	if !ok {
		t.Fatal("a binary declaring a preview must offer a Previewer")
	}
	if _, ok := h.Previewer(); !ok {
		t.Fatal("asking twice must give the same answer")
	}
	if got := p.PreviewMediaType(); got != "model/gltf-binary" {
		t.Errorf("media type = %q", got)
	}
	out, err := p.Preview([]byte("o Desk\nv 0 0 0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, []byte("glTF")) {
		t.Errorf("preview = %q, want the decoded blob", out)
	}
	if got := strings.Join(calls(t, log), " "); got != "info preview" {
		t.Errorf("calls = %q, want info once, then preview", got)
	}
}

// A binary that declares no preview is never sent one: every handler binary
// that exists would answer an undeclared call with "unknown subcommand".
func TestNoPreviewerWhenUndeclared(t *testing.T) {
	bin, log := fakePreviewHandler(t, "")
	h := NewSubprocessHandler(nil, bin, InstalledMeta{ID: "obj", Formats: []string{".obj"}})
	if _, ok := h.Previewer(); ok {
		t.Fatal("a binary that declares no preview must not offer a Previewer")
	}
	if got := calls(t, log); len(got) != 1 || got[0] != "info" {
		t.Errorf("calls = %v, want only info", got)
	}
}

// A binary with no info call at all (it predates it) has no preview either.
func TestNoPreviewerWithoutInfo(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "forge-handler-old")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho \"unknown subcommand: $1\" >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := NewSubprocessHandler(nil, bin, InstalledMeta{ID: "old", Formats: []string{".old"}})
	if _, ok := h.Previewer(); ok {
		t.Fatal("a binary without info must not offer a Previewer")
	}
}
