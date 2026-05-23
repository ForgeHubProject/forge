package text

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
	"github.com/yakupatahanov/forge/internal/handler"
)

// Handler handles any plain-text file. Registered last as the catch-all fallback.
type Handler struct{}

func New() *Handler { return &Handler{} }

// Match returns true for all paths — text is the fallback handler.
func (h *Handler) Match(_ string) bool { return true }

func (h *Handler) Format() string { return "text" }

func (h *Handler) Diff(base, head handler.Blob) (handler.StructuredDiff, error) {
	dmp := diffmatchpatch.New()
	baseChars, headChars, lines := dmp.DiffLinesToChars(normalizeLF(string(base)), normalizeLF(string(head)))
	diffs := dmp.DiffMain(baseChars, headChars, false)
	diffs = dmp.DiffCharsToLines(diffs, lines)

	var changes []handler.DiffChange
	baseLine, headLine := 1, 1

	for _, d := range diffs {
		ls := splitLines(d.Text)
		switch d.Type {
		case diffmatchpatch.DiffDelete:
			for _, line := range ls {
				changes = append(changes, handler.DiffChange{
					Path:   fmt.Sprintf("line:%d", baseLine),
					Kind:   handler.Removed,
					Before: line,
				})
				baseLine++
			}
		case diffmatchpatch.DiffInsert:
			for _, line := range ls {
				changes = append(changes, handler.DiffChange{
					Path:  fmt.Sprintf("line:%d", headLine),
					Kind:  handler.Added,
					After: line,
				})
				headLine++
			}
		case diffmatchpatch.DiffEqual:
			n := len(ls)
			baseLine += n
			headLine += n
		}
	}

	return handler.StructuredDiff{
		Version: "1.0",
		Format:  "text",
		Changes: changes,
	}, nil
}

// Merge performs a 3-way line merge matching git's behaviour:
// non-overlapping changes from both sides are applied automatically;
// overlapping changes produce git-style conflict markers in the output
// and are reported in ConflictInfo.
func (h *Handler) Merge(base, ours, theirs handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	baseLines := toLines(string(base))
	oursLines := toLines(string(ours))
	theirsLines := toLines(string(theirs))

	ourHunks := computeHunks(baseLines, oursLines)
	theirHunks := computeHunks(baseLines, theirsLines)

	merged, conflicts := merge3(baseLines, ourHunks, theirHunks, "ours", "theirs")

	result := handler.Blob(strings.Join(merged, ""))
	if len(conflicts) == 0 {
		return result, nil, nil
	}
	return result, &handler.ConflictInfo{Conflicts: conflicts}, nil
}

func normalizeLF(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
