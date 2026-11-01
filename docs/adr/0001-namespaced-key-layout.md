---
status: superseded by ADR-0002
---

# Namespaced key layout: `ns:kind:{key}`, opt-in

Setting a Namespace (`New(url, WithNamespace("app"))`) switches a client to a new key layout, `app:fifo:{orders}` / `app:prio:{orders}`, with Sidecar Keys and wake channels appended (`app:prio:{orders}:seq`). Without a Namespace the legacy layout (`orders`, `orders:idle`, …) is kept byte for byte, so upgrading never strands data in existing deployments; opting in to a Namespace is the one migration point, so hashtags and the Order Kind segment ride along with it instead of costing a second key move later.

## Considered Options

- **Hashtag on the whole namespace (`{app}:orders`)**: rejected; every queue lands in one cluster slot.
- **Hashtags always, major version bump**: rejected; breaks every deployed queue for users who never asked for a namespace.
- **Detect FIFO/priority collisions at runtime (`TYPE` check)**: rejected; only catches the clash once data exists, costs a round trip per constructor, and Redis already fails loudly with `WRONGTYPE` on the legacy layout.

## Consequences

- The hashtag wraps only the Queue Key, so one queue and all its Sidecar Keys share a cluster slot and different queues spread out. This prepares for Redis Cluster but does not deliver it: the client is still a single-node `*redis.Client` (follow-up).
- `{` and `}` are reserved: rejected in the Namespace, and in the Queue Key under a Namespace, since Redis hashes on the first `{…}` and a user brace would silently pin every queue to one slot. Legacy Layout keys still accept braces, because existing deployments may already use them as their own hashtags.
- FIFO and priority queues with the same Queue Key are independent under a Namespace, but still collide (`WRONGTYPE`) on the legacy layout.
- An invalid option makes `New` panic, like an invalid URL already does. All validation sits in one place so an error-returning constructor can be added if anyone needs it.
