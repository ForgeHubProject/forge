// Package gltf provides a format-aware handler for .gltf and .glb files.
// It diffs scene-graph elements (nodes, materials, meshes, animations)
// semantically rather than as raw bytes.
package gltf

import (
	"bytes"
	"fmt"
	"math"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/qmuntal/gltf"
	"github.com/yakupatahanov/forge/internal/handler"
)

// Handler is the glTF/GLB format handler.
type Handler struct{}

// New returns a new Handler.
func New() *Handler { return &Handler{} }

// Format implements handler.Namer.
func (h *Handler) Format() string { return "gltf" }

// Match returns true for .gltf and .glb files.
func (h *Handler) Match(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".gltf" || ext == ".glb"
}

// Merge is not yet supported for glTF.
func (h *Handler) Merge(_, _, _ handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	return nil, nil, handler.ErrNotSupported
}

// Diff produces a hierarchical semantic diff of two glTF/GLB blobs,
// comparing nodes (translation/rotation/scale), materials (PBR factors),
// meshes (name/primitive count), and animations (name/channel count).
func (h *Handler) Diff(base, head handler.Blob) (handler.StructuredDiff, error) {
	docA, err := parseDoc(base)
	if err != nil {
		return handler.StructuredDiff{}, fmt.Errorf("parsing base: %w", err)
	}
	docB, err := parseDoc(head)
	if err != nil {
		return handler.StructuredDiff{}, fmt.Errorf("parsing head: %w", err)
	}

	var changes []handler.DiffChange
	if c := diffNodes(docA, docB); c != nil {
		changes = append(changes, *c)
	}
	if c := diffMaterials(docA, docB); c != nil {
		changes = append(changes, *c)
	}
	if c := diffMeshes(docA, docB); c != nil {
		changes = append(changes, *c)
	}
	if c := diffAnimations(docA, docB); c != nil {
		changes = append(changes, *c)
	}

	return handler.StructuredDiff{Version: "1.0", Format: "gltf", Changes: changes}, nil
}

// parseDoc decodes a glTF/GLB blob into a Document.
// Buffer-loading errors (external .bin files not available in blob context)
// are tolerated as long as the JSON structure was parsed successfully.
func parseDoc(blob handler.Blob) (*gltf.Document, error) {
	doc := new(gltf.Document)
	err := gltf.NewDecoder(bytes.NewReader(blob)).Decode(doc)
	if err != nil && doc.Asset.Version == "" {
		return nil, fmt.Errorf("failed to parse glTF: %w", err)
	}
	return doc, nil
}

// ── nodes ─────────────────────────────────────────────────────────────────────

func diffNodes(a, b *gltf.Document) *handler.DiffChange {
	aMap, aOrder := nodeMap(a.Nodes)
	bMap, _ := nodeMap(b.Nodes)

	// Stable order: base nodes first, then nodes only in head.
	seen := make(map[string]bool)
	names := make([]string, 0, len(a.Nodes)+len(b.Nodes))
	for _, k := range aOrder {
		names = append(names, k)
		seen[k] = true
	}
	for i, n := range b.Nodes {
		k := nodeName(n, i)
		if !seen[k] {
			names = append(names, k)
		}
	}

	var children []handler.DiffChange
	for _, name := range names {
		an, inA := aMap[name]
		bn, inB := bMap[name]

		switch {
		case !inA:
			children = append(children, handler.DiffChange{
				Path: "nodes." + name, Label: name,
				Kind: handler.Added, After: "node",
			})
		case !inB:
			children = append(children, handler.DiffChange{
				Path: "nodes." + name, Label: name,
				Kind: handler.Removed, Before: "node",
			})
		default:
			if props := diffNodeProps(an, bn); len(props) > 0 {
				children = append(children, handler.DiffChange{
					Path:     "nodes." + name,
					Label:    name,
					Kind:     handler.Modified,
					Children: props,
				})
			}
		}
	}

	if len(children) == 0 {
		return nil
	}
	return &handler.DiffChange{
		Path: "nodes", Label: "nodes",
		Kind:     handler.Modified,
		Children: children,
	}
}

