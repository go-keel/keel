// Package appid implements the "config value → hostname → name-pid" instance ID fallback.
//
// Only two places need it: the root package keel (kratos.ID in AppFactory.New) and core/ksnowflake
// (the redis key owner in NewAuto). It lives in the repo's top-level internal/ rather than under a
// specific component package because the root package cannot reach an internal under the core/ subtree; only the
// top-level internal, visible to the whole repo tree, covers both places.
// It is not public: it is not a framework concept, just the shared implementation of two fallbacks, and has no
// reason to be an API users need to know.
package appid

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"

	"github.com/go-keel/keel/conf"
)

// hostnameFunc / pidFunc can be replaced in tests to trigger the "hostname unavailable" fallback, which almost
// never happens in a real environment. InstanceID itself asserts nothing about upstream behavior; this only makes
// the branch testable.
var (
	hostnameFunc = os.Hostname
	pidFunc      = os.Getpid
)

// InstanceID implements the "config value → hostname → name-pid" instance ID fallback. kratos.ID in app.go (the
// application instance ID) and the redis key owner of ksnowflake.NewAuto (the ownership marker for node
// allocation) share this one implementation, so the application instance ID and the owner holding a node number
// always match, and the two can be treated as the same instance when debugging.
//
// The hostname check deliberately takes the stricter side: the hostname is used only if hostnameFunc returns no
// error and a non-empty value; on error, even a non-empty string is not trusted and it falls back to name-pid,
// since a return value should not be trusted when an error is explicitly reported. The impact is negligible: the
// standard library's os.Hostname() always returns an empty string on error on Linux/macOS, so this case does not
// actually happen on supported platforms.
func InstanceID(app *conf.App) string {
	if id := app.GetId(); id != "" {
		return id
	}
	if host, err := hostnameFunc(); err == nil && host != "" {
		return host
	}
	return fmt.Sprintf("%s-%d", app.GetName(), pidFunc())
}

// LeaseOwner is the value a process writes into a Redis lease it holds: the instance ID plus a token drawn once per
// call. The instance ID alone is not unique: replicas on one host share the hostname, and a holder that paused past
// the lease TTL would then renew a lease another replica had taken in the meantime, so both would act as holder.
func LeaseOwner(app *conf.App) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b) // crypto/rand.Read never returns an error on supported platforms
	return InstanceID(app) + "/" + hex.EncodeToString(b)
}
