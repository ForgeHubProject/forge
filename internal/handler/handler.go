package handler

import "errors"

// ErrNotSupported is returned by handlers that do not implement a given operation.
var ErrNotSupported = errors.New("not supported")

// Blob is raw file content.
type Blob []byte

// ChangeKind describes the nature of a semantic change.
type ChangeKind string

const (
	Added    ChangeKind = "added"
	Removed  ChangeKind = "removed"
	Modified ChangeKind = "modified"
)

// DiffChange is one semantic unit of change within a StructuredDiff.
type DiffChange struct {
	Path     string       `json:"path"`
	Kind     ChangeKind   `json:"kind"`
	Label    string       `json:"label,omitempty"`
	Before   any          `json:"before,omitempty"`
	After    any          `json:"after,omitempty"`
	Children []DiffChange `json:"children,omitempty"`
}

// StructuredDiff is the wire format returned by ForgeHandler.Diff.
type StructuredDiff struct {
	Version  string       `json:"version"`
	Format   string       `json:"format"`
	Changes  []DiffChange `json:"changes"`
}

// ConflictInfo describes semantic conflicts from a 3-way merge.
type ConflictInfo struct {
	Conflicts []SemanticConflict `json:"conflicts"`
}

// SemanticConflict is a single unresolvable conflict at a semantic path.
type SemanticConflict struct {
	Path   string `json:"path"`
	Ours   any    `json:"ours"`
	Theirs any    `json:"theirs"`
}

// ForgeHandler implements format-aware diff and merge for one file type.
type ForgeHandler interface {
	// Match returns true if this handler should process the given file path.
	Match(path string) bool

	// Diff produces a structured, semantic diff between two blobs.
	Diff(base, head Blob) (StructuredDiff, error)

	// Merge attempts a 3-way merge. Returns the merged blob on success, or
	// ConflictInfo describing which semantic units could not be reconciled.
	// Handlers that cannot merge return ErrNotSupported.
	Merge(base, ours, theirs Blob) (Blob, *ConflictInfo, error)
}

// Namer is an optional interface handlers may implement to expose their
// format identifier. Used by forge status and other display contexts.
type Namer interface {
	Format() string
}

// ConflictSidecar is written by forge merge-file alongside the merged file
// whenever semantic conflicts exist. forge mergetool reads it to drive the
// interactive conflict resolver.
type ConflictSidecar struct {
	Handler   string             `json:"handler"`
	Conflicts []SemanticConflict `json:"conflicts"`
	TheirsB64 string             `json:"theirs_blob"` // base64-encoded theirs blob
}

// ConflictApplier is an optional interface handlers may implement to support
// interactive conflict resolution in forge mergetool.
type ConflictApplier interface {
	ApplyChoices(merged, theirs Blob, takePaths []string) (Blob, error)
}
