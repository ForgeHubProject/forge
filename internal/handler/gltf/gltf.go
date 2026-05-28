// Package gltf provides a format-aware handler for .gltf and .glb files.
// It diffs scene-graph elements (nodes, materials, meshes, animations)
// semantically rather than as raw bytes.
package gltf

import (
	"bytes"
	"encoding/json"
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

// Merge performs a 3-way semantic merge of glTF/GLB blobs.
//
// The algorithm mirrors what git does for text at the line level, but operates
// on named scene-graph units instead:
//
//	only ours changed a property  → take ours  (already in result)
//	only theirs changed a property → take theirs (applied to result)
//	both changed to the same value → take either (no conflict)
//	both changed to different values → keep ours, record conflict
//
// Added/removed elements follow the same logic at the element level.
// The merged output is always a valid glTF/GLB; conflicts are reported in
// ConflictInfo and the caller (forge merge-file) exits 1, matching git behaviour.
func (h *Handler) Merge(base, ours, theirs handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	docBase, err := parseDoc(base)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing base: %w", err)
	}
	docOurs, err := parseDoc(ours)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing ours: %w", err)
	}
	docTheirs, err := parseDoc(theirs)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing theirs: %w", err)
	}

	var conflicts []handler.SemanticConflict

	docOurs.Nodes = mergeNodeList(docBase.Nodes, docOurs.Nodes, docTheirs.Nodes, &conflicts)
	docOurs.Materials = mergeMaterialList(docBase.Materials, docOurs.Materials, docTheirs.Materials, &conflicts)
	docOurs.Meshes = mergeMeshList(docBase.Meshes, docOurs.Meshes, docTheirs.Meshes, &conflicts)
	docOurs.Animations = mergeAnimationList(docBase.Animations, docOurs.Animations, docTheirs.Animations, &conflicts)

	result, err := encodeBlob(docOurs, isGLB(ours))
	if err != nil {
		return nil, nil, fmt.Errorf("encoding merged glTF: %w", err)
	}

	var ci *handler.ConflictInfo
	if len(conflicts) > 0 {
		ci = &handler.ConflictInfo{Conflicts: conflicts}
	}
	return result, ci, nil
}

// ── merge: nodes ──────────────────────────────────────────────────────────────

func mergeNodeList(base, ours, theirs []*gltf.Node, conflicts *[]handler.SemanticConflict) []*gltf.Node {
	baseMap, _ := nodeMap(base)
	oursMap, _ := nodeMap(ours)
	theirsMap, theirsOrder := nodeMap(theirs)

	// Walk ours order first, then append anything only theirs added.
	seen := make(map[string]bool)
	var names []string
	for i, n := range ours {
		k := nodeName(n, i)
		names = append(names, k)
		seen[k] = true
	}
	for i, n := range theirs {
		k := nodeName(n, i)
		if !seen[k] {
			names = append(names, k)
			seen[k] = true
		}
	}
	_ = theirsOrder

	var result []*gltf.Node
	for _, name := range names {
		bn := baseMap[name]   // nil if newly added by one side
		on, inOurs := oursMap[name]
		tn, inTheirs := theirsMap[name]

		switch {
		case inOurs && inTheirs:
			result = append(result, merge3Node(bn, on, tn, name, conflicts))

		case inOurs && !inTheirs:
			if bn != nil {
				// theirs removed it, ours kept it → conflict, keep ours
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "nodes." + name, Ours: "kept", Theirs: "removed",
				})
			}
			// !bn: only ours added → include
			result = append(result, on)

		case !inOurs && inTheirs:
			if bn != nil {
				// ours removed it, theirs kept it → conflict, ours wins (omit)
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "nodes." + name, Ours: "removed", Theirs: "kept",
				})
			} else {
				// only theirs added → include
				result = append(result, tn)
			}
		// !inOurs && !inTheirs: both removed → omit
		}
	}
	return result
}

