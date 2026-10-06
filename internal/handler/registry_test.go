package handler

import (
	"strings"
	"testing"
)

type stubHandler struct {
	name string
	ext  string
}

func (s stubHandler) Match(path string) bool { return strings.HasSuffix(path, s.ext) }
func (s stubHandler) Diff(base, head Blob) (StructuredDiff, error) {
	return StructuredDiff{}, nil
}
func (s stubHandler) Merge(base, ours, theirs Blob) (Blob, *ConflictInfo, error) {
	return nil, nil, nil
}

func TestRegistry_ResolveEmpty(t *testing.T) {
	if _, err := NewRegistry().Resolve("a.txt"); err == nil {
		t.Fatal("expected error from empty registry")
	}
}

func TestRegistry_FirstMatchWins(t *testing.T) {
	r := NewRegistry()
	r.Register(stubHandler{name: "specific", ext: ".glb"})
	r.Register(stubHandler{name: "catchall", ext: ""})

	h, err := r.Resolve("model.glb")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.(stubHandler).name; got != "specific" {
		t.Errorf("got %q, want specific", got)
	}

	h, err = r.Resolve("notes.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := h.(stubHandler).name; got != "catchall" {
		t.Errorf("got %q, want catchall", got)
	}
}

func TestRegistry_NoMatch(t *testing.T) {
	r := NewRegistry()
	r.Register(stubHandler{name: "glb", ext: ".glb"})
	_, err := r.Resolve("a.txt")
	if err == nil || !strings.Contains(err.Error(), `"a.txt"`) {
		t.Fatalf("expected no-handler error naming path, got %v", err)
	}
}
