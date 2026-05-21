package domain

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yakupatahanov/forge/internal/handler"
)

// Extensions covered by the image domain.
var imageExtensions = map[string]bool{
	".png": true, ".jpg": true, ".jpeg": true,
	".tiff": true, ".tif": true,
	".exr": true, ".hdr": true,
	".tga": true, ".bmp": true, ".webp": true,
	".psd": true, ".psb": true, ".xcf": true,
}

// ImageDomain covers raster image and layered image formats.
// Specific handlers (PsdHandler, …) register into it via DomainRegister.
// Files with no specific handler fall back to a size-based diff.
type ImageDomain struct {
	BaseDomain
}

// NewImage returns a new ImageDomain with no specific handlers loaded.
func NewImage() *ImageDomain { return &ImageDomain{} }

func (d *ImageDomain) Format() string { return "image" }

func (d *ImageDomain) Match(path string) bool {
	return imageExtensions[strings.ToLower(filepath.Ext(path))]
}

// Diff is the domain-level fallback. Reports file-size changes only;
// pixel-level or layer-level diff requires a specific handler.
func (d *ImageDomain) Diff(base, head handler.Blob) (handler.StructuredDiff, error) {
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
	return handler.StructuredDiff{Version: "1.0", Format: "image", Changes: changes}, nil
}

// Merge is not supported at domain level; specific handlers implement it.
func (d *ImageDomain) Merge(_, _, _ handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	return nil, nil, handler.ErrNotSupported
}

// Extensions returns a sorted list of file extensions this domain covers.
func (d *ImageDomain) Extensions() []string {
	exts := make([]string, 0, len(imageExtensions))
	for ext := range imageExtensions {
		exts = append(exts, ext)
	}
	sort.Strings(exts)
	return exts
}