// merge3Node merges a single node's properties 3-way.
// bn may be nil when both sides added a node with the same name.
func merge3Node(bn, on, tn *gltf.Node, name string, conflicts *[]handler.SemanticConflict) *gltf.Node {
	out := cloneNode(on)

	var baseTr, baseSc [3]float64
	var baseRotQ [4]float64
	if bn != nil {
		baseTr = bn.TranslationOrDefault()
		baseRotQ = bn.RotationOrDefault()
		baseSc = bn.ScaleOrDefault()
	} else {
		baseRotQ = gltf.DefaultRotation
		baseSc = gltf.DefaultScale
	}

	ourTr, theirTr := on.TranslationOrDefault(), tn.TranslationOrDefault()
	if !nearEq3(ourTr, baseTr) && !nearEq3(theirTr, baseTr) {
		if nearEq3(ourTr, theirTr) {
			out.Translation = ourTr
		} else {
			*conflicts = append(*conflicts, handler.SemanticConflict{
				Path: "nodes." + name + ".translation",
				Ours: fmtVec3(ourTr), Theirs: fmtVec3(theirTr),
			})
		}
	} else if nearEq3(ourTr, baseTr) && !nearEq3(theirTr, baseTr) {
		out.Translation = theirTr
	}

	ourRot, theirRot := on.RotationOrDefault(), tn.RotationOrDefault()
	if !nearEq4(ourRot, baseRotQ) && !nearEq4(theirRot, baseRotQ) {
		if nearEq4(ourRot, theirRot) {
			out.Rotation = ourRot
		} else {
			*conflicts = append(*conflicts, handler.SemanticConflict{
				Path: "nodes." + name + ".rotation",
				Ours: fmtRot(ourRot), Theirs: fmtRot(theirRot),
			})
		}
	} else if nearEq4(ourRot, baseRotQ) && !nearEq4(theirRot, baseRotQ) {
		out.Rotation = theirRot
	}

	ourSc, theirSc := on.ScaleOrDefault(), tn.ScaleOrDefault()
	if !nearEq3(ourSc, baseSc) && !nearEq3(theirSc, baseSc) {
		if nearEq3(ourSc, theirSc) {
			out.Scale = ourSc
		} else {
			*conflicts = append(*conflicts, handler.SemanticConflict{
				Path: "nodes." + name + ".scale",
				Ours: fmtVec3(ourSc), Theirs: fmtVec3(theirSc),
			})
		}
	} else if nearEq3(ourSc, baseSc) && !nearEq3(theirSc, baseSc) {
		out.Scale = theirSc
	}

	// Mesh reference
	baseMesh := ptrLabel(func() *int {
		if bn != nil {
			return bn.Mesh
		}
		return nil
	}(), "mesh")
	ourMesh, theirMesh := ptrLabel(on.Mesh, "mesh"), ptrLabel(tn.Mesh, "mesh")
	if ourMesh == baseMesh && theirMesh != baseMesh {
		out.Mesh = tn.Mesh
	} else if ourMesh != baseMesh && theirMesh != baseMesh && ourMesh != theirMesh {
		*conflicts = append(*conflicts, handler.SemanticConflict{
			Path: "nodes." + name + ".mesh",
			Ours: ourMesh, Theirs: theirMesh,
		})
	}

	return out
}

func cloneNode(n *gltf.Node) *gltf.Node {
	c := *n
	if n.Mesh != nil {
		m := *n.Mesh
		c.Mesh = &m
	}
	if n.Skin != nil {
		s := *n.Skin
		c.Skin = &s
	}
	if len(n.Children) > 0 {
		c.Children = make([]int, len(n.Children))
		copy(c.Children, n.Children)
	}
	return &c
}

// ── merge: materials ──────────────────────────────────────────────────────────

