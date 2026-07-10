package fhr

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
	return bundlePath, nil
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