func nodeMap(nodes []*gltf.Node) (map[string]*gltf.Node, []string) {
	m := make(map[string]*gltf.Node, len(nodes))
	order := make([]string, 0, len(nodes))
	for i, n := range nodes {
		k := nodeName(n, i)
		if _, dup := m[k]; !dup {
			m[k] = n
			order = append(order, k)
		}
	}
	return m, order
}

func nodeName(n *gltf.Node, i int) string {
	if n.Name != "" {
		return n.Name
	}
	return fmt.Sprintf("node[%d]", i)
}

func diffNodeProps(a, b *gltf.Node) []handler.DiffChange {
	var changes []handler.DiffChange

	if ta, tb := a.TranslationOrDefault(), b.TranslationOrDefault(); !nearEq3(ta, tb) {
		changes = append(changes, handler.DiffChange{
			Path: "translation", Label: "translation",
			Kind: handler.Modified, Before: fmtVec3(ta), After: fmtVec3(tb),
		})
	}

	if ra, rb := a.RotationOrDefault(), b.RotationOrDefault(); !nearEq4(ra, rb) {
		changes = append(changes, handler.DiffChange{
			Path: "rotation", Label: "rotation",
			Kind: handler.Modified, Before: fmtVec4(ra), After: fmtVec4(rb),
		})
	}

	if sa, sb := a.ScaleOrDefault(), b.ScaleOrDefault(); !nearEq3(sa, sb) {
		changes = append(changes, handler.DiffChange{
			Path: "scale", Label: "scale",
			Kind: handler.Modified, Before: fmtVec3(sa), After: fmtVec3(sb),
		})
	}

	meshA, meshB := ptrLabel(a.Mesh, "mesh"), ptrLabel(b.Mesh, "mesh")
	if meshA != meshB {
		changes = append(changes, handler.DiffChange{
			Path: "mesh", Label: "mesh",
			Kind: handler.Modified, Before: meshA, After: meshB,
		})
	}

	return changes
}

// ── materials ─────────────────────────────────────────────────────────────────

func diffMaterials(a, b *gltf.Document) *handler.DiffChange {
	aMap, aOrder := materialMap(a.Materials)
	bMap, _ := materialMap(b.Materials)

	seen := make(map[string]bool)
	names := make([]string, 0, len(a.Materials)+len(b.Materials))
	for _, k := range aOrder {
		names = append(names, k)
		seen[k] = true
	}
	for i, m := range b.Materials {
		k := materialName(m, i)
		if !seen[k] {
			names = append(names, k)
		}
	}

	var children []handler.DiffChange
	for _, name := range names {
		am, inA := aMap[name]
		bm, inB := bMap[name]

		switch {
		case !inA:
			children = append(children, handler.DiffChange{
				Path: "materials." + name, Label: name,
				Kind: handler.Added, After: "material",
			})
		case !inB:
			children = append(children, handler.DiffChange{
				Path: "materials." + name, Label: name,
				Kind: handler.Removed, Before: "material",
			})
		default:
			if props := diffMaterialProps(am, bm); len(props) > 0 {
				children = append(children, handler.DiffChange{
					Path:     "materials." + name,
					Label:    name,
					Kind:     handler.Modified,
					Children: props,
				})
			}
		}
	}

	if len(children) == 0 {
		return nil
	}
	return &handler.DiffChange{
		Path: "materials", Label: "materials",
		Kind:     handler.Modified,
		Children: children,
	}
}

func materialMap(mats []*gltf.Material) (map[string]*gltf.Material, []string) {
	m := make(map[string]*gltf.Material, len(mats))
	order := make([]string, 0, len(mats))
	for i, mat := range mats {
		k := materialName(mat, i)
		if _, dup := m[k]; !dup {
			m[k] = mat
			order = append(order, k)
		}
	}
	return m, order
}

func materialName(m *gltf.Material, i int) string {
	if m.Name != "" {
		return m.Name
	}
	return fmt.Sprintf("material[%d]", i)
}