func mergeMaterialList(base, ours, theirs []*gltf.Material, conflicts *[]handler.SemanticConflict) []*gltf.Material {
	baseMap, _ := materialMap(base)
	oursMap, _ := materialMap(ours)
	theirsMap, _ := materialMap(theirs)

	seen := make(map[string]bool)
	var names []string
	for i, m := range ours {
		k := materialName(m, i)
		names = append(names, k)
		seen[k] = true
	}
	for i, m := range theirs {
		k := materialName(m, i)
		if !seen[k] {
			names = append(names, k)
			seen[k] = true
		}
	}

	var result []*gltf.Material
	for _, name := range names {
		bm := baseMap[name]
		om, inOurs := oursMap[name]
		tm, inTheirs := theirsMap[name]

		switch {
		case inOurs && inTheirs:
			result = append(result, merge3Material(bm, om, tm, name, conflicts))
		case inOurs && !inTheirs:
			if bm != nil {
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "materials." + name, Ours: "kept", Theirs: "removed",
				})
			}
			result = append(result, om)
		case !inOurs && inTheirs:
			if bm != nil {
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "materials." + name, Ours: "removed", Theirs: "kept",
				})
			} else {
				result = append(result, tm)
			}
		}
	}
	return result
}

func merge3Material(bm, om, tm *gltf.Material, name string, conflicts *[]handler.SemanticConflict) *gltf.Material {
	out := cloneMaterial(om)

	bPBR := pbrOrDefault(func() *gltf.Material {
		if bm != nil {
			return bm
		}
		return &gltf.Material{}
	}())
	oPBR := pbrOrDefault(om)
	tPBR := pbrOrDefault(tm)

	// baseColorFactor
	baseBC := bPBR.BaseColorFactorOrDefault()
	ourBC, theirBC := oPBR.BaseColorFactorOrDefault(), tPBR.BaseColorFactorOrDefault()
	if ourBC == baseBC && theirBC != baseBC {
		setBaseColor(out, theirBC)
	} else if ourBC != baseBC && theirBC != baseBC && ourBC != theirBC {
		*conflicts = append(*conflicts, handler.SemanticConflict{
			Path: "materials." + name + ".baseColorFactor",
			Ours: fmtVec4(ourBC), Theirs: fmtVec4(theirBC),
		})
	}

	// metallicFactor
	baseMet := bPBR.MetallicFactorOrDefault()
	ourMet, theirMet := oPBR.MetallicFactorOrDefault(), tPBR.MetallicFactorOrDefault()
	if nearEq(ourMet, baseMet) && !nearEq(theirMet, baseMet) {
		setMetallic(out, theirMet)
	} else if !nearEq(ourMet, baseMet) && !nearEq(theirMet, baseMet) && !nearEq(ourMet, theirMet) {
		*conflicts = append(*conflicts, handler.SemanticConflict{
			Path: "materials." + name + ".metallicFactor",
			Ours: fmtF(ourMet), Theirs: fmtF(theirMet),
		})
	}

	// roughnessFactor
	baseRough := bPBR.RoughnessFactorOrDefault()
	ourRough, theirRough := oPBR.RoughnessFactorOrDefault(), tPBR.RoughnessFactorOrDefault()
	if nearEq(ourRough, baseRough) && !nearEq(theirRough, baseRough) {
		setRoughness(out, theirRough)
	} else if !nearEq(ourRough, baseRough) && !nearEq(theirRough, baseRough) && !nearEq(ourRough, theirRough) {
		*conflicts = append(*conflicts, handler.SemanticConflict{
			Path: "materials." + name + ".roughnessFactor",
			Ours: fmtF(ourRough), Theirs: fmtF(theirRough),
		})
	}

	// alphaMode
	var baseAlpha gltf.AlphaMode
	if bm != nil {
		baseAlpha = bm.AlphaMode
	}
	if om.AlphaMode == baseAlpha && tm.AlphaMode != baseAlpha {
		out.AlphaMode = tm.AlphaMode
	} else if om.AlphaMode != baseAlpha && tm.AlphaMode != baseAlpha && om.AlphaMode != tm.AlphaMode {
		*conflicts = append(*conflicts, handler.SemanticConflict{
			Path: "materials." + name + ".alphaMode",
			Ours: string(om.AlphaMode), Theirs: string(tm.AlphaMode),
		})
	}

	// doubleSided
	var baseDS bool
	if bm != nil {
		baseDS = bm.DoubleSided
	}
	if om.DoubleSided == baseDS && tm.DoubleSided != baseDS {
		out.DoubleSided = tm.DoubleSided
	} else if om.DoubleSided != baseDS && tm.DoubleSided != baseDS && om.DoubleSided != tm.DoubleSided {
		*conflicts = append(*conflicts, handler.SemanticConflict{
			Path:   "materials." + name + ".doubleSided",
			Ours:   fmt.Sprintf("%v", om.DoubleSided),
			Theirs: fmt.Sprintf("%v", tm.DoubleSided),
		})
	}

	return out
}

