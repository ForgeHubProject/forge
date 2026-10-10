package handler

import (
	"fmt"
	"path/filepath"
	"strings"
)

// NormalizeExt returns ext lowercased with its leading dot: "OBJ" → ".obj".
func NormalizeExt(ext string) string {
	ext = strings.ToLower(strings.TrimSpace(ext))
	if ext != "" && !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	return ext
}

// Convert transcodes blob, the content of srcPath, into the format of dstExt by
// pivoting through glTF: the source format's handler imports it, the target
// format's handler exports the result. Every 3D format needs one importer and
// one exporter, not a converter per pair.
//
// Each side is found the way the registry finds any handler (first match), so
// the same formats that diff can convert; a handler that does not declare the
// call it is asked for is named in the error rather than guessed at.
func Convert(reg *Registry, srcPath, dstExt string, blob Blob) (Blob, error) {
	dstExt = NormalizeExt(dstExt)
	if dstExt == "" {
		return nil, fmt.Errorf("no target format given")
	}
	srcExt := NormalizeExt(filepath.Ext(srcPath))

	src, err := reg.Resolve(srcPath)
	if err != nil {
		return nil, fmt.Errorf("cannot read %s: %w", srcExt, err)
	}
	ip, ok := src.(ImporterProvider)
	if !ok {
		return nil, fmt.Errorf("%s is not a 3D format forge can read: no installed handler imports it (see `forge formats list`)", srcExt)
	}
	importer, ok := ip.Importer()
	if !ok {
		return nil, fmt.Errorf("%s has no converter: its handler does not declare `import`", srcExt)
	}

	dst, err := reg.Resolve("x" + dstExt)
	if err != nil {
		return nil, fmt.Errorf("cannot write %s: %w", dstExt, err)
	}
	ep, ok := dst.(ExporterProvider)
	if !ok {
		return nil, fmt.Errorf("%s is not a 3D format forge can write: no installed handler exports it (see `forge formats list`)", dstExt)
	}
	exporter, ok := ep.Exporter()
	if !ok {
		return nil, fmt.Errorf("%s cannot be written: its handler does not declare `export`", dstExt)
	}

	glb, err := importer.Import(blob)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", srcExt, err)
	}
	out, err := exporter.Export(glb, dstExt)
	if err != nil {
		return nil, fmt.Errorf("writing %s: %w", dstExt, err)
	}
	return out, nil
}
