# Upgrading

Read this on every upgrade of `github.com/go-keel/keel`. It lists only the changes that need action from users: a
changed public API, a changed default, a config or calling-code change. Whether a change is listed depends only on
that, not on which part of the version moved: a patch release can need action too. New capabilities, internal
refactors and bug fixes are not listed; see the commits.

Newest entries go on top. Each entry is anchored to a version and says three things: **what changed**, **why**, **how
to apply**. The core module is versioned with tags; optional modules (`core/kdb/mysql`, `core/kdb/postgres`,
`core/kasynq`, `core/knats`, `core/khttpclient`) follow the core's commit with pseudo-versions: after upgrading the
core, fetch each optional module you use again at the new tag's commit (`docs/quickstart.md`, "Add the dependency").

## v0.1.1

### `auth.NewContext` takes `jwtv5.MapClaims`

**What changed:** `middleware/auth.NewContext(ctx, claims)` takes `jwtv5.MapClaims` instead of any `jwtv5.Claims`.

**Why:** `auth.FromContext` returns only `jwtv5.MapClaims`. Claims of any other type were stored without error and then
read back as "not signed in" everywhere, so a server with its own authentication middleware lost its callers silently.

**How to apply:** where you call `auth.NewContext` with another claims type (such as `jwtv5.RegisteredClaims` or your
own struct), build a `jwtv5.MapClaims` with the same fields instead. Calls that already pass `jwtv5.MapClaims` compile
unchanged.
