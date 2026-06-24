package text

import (
	"strings"
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

var h = New()

// TestMerge_CleanNonOverlapping verifies that non-overlapping changes from
// both sides are applied automatically with no conflict.
func TestMerge_CleanNonOverlapping(t *testing.T) {
	base := handler.Blob("line1\nline2\nline3\nline4\n")
	ours := handler.Blob("line1\nLINE2\nline3\nline4\n")   // changed line 2
	theirs := handler.Blob("line1\nline2\nline3\nLINE4\n") // changed line 4

	merged, ci, err := h.Merge(base, ours, theirs)
	assertNoError(t, err)
	assertNoConflict(t, ci)
	assertContent(t, merged, "line1\nLINE2\nline3\nLINE4\n")
}

// TestMerge_BothSidesSameChange verifies that identical changes on both
// sides are applied exactly once.
func TestMerge_BothSidesSameChange(t *testing.T) {
	base := handler.Blob("a\nb\nc\n")
	ours := handler.Blob("a\nB\nc\n")
	theirs := handler.Blob("a\nB\nc\n")

	merged, ci, err := h.Merge(base, ours, theirs)
	assertNoError(t, err)
	assertNoConflict(t, ci)
	assertContent(t, merged, "a\nB\nc\n")
}

// TestMerge_Conflict verifies that overlapping, differing changes produce
// git-style conflict markers and a populated ConflictInfo.
func TestMerge_Conflict(t *testing.T) {
	base := handler.Blob("a\nb\nc\n")
	ours := handler.Blob("a\nOURS\nc\n")
	theirs := handler.Blob("a\nTHEIRS\nc\n")

	merged, ci, err := h.Merge(base, ours, theirs)
	assertNoError(t, err)
	if ci == nil || len(ci.Conflicts) == 0 {
		t.Fatal("expected conflicts, got none")
	}

	got := string(merged)
	for _, marker := range []string{"<<<<<<< ours", "=======", ">>>>>>> theirs", "OURS", "THEIRS"} {
		if !strings.Contains(got, marker) {
			t.Errorf("expected %q in output; got:\n%s", marker, got)
		}
	}
}

// TestMerge_OursAddsLine verifies a pure insert on one side applies cleanly.
func TestMerge_OursAddsLine(t *testing.T) {
	base := handler.Blob("a\nb\n")
	ours := handler.Blob("a\nnew\nb\n")
	theirs := handler.Blob("a\nb\n") // unchanged

	merged, ci, err := h.Merge(base, ours, theirs)
	assertNoError(t, err)
	assertNoConflict(t, ci)
	assertContent(t, merged, "a\nnew\nb\n")
}

// TestMerge_TheirsDeletesLine verifies a pure delete on one side applies cleanly.
func TestMerge_TheirsDeletesLine(t *testing.T) {
	base := handler.Blob("a\nb\nc\n")
	ours := handler.Blob("a\nb\nc\n")  // unchanged
	theirs := handler.Blob("a\nc\n")   // deleted b

	merged, ci, err := h.Merge(base, ours, theirs)
	assertNoError(t, err)
	assertNoConflict(t, ci)
	assertContent(t, merged, "a\nc\n")
}

// TestMerge_BothAddAtSamePosition verifies that two different inserts at the
// same position are reported as a conflict.
func TestMerge_BothAddAtSamePosition(t *testing.T) {
	base := handler.Blob("a\nb\n")
	ours := handler.Blob("a\nfrom-ours\nb\n")
	theirs := handler.Blob("a\nfrom-theirs\nb\n")

	merged, ci, err := h.Merge(base, ours, theirs)
	assertNoError(t, err)
	if ci == nil || len(ci.Conflicts) == 0 {
		t.Fatal("expected conflict for simultaneous inserts at same position")
	}
	got := string(merged)
	if !strings.Contains(got, "from-ours") || !strings.Contains(got, "from-theirs") {
		t.Errorf("both sides should appear in conflict output; got:\n%s", got)
	}
}

// TestMerge_NoChanges verifies that merging identical files is a no-op.
func TestMerge_NoChanges(t *testing.T) {
	base := handler.Blob("unchanged\n")
	merged, ci, err := h.Merge(base, base, base)
	assertNoError(t, err)
	assertNoConflict(t, ci)
	assertContent(t, merged, "unchanged\n")
}

// ── helpers ────────────────────────────────────────────────────────────────────────────────

func assertNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertNoConflict(t *testing.T, ci *handler.ConflictInfo) {
	t.Helper()
	if ci != nil {
		t.Fatalf("expected clean merge, got conflicts: %v", ci.Conflicts)
	}
}

func assertContent(t *testing.T, got handler.Blob, want string) {
	t.Helper()
	if string(got) != want {
		t.Errorf("content mismatch\ngot:  %q\nwant: %q", string(got), want)
	}
}
