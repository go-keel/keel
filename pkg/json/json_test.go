package json_test

import (
	"testing"

	"github.com/go-keel/keel/pkg/json"
)

// Locks in the one option Keel adds on top of v2: map keys are output in order. v2 writes keys in iteration order by
// default, so with enough keys a missing option almost certainly produces out-of-order output, exposed by comparing
// against a literal.

func TestMarshalSortsTopLevelMapKeys(t *testing.T) {
	m := map[string]int{"k": 1, "c": 2, "x": 3, "a": 4, "q": 5, "f": 6, "m": 7, "b": 8}
	got, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":4,"b":8,"c":2,"f":6,"k":1,"m":7,"q":5,"x":3}`
	if got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestMarshalSortsMapKeysInsideStruct(t *testing.T) {
	type payload struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	}
	p := payload{
		Name:   "n",
		Labels: map[string]string{"zone": "z", "app": "a", "tier": "t", "env": "e", "owner": "o", "region": "r"},
	}
	got, err := json.MarshalToBytes(p)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"name":"n","labels":{"app":"a","env":"e","owner":"o","region":"r","tier":"t","zone":"z"}}`
	if string(got) != want {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestUnmarshalEmptyInputReturnsNil(t *testing.T) {
	var v map[string]any
	if err := json.Unmarshal("", &v); err != nil {
		t.Fatalf("Unmarshal(\"\") = %v, want nil", err)
	}
	if err := json.UnmarshalBytes(nil, &v); err != nil {
		t.Fatalf("UnmarshalBytes(nil) = %v, want nil", err)
	}
}
