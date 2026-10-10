package fhr

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTranscoder writes a handler binary whose info declares the given
// capabilities JSON, that answers import with "GLB:<input blob>" and export
// with "<format>:<input blob>", and logs the stdin it was given.
func fakeTranscoder(t *testing.T, caps string) (bin, log string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "forge-handler-x")
	log = filepath.Join(dir, "stdin.log")
	info := `{"id":"x","formats":[".x"],"protocol":"1.0","capabilities":` + caps + `}`
	script := "#!/bin/sh\ncase \"$1\" in\n" +
		"  info) cat >/dev/null; echo '" + info + "' ;;\n" +
		"  import|export) echo \"$1\" >> '" + log + "'; cat >> '" + log + "'; echo >> '" + log + "'; echo '{\"blob\":\"R0xC\"}' ;;\n" +
		"  *) echo \"unknown subcommand: $1\" >&2; exit 1 ;;\nesac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log
}

func TestImporterExporterOnlyWhenDeclared(t *testing.T) {
	bin, _ := fakeTranscoder(t, `{"semanticCompare":true,"import":true}`)
	h := NewSubprocessHandler(nil, bin, InstalledMeta{ID: "x", Formats: []string{".x"}})
	if _, ok := h.Importer(); !ok {
		t.Error("a binary declaring import must offer an Importer")
	}
	if _, ok := h.Exporter(); ok {
		t.Error("a binary that did not declare export must not be sent export")
	}

	bin, _ = fakeTranscoder(t, `{"semanticCompare":true}`)
	h = NewSubprocessHandler(nil, bin, InstalledMeta{ID: "x", Formats: []string{".x"}})
	if _, ok := h.Importer(); ok {
		t.Error("no declaration, no Importer")
	}
}

func TestImportAndExportSpeakTheProtocol(t *testing.T) {
	bin, log := fakeTranscoder(t, `{"import":true,"export":true}`)
	h := NewSubprocessHandler(nil, bin, InstalledMeta{ID: "x", Formats: []string{".x"}})

	imp, _ := h.Importer()
	out, err := imp.Import([]byte("abc"))
	if err != nil || string(out) != "GLB" {
		t.Fatalf("import: %q err=%v", out, err)
	}
	exp, _ := h.Exporter()
	if out, err = exp.Export([]byte("glb"), ".x"); err != nil || string(out) != "GLB" {
		t.Fatalf("export: %q err=%v", out, err)
	}

	b, _ := os.ReadFile(log)
	got := string(b)
	// base64("abc") = YWJj, base64("glb") = Z2xi
	for _, want := range []string{`import`, `{"blob":"YWJj"}`, `export`, `{"blob":"Z2xi","format":".x"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("stdin log missing %s:\n%s", want, got)
		}
	}
}
