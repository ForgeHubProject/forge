package fhr

import (
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/BurntSushi/toml"
)

// FHRManifest is a parsed manifest.toml from an FHR registry.
type FHRManifest struct {
	Name        string                 `toml:"name"`
	URL         string                 `toml:"url"`
	Version     string                 `toml:"version"`
	Description string                 `toml:"description"`
	Maintainer  string                 `toml:"maintainer"`
	Formats     map[string]FormatEntry `toml:"formats"`
	Assets      AssetsSection          `toml:"assets"`
}

// FormatEntry maps a file extension to its handler name and version.
type FormatEntry struct {
	Handler string `toml:"handler"`
	Version string `toml:"version"`
}

// AssetsSection holds download URLs keyed by handler → version → platform/kind.
type AssetsSection struct {
	Handlers  map[string]VersionMap `toml:"handlers"`
	Renderers map[string]VersionMap `toml:"renderers"`
}

// VersionMap is version → (platform → URL).
type VersionMap map[string]map[string]string

// FetchManifest downloads and parses manifest.toml from the given URL.
func FetchManifest(url string) (*FHRManifest, error) {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return nil, fmt.Errorf("fetching manifest from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching manifest from %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading manifest body: %w", err)
	}
	var m FHRManifest
	if _, err := toml.Decode(string(data), &m); err != nil {
		return nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return &m, nil
}

// HandlerForExt returns the handler ID and version for a file extension.
func (m *FHRManifest) HandlerForExt(ext string) (id, version string, err error) {
	fe, ok := m.Formats[ext]
	if !ok {
		return "", "", fmt.Errorf("no handler registered for extension %q in this source", ext)
	}
	return fe.Handler, fe.Version, nil
}

// HandlerAssetURL returns the full download URL for a handler binary.
// platformKey is "linux-amd64", "darwin-arm64", etc.
// Relative asset paths are resolved against the manifest's url field.
func (m *FHRManifest) HandlerAssetURL(handlerID, version, platformKey string) (string, error) {
	versions, ok := m.Assets.Handlers[handlerID]
	if !ok {
		return "", fmt.Errorf("handler %q not found in manifest assets", handlerID)
	}
	platforms, ok := versions[version]
	if !ok {
		return "", fmt.Errorf("handler %q version %s not found in manifest assets", handlerID, version)
	}
	assetPath, ok := platforms[platformKey]
	if !ok {
		return "", fmt.Errorf("handler %q v%s: no asset for platform %q", handlerID, version, platformKey)
	}
	if strings.HasPrefix(assetPath, "http://") || strings.HasPrefix(assetPath, "https://") {
		return assetPath, nil
	}
	// Relative path: resolve against the manifest's declared url field.
	base := strings.TrimSuffix(m.URL, "/")
	return base + "/" + assetPath, nil
}
