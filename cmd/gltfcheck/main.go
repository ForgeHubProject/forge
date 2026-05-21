//go:build ignore

package main

import (
	"encoding/json"
	"fmt"
	"github.com/yakupatahanov/forge/internal/handler/gltf"
)

func main() {
	base := map[string]any{
		"asset": map[string]any{"version": "2.0"},
		"nodes": []any{
			map[string]any{
				"name":        "Armature",
				"translation": []float64{0, 1.2, 0},
			},
		},
		"materials": []any{
			map[string]any{
				"name": "Skin",
				"pbrMetallicRoughness": map[string]any{
					"baseColorFactor": []float64{0.8, 0.6, 0.5, 1.0},
				},
			},
		},
	}
	head := map[string]any{
		"asset": map[string]any{"version": "2.0"},
		"nodes": []any{
			map[string]any{
				"name":        "Armature",
				"translation": []float64{0, 1.8, 0},
			},
		},
		"materials": []any{
			map[string]any{
				"name": "Skin",
				"pbrMetallicRoughness": map[string]any{
					"baseColorFactor": []float64{0.9, 0.7, 0.6, 1.0},
				},
			},
		},
	}

	encBase, _ := json.Marshal(base)
	encHead, _ := json.Marshal(head)

	h := gltf.New()
	diff, err := h.Diff(encBase, encHead)
	if err != nil {
		fmt.Println("ERROR:", err)
		return
	}

	fmt.Printf("Format: %s  Changes: %d\n", diff.Format, len(diff.Changes))
	for _, c := range diff.Changes {
		fmt.Printf("  section [%s] %q  children=%d\n", c.Kind, c.Label, len(c.Children))
		for _, child := range c.Children {
			fmt.Printf("    item [%s] %q  children=%d\n", child.Kind, child.Label, len(child.Children))
			for _, prop := range child.Children {
				fmt.Printf("      prop [%s] %q  %v → %v\n", prop.Kind, prop.Label, prop.Before, prop.After)
			}
		}
	}

	fmt.Println("\nMatch:", h.Match("char.glb"), h.Match("scene.gltf"), h.Match("tex.png"))

	d2, _ := h.Diff(encBase, encBase)
	if len(d2.Changes) == 0 {
		fmt.Println("Same-file diff: ✓ no changes")
	} else {
		fmt.Println("Same-file diff: FAIL", len(d2.Changes), "changes")
	}
}
