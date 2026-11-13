// Package authslot is the one path that fills a chain's auth slot. Package mw sets Fill when it is initialised and the
// api server constructors call it; it lives under internal so users cannot import it, which keeps the server's own
// auth config the only way to mount authentication (ADR keel-0002).
package authslot

import "github.com/go-kratos/kratos/v3/middleware"

// Fill puts m into the auth slot of chain, which must be a *mw.Chain, recording an error on the chain when it has no
// auth slot. Set by package mw; any program that links the api package also links mw.
var Fill func(chain any, m middleware.Middleware)
