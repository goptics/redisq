---
status: accepted, supersedes ADR-0001
---

# Every key under `varmq:`, no legacy layout

Every Redis name redisq creates is now `varmq:[ns:]kind:{key}`, with or without a Namespace, so all of the goptics queue data sits under one root (`SCAN varmq:*`) and every queue gets the cluster-safe hashtag and the FIFO/priority split. ADR-0001 kept the old un-prefixed names when no Namespace was set to avoid a breaking change. We gave that up for one layout instead of two, and shipped it as v2 (`github.com/goptics/redisq/v2`) so `go get -u` on v1 never moves anyone's keys.

## Considered Options

- **`WithLegacyKeys()` escape hatch**: rejected; two layouts to maintain and test indefinitely. A queue drains in minutes, so the migration path is "drain v1 first", documented in the README and changelog.
- **`redisq:` as the root**: rejected in favour of `varmq:`, the ecosystem name, since redisq is varmq's Redis backend.

## Consequences

- v2 does not read v1 keys; anything left queued in v1 is invisible after the upgrade.
- `{`, `}` and an empty Queue Key are rejected on every queue, not only namespaced ones.
