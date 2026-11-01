---
status: accepted, supersedes ADR-0001
---

# Every key under `redisq:`, no legacy layout

Every Redis name redisq creates is now `redisq:[ns:]kind:{key}`, with or without a Namespace, so all of redisq's data sits under one root (`SCAN redisq:*`) and every queue gets the cluster-safe hashtag and the FIFO/priority split. ADR-0001 kept the old un-prefixed names when no Namespace was set to avoid a breaking change; we gave that up for one layout instead of two.

## Considered Options

- **`WithLegacyKeys()` escape hatch**: rejected; two layouts to maintain and test indefinitely. A queue drains in minutes, so the migration path is "drain first", documented in the README and changelog.
- **`varmq:` as the root**: rejected; redisq is used without varmq too, and a key should name the library that wrote it.
- **Module path `/v2`**: rejected; the Go API stays compatible (`New` only gained a variadic parameter), so the module path is unchanged and the key change is called out as breaking in the changelog.

## Consequences

- Keys written by 1.5.x and earlier are not read; anything left queued there is invisible after the upgrade, and `go get -u` brings this in without an import change.
- `{`, `}` and an empty Queue Key are rejected on every queue, not only namespaced ones.
