// Package json is a thin wrapper over the standard library's encoding/json, the one JSON codec Keel uses internally
// (event payloads, task payloads, HTTP client bodies), so a value encodes the same way on every path.
//
// It adds only one thing: empty input unmarshals to a nil error. See docs/utilities.md.
package json

import stdjson "encoding/json"

// Marshal serialises v to a JSON string, with map keys in order.
func Marshal(v any) (string, error) {
	output, err := MarshalToBytes(v)
	if err != nil {
		return "", err
	}
	return string(output), nil
}

// MarshalOrDefault is like Marshal but returns def on failure, for places like logging where the error is not worth handling.
func MarshalOrDefault(v any, def string) string {
	output, err := MarshalToBytes(v)
	if err != nil {
		return def
	}
	return string(output)
}

// MarshalToBytes is like Marshal but returns a byte slice.
func MarshalToBytes(v any) ([]byte, error) {
	return stdjson.Marshal(v)
}

// Unmarshal deserialises a JSON string into v; an empty string returns nil.
func Unmarshal(data string, v any) error {
	return UnmarshalBytes([]byte(data), v)
}

// UnmarshalBytes deserialises JSON bytes into v.
//
// Empty input returns nil rather than encoding/json's "unexpected end of JSON input": message bodies and cached values
// are commonly empty by default, and callers use this to tell "no content" from "broken content".
func UnmarshalBytes(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return stdjson.Unmarshal(data, v)
}
