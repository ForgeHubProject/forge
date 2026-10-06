package manifest

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeManifest(t *testing.T, root, content string) {
	t.Helper()
	dir := filepath.Join(root, ".forge")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "handlers"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadHandlers_Missing(t *testing.T) {
	m, err := LoadHandlers(t.TempDir())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(m.Domains.Require) != 0 || len(m.Community) != 0 {
		t.Fatalf("expected empty manifest, got %+v", m)
	}
}

func TestLoadHandlers_Valid(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, `
[domains]
require = ["3d", "text"]

[community.cad]
registry = "https://example.com/fhr"
version = "1.2.0"
`)
	m, err := LoadHandlers(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"3d", "text"}; !reflect.DeepEqual(m.Domains.Require, want) {
		t.Errorf("require = %v, want %v", m.Domains.Require, want)
	}
	want := CommunitySource{Registry: "https://example.com/fhr", Version: "1.2.0"}
	if got := m.Community["cad"]; got != want {
		t.Errorf("community[cad] = %+v, want %+v", got, want)
	}
}

func TestLoadHandlers_InvalidTOML(t *testing.T) {
	root := t.TempDir()
	writeManifest(t, root, "[domains\nrequire = ")
	if _, err := LoadHandlers(root); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestLoadHandlers_ReadError(t *testing.T) {
	root := t.TempDir()
	// .forge/handlers as a directory → ReadFile fails with a non-NotExist error.
	if err := os.MkdirAll(filepath.Join(root, ".forge", "handlers"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadHandlers(root); err == nil {
		t.Fatal("expected read error")
	}
}