func cloneMaterial(m *gltf.Material) *gltf.Material {
	c := *m
	if m.PBRMetallicRoughness != nil {
		pbr := *m.PBRMetallicRoughness
		if pbr.BaseColorFactor != nil {
			bc := *pbr.BaseColorFactor
			pbr.BaseColorFactor = &bc
		}
		if pbr.MetallicFactor != nil {
			mf := *pbr.MetallicFactor
			pbr.MetallicFactor = &mf
		}
		if pbr.RoughnessFactor != nil {
			rf := *pbr.RoughnessFactor
			pbr.RoughnessFactor = &rf
		}
		c.PBRMetallicRoughness = &pbr
	}
	return &c
}

func setBaseColor(m *gltf.Material, v [4]float64) {
	if m.PBRMetallicRoughness == nil {
		m.PBRMetallicRoughness = &gltf.PBRMetallicRoughness{}
	}
	m.PBRMetallicRoughness.BaseColorFactor = &v
}

func setMetallic(m *gltf.Material, v float64) {
	if m.PBRMetallicRoughness == nil {
		m.PBRMetallicRoughness = &gltf.PBRMetallicRoughness{}
	}
	m.PBRMetallicRoughness.MetallicFactor = &v
}

func setRoughness(m *gltf.Material, v float64) {
	if m.PBRMetallicRoughness == nil {
		m.PBRMetallicRoughness = &gltf.PBRMetallicRoughness{}
	}
	m.PBRMetallicRoughness.RoughnessFactor = &v
}

// ── merge: meshes ─────────────────────────────────────────────────────────────

// mergeMeshList merges mesh arrays 3-way treating each mesh as an atomic unit.
// Geometry data cannot be merged at the property level, so conflicting changes
// to the same mesh are reported and ours is kept.
func mergeMeshList(base, ours, theirs []*gltf.Mesh, conflicts *[]handler.SemanticConflict) []*gltf.Mesh {
	baseMap, _ := meshMap(base)
	oursMap, _ := meshMap(ours)
	theirsMap, _ := meshMap(theirs)

	seen := make(map[string]bool)
	var names []string
	for i, m := range ours {
		k := meshName(m, i)
		names = append(names, k)
		seen[k] = true
	}
	for i, m := range theirs {
		k := meshName(m, i)
		if !seen[k] {
			names = append(names, k)
			seen[k] = true
		}
	}

	var result []*gltf.Mesh
	for _, name := range names {
		bm := baseMap[name]
		om, inOurs := oursMap[name]
		tm, inTheirs := theirsMap[name]

		switch {
		case inOurs && inTheirs:
			ourChanged := !jsonEqual(bm, om)
			theirChanged := !jsonEqual(bm, tm)
			switch {
			case !ourChanged && theirChanged:
				result = append(result, tm)
			case ourChanged && theirChanged && !jsonEqual(om, tm):
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path:   "meshes." + name,
					Ours:   fmt.Sprintf("%d primitives", len(om.Primitives)),
					Theirs: fmt.Sprintf("%d primitives", len(tm.Primitives)),
				})
				result = append(result, om)
			default:
				result = append(result, om)
			}
		case inOurs && !inTheirs:
			if bm != nil {
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "meshes." + name, Ours: "kept", Theirs: "removed",
				})
			}
			result = append(result, om)
		case !inOurs && inTheirs:
			if bm != nil {
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "meshes." + name, Ours: "removed", Theirs: "kept",
				})
			} else {
				result = append(result, tm)
			}
		}
	}
	return result
}

// ── merge: animations ─────────────────────────────────────────────────────────

