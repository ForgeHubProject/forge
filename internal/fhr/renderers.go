package fhr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RendererMeta records a renderer bundle installed under ~/.forge/renderers.
type RendererMeta struct {
	ID     string `json:"id"`
	Build  string `json:"build"`
	Source string `json:"source"`
	Hash   string `json:"hash"`
}

// RenderersDir returns (and creates if needed) ~/.forge/renderers.
func RenderersDir() (string, error) {
	d, err := forgeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(d, "renderers")
	return p, os.MkdirAll(p, 0755)
}

func rendererFileName(handlerID string) string { return handlerID + ".js" }

// renderer3DFileName is the optional heavy "3D chunk" a renderer bundle may
// lazy-load (e.g. gltf-scene's three.js viewport). Stored next to the main
// bundle so `forge diff --web` can serve it as a sibling.
func renderer3DFileName(handlerID string) string { return handlerID + "-3d.js" }

// chunk3DURL derives the lazy 3D chunk URL from a renderer asset URL by the
// published naming convention: renderer-<h>.js → renderer-<h>-3d.js.
func chunk3DURL(rendererURL string) string {
	return strings.TrimSuffix(rendererURL, ".js") + "-3d.js"
}

// DownloadRenderer downloads a handler's renderer bundle from the manifest and
// installs it under ~/.forge/renderers. Returns the path to the installed bundle.
func DownloadRenderer(m *FHRManifest, handlerID, sourceURL string) (string, error) {
	renderersDir, err := RenderersDir()
	if err != nil {
		return "", err
	}
	assetURL, err := m.RendererAssetURL(handlerID)
	if err != nil {
		return "", err
	}

	bundlePath := filepath.Join(renderersDir, rendererFileName(handlerID))
	fmt.Printf("Downloading renderer for %s...\n", handlerID)
	hash, err := downloadFile(assetURL, bundlePath)
	if err != nil {
		return "", fmt.Errorf("downloading renderer for %s: %w", handlerID, err)
	}

	build := ""
	for _, entry := range m.Formats {
		if entry.Handler == handlerID {
			build = entry.Build
			break
		}
	}
	meta := RendererMeta{ID: handlerID, Build: build, Source: sourceURL, Hash: hash}
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(bundlePath+".json", data, 0644)
	}

	// Best-effort: some renderers lazy-load a heavy "3D chunk" sibling (e.g.
	// gltf-scene's three.js viewport). Fetch it if the release publishes one so
	// `forge diff --web` can serve it; a 404 just means this handler has none.
	chunkPath := filepath.Join(renderersDir, renderer3DFileName(handlerID))
	if _, err := downloadFile(chunk3DURL(assetURL), chunkPath); err != nil {
		_ = os.Remove(chunkPath) // clear any stale chunk from a prior release
	}

	return bundlePath, nil
}

// InstalledRenderer3D returns the path to an installed renderer's optional 3D
// chunk, or "" if the handler has none.
func InstalledRenderer3D(handlerID string) string {
	renderersDir, err := RenderersDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(renderersDir, renderer3DFileName(handlerID))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// InstalledRenderer returns the path to an installed renderer bundle, or "" if absent.
func InstalledRenderer(handlerID string) string {
	renderersDir, err := RenderersDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(renderersDir, rendererFileName(handlerID))
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// InstalledRendererBuild returns the build SHA recorded for an installed
// renderer bundle, or "" if not installed.
func InstalledRendererBuild(handlerID string) string {
	renderersDir, err := RenderersDir()
	if err != nil {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(renderersDir, rendererFileName(handlerID)+".json"))
	if err != nil {
		return ""
	}
	var meta RendererMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return ""
	}
	return meta.Build
}
