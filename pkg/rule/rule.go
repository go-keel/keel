// Package rule builds developer-facing errors: misuse found at startup, during assembly or at registration. Each
// error carries a rule ID that has a section in the owning repository's error page, so an agent can grep the ID in
// the source and the docs, and read why it happened and how to fix it.
//
// Runtime failures (a database that is down, a timeout) and errors returned to end users are not rule errors.
package rule

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// Namespace is one repository's set of rule IDs: its name prefixes every message and Doc is the error page, relative
// to the repository root, that has one "## <id>" section per ID. A repository declares a single package-level
// variable named Rules of this type; CheckDocs finds the IDs by that name.
type Namespace struct {
	Name string
	Doc  string
}

// New returns a rule error. problem names what is wrong (the config key, file, registration or call involved); fix is
// one directly actionable sentence. Both are single sentences without a trailing period. id must be a string literal
// with a section in the error page; CheckDocs enforces both.
func (n Namespace) New(id, problem, fix string) *Error {
	return &Error{ns: n, ID: id, Problem: problem, Fix: fix}
}

// Wrap is New for an error that callers match with errors.Is or errors.As: err stays in the chain.
func (n Namespace) Wrap(err error, id, problem, fix string) *Error {
	return &Error{ns: n, ID: id, Problem: problem, Fix: fix, err: err}
}

// Error is a developer-facing error with a rule ID.
type Error struct {
	ns      Namespace
	ID      string
	Problem string
	Fix     string
	err     error
}

// Error formats as "<namespace>/<id>: <problem>. <fix>. See <doc>#<id>".
func (e *Error) Error() string {
	return fmt.Sprintf("%s/%s: %s. %s. See %s#%s", e.ns.Name, e.ID, e.Problem, e.Fix, e.ns.Doc, e.ID)
}

// Unwrap returns the error passed to Wrap, if any.
func (e *Error) Unwrap() error { return e.err }

var idPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*\.[a-z0-9]+(-[a-z0-9]+)*$`)

// CheckDocs compares the rule IDs used in the Go source under root (Rules.New / Rules.Wrap calls, test files and
// vendored or generated trees excluded) with the "## <id>" headings of ns.Doc. It reports IDs used but not documented,
// documented but not used, not string literals, or not of the form <component>.<what-went-wrong>.
func CheckDocs(root string, ns Namespace) error {
	used := map[string][]string{}
	var problems []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", "vendor", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || !isRules(sel.X) {
				return true
			}
			arg := -1
			switch sel.Sel.Name {
			case "New":
				arg = 0
			case "Wrap":
				arg = 1
			}
			if arg < 0 || len(call.Args) <= arg {
				return true
			}
			pos := fset.Position(call.Pos()).String()
			lit, ok := call.Args[arg].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				problems = append(problems, pos+": rule ID must be a string literal so it can be checked against the docs")
				return true
			}
			id, _ := strconv.Unquote(lit.Value)
			if !idPattern.MatchString(id) {
				problems = append(problems, fmt.Sprintf("%s: rule ID %q is not <component>.<what-went-wrong> (lowercase, dashes)", pos, id))
			}
			used[id] = append(used[id], pos)
			return true
		})
		return nil
	})
	if err != nil {
		return err
	}

	doc, err := os.ReadFile(filepath.Join(root, ns.Doc))
	if err != nil {
		return fmt.Errorf("rule: read %s: %w", ns.Doc, err)
	}
	documented := map[string]bool{}
	for line := range strings.SplitSeq(string(doc), "\n") {
		if id, ok := strings.CutPrefix(strings.TrimSpace(line), "## "); ok && idPattern.MatchString(id) {
			documented[id] = true
		}
	}
	for id, at := range used {
		if !documented[id] {
			problems = append(problems, fmt.Sprintf("rule ID %q is used at %s but has no \"## %s\" section in %s", id, at[0], id, ns.Doc))
		}
	}
	for id := range documented {
		if _, ok := used[id]; !ok {
			problems = append(problems, fmt.Sprintf("%s documents %q but no code uses it; delete the section or the ID was renamed", ns.Doc, id))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	slices.Sort(problems)
	return errors.New(strings.Join(problems, "\n"))
}

// isRules matches Rules and pkg.Rules.
func isRules(x ast.Expr) bool {
	switch v := x.(type) {
	case *ast.Ident:
		return v.Name == "Rules"
	case *ast.SelectorExpr:
		return v.Sel.Name == "Rules"
	}
	return false
}
