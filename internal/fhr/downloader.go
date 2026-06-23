package fhr

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
)

// InstalledMeta records a handler installed under ~/.forge/plugins.
type InstalledMeta struct {
	ID      string   `json:"id"`
	Version string   `json:"version"`
	Source  string   `json:"source"`
	Formats []string `json:"formats"`
}

// PluginsDir returns (and creates if needed) ~/.forge/plugins.
func PluginsDir() (string, error) {
	d, err := forgeDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(d, "plugins")
	return p, os.MkdirAll(p, 0755)
}

// PlatformKey returns the OS/arch key used in manifest asset tables.
func PlatformKey() string {
	goos, goarch := runtime.GOOS, runtime.GOARCH
	switch {
	case goos == "darwin" && goarch == "arm64":
		return "darwin-arm64"
	case goos == "darwin":
		return "darwin-amd64"
	case goos == "linux":
		return "linux-amd64"
	case goos == "windows":
		return "windows-amd64"
	default:
		return goos + "-" + goarch
	}
}

// DownloadHandler downloads a handler binary from the manifest and installs it
// under ~/.forge/plugins. Returns the path to the installed binary.
func DownloadHandler(m *FHRManifest, handlerID, version, sourceURL string) (string, error) {
	pluginsDir, err := PluginsDir()
	if err != nil {
		return "", err
	}

	assetURL, err := m.HandlerAssetURL(handlerID, version, PlatformKey())
	if err != nil {
		return "", err
	}

	binaryName := "forge-handler-" + handlerID
	if runtime.GOOS == "windows" {
		binaryName += ".exe"
	}
	binaryPath := filepath.Join(pluginsDir, binaryName)

	fmt.Printf("Downloading %s...\n", binaryName)
	if err := downloadFile(assetURL, binaryPath); err != nil {
		return "", fmt.Errorf("downloading %s: %w", binaryName, err)
	}
	if err := os.Chmod(binaryPath, 0755); err != nil {
		return "", fmt.Errorf("setting executable bit on %s: %w", binaryName, err)
	}

	meta := InstalledMeta{
		ID:      handlerID,
		Version: version,
		Source:  sourceURL,
		Formats: formatsForHandler(m, handlerID, version),
	}
	if data, err := json.MarshalIndent(meta, "", "  "); err == nil {
		_ = os.WriteFile(binaryPath+".json", data, 0644)
	}

	return binaryPath, nil
}

// LoadInstalledHandlers returns metadata for all handlers in ~/.forge/plugins.
func LoadInstalledHandlers() []InstalledMeta {
	pluginsDir, err := PluginsDir()
	if err != nil {
		return nil
	}
	entries, err := os.ReadDir(pluginsDir)
	if err != nil {
		return nil
	}
	var handlers []InstalledMeta
	for _, e := range entries {
		if filepath.Ext(e.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(pluginsDir, e.Name()))
		if err != nil {
			continue
		}
		var meta InstalledMeta
		if err := json.Unmarshal(data, &meta); err != nil {
			continue
		}
		handlers = append(handlers, meta)
	}
	return handlers
}

// InstalledHandlerBinary returns the path to an installed handler binary, or "" if absent.
func InstalledHandlerBinary(handlerID string) string {
	pluginsDir, err := PluginsDir()
	if err != nil {
		return ""
	}
	name := "forge-handler-" + handlerID
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	p := filepath.Join(pluginsDir, name)
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

func formatsForHandler(m *FHRManifest, handlerID, version string) []string {
	var fmts []string
	for ext, fe := range m.Formats {
		if fe.Handler == handlerID && fe.Version == version {
			fmts = append(fmts, ext)
		}
	}
	return fmts
}

func downloadFile(url, dest string) error {
	resp, err := http.Get(url) //nolint:noctx
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(f, resp.Body)
	return err
}
