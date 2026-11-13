// In-package: checks the gRPC full-method parser the whitelist uses internally.

package auth

import (
	"testing"
)

func TestParseFullMethodToOperation(t *testing.T) {
	op, svc, m := parseFullMethodToOperation("api.template.v1.Example.CreateExample")
	if op != "/api.template.v1.Example/CreateExample" {
		t.Errorf("operation = %q", op)
	}
	if svc != "api.template.v1.Example" || m != "CreateExample" {
		t.Errorf("service/method = %q/%q", svc, m)
	}
}

func TestParseFullMethodToOperation_NoDot(t *testing.T) {
	op, svc, m := parseFullMethodToOperation("nodot")
	if op != "" || svc != "" || m != "" {
		t.Errorf("malformed input should yield empties, got %q/%q/%q", op, svc, m)
	}
}

func TestOperationWhiteList_Contains(t *testing.T) {
	l := &OperationWhiteList{operations: map[string]struct{}{"/svc/Method": {}}}
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
