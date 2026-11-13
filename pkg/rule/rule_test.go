package rule_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-keel/keel/pkg/rule"
)

var ns = rule.Namespace{Name: "demo", Doc: "docs/errors.md"}

func TestErrorFormatAndUnwrap(t *testing.T) {
	sentinel := errors.New("sentinel")
	err := ns.Wrap(sentinel, "cfg.missing", "config section x not found", "add x: {} to the config")
	want := "demo/cfg.missing: config section x not found. add x: {} to the config. See docs/errors.md#cfg.missing"
	if err.Error() != want {
		t.Errorf("Error() = %q, want %q", err.Error(), want)
	}
	if !errors.Is(err, sentinel) {
		t.Error("Wrap must keep the wrapped error matchable with errors.Is")
	}
}

func write(t *testing.T, root, path, body string) {
	t.Helper()
	p := filepath.Join(root, path)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCheckDocs(t *testing.T) {
	root := t.TempDir()
	write(t, root, "a/a.go", `package a
var x = Rules.New("cfg.missing", "p", "f")
var y = errs.Rules.Wrap(err, "db.driver-missing", "p", "f")
var z = Rules.New(id, "p", "f")
var w = Rules.New("BadID", "p", "f")
`)
	write(t, root, "a/a_test.go", `package a
var t = Rules.New("test.only", "p", "f")
`)
	write(t, root, "docs/errors.md", "# Errors\n\n## cfg.missing\n\n## stale.entry\n")

	err := rule.CheckDocs(root, ns)
	if err == nil {
		t.Fatal("want problems")
	}
	msg := err.Error()
	for _, want := range []string{
		`"db.driver-missing" is used`, // used, not documented
		`documents "stale.entry"`,     // documented, not used
		"must be a string literal",    // non-literal ID
		`"BadID" is not`,              // bad form
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "test.only") || strings.Contains(msg, `"cfg.missing"`) {
		t.Errorf("test files are ignored and documented IDs pass:\n%s", msg)
	}

	clean := t.TempDir()
	write(t, clean, "a.go", "package a\nvar x = Rules.New(\"cfg.missing\", \"p\", \"f\")\n")
	write(t, clean, "docs/errors.md", "## cfg.missing\n")
	if err := rule.CheckDocs(clean, ns); err != nil {
		t.Errorf("clean tree: %v", err)
	}
}