func mergeAnimationList(base, ours, theirs []*gltf.Animation, conflicts *[]handler.SemanticConflict) []*gltf.Animation {
	baseMap, _ := animMap(base)
	oursMap, _ := animMap(ours)
	theirsMap, _ := animMap(theirs)

	seen := make(map[string]bool)
	var names []string
	for i, a := range ours {
		k := animName(a, i)
		names = append(names, k)
		seen[k] = true
	}
	for i, a := range theirs {
		k := animName(a, i)
		if !seen[k] {
			names = append(names, k)
			seen[k] = true
		}
	}

	var result []*gltf.Animation
	for _, name := range names {
		ba := baseMap[name]
		oa, inOurs := oursMap[name]
		ta, inTheirs := theirsMap[name]

		switch {
		case inOurs && inTheirs:
			ourChanged := !jsonEqual(ba, oa)
			theirChanged := !jsonEqual(ba, ta)
			switch {
			case !ourChanged && theirChanged:
				result = append(result, ta)
			case ourChanged && theirChanged && !jsonEqual(oa, ta):
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path:   "animations." + name,
					Ours:   fmt.Sprintf("%d channels", len(oa.Channels)),
					Theirs: fmt.Sprintf("%d channels", len(ta.Channels)),
				})
				result = append(result, oa)
			default:
				result = append(result, oa)
			}
		case inOurs && !inTheirs:
			if ba != nil {
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "animations." + name, Ours: "kept", Theirs: "removed",
				})
			}
			result = append(result, oa)
		case !inOurs && inTheirs:
			if ba != nil {
				*conflicts = append(*conflicts, handler.SemanticConflict{
					Path: "animations." + name, Ours: "removed", Theirs: "kept",
				})
			} else {
				result = append(result, ta)
			}
		}
	}
	return result
}

// jsonEqual reports whether a and b serialize to the same JSON. Used to detect
// whether a mesh or animation changed between base and one of the merge sides.
func jsonEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return bytes.Equal(aj, bj)
}

// ── conflict resolution ───────────────────────────────────────────────────────

// ApplyChoices implements handler.ConflictApplier.
// merged already holds "ours" for every conflict; takePaths lists the conflict
// paths where the user chose "incoming" (theirs). Those values are copied from
// theirs into merged and the result is re-encoded as a valid glTF/GLB.
func (h *Handler) ApplyChoices(merged, theirs handler.Blob, takePaths []string) (handler.Blob, error) {
	if len(takePaths) == 0 {
		return merged, nil
	}
	docM, err := parseDoc(merged)
	if err != nil {
		return nil, fmt.Errorf("parsing merged: %w", err)
	}
	docT, err := parseDoc(theirs)
	if err != nil {
		return nil, fmt.Errorf("parsing theirs: %w", err)
	}
	for _, path := range takePaths {
		applyChoice(docM, docT, path)
	}
	return encodeBlob(docM, isGLB(merged))
}

// applyChoice copies the value at path from docT into docM.
// path format: "nodes.Name[.property]" or "materials.Name[.property]"
func applyChoice(docM, docT *gltf.Document, path string) {
	parts := strings.SplitN(path, ".", 3)
	if len(parts) < 2 {
		return
	}
	name := parts[1]

	switch parts[0] {
	case "nodes":
		tn := nodeByName(docT.Nodes, name)
		mn := nodeByName(docM.Nodes, name)
		if len(parts) == 2 {
			if tn != nil && mn == nil {
				docM.Nodes = append(docM.Nodes, tn) // ours removed, theirs kept → add
			} else if tn == nil && mn != nil {
				docM.Nodes = removeNode(docM.Nodes, name) // theirs removed, ours kept → remove
			}
			return
		}
		if mn == nil || tn == nil {
			return
		}
		switch parts[2] {
		case "translation":
			mn.Translation = tn.TranslationOrDefault()
		case "rotation":
			mn.Rotation = tn.RotationOrDefault()
		case "scale":
			mn.Scale = tn.ScaleOrDefault()
		case "mesh":
			mn.Mesh = tn.Mesh
		}

	case "materials":
		tm := materialByName(docT.Materials, name)
		mm := materialByName(docM.Materials, name)
		if len(parts) == 2 {
			if tm != nil && mm == nil {
				docM.Materials = append(docM.Materials, tm)
			} else if tm == nil && mm != nil {
				docM.Materials = removeMaterial(docM.Materials, name)
			}
			return
		}
		if mm == nil || tm == nil {
			return
		}
		tPBR := pbrOrDefault(tm)
		switch parts[2] {
		case "baseColorFactor":
			setBaseColor(mm, tPBR.BaseColorFactorOrDefault())
		case "metallicFactor":
			setMetallic(mm, tPBR.MetallicFactorOrDefault())
		case "roughnessFactor":
			setRoughness(mm, tPBR.RoughnessFactorOrDefault())
		case "alphaMode":
			mm.AlphaMode = tm.AlphaMode
		case "doubleSided":
			mm.DoubleSided = tm.DoubleSided
		}
	}
}

