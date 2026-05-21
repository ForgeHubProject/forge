// Package domain provides BaseDomain and the official Forge domain
// implementations (ThreeDDomain, ImageDomain).
package domain

import "github.com/yakupatahanov/forge/internal/handler"

// BaseDomain is an embeddable struct that implements DomainRegister and
// DomainResolve. Concrete domains embed it and supply Match, Diff, Merge,
// and Format.
type BaseDomain struct {
	handlers []handler.ForgeHandler
}

// DomainRegister adds a specific handler to this domain.
// Handlers registered first take priority (most-specific first).
func (b *BaseDomain) DomainRegister(h handler.ForgeHandler) {
	b.handlers = append(b.handlers, h)
}

// DomainResolve returns the first handler whose Match returns true for path,
// or nil if no specific handler matches.
func (b *BaseDomain) DomainResolve(path string) handler.ForgeHandler {
	for _, h := range b.handlers {
		if h.Match(path) {
			return h
		}
	}
	return nil
}
