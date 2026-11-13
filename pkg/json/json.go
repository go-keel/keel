// Package json is a thin wrapper over the standard library's encoding/json/v2.
//
// It adds only one thing to v2's default behaviour: maps are serialised with sorted keys (Deterministic). By default
// v2 writes keys in map iteration order, so the same value serialises differently every time: logs do not line up,
// cache or dedup keys computed from the output differ, and tests cannot assert against literals. If the framework
// does not settle this, every user has to remember to add the option.
//
// Everything else follows v2's default behaviour, without backfilling v1 semantics: backfilling any of them would make
// it a private dialect that is neither v1 nor v2, and users could not match it against the standard library docs.
// See docs/utilities.md.
package json

import jsonv2 "encoding/json/v2"

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
	return jsonv2.Marshal(v, jsonv2.Deterministic(true))
}

// Unmarshal deserialises a JSON string into v; an empty string returns nil.
func Unmarshal(data string, v any) error {
	return UnmarshalBytes([]byte(data), v)
}

// UnmarshalBytes deserialises JSON bytes into v.
//
// Empty input returns nil rather than v2's unexpected EOF: message bodies and cached values are commonly empty by
// default, and callers use this to tell "no content" from "broken content".
func UnmarshalBytes(data []byte, v any) error {
	if len(data) == 0 {
		return nil
	}
	return jsonv2.Unmarshal(data, v)
}
