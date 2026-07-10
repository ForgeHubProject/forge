package fhr

import (
	"crypto/sha256"
	"encoding/hex"
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
	Description string                 `toml:"description"`
	Maintainer  string                 `toml:"maintainer"`
	Formats     map[string]FormatEntry `toml:"formats"`
	Assets      AssetsSection          `toml:"assets"`
}

// FormatEntry maps a file extension to its handler and the build SHA at last publish.
type FormatEntry struct {
	Handler string `toml:"handler"`
	Build   string `toml:"build"`
}

// AssetsSection holds download URLs keyed by handler → platform → URL, plus
// renderer bundle URLs keyed by handler → URL (renderers are not platform-keyed;
// one self-contained ESM bundle serves every consumer).
type AssetsSection struct {
	Handlers  map[string]map[string]string `toml:"handlers"`
	Renderers map[string]string            `toml:"renderers"`
}

// FetchManifest downloads and parses manifest.toml from the given URL.
func FetchManifest(url string) (*FHRManifest, error) {
	_, m, err := FetchManifestWithHash(url)
	return m, err
}

// FetchManifestWithHash downloads manifest.toml, returns its SHA-256 content hash
// (hex-encoded) and the parsed manifest. The hash is used by .forge-handlers to
// detect whether the upstream manifest has changed since last update.
func FetchManifestWithHash(url string) (hash string, m *FHRManifest, err error) {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return "", nil, fmt.Errorf("fetching manifest from %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", nil, fmt.Errorf("fetching manifest from %s: HTTP %d", url, resp.StatusCode)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, fmt.Errorf("reading manifest body: %w", err)
	}
	sum := sha256.Sum256(data)
	hash = hex.EncodeToString(sum[:])
	var manifest FHRManifest
	if _, err := toml.Decode(string(data), &manifest); err != nil {
		return "", nil, fmt.Errorf("parsing manifest: %w", err)
	}
	return hash, &manifest, nil
}

// HandlerForExt returns the handler ID and build SHA for a file extension.
func (m *FHRManifest) HandlerForExt(ext string) (id, build string, err error) {
	fe, ok := m.Formats[ext]
	if !ok {
		return "", "", fmt.Errorf("no handler registered for extension %q in this source", ext)
	}
	return fe.Handler, fe.Build, nil
}

// HandlerAssetURL returns the full download URL for a handler binary.
// platformKey is "linux-amd64", "darwin-arm64", etc.
// Relative asset paths are resolved against the manifest's url field.
func (m *FHRManifest) HandlerAssetURL(handlerID, platformKey string) (string, error) {
	platforms, ok := m.Assets.Handlers[handlerID]
	if !ok {
		return "", fmt.Errorf("handler %q not found in manifest assets", handlerID)
	}
	assetPath, ok := platforms[platformKey]
	if !ok {
		return "", fmt.Errorf("handler %q: no asset for platform %q", handlerID, platformKey)
	}
	if strings.HasPrefix(assetPath, "http://") || strings.HasPrefix(assetPath, "https://") {
		return assetPath, nil
	}
	base := strings.TrimSuffix(m.URL, "/")
	return base + "/" + assetPath, nil
}

// RendererAssetURL returns the full download URL for a handler's renderer bundle.
// Relative asset paths are resolved against the manifest's url field.
func (m *FHRManifest) RendererAssetURL(handlerID string) (string, error) {
	assetPath, ok := m.Assets.Renderers[handlerID]
	if !ok {
		return "", fmt.Errorf("handler %q has no renderer in this source", handlerID)
	}
	if strings.HasPrefix(assetPath, "http://") || strings.HasPrefix(assetPath, "https://") {
		return assetPath, nil
	}
	base := strings.TrimSuffix(m.URL, "/")
	return base + "/" + assetPath, nil
}
