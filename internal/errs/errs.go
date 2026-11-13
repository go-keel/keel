// Package errs holds Keel's rule namespace: every developer-facing error Keel returns uses Rules.New or Rules.Wrap.
package errs

import (
	"errors"
	"fmt"

	"github.com/go-keel/keel/pkg/rule"
)

var Rules = rule.Namespace{Name: "keel", Doc: "docs/errors.md"}

// While passes err through when it carries a rule error, which already names the config and the fix; any other error
// (a key file that cannot be read, a library failing to build) gets "<doing>: " in front so it still says where it
// came from.
func While(doing string, err error) error {
	if _, ok := errors.AsType[*rule.Error](err); ok {
		return err
	}
	return fmt.Errorf("%s: %w", doing, err)
}
