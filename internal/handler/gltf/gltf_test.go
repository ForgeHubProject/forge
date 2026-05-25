package gltf

import (
	"encoding/json"
	"testing"
)

// minDoc returns a minimal glTF JSON blob with the given nodes and materials.
func minDoc(nodes []any, materials []any) []byte {
	doc := map[string]any{
		"asset": map[string]any{"version": "2.0"},
	}
	if nodes != nil {
		doc["nodes"] = nodes
	}
	if materials != nil {
		doc["materials"] = materials
	}
	b, _ := json.Marshal(doc)
	return b
}

func node(name string, translation []float64) map[string]any {
	n := map[string]any{"name": name}
	if translation != nil {
		n["translation"] = translation
	}
	return n
}

func material(name string, baseColor []float64) map[string]any {
	m := map[string]any{"name": name}
	if baseColor != nil {
		m["pbrMetallicRoughness"] = map[string]any{
			"baseColorFactor": baseColor,
		}
	}
	return m
}

// ── diff tests ────────────────────────────────────────────────────────────────

func TestDiff_NoChanges(t *testing.T) {
	doc := minDoc(
		[]any{node("Cube", []float64{0, 0, 0})},
		nil,
	)
	h := New()
	diff, err := h.Diff(doc, doc)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 0 {
		t.Errorf("expected no changes, got %d", len(diff.Changes))
	}
}

