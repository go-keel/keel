package errs_test

import (
	"testing"

	"github.com/go-keel/keel/internal/errs"
	"github.com/go-keel/keel/pkg/rule"
)

func TestRulesDocumented(t *testing.T) {
	if err := rule.CheckDocs("../..", errs.Rules); err != nil {
		t.Fatal(err)
	}
}
