package locale

import (
	"context"

	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/nicksnyder/go-i18n/v2/i18n"

	"github.com/go-keel/keel/middleware/metadata"
)

type Bundle struct {
	*i18n.Bundle
}

func NewBundle(bundle *i18n.Bundle) *Bundle {
	return &Bundle{Bundle: bundle}
}

type localeKey struct{}

// Server creates a middleware that injects a localizer into the context.
func Server(bundle *Bundle) middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, request any) (response any, err error) {

			// Read the Accept-Language metadata from the context
			lang := metadata.GetAcceptLanguage(ctx)
			localizer := i18n.NewLocalizer(bundle.Bundle, lang)

			// Inject the localizer into the context
			ctx = context.WithValue(ctx, localeKey{}, localizer)
			return handler(ctx, request)
		}
	}
}

// FromContext retrieves the localizer from the context.
func FromContext(ctx context.Context) (*i18n.Localizer, bool) {
	if localizer, ok := ctx.Value(localeKey{}).(*i18n.Localizer); ok {
		return localizer, true
	}
	return nil, false
}
