package handler

import (
	"fmt"
	"path/filepath"
)

// Registry resolves a ForgeHandler for a given file path.
// Entries may be standalone ForgeHandlers or Domains.
// Domains are checked first; within a domain, specific handlers take priority
// over the domain-level fallback.
// Register most-specific entries first.
type Registry struct {
	entries []ForgeHandler
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{} }

// Register adds a handler or domain. Entries registered first take priority.
func (r *Registry) Register(h ForgeHandler) {
	r.entries = append(r.entries, h)
}

// Resolve returns the most specific handler for path.
// If a Domain matches, it delegates to the domain's own sub-resolution.
func (r *Registry) Resolve(path string) (ForgeHandler, error) {
	_, h, err := r.ResolveFull(path)
	return h, err
}

// Resolution holds the result of a full registry lookup.
type Resolution struct {
	Domain   Domain       // the matched domain, or nil for standalone handlers
	Handler  ForgeHandler // the most specific handler (may equal Domain if no specific match)
	Specific bool         // true if Handler is a dedicated format handler, not a domain fallback
}

// ResolveFull returns the matched domain (if any) alongside the specific
// handler, letting callers build rich labels like "[3d › gltf]".
func (r *Registry) ResolveFull(path string) (Domain, ForgeHandler, error) {
	for _, entry := range r.entries {
		if d, ok := entry.(Domain); ok {
			if !d.Match(path) {
				continue
			}
			if specific := d.DomainResolve(path); specific != nil {
				return d, specific, nil
			}
			return d, d, nil // domain is its own fallback
		}
		if entry.Match(path) {
			return nil, entry, nil
		}
	}
	return nil, nil, fmt.Errorf("no handler for %q — install one with: forge formats add %s", path, filepath.Ext(path))
}

// Domains returns all Domain entries in the registry.
func (r *Registry) Domains() []Domain {
	var domains []Domain
	for _, e := range r.entries {
		if d, ok := e.(Domain); ok {
			domains = append(domains, d)
		}
	}
	return domains
}
