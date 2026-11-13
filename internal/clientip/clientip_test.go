package clientip_test

import (
	"testing"

	"github.com/go-keel/keel/conf"
	"github.com/go-keel/keel/internal/clientip"
)

func mustNew(t *testing.T, cfg *conf.Server_ClientIP) *clientip.Resolver {
	t.Helper()
	r, err := clientip.New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return r
}

// Anchors of the address rule: untrusted peer reads no header, right to left, invalid entry falls back to the peer, fully trusted list takes the leftmost.
func TestResolve(t *testing.T) {
	proxies := mustNew(t, &conf.Server_ClientIP{TrustedProxies: []string{"10.0.0.0/8", "127.0.0.1"}})
	ignore := mustNew(t, &conf.Server_ClientIP{IgnoreForwardedFor: true})
	for _, tt := range []struct {
		name string
		r    *clientip.Resolver
		peer string
		xff  []string
		want string
	}{
		{"peer not in list, forged XFF not read", clientip.Default, "8.8.8.8:1234", []string{"1.1.1.1"}, "8.8.8.8"},
		{"default trusts local IPv4", clientip.Default, "127.0.0.1:1234", []string{"1.1.1.1"}, "1.1.1.1"},
		{"default trusts the whole local range", clientip.Default, "127.0.0.2:1234", []string{"1.1.1.1"}, "1.1.1.1"},
		{"default trusts local IPv6", clientip.Default, "[::1]:1234", []string{"1.1.1.1"}, "1.1.1.1"},
		{"no XFF uses peer", clientip.Default, "127.0.0.1:1234", nil, "127.0.0.1"},
		{"client entry appended on the left, take rightmost untrusted entry", clientip.Default, "127.0.0.1:1", []string{"6.6.6.6, 1.1.1.1"}, "1.1.1.1"},
		{"right to left skips several listed hops", proxies, "10.0.0.1:1", []string{"6.6.6.6, 1.1.1.1, 10.0.0.9, 127.0.0.1"}, "1.1.1.1"},
		{"multiple lines joined, first line is forged", clientip.Default, "127.0.0.1:1", []string{"6.6.6.6", "1.1.1.1"}, "1.1.1.1"},
		{"rightmost untrusted entry is unknown, fall back to peer", clientip.Default, "127.0.0.1:1", []string{"6.6.6.6, unknown"}, "127.0.0.1"},
		{"rightmost untrusted entry is empty, fall back to peer", proxies, "10.0.0.1:1", []string{"6.6.6.6, , 10.0.0.2"}, "10.0.0.1"},
		{"rightmost untrusted entry has a zone, fall back to peer", clientip.Default, "127.0.0.1:1", []string{"fe80::1%eth0"}, "127.0.0.1"},
		{"whole list trusted, take leftmost", proxies, "10.0.0.1:1", []string{"10.0.0.3, 10.0.0.2"}, "10.0.0.3"},
		{"IPv4 with port", clientip.Default, "127.0.0.1:1", []string{"1.2.3.4:5678"}, "1.2.3.4"},
		{"bare IPv6", clientip.Default, "127.0.0.1:1", []string{"2001:db8::1"}, "2001:db8::1"},
		{"bracketed IPv6", clientip.Default, "127.0.0.1:1", []string{"[2001:db8::1]"}, "2001:db8::1"},
		{"bracketed IPv6 with port", clientip.Default, "127.0.0.1:1", []string{"[2001:db8::1]:443"}, "2001:db8::1"},
		{"IPv4-mapped IPv6 restored", clientip.Default, "127.0.0.1:1", []string{"::ffff:1.2.3.4"}, "1.2.3.4"},
		{"IPv4-mapped peer is trusted as local", clientip.Default, "[::ffff:127.0.0.1]:1", []string{"1.1.1.1"}, "1.1.1.1"},
		{"non-local peer returned normalised", clientip.Default, "[::ffff:8.8.8.8]:1", nil, "8.8.8.8"},
		{"IPv6 normalised form", clientip.Default, "[2001:DB8:0::1]:1", nil, "2001:db8::1"},
		{"peer with zone treated as unavailable", clientip.Default, "[fe80::1%eth0]:1", nil, ""},
		{"peer cannot be parsed (unix socket)", clientip.Default, "@", nil, ""},
		{"ignore_forwarded_for distrusts even local host", ignore, "127.0.0.1:1", []string{"1.1.1.1"}, "127.0.0.1"},
		{"written list replaces default, local host no longer trusted", mustNew(t, &conf.Server_ClientIP{TrustedProxies: []string{"10.0.0.0/8"}}), "127.0.0.1:1", []string{"1.1.1.1"}, "127.0.0.1"},
		{"list in IPv4-mapped form still matches", mustNew(t, &conf.Server_ClientIP{TrustedProxies: []string{"::ffff:10.0.0.0/104"}}), "10.1.2.3:1", []string{"1.1.1.1"}, "1.1.1.1"},
	} {
		if got := tt.r.Resolve(tt.peer, tt.xff); got != tt.want {
			t.Errorf("%s: got %q, want %q", tt.name, got, tt.want)
		}
	}
}

// Omitting the section, or writing only an empty list, both mean the default (trust only the local host).
func TestNew_DefaultsToLoopback(t *testing.T) {
	for _, cfg := range []*conf.Server_ClientIP{nil, {}} {
		if r := mustNew(t, cfg); r != clientip.Default {
			t.Errorf("cfg %v: want Default", cfg)
		}
	}
}

// When the config is built directly, bypassing kconfig, an invalid entry still errors instead of being silently dropped.
func TestNew_RejectsInvalid(t *testing.T) {
	for _, s := range []string{"10.0.0.1/8", "not-an-ip", "fe80::1%eth0"} {
		if _, err := clientip.New(&conf.Server_ClientIP{TrustedProxies: []string{s}}); err == nil {
			t.Errorf("%q: want error", s)
		}
	}
}
