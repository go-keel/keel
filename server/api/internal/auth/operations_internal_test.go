// In-package: checks the gRPC full-method parser the whitelist uses internally.

package auth

import (
	"testing"
)

func TestParseFullMethodToOperation(t *testing.T) {
	if op := parseFullMethodToOperation("api.template.v1.Example.CreateExample"); op != "/api.template.v1.Example/CreateExample" {
		t.Errorf("operation = %q", op)
	}
}

func TestParseFullMethodToOperation_NoDot(t *testing.T) {
	if op := parseFullMethodToOperation("nodot"); op != "" {
		t.Errorf("malformed input should yield empty, got %q", op)
	}
}

func TestOperationWhiteList_Contains(t *testing.T) {
	l := &operationWhiteList{operations: map[string]struct{}{"/svc/Method": {}}}
	if !l.Contains("/svc/Method") {
		t.Error("should contain registered operation")
	}
	if l.Contains("/svc/Other") {
		t.Error("should not contain unregistered")
	}
	if l.Contains("") {
		t.Error("empty operation must never match (non-RPC routes default to auth required)")
	}
}
