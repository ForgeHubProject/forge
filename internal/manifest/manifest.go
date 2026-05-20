package manifest

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

// Handlers represents the .forge/handlers manifest committed in a repo root.
type Handlers struct {
	Require map[string]HandlerRequirement `toml:"require"`
}

// HandlerRequirement declares a community handler needed for a glob pattern.
type HandlerRequirement struct {
	Registry string `toml:"registry"`
	Handler  string `toml:"handler"`
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
