// Package locale puts a go-i18n localizer for the request's language into the context and translates with fallback.
// Bundles and localizers are go-i18n's own types; building the bundle and loading message files is the service's
// business. Keel itself sends no translated text.
package locale

import (
	"context"
	"errors"
	"log/slog"

	"github.com/go-kratos/kratos/v3/middleware"
	"github.com/nicksnyder/go-i18n/v2/i18n"

	"github.com/go-keel/keel/middleware/metadata"
)

type localeKey struct{}

// Server creates a middleware that injects a localizer for the request's Accept-Language into the context.
func Server(bundle *i18n.Bundle) middleware.Middleware {
	return func(handler middleware.Handler) middleware.Handler {
		return func(ctx context.Context, request any) (response any, err error) {
			localizer := i18n.NewLocalizer(bundle, metadata.GetAcceptLanguage(ctx))
			return handler(context.WithValue(ctx, localeKey{}, localizer), request)
		}
	}
}

// FromContext retrieves the localizer from the context; false means the chain has no locale middleware. Use it for
// what Localize does not cover (template data, plural forms).
func FromContext(ctx context.Context) (*i18n.Localizer, bool) {
	localizer, ok := ctx.Value(localeKey{}).(*i18n.Localizer)
	return localizer, ok
}

// Localize translates msg into the request's language. It never fails: without a translation in that language it
// returns the default language's text, then msg.Other; without a localizer in ctx it returns msg.Other. A text that
// cannot be rendered (a template that does not parse) is logged and msg.Other is returned. Write msg as a literal
// (&i18n.Message{ID: ..., Other: ...}) so goi18n extract finds it.
func Localize(ctx context.Context, msg *i18n.Message) string {
	localizer, ok := FromContext(ctx)
	if !ok {
		return msg.Other
	}
	s, err := localizer.LocalizeMessage(msg)
	if err == nil {
		return s
	}
	// A missing translation comes back as MessageNotFoundErr with the fallback text, which is the designed path. With
	// an empty text the fallback itself could not be rendered: go-i18n then reports only "not found".
	if _, notFound := errors.AsType[*i18n.MessageNotFoundErr](err); notFound && s != "" {
		return s
	}
	if msg.Other != "" {
		slog.ErrorContext(ctx, "locale: localize failed, using the default text", "id", msg.ID, "error", err)
	}
	return msg.Other
}
