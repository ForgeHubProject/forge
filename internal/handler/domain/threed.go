package domain

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yakupatahanov/forge/internal/handler"
)

// Extensions covered by the 3D domain.
var threeDExtensions = map[string]bool{
	".glb": true, ".gltf": true,
	".fbx": true, ".obj": true, ".blend": true,
	".usd": true, ".usda": true, ".usdc": true, ".usdz": true,
	".dae": true, ".3ds": true, ".ply": true, ".stl": true,
	".step": true, ".stp": true,
}

// ThreeDDomain covers all 3D geometry and scene formats.
// Specific handlers (GltfHandler, ObjHandler, …) register into it via
// DomainRegister. Files with no specific handler fall back to a size-based diff.
type ThreeDDomain struct {
	BaseDomain
}

// NewThreeD returns a new ThreeDDomain with no specific handlers loaded.
func NewThreeD() *ThreeDDomain { return &ThreeDDomain{} }

func (d *ThreeDDomain) Format() string { return "3d" }

func (d *ThreeDDomain) Match(path string) bool {
	return threeDExtensions[strings.ToLower(filepath.Ext(path))]
}

// Diff is the domain-level fallback used when no specific handler matches.
// Reports file-size changes only; semantic diff requires a specific handler.
func (d *ThreeDDomain) Diff(base, head handler.Blob) (handler.StructuredDiff, error) {
	var changes []handler.DiffChange
	if len(base) != len(head) {
		changes = append(changes, handler.DiffChange{
			Path:   "file.size",
			Kind:   handler.Modified,
			Label:  "file size",
			Before: fmt.Sprintf("%d bytes", len(base)),
			After:  fmt.Sprintf("%d bytes", len(head)),
		})
	}
	return handler.StructuredDiff{Version: "1.0", Format: "3d", Changes: changes}, nil
}

// Merge is not supported at domain level; specific handlers implement it.
func (d *ThreeDDomain) Merge(_, _, _ handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	return nil, nil, handler.ErrNotSupported
}

// Extensions returns a sorted list of file extensions this domain covers.
func (d *ThreeDDomain) Extensions() []string {
	exts := make([]string, 0, len(threeDExtensions))
	for ext := range threeDExtensions {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return exts
}
