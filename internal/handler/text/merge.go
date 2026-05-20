package text

import (
	"fmt"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"
	"github.com/yakupatahanov/forge/internal/handler"
)

// hunk is a contiguous edit relative to the base line array.
// A pure insert has start == end.
type hunk struct {
	start int      // first base line affected (inclusive)
	end   int      // first base line not affected (exclusive)
	lines []string // replacement lines; empty means pure delete
}

// computeHunks returns the sorted, non-overlapping edit hunks that transform
// base into modified. Lines must preserve their trailing \n (use toLines).
func computeHunks(base, modified []string) []hunk {
	dmp := diffmatchpatch.New()
	bChars, mChars, lineArr := dmp.DiffLinesToChars(
		strings.Join(base, ""),
		strings.Join(modified, ""),
	)
	diffs := dmp.DiffMain(bChars, mChars, false)
	diffs = dmp.DiffCharsToLines(diffs, lineArr)

	var result []hunk
	idx := 0

	for i := 0; i < len(diffs); {
		d := diffs[i]
		ls := toLines(d.Text)

		switch d.Type {
		case diffmatchpatch.DiffEqual:
			idx += len(ls)
			i++

		case diffmatchpatch.DiffDelete:
			h := hunk{start: idx, end: idx + len(ls)}
			idx += len(ls)
			i++
			// A delete followed by inserts is a replacement.
			for i < len(diffs) && diffs[i].Type == diffmatchpatch.DiffInsert {
				h.lines = append(h.lines, toLines(diffs[i].Text)...)
				i++
			}
			result = append(result, h)

		case diffmatchpatch.DiffInsert:
			result = append(result, hunk{start: idx, end: idx, lines: ls})
			i++
		}
	}
	return result
}

// merge3 performs a 3-way line merge.
// Returns the merged line slice and any semantic conflicts.
// Conflicts are represented with git-style markers in the output.
func merge3(
	base []string,
	ourHunks, theirHunks []hunk,
	oursLabel, theirsLabel string,
) ([]string, []handler.SemanticConflict) {
	var out []string
	var conflicts []handler.SemanticConflict

	baseIdx := 0
	oi, ti := 0, 0

	for {
		nextOur := len(base) + 1
		nextTheir := len(base) + 1
		if oi < len(ourHunks) {
			nextOur = ourHunks[oi].start
		}
		if ti < len(theirHunks) {
			nextTheir = theirHunks[ti].start
		}
		if nextOur > len(base) && nextTheir > len(base) {
			break
		}

		// Copy equal lines up to the next event.
		next := min(nextOur, nextTheir)
		for baseIdx < next {
			out = append(out, base[baseIdx])
			baseIdx++
		}

		// Greedily collect all hunks from both sides that overlap the current
		// zone. Pure inserts (start==end) extend the zone only if something
		// else grows it past their position.
		zoneEnd := baseIdx
		var ourActive, theirActive []hunk

		changed := true
		for changed {
			changed = false
			for oi < len(ourHunks) && ourHunks[oi].start <= zoneEnd {
				if ourHunks[oi].end > zoneEnd {
					zoneEnd = ourHunks[oi].end
				}
				ourActive = append(ourActive, ourHunks[oi])
				oi++
				changed = true
			}
			for ti < len(theirHunks) && theirHunks[ti].start <= zoneEnd {
				if theirHunks[ti].end > zoneEnd {
					zoneEnd = theirHunks[ti].end
				}
				theirActive = append(theirActive, theirHunks[ti])
				ti++
				changed = true
			}
		}

		if len(ourActive) == 0 && len(theirActive) == 0 {
			break
		}

		ourResult := applyHunks(base, baseIdx, zoneEnd, ourActive)
		theirResult := applyHunks(base, baseIdx, zoneEnd, theirActive)

		switch {
		case len(ourActive) == 0:
			out = append(out, theirResult...)
		case len(theirActive) == 0:
			out = append(out, ourResult...)
		case linesEqual(ourResult, theirResult):
			out = append(out, ourResult...)
		default:
			out = append(out, "<<<<<<< "+oursLabel+"\n")
			out = append(out, ourResult...)
			out = append(out, "=======\n")
			out = append(out, theirResult...)
			out = append(out, ">>>>>>> "+theirsLabel+"\n")

			path := fmt.Sprintf("line:%d", baseIdx+1)
			if zoneEnd > baseIdx+1 {
				path = fmt.Sprintf("lines:%d-%d", baseIdx+1, zoneEnd)
			}
			conflicts = append(conflicts, handler.SemanticConflict{
				Path:   path,
				Ours:   ourResult,
				Theirs: theirResult,
			})
		}
		baseIdx = zoneEnd
	}

	// Copy any remaining base lines after the last hunk.
	for baseIdx < len(base) {
		out = append(out, base[baseIdx])
		baseIdx++
	}

	return out, conflicts
}

// applyHunks reconstructs what one side produces for base[start:end].
// hunks must be sorted and non-overlapping (guaranteed by computeHunks).
func applyHunks(base []string, start, end int, hunks []hunk) []string {
	var result []string
	idx := start
	for _, h := range hunks {
		for idx < h.start {
			result = append(result, base[idx])
			idx++
		}
		result = append(result, h.lines...)
		idx = h.end
	}
	for idx < end {
		result = append(result, base[idx])
		idx++
	}
	return result
}

func linesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// toLines splits s into lines, preserving each line's trailing \n.
// Used for merge reconstruction (contrast with splitLines which strips \n).
func toLines(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.SplitAfter(s, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	return parts
}
