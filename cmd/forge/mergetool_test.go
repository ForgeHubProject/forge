package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

// recordingApplier is a handler that can apply choices and says whether it was
// asked to.
type recordingApplier struct {
	calls int
	take  []string
}

func (r *recordingApplier) ApplyChoices(_, _ handler.Blob, takePaths []string) (handler.Blob, error) {
	r.calls++
	r.take = takePaths
	return handler.Blob("applied"), nil
}

// Keeping every conflict at the current side is the merged file already on disk,
// so there is nothing to apply and the handler is not asked. The call would have
// to return what is already there, and a call that can only do that is one more
// thing that can fail in front of a human who chose the safest option there is.
func TestApplyConflictChoicesDoesNotAskForWhatItAlreadyHas(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "asset.unit")
	sidecar := path + ".forge-conflict"
	sc := handler.ConflictSidecar{
		Handler:   "unit-stub",
		Conflicts: []handler.SemanticConflict{{Path: "alpha"}, {Path: "beta"}},
		TheirsB64: base64.StdEncoding.EncodeToString([]byte("theirs")),
	}
	applier := &recordingApplier{}
	quietStdout(t)

	writeFileT(t, path, "merged")
	writeFileT(t, sidecar, "{}")
	if !applyConflictChoices(path, sidecar, sc, []bool{false, false}, applier) {
		t.Fatal("keeping both conflicts at current is a resolution")
	}
	if applier.calls != 0 {
		t.Fatalf("the handler was asked to apply nothing: %d call(s)", applier.calls)
	}
	if got := readFileT(t, path); got != "merged" {
		t.Fatalf("the merged file must be left as it is, got %q", got)
	}

	// One conflict taken from the incoming side is what the handler is for, and
	// only that path is named.
	writeFileT(t, path, "merged")
	writeFileT(t, sidecar, "{}")
	if !applyConflictChoices(path, sidecar, sc, []bool{false, true}, applier) {
		t.Fatal("a handler that applied the choices resolved the file")
	}
	if applier.calls != 1 || len(applier.take) != 1 || applier.take[0] != "beta" {
		t.Fatalf("the handler must be asked for exactly what was taken from incoming: %d call(s), %v", applier.calls, applier.take)
	}
	if got := readFileT(t, path); got != "applied" {
		t.Fatalf("what the handler returned is what is written, got %q", got)
	}
}

// quietStdout keeps the interactive summary out of the test log. Nothing reads
// stdin: with no answer to read, promptConfirm takes its default.
func quietStdout(t *testing.T) {
	t.Helper()
	real := os.Stdout
	f, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = f
	t.Cleanup(func() { os.Stdout = real; f.Close() })
}

func readFileT(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
