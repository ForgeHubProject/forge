package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

// stubHandler matches by extension, as every format handler does, and records
// the merges it was asked for.
type stubHandler struct {
	ext       string
	result    string
	conflicts []handler.SemanticConflict
	merges    int
}

func (s *stubHandler) Match(path string) bool { return s.ext == "" || strings.HasSuffix(path, s.ext) }

func (s *stubHandler) Diff(_, _ handler.Blob) (handler.StructuredDiff, error) {
	return handler.StructuredDiff{}, nil
}

func (s *stubHandler) Merge(_, _, _ handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	s.merges++
	if len(s.conflicts) > 0 {
		return handler.Blob(s.result), &handler.ConflictInfo{Conflicts: s.conflicts}, nil
	}
	return handler.Blob(s.result), nil, nil
}

// gitDriverFiles lays out what git hands a merge driver: three temporary files
// named .merge_file_XXXXXX — no extension — beside the real path, which only
// the fourth argument (%P) carries.
func gitDriverFiles(t *testing.T) (dir string, args []string) {
	t.Helper()
	dir = t.TempDir()
	for name, content := range map[string]string{
		".merge_file_base01": "base", ".merge_file_ours02": "ours", ".merge_file_thrs03": "theirs",
	} {
		writeFileT(t, filepath.Join(dir, name), content)
	}
	return dir, []string{
		filepath.Join(dir, ".merge_file_base01"),
		filepath.Join(dir, ".merge_file_ours02"),
		filepath.Join(dir, ".merge_file_thrs03"),
		filepath.Join(dir, "model.unit"),
	}
}

// As a git merge driver, merge-file must choose the handler by the file's real
// path. It resolved the temporary name instead, which no format handler
// matches, so every file fell through to the plain-text handler: semantic
// merge never ran, and binary files (.glb) got conflict markers written into
// them.
func TestMergeFileResolvesTheHandlerByTheRealPath(t *testing.T) {
	dir, args := gitDriverFiles(t)
	format := &stubHandler{ext: ".unit", result: "semantic"}
	text := &stubHandler{result: "<<<<<<< line-merged"}
	reg := handler.NewRegistry()
	reg.Register(format)
	reg.Register(text)
	quietStdout(t)

	conflicted, err := mergeFile(reg, args)
	if err != nil || conflicted {
		t.Fatalf("clean merge expected: conflicted=%v err=%v", conflicted, err)
	}
	if format.merges != 1 || text.merges != 0 {
		t.Fatalf("the .unit handler must merge (format %d, text %d)", format.merges, text.merges)
	}
	if got := readFileT(t, args[1]); got != "semantic" {
		t.Fatalf("the merge result goes to OURS, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "model.unit.forge-conflict")); err == nil {
		t.Fatal("a clean merge writes no sidecar")
	}
}

// Conflicts are reported against the real file: the sidecar forge mergetool
// reads sits next to it, not next to a temp file git is about to delete.
func TestMergeFileWritesTheSidecarBesideTheRealFile(t *testing.T) {
	dir, args := gitDriverFiles(t)
	format := &stubHandler{ext: ".unit", result: "ours-kept", conflicts: []handler.SemanticConflict{{Path: "nodes/A"}}}
	reg := handler.NewRegistry()
	reg.Register(format)
	quietStdout(t)
	quietStderr(t)

	conflicted, err := mergeFile(reg, args)
	if err != nil || !conflicted {
		t.Fatalf("a conflicted merge expected: conflicted=%v err=%v", conflicted, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "model.unit.forge-conflict")); err != nil {
		t.Fatalf("sidecar missing beside the real file: %v", err)
	}
}

// Run by hand with three real files and no fourth argument, the ours path is
// the real one, as before.
func TestMergeFileWithoutAPathResolvesOurs(t *testing.T) {
	dir := t.TempDir()
	var args []string
	for _, n := range []string{"base.unit", "ours.unit", "theirs.unit"} {
		writeFileT(t, filepath.Join(dir, n), n)
		args = append(args, filepath.Join(dir, n))
	}
	format := &stubHandler{ext: ".unit", result: "semantic"}
	reg := handler.NewRegistry()
	reg.Register(format)
	quietStdout(t)

	if _, err := mergeFile(reg, args); err != nil || format.merges != 1 {
		t.Fatalf("merges=%d err=%v", format.merges, err)
	}
}

func quietStderr(t *testing.T) {
	t.Helper()
	real := os.Stderr
	f, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = f
	t.Cleanup(func() { os.Stderr = real; f.Close() })
}
