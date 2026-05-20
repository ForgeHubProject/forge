package handler

import "fmt"

// Registry resolves a ForgeHandler for a given file path.
// Handlers are checked in registration order — register most-specific first.
type Registry struct {
	handlers []ForgeHandler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{}
}

// Register adds a handler. Handlers registered first take priority.
func (r *Registry) Register(h ForgeHandler) {
	r.handlers = append(r.handlers, h)
}

// Resolve returns the first handler whose Match returns true for path.
// Reports a descriptive error if no handler matches.
func (r *Registry) Resolve(path string) (ForgeHandler, error) {
	for _, h := range r.handlers {
		if h.Match(path) {
			return h, nil
		}
	}
	return nil, fmt.Errorf("no handler available for %q — install one with: forge handler install", path)
}
