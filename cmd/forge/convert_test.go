package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

// xcoder matches one extension and converts through an upper-case "glb".
type xcoder struct{ ext string }

func (x xcoder) Match(p string) bool { return strings.HasSuffix(p, x.ext) }
func (xcoder) Diff(_, _ handler.Blob) (handler.StructuredDiff, error) {
	return handler.StructuredDiff{}, nil
}
func (xcoder) Merge(_, _, _ handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	return nil, nil, nil
}
func (x xcoder) Importer() (handler.Importer, bool) { return x, true }
func (x xcoder) Exporter() (handler.Exporter, bool) { return x, true }
func (xcoder) Import(b handler.Blob) (handler.Blob, error) {
	return handler.Blob(strings.ToUpper(string(b))), nil
}
func (x xcoder) Export(g handler.Blob, f string) (handler.Blob, error) {
	return handler.Blob(strings.ToLower(string(g)) + f), nil
}

func convertRegistry() *handler.Registry {
	r := handler.NewRegistry()
	r.Register(xcoder{".a"})
	r.Register(xcoder{".b"})
	return r
}

func TestConvertFileDefaultsOutputBesideInput(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "part.a")
	if err := os.WriteFile(src, []byte("MeSh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := convertFile(convertRegistry(), src, "b", "", false, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "part.b"))
	if err != nil || string(got) != "mesh.b" {
		t.Fatalf("output %q err=%v", got, err)
	}
	// A second run refuses to clobber, and --force allows it.
	if err := convertFile(convertRegistry(), src, "b", "", false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if err := convertFile(convertRegistry(), src, "b", "", true, &bytes.Buffer{}); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestConvertFileStdoutAndSameFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "part.a")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	var out bytes.Buffer
	if err := convertFile(convertRegistry(), src, ".b", "-", false, &out); err != nil || out.String() != "x.b" {
		t.Fatalf("stdout %q err=%v", out.String(), err)
	}
	// Converting a format to itself with no -o would overwrite the input.
	if err := convertFile(convertRegistry(), src, ".a", "", true, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "refusing to overwrite the input") {
		t.Fatalf("expected the input to be protected even with --force, got %v", err)
	}
	if b, _ := os.ReadFile(src); string(b) != "x" {
		t.Fatalf("input was modified: %q", b)
	}
}

func TestConvertFileMissingInputAndHandler(t *testing.T) {
	dir := t.TempDir()
	if err := convertFile(convertRegistry(), filepath.Join(dir, "nope.a"), "b", "", false, &bytes.Buffer{}); err == nil {
		t.Fatal("a missing input should be an error")
	}
	src := filepath.Join(dir, "part.z")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	if err := convertFile(convertRegistry(), src, "b", "", false, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "cannot read .z") {
		t.Fatalf("got %v", err)
	}
}
