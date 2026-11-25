package locale_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/nicksnyder/go-i18n/v2/i18n"
	"golang.org/x/text/language"

	"github.com/go-keel/keel/middleware/locale"
	"github.com/go-keel/keel/middleware/metadata"
)

// localizeIn runs Localize behind the locale middleware for a request with this Accept-Language.
func localizeIn(t *testing.T, b *i18n.Bundle, lang string, msg *i18n.Message) string {
	t.Helper()
	ctx, err := metadata.Set(context.Background(), metadata.KeyAcceptLanguage, lang)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	_, _ = locale.Server(b)(func(ctx context.Context, _ any) (any, error) {
		got = locale.Localize(ctx, msg)
		return nil, nil
	})(ctx, nil)
	return got
}

func TestLocalize_Fallbacks(t *testing.T) {
	b := i18n.NewBundle(language.English)
	if err := b.AddMessages(language.MustParse("zh-CN"), &i18n.Message{ID: "Hello", Other: "你好"}); err != nil {
		t.Fatal(err)
	}
	msg := &i18n.Message{ID: "Hello", Other: "Hello"}
	if got := localizeIn(t, b, "zh-CN", msg); got != "你好" {
		t.Errorf("translated: got %q", got)
	}
	if got := localizeIn(t, b, "ja", msg); got != "Hello" {
		t.Errorf("missing translation: got %q, want the English text", got)
	}
	if got := locale.Localize(context.Background(), msg); got != "Hello" {
		t.Errorf("no localizer: got %q, want the English text", got)
	}
}

// A broken template does not panic: it is logged and the English text is sent.
func TestLocalize_BrokenTemplateLogged(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })

	msg := &i18n.Message{ID: "Broken", Other: "Hello {{.Name"}
	// "ja" is in the bundle without this message: the fallback to the broken default text must not come back empty.
	b := i18n.NewBundle(language.English)
	if err := b.AddMessages(language.Japanese, &i18n.Message{ID: "Unrelated", Other: "x"}); err != nil {
		t.Fatal(err)
	}
	for _, lang := range []string{"en", "ja"} {
		if got := localizeIn(t, b, lang, msg); got != msg.Other {
			t.Errorf("%s: got %q, want the raw English text", lang, got)
		}
	}
	if !strings.Contains(buf.String(), "level=ERROR") || !strings.Contains(buf.String(), "Broken") {
		t.Errorf("want an ERROR log naming the message, got %q", buf.String())
	}
}