func nodeByName(nodes []*gltf.Node, name string) *gltf.Node {
	for i, n := range nodes {
		if nodeName(n, i) == name {
			return n
		}
	}
	return nil
}

func removeNode(nodes []*gltf.Node, name string) []*gltf.Node {
	out := nodes[:0:0]
	for i, n := range nodes {
		if nodeName(n, i) != name {
			out = append(out, n)
		}
	}
	return out
}

func materialByName(mats []*gltf.Material, name string) *gltf.Material {
	for i, m := range mats {
		if materialName(m, i) == name {
			return m
		}
	}
	return nil
}

func removeMaterial(mats []*gltf.Material, name string) []*gltf.Material {
	out := mats[:0:0]
	for i, m := range mats {
		if materialName(m, i) != name {
			out = append(out, m)
		}
	}
	return out
}

// ── serialisation ─────────────────────────────────────────────────────────────

// isGLB returns true if the blob starts with the GLB magic bytes ("glTF").
func isGLB(blob handler.Blob) bool {
	return len(blob) >= 4 && string(blob[:4]) == "glTF"
}

func encodeBlob(doc *gltf.Document, binary bool) ([]byte, error) {
	var buf bytes.Buffer
	enc := gltf.NewEncoder(&buf)
	enc.AsBinary = binary
	if err := enc.Encode(doc); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
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
			Kind: handler.Modified, Before: fmtRot(ra), After: fmtRot(rb),
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

// quatToEulerXYZ converts a glTF quaternion [x,y,z,w] to XYZ Euler angles in
// degrees. Quaternions are orientation-only (no winding count), so a value like
// "594°" from a DCC tool becomes its equivalent orientation in [-180°, 180°].
func quatToEulerXYZ(q [4]float64) [3]float64 {
	qx, qy, qz, qw := q[0], q[1], q[2], q[3]
	const toDeg = 180.0 / math.Pi

	// X (roll)
	sinr := 2 * (qw*qx + qy*qz)
	cosr := 1 - 2*(qx*qx+qy*qy)
	x := math.Atan2(sinr, cosr) * toDeg

	// Y (pitch) — clamp to avoid NaN at poles
	sinp := 2 * (qw*qy - qz*qx)
	if sinp > 1 {
		sinp = 1
	} else if sinp < -1 {
		sinp = -1
	}
	y := math.Asin(sinp) * toDeg

	// Z (yaw)
	siny := 2 * (qw*qz + qx*qy)
	cosy := 1 - 2*(qy*qy+qz*qz)
	z := math.Atan2(siny, cosy) * toDeg

	return [3]float64{x, y, z}
}

// fmtRot formats a quaternion as human-readable XYZ Euler degrees.
func fmtRot(q [4]float64) string {
	e := quatToEulerXYZ(q)
	return fmt.Sprintf("x=%s° y=%s° z=%s°", fmtF(e[0]), fmtF(e[1]), fmtF(e[2]))
}
func fmtF(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 32)
}

func fmtVec3(v [3]float64) string {
	return fmt.Sprintf("x=%s y=%s z=%s", fmtF(v[0]), fmtF(v[1]), fmtF(v[2]))
}

func fmtVec4(v [4]float64) string {
	return fmt.Sprintf("r=%s g=%s b=%s a=%s", fmtF(v[0]), fmtF(v[1]), fmtF(v[2]), fmtF(v[3]))
}

func ptrLabel(p *int, prefix string) string {
	if p == nil {
		return "<none>"
	}
	return fmt.Sprintf("%s[%d]", prefix, *p)
}