func diffMaterialProps(a, b *gltf.Material) []handler.DiffChange {
	var changes []handler.DiffChange

	// PBR metallic-roughness
	aPBR := pbrOrDefault(a)
	bPBR := pbrOrDefault(b)

	if ca, cb := aPBR.BaseColorFactorOrDefault(), bPBR.BaseColorFactorOrDefault(); ca != cb {
		changes = append(changes, handler.DiffChange{
			Path: "baseColorFactor", Label: "baseColorFactor",
			Kind: handler.Modified, Before: fmtVec4(ca), After: fmtVec4(cb),
		})
	}
	if ma, mb := aPBR.MetallicFactorOrDefault(), bPBR.MetallicFactorOrDefault(); !nearEq(ma, mb) {
		changes = append(changes, handler.DiffChange{
			Path: "metallicFactor", Label: "metallicFactor",
			Kind: handler.Modified, Before: fmtF(ma), After: fmtF(mb),
		})
	}
	if ra, rb := aPBR.RoughnessFactorOrDefault(), bPBR.RoughnessFactorOrDefault(); !nearEq(ra, rb) {
		changes = append(changes, handler.DiffChange{
			Path: "roughnessFactor", Label: "roughnessFactor",
			Kind: handler.Modified, Before: fmtF(ra), After: fmtF(rb),
		})
	}

	if a.EmissiveFactor != b.EmissiveFactor {
		changes = append(changes, handler.DiffChange{
			Path: "emissiveFactor", Label: "emissiveFactor",
			Kind: handler.Modified, Before: fmtVec3(a.EmissiveFactor), After: fmtVec3(b.EmissiveFactor),
		})
	}

	if a.AlphaMode != b.AlphaMode {
		changes = append(changes, handler.DiffChange{
			Path: "alphaMode", Label: "alphaMode",
			Kind: handler.Modified, Before: string(a.AlphaMode), After: string(b.AlphaMode),
		})
	}

	if a.DoubleSided != b.DoubleSided {
		changes = append(changes, handler.DiffChange{
			Path: "doubleSided", Label: "doubleSided",
			Kind: handler.Modified, Before: fmt.Sprintf("%v", a.DoubleSided), After: fmt.Sprintf("%v", b.DoubleSided),
		})
	}

	return changes
}

func pbrOrDefault(m *gltf.Material) *gltf.PBRMetallicRoughness {
	if m.PBRMetallicRoughness != nil {
		return m.PBRMetallicRoughness
	}
	return &gltf.PBRMetallicRoughness{}
}

// ── meshes ────────────────────────────────────────────────────────────────────

func diffMeshes(a, b *gltf.Document) *handler.DiffChange {
	aMap, aOrder := meshMap(a.Meshes)
	bMap, _ := meshMap(b.Meshes)

	seen := make(map[string]bool)
	names := make([]string, 0, len(a.Meshes)+len(b.Meshes))
	for _, k := range aOrder {
		names = append(names, k)
		seen[k] = true
	}
	for i, m := range b.Meshes {
		k := meshName(m, i)
		if !seen[k] {
			names = append(names, k)
		}
	}

	var children []handler.DiffChange
	for _, name := range names {
		am, inA := aMap[name]
		bm, inB := bMap[name]

		switch {
		case !inA:
			children = append(children, handler.DiffChange{
				Path: "meshes." + name, Label: name,
				Kind: handler.Added, After: fmt.Sprintf("%d primitives", len(bm.Primitives)),
			})
		case !inB:
			children = append(children, handler.DiffChange{
				Path: "meshes." + name, Label: name,
				Kind: handler.Removed, Before: fmt.Sprintf("%d primitives", len(am.Primitives)),
			})
		default:
			if len(am.Primitives) != len(bm.Primitives) {
				children = append(children, handler.DiffChange{
					Path:  "meshes." + name,
					Label: name,
					Kind:  handler.Modified,
					Children: []handler.DiffChange{{
						Path: "primitives", Label: "primitives",
						Kind:   handler.Modified,
						Before: fmt.Sprintf("%d", len(am.Primitives)),
						After:  fmt.Sprintf("%d", len(bm.Primitives)),
					}},
				})
			}
		}
	}

	if len(children) == 0 {
		return nil
	}
	return &handler.DiffChange{
		Path: "meshes", Label: "meshes",
		Kind:     handler.Modified,
		Children: children,
	}
}

