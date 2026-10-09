package handler

import (
	"errors"
	"strings"
	"testing"
)

// fmtHandler is a format handler with optional transcoding: it matches one
// extension, and imports to / exports from a made-up "glb" it can recognise.
type fmtHandler struct {
	ext        string
	canImport  bool
	canExport  bool
	importErr  error
	exportErr  error
	gotFormat  string
	declareNot bool // implements the providers but declares nothing
}

func (f *fmtHandler) Match(p string) bool { return strings.HasSuffix(strings.ToLower(p), f.ext) }
func (f *fmtHandler) Diff(_, _ Blob) (StructuredDiff, error) {
	return StructuredDiff{}, nil
}
func (f *fmtHandler) Merge(_, _, _ Blob) (Blob, *ConflictInfo, error) { return nil, nil, nil }

func (f *fmtHandler) Importer() (Importer, bool) { return f, f.canImport && !f.declareNot }
func (f *fmtHandler) Exporter() (Exporter, bool) { return f, f.canExport && !f.declareNot }

func (f *fmtHandler) Import(b Blob) (Blob, error) {
	if f.importErr != nil {
		return nil, f.importErr
	}
	return Blob("GLB(" + string(b) + ")"), nil
}

func (f *fmtHandler) Export(glb Blob, format string) (Blob, error) {
	f.gotFormat = format
	if f.exportErr != nil {
		return nil, f.exportErr
	}
	return Blob(f.ext + ":" + string(glb)), nil
}

func registry(hs ...ForgeHandler) *Registry {
	r := NewRegistry()
	for _, h := range hs {
		r.Register(h)
	}
	return r
}

func TestConvertPivotsThroughGLB(t *testing.T) {
	dst := &fmtHandler{ext: ".stl", canExport: true}
	r := registry(&fmtHandler{ext: ".obj", canImport: true}, dst)
	out, err := Convert(r, "parts/Bracket.OBJ", "STL", Blob("mesh"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != ".stl:GLB(mesh)" {
		t.Fatalf("got %q", out)
	}
	if dst.gotFormat != ".stl" {
		t.Fatalf("the exporter must be told the normalised target, got %q", dst.gotFormat)
	}
}

func TestConvertErrorsNameTheMissingSide(t *testing.T) {
	cases := []struct {
		name string
		reg  *Registry
		src  string
		to   string
		want string
	}{
		{"no source handler", registry(&fmtHandler{ext: ".stl", canExport: true}), "a.obj", ".stl", "cannot read .obj"},
		{"source cannot import", registry(&fmtHandler{ext: ".obj"}, &fmtHandler{ext: ".stl", canExport: true}), "a.obj", ".stl", "does not declare `import`"},
		{"no target handler", registry(&fmtHandler{ext: ".obj", canImport: true}), "a.obj", ".nope", "cannot write .nope"},
		{"target cannot export", registry(&fmtHandler{ext: ".obj", canImport: true}, &fmtHandler{ext: ".stl"}), "a.obj", ".stl", "does not declare `export`"},
		{"no target at all", registry(), "a.obj", "", "no target format"},
	}
	for _, c := range cases {
		_, err := Convert(c.reg, c.src, c.to, Blob("x"))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want it to contain %q", c.name, err, c.want)
		}
	}
}

func TestConvertReportsWhichSideFailed(t *testing.T) {
	boom := errors.New("boom")
	r := registry(&fmtHandler{ext: ".obj", canImport: true, importErr: boom}, &fmtHandler{ext: ".stl", canExport: true})
	if _, err := Convert(r, "a.obj", ".stl", nil); err == nil || !strings.Contains(err.Error(), "reading .obj") || !errors.Is(err, boom) {
		t.Fatalf("import failure: %v", err)
	}
	r = registry(&fmtHandler{ext: ".obj", canImport: true}, &fmtHandler{ext: ".stl", canExport: true, exportErr: boom})
	if _, err := Convert(r, "a.obj", ".stl", nil); err == nil || !strings.Contains(err.Error(), "writing .stl") || !errors.Is(err, boom) {
		t.Fatalf("export failure: %v", err)
	}
}

func TestNormalizeExt(t *testing.T) {
	for in, want := range map[string]string{"obj": ".obj", ".GLB": ".glb", " 3mf ": ".3mf", "": ""} {
		if got := NormalizeExt(in); got != want {
			t.Errorf("NormalizeExt(%q) = %q, want %q", in, got, want)
		}
	}
}
