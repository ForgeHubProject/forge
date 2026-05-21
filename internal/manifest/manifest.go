// Package manifest reads and writes the .forge/handlers file committed in a
// repo root. The manifest declares which domains the repository requires so
// that forge clone can report missing ones immediately after cloning.
package manifest

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Handlers represents the .forge/handlers manifest.
type Handlers struct {
	// Domains lists official Forge domains required by the repo.
	// Official domains (3d, image, text, audio, video) ship with Forge.
	Domains DomainConfig `toml:"domains"`

	// Community lists community domains that require an external registry.
	// Keys are domain names; values specify where to install them from.
	Community map[string]CommunitySource `toml:"community"`
}

// DomainConfig is the [domains] section.
type DomainConfig struct {
	Require []string `toml:"require"`
}

// CommunitySource points to an external registry for a community domain.
type CommunitySource struct {
	Registry string `toml:"registry"`
	Version  string `toml:"version"`
}

// LoadHandlers reads and parses .forge/handlers from repoRoot.
// Returns an empty manifest (not an error) if the file does not exist.
func LoadHandlers(repoRoot string) (Handlers, error) {
	path := filepath.Join(repoRoot, ".forge", "handlers")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Handlers{}, nil
	}
	if err != nil {
		return Handlers{}, err
	}

	var m Handlers
	if err := toml.Unmarshal(data, &m); err != nil {
		return Handlers{}, err
	}
	return m, nil
}