func meshMap(meshes []*gltf.Mesh) (map[string]*gltf.Mesh, []string) {
	m := make(map[string]*gltf.Mesh, len(meshes))
	order := make([]string, 0, len(meshes))
	for i, mesh := range meshes {
		k := meshName(mesh, i)
		if _, dup := m[k]; !dup {
			m[k] = mesh
			order = append(order, k)
		}
	}
	return m, order
}

func meshName(m *gltf.Mesh, i int) string {
	if m.Name != "" {
		return m.Name
	}
	return fmt.Sprintf("mesh[%d]", i)
}

// ── animations ────────────────────────────────────────────────────────────────

func diffAnimations(a, b *gltf.Document) *handler.DiffChange {
	aMap, aOrder := animMap(a.Animations)
	bMap, _ := animMap(b.Animations)

	seen := make(map[string]bool)
	names := make([]string, 0, len(a.Animations)+len(b.Animations))
	for _, k := range aOrder {
		names = append(names, k)
		seen[k] = true
	}
	for i, an := range b.Animations {
		k := animName(an, i)
		if !seen[k] {
			names = append(names, k)
		}
	}

	var children []handler.DiffChange
	for _, name := range names {
		aa, inA := aMap[name]
		ba, inB := bMap[name]

		switch {
		case !inA:
			children = append(children, handler.DiffChange{
				Path: "animations." + name, Label: name,
				Kind: handler.Added, After: fmt.Sprintf("%d channels", len(ba.Channels)),
			})
		case !inB:
			children = append(children, handler.DiffChange{
				Path: "animations." + name, Label: name,
				Kind: handler.Removed, Before: fmt.Sprintf("%d channels", len(aa.Channels)),
			})
		default:
			if len(aa.Channels) != len(ba.Channels) {
				children = append(children, handler.DiffChange{
					Path:  "animations." + name,
					Label: name,
					Kind:  handler.Modified,
					Children: []handler.DiffChange{{
						Path: "channels", Label: "channels",
						Kind:   handler.Modified,
						Before: fmt.Sprintf("%d", len(aa.Channels)),
						After:  fmt.Sprintf("%d", len(ba.Channels)),
					}},
				})
			}
		}
	}

	if len(children) == 0 {
		return nil
	}
	return &handler.DiffChange{
		Path: "animations", Label: "animations",
		Kind:     handler.Modified,
		Children: children,
	}
}

func animMap(anims []*gltf.Animation) (map[string]*gltf.Animation, []string) {
	m := make(map[string]*gltf.Animation, len(anims))
	order := make([]string, 0, len(anims))
	for i, a := range anims {
		k := animName(a, i)
		if _, dup := m[k]; !dup {
			m[k] = a
			order = append(order, k)
		}
	}
	return m, order
}

func animName(a *gltf.Animation, i int) string {
	if a.Name != "" {
		return a.Name
	}
	return fmt.Sprintf("anim[%d]", i)
}

// ── formatting helpers ────────────────────────────────────────────────────────

const eps = 1e-5

func nearEq(a, b float64) bool  { return math.Abs(a-b) < eps }
func nearEq3(a, b [3]float64) bool {
	return nearEq(a[0], b[0]) && nearEq(a[1], b[1]) && nearEq(a[2], b[2])
}
func nearEq4(a, b [4]float64) bool {
	return nearEq(a[0], b[0]) && nearEq(a[1], b[1]) && nearEq(a[2], b[2]) && nearEq(a[3], b[3])
}

// fmtF formats a float64 using float32 precision (matches GLB binary storage).
func fmtF(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 32)
}

func fmtVec3(v [3]float64) string {
	return fmt.Sprintf("[%s %s %s]", fmtF(v[0]), fmtF(v[1]), fmtF(v[2]))
}

func fmtVec4(v [4]float64) string {
	return fmt.Sprintf("[%s %s %s %s]", fmtF(v[0]), fmtF(v[1]), fmtF(v[2]), fmtF(v[3]))
}

func ptrLabel(p *int, prefix string) string {
	if p == nil {
		return "<none>"
	}
	return fmt.Sprintf("%s[%d]", prefix, *p)
}
