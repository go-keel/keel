// In-package: checks the eviction-policy error for each policy without a Redis server.

package kasynq

import (
	"strings"
	"testing"
)

func TestEvictionPolicyError(t *testing.T) {
	for _, tc := range []struct {
		policy string
		ok     bool
	}{
		{"noeviction", true},
		{"volatile-lru", true},
		{"volatile-ttl", true},
		{"allkeys-lru", false},
		{"allkeys-random", false},
	} {
		err := evictionPolicyError("# Memory\r\nused_memory:1\r\nmaxmemory_policy:" + tc.policy + "\r\n")
		if (err == nil) != tc.ok {
			t.Errorf("%s: err = %v, want ok=%v", tc.policy, err, tc.ok)
		}
		if err != nil && !strings.Contains(err.Error(), tc.policy) {
			t.Errorf("%s: error should name the policy: %v", tc.policy, err)
		}
	}
	if err := evictionPolicyError("# Memory\r\nused_memory:1\r\n"); err == nil {
		t.Error("missing field: want an error")
	}
}
