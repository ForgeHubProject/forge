package fhr

import (
	"testing"

	"github.com/BurntSushi/toml"
)

const manifestWithRenderer = `
name = "fhr-official"
url  = "https://github.com/forgehubproject/fhr"

[formats]
".gltf" = { handler = "gltf-scene", build = "be1face" }
".glb"  = { handler = "gltf-scene", build = "be1face" }

[assets.handlers."gltf-scene"]
"linux-amd64" = "https://example.com/gltf/linux-amd64"
"wasm"        = "https://example.com/gltf/handler.wasm"

[assets.renderers]
"gltf-scene" = "https://example.com/gltf/renderer.js"
`

func parseManifest(t *testing.T, s string) *FHRManifest {
	t.Helper()
	var m FHRManifest
	if _, err := toml.Decode(s, &m); err != nil {
		t.Fatalf("decoding manifest: %v", err)
	}
	return &m
}

func TestRendererAssetURL_Absolute(t *testing.T) {
	m := parseManifest(t, manifestWithRenderer)
	url, err := m.RendererAssetURL("gltf-scene")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://example.com/gltf/renderer.js" {
		t.Fatalf("unexpected renderer URL: %s", url)
	}
}

func TestRendererAssetURL_Missing(t *testing.T) {
	m := parseManifest(t, manifestWithRenderer)
	if _, err := m.RendererAssetURL("step-cad"); err == nil {
		t.Fatal("expected error for handler with no renderer")
	}
}

func TestRendererAssetURL_Relative(t *testing.T) {
	m := parseManifest(t, `
url = "https://cdn.example.com/base"
[assets.renderers]
"gltf-scene" = "renderers/gltf-scene.js"
`)
	url, err := m.RendererAssetURL("gltf-scene")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://cdn.example.com/base/renderers/gltf-scene.js" {
		t.Fatalf("relative path not resolved against manifest url: %s", url)
	}
}

func TestWasmHandlerAssetURL(t *testing.T) {
	m := parseManifest(t, manifestWithRenderer)
	url, err := m.HandlerAssetURL("gltf-scene", "wasm")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://example.com/gltf/handler.wasm" {
		t.Fatalf("unexpected wasm URL: %s", url)
	}
}