func TestDiff_NodeTranslation(t *testing.T) {
	base := minDoc([]any{node("Armature", []float64{0, 0, 0})}, nil)
	head := minDoc([]any{node("Armature", []float64{0, 1.8, 0})}, nil)

	diff, err := New().Diff(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 {
		t.Fatalf("expected 1 section change, got %d", len(diff.Changes))
	}
	nodes := diff.Changes[0]
	if nodes.Label != "nodes" {
		t.Errorf("expected section label 'nodes', got %q", nodes.Label)
	}
	if len(nodes.Children) != 1 || nodes.Children[0].Label != "Armature" {
		t.Fatalf("expected Armature child")
	}
	props := nodes.Children[0].Children
	if len(props) != 1 || props[0].Label != "translation" {
		t.Fatalf("expected translation property, got %v", props)
	}
	if props[0].Before != "[0 0 0]" || props[0].After != "[0 1.8 0]" {
		t.Errorf("unexpected values: %v → %v", props[0].Before, props[0].After)
	}
}

func TestDiff_NodeAdded(t *testing.T) {
	base := minDoc([]any{node("Cube", nil)}, nil)
	head := minDoc([]any{node("Cube", nil), node("Lamp", nil)}, nil)

	diff, err := New().Diff(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 {
		t.Fatalf("expected 1 section, got %d", len(diff.Changes))
	}
	ch := diff.Changes[0].Children
	if len(ch) != 1 || ch[0].Label != "Lamp" {
		t.Fatalf("expected Lamp added, got %v", ch)
	}
}

func TestDiff_MaterialBaseColor(t *testing.T) {
	base := minDoc(nil, []any{material("Skin", []float64{0.8, 0.6, 0.5, 1.0})})
	head := minDoc(nil, []any{material("Skin", []float64{0.9, 0.7, 0.6, 1.0})})

	diff, err := New().Diff(base, head)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 1 {
		t.Fatalf("expected 1 section, got %d", len(diff.Changes))
	}
	prop := diff.Changes[0].Children[0].Children[0]
	if prop.Label != "baseColorFactor" {
		t.Errorf("expected baseColorFactor, got %q", prop.Label)
	}
}

// ── merge tests ───────────────────────────────────────────────────────────────

func TestMerge_CleanNonOverlapping(t *testing.T) {
	// ours moved Armature, theirs changed Skin material — no overlap
	base := minDoc(
		[]any{node("Armature", []float64{0, 0, 0})},
		[]any{material("Skin", []float64{0.8, 0.6, 0.5, 1})},
	)
	ours := minDoc(
		[]any{node("Armature", []float64{0, 1.8, 0})}, // translation changed
		[]any{material("Skin", []float64{0.8, 0.6, 0.5, 1})},
	)
	theirs := minDoc(
		[]any{node("Armature", []float64{0, 0, 0})},
		[]any{material("Skin", []float64{0.9, 0.7, 0.6, 1})}, // color changed
	)

	h := New()
	result, ci, err := h.Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci != nil {
		t.Errorf("expected clean merge, got conflicts: %v", ci.Conflicts)
	}

	// Parse result and verify both changes are present
	doc, err := parseDoc(result)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Nodes) != 1 || doc.Nodes[0].Name != "Armature" {
		t.Fatal("Armature node missing from merged result")
	}
	tr := doc.Nodes[0].TranslationOrDefault()
	if !nearEq(tr[1], 1.8) {
		t.Errorf("expected Armature.translation.y = 1.8, got %v", tr[1])
	}
	if len(doc.Materials) != 1 {
		t.Fatal("Skin material missing from merged result")
	}
	bc := doc.Materials[0].PBRMetallicRoughness.BaseColorFactorOrDefault()
	if !nearEq(bc[0], 0.9) {
		t.Errorf("expected baseColorFactor.r = 0.9, got %v", bc[0])
	}
}

func TestMerge_BothSidesChangeSameProperty_Conflict(t *testing.T) {
	base := minDoc([]any{node("Cube", []float64{0, 0, 0})}, nil)
	ours := minDoc([]any{node("Cube", []float64{0, 1.0, 0})}, nil)
	theirs := minDoc([]any{node("Cube", []float64{0, 2.0, 0})}, nil)

	_, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci == nil || len(ci.Conflicts) == 0 {
		t.Fatal("expected conflict, got none")
	}
	if ci.Conflicts[0].Path != "nodes.Cube.translation" {
		t.Errorf("unexpected conflict path: %s", ci.Conflicts[0].Path)
	}
}

func TestMerge_BothSidesChangeSamePropertyToSameValue_Clean(t *testing.T) {
	// Both independently made the same change — should merge cleanly
	base := minDoc([]any{node("Cube", []float64{0, 0, 0})}, nil)
	ours := minDoc([]any{node("Cube", []float64{0, 5.0, 0})}, nil)
	theirs := minDoc([]any{node("Cube", []float64{0, 5.0, 0})}, nil)

	_, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci != nil {
		t.Errorf("expected clean merge for convergent changes, got conflicts: %v", ci.Conflicts)
	}
}

func TestMerge_TheirsAddsNode(t *testing.T) {
	base := minDoc([]any{node("Cube", nil)}, nil)
	ours := minDoc([]any{node("Cube", nil)}, nil)
	theirs := minDoc([]any{node("Cube", nil), node("Lamp", nil)}, nil)

	result, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci != nil {
		t.Errorf("expected clean merge, got conflicts: %v", ci.Conflicts)
	}
	doc, _ := parseDoc(result)
	if len(doc.Nodes) != 2 {
		t.Errorf("expected 2 nodes in merged result, got %d", len(doc.Nodes))
	}
}

func TestMerge_TheirsRemovesNode_OursKept_Conflict(t *testing.T) {
	base := minDoc([]any{node("Cube", nil), node("Lamp", nil)}, nil)
	ours := minDoc([]any{node("Cube", nil), node("Lamp", nil)}, nil) // kept both
	theirs := minDoc([]any{node("Cube", nil)}, nil)                  // removed Lamp

	_, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci == nil || len(ci.Conflicts) == 0 {
		t.Fatal("expected conflict for remove-vs-keep")
	}
	if ci.Conflicts[0].Path != "nodes.Lamp" {
		t.Errorf("unexpected conflict path: %s", ci.Conflicts[0].Path)
	}
}

func TestMerge_IdenticalFiles_Clean(t *testing.T) {
	doc := minDoc([]any{node("Cube", []float64{1, 2, 3})}, nil)
	_, ci, err := New().Merge(doc, doc, doc)
	if err != nil {
		t.Fatal(err)
	}
	if ci != nil {
		t.Errorf("expected no conflicts for identical merge, got: %v", ci.Conflicts)
	}
}

// ── ApplyChoices tests ────────────────────────────────────────────────────────

func TestApplyChoices_TakeTheirsTranslation(t *testing.T) {
	base := minDoc([]any{node("Cube", []float64{0, 0, 0})}, nil)
	ours := minDoc([]any{node("Cube", []float64{0, 1.0, 0})}, nil)
	theirs := minDoc([]any{node("Cube", []float64{0, 2.0, 0})}, nil)

	h := New()
	merged, ci, err := h.Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci == nil || len(ci.Conflicts) == 0 {
		t.Fatal("expected conflict")
	}

	// User picks incoming (theirs) for the translation conflict.
	result, err := h.ApplyChoices(merged, theirs, []string{"nodes.Cube.translation"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseDoc(result)
	if err != nil {
		t.Fatal(err)
	}
	tr := doc.Nodes[0].TranslationOrDefault()
	if !nearEq(tr[1], 2.0) {
		t.Errorf("expected y=2.0 (theirs), got %v", tr[1])
	}
}

func TestApplyChoices_KeepOurs_NoChange(t *testing.T) {
	base := minDoc([]any{node("Cube", []float64{0, 0, 0})}, nil)
	ours := minDoc([]any{node("Cube", []float64{0, 1.0, 0})}, nil)
	theirs := minDoc([]any{node("Cube", []float64{0, 2.0, 0})}, nil)

	h := New()
	merged, _, err := h.Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}

	// User picks current (ours) for everything — no takePaths.
	result, err := h.ApplyChoices(merged, theirs, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := parseDoc(result)
	if err != nil {
		t.Fatal(err)
	}
	tr := doc.Nodes[0].TranslationOrDefault()
	if !nearEq(tr[1], 1.0) {
		t.Errorf("expected y=1.0 (ours), got %v", tr[1])
	}
}

func TestApplyChoices_TakeTheirsRemovedNode(t *testing.T) {
	// Ours kept Lamp; theirs removed it. User picks theirs (remove).
	base := minDoc([]any{node("Cube", nil), node("Lamp", nil)}, nil)
	ours := minDoc([]any{node("Cube", nil), node("Lamp", nil)}, nil)
	theirs := minDoc([]any{node("Cube", nil)}, nil)

	h := New()
	merged, ci, err := h.Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci == nil {
		t.Fatal("expected conflict")
	}

	result, err := h.ApplyChoices(merged, theirs, []string{"nodes.Lamp"})
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := parseDoc(result)
	if len(doc.Nodes) != 1 {
		t.Errorf("expected 1 node after removing Lamp, got %d", len(doc.Nodes))
	}
}

// ── mesh merge tests ──────────────────────────────────────────────────────────

func minDocWithMeshes(meshes []any) []byte {
	doc := map[string]any{
		"asset":  map[string]any{"version": "2.0"},
		"meshes": meshes,
	}
	b, _ := json.Marshal(doc)
	return b
}

func mesh(name string, primCount int) map[string]any {
	prims := make([]any, primCount)
	for i := range prims {
		prims[i] = map[string]any{}
	}
	return map[string]any{"name": name, "primitives": prims}
}

func TestMergeMesh_TheirsAddsMesh(t *testing.T) {
	base := minDocWithMeshes([]any{mesh("Body", 1)})
	ours := minDocWithMeshes([]any{mesh("Body", 1)})
	theirs := minDocWithMeshes([]any{mesh("Body", 1), mesh("Wheel", 2)})

	_, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci != nil {
		t.Errorf("expected clean merge, got conflicts: %v", ci.Conflicts)
	}
	doc, _ := parseDoc(minDocWithMeshes([]any{mesh("Body", 1), mesh("Wheel", 2)}))
	result, _, _ := New().Merge(base, ours, theirs)
	merged, _ := parseDoc(result)
	if len(merged.Meshes) != len(doc.Meshes) {
		t.Errorf("expected %d meshes, got %d", len(doc.Meshes), len(merged.Meshes))
	}
}

func TestMergeMesh_BothChangeSameMesh_Conflict(t *testing.T) {
	base := minDocWithMeshes([]any{mesh("Body", 1)})
	ours := minDocWithMeshes([]any{mesh("Body", 2)}) // ours changed primitives
	theirs := minDocWithMeshes([]any{mesh("Body", 3)}) // theirs changed differently

	_, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci == nil || len(ci.Conflicts) == 0 {
		t.Fatal("expected conflict for divergent mesh changes")
	}
	if ci.Conflicts[0].Path != "meshes.Body" {
		t.Errorf("unexpected conflict path: %s", ci.Conflicts[0].Path)
	}
}

func TestMergeMesh_OnlyTheirsChangesMesh_Clean(t *testing.T) {
	base := minDocWithMeshes([]any{mesh("Body", 1)})
	ours := minDocWithMeshes([]any{mesh("Body", 1)})   // ours unchanged
	theirs := minDocWithMeshes([]any{mesh("Body", 3)}) // theirs changed

	result, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci != nil {
		t.Errorf("expected clean merge, got conflicts: %v", ci.Conflicts)
	}
	merged, _ := parseDoc(result)
	if len(merged.Meshes[0].Primitives) != 3 {
		t.Errorf("expected theirs' 3 primitives, got %d", len(merged.Meshes[0].Primitives))
	}
}

func TestMergeMesh_TheirsRemovesMesh_Conflict(t *testing.T) {
	base := minDocWithMeshes([]any{mesh("Body", 1), mesh("Wheel", 2)})
	ours := minDocWithMeshes([]any{mesh("Body", 1), mesh("Wheel", 2)}) // kept both
	theirs := minDocWithMeshes([]any{mesh("Body", 1)})                 // removed Wheel

	_, ci, err := New().Merge(base, ours, theirs)
	if err != nil {
		t.Fatal(err)
	}
	if ci == nil || len(ci.Conflicts) == 0 {
		t.Fatal("expected conflict for remove-vs-keep")
	}
	if ci.Conflicts[0].Path != "meshes.Wheel" {
		t.Errorf("unexpected conflict path: %s", ci.Conflicts[0].Path)
	}
}
