package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
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

func readFile(p string) func() ([]byte, error) {
	return func() ([]byte, error) { return os.ReadFile(p) }
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
	if err := convertFile(convertRegistry(), src, "b", "", false, &bytes.Buffer{}, readFile(src)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "part.b"))
	if err != nil || string(got) != "mesh.b" {
		t.Fatalf("output %q err=%v", got, err)
	}
	// A second run refuses to clobber, and --force allows it.
	if err := convertFile(convertRegistry(), src, "b", "", false, &bytes.Buffer{}, readFile(src)); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected a refusal, got %v", err)
	}
	if err := convertFile(convertRegistry(), src, "b", "", true, &bytes.Buffer{}, readFile(src)); err != nil {
		t.Fatalf("--force: %v", err)
	}
}

func TestConvertFileStdoutAndSameFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "part.a")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	var out bytes.Buffer
	if err := convertFile(convertRegistry(), src, ".b", "-", false, &out, readFile(src)); err != nil || out.String() != "x.b" {
		t.Fatalf("stdout %q err=%v", out.String(), err)
	}
	// Converting a format to itself with no -o would overwrite the input.
	if err := convertFile(convertRegistry(), src, ".a", "", true, &bytes.Buffer{}, readFile(src)); err == nil || !strings.Contains(err.Error(), "refusing to overwrite the input") {
		t.Fatalf("expected the input to be protected even with --force, got %v", err)
	}
	if b, _ := os.ReadFile(src); string(b) != "x" {
		t.Fatalf("input was modified: %q", b)
	}
}

func TestConvertFileMissingInputAndHandler(t *testing.T) {
	dir := t.TempDir()
	if err := convertFile(convertRegistry(), filepath.Join(dir, "nope.a"), "b", "", false, &bytes.Buffer{}, readFile(filepath.Join(dir, "nope.a"))); err == nil {
		t.Fatal("a missing input should be an error")
	}
	src := filepath.Join(dir, "part.z")
	_ = os.WriteFile(src, []byte("x"), 0o644)
	if err := convertFile(convertRegistry(), src, "b", "", false, &bytes.Buffer{}, readFile(src)); err == nil || !strings.Contains(err.Error(), "cannot read .z") {
		t.Fatalf("got %v", err)
	}
}

// git runs git in dir, failing the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@t", "-c", "commit.gpgsign=false"}, args...)...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestConvertAtARevision(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "-q")
	if err := os.MkdirAll(filepath.Join(dir, "parts"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := filepath.Join(dir, "parts", "bracket.a")
	_ = os.WriteFile(f, []byte("OLD"), 0o644)
	git(t, dir, "add", ".")
	git(t, dir, "commit", "-q", "-m", "one")
	git(t, dir, "tag", "v1")
	_ = os.WriteFile(f, []byte("NEW"), 0o644)
	git(t, dir, "commit", "-qam", "two")

	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(filepath.Join(dir, "parts")); err != nil { // paths are relative to where the command runs
		t.Fatal(err)
	}

	// The old content, by tag, branch-relative and by commit; the file on disk is not touched.
	for _, rev := range []string{"v1", "HEAD~1", git(t, dir, "rev-parse", "HEAD~1")} {
		out := filepath.Join(dir, "out-"+strings.NewReplacer("~", "-", "/", "-").Replace(rev[:min(len(rev), 8)])+".b")
		err := convertFile(convertRegistry(), "bracket.a", "b", out, true, &bytes.Buffer{}, func() ([]byte, error) {
			return readAtRevision(context.Background(), rev, "bracket.a")
		})
		if err != nil {
			t.Fatalf("%s: %v", rev, err)
		}
		if got, _ := os.ReadFile(out); string(got) != "old.b" {
			t.Fatalf("%s: got %q, want the OLD content converted", rev, got)
		}
	}
	if got, _ := os.ReadFile(f); string(got) != "NEW" {
		t.Fatalf("the working tree file changed: %q", got)
	}

	// Refusals: an unknown revision, a path that is not in it, an option-looking revision.
	if _, err := readAtRevision(context.Background(), "nope", "bracket.a"); err == nil || !strings.Contains(err.Error(), "unknown revision") {
		t.Fatalf("unknown revision: %v", err)
	}
	if _, err := readAtRevision(context.Background(), "v1", "missing.a"); err == nil || !strings.Contains(err.Error(), "does not exist at v1") {
		t.Fatalf("missing path: %v", err)
	}
	if _, err := readAtRevision(context.Background(), "--output=/tmp/x", "bracket.a"); err == nil || !strings.Contains(err.Error(), "not a revision") {
		t.Fatalf("option-looking revision: %v", err)
	}
}

func TestDefaultRevOutIsBesideTheCurrentDirectory(t *testing.T) {
	if got := defaultRevOut("parts/bracket.obj", "STL", ""); got != "bracket.stl" {
		t.Fatalf("got %q", got)
	}
	if got := defaultRevOut("parts/bracket.obj", "stl", "x/y.stl"); got != "x/y.stl" {
		t.Fatalf("an explicit -o wins, got %q", got)
	}
}
