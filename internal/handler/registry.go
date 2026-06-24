package handler

import "fmt"

// Registry resolves a ForgeHandler for a given file path.
// Register most-specific handlers first; the first matching handler wins.
type Registry struct {
	entries []ForgeHandler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a handler. Entries registered first take priority.
func (r *Registry) Register(h ForgeHandler) {
	r.entries = append(r.entries, h)
}

// Resolve returns the first handler that matches path.
func (r *Registry) Resolve(path string) (ForgeHandler, error) {
	for _, h := range r.entries {
		if h.Match(path) {
			return h, nil
		}
	}
	return nil, fmt.Errorf("no handler for %q", path)
}
