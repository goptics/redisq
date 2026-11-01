# redisq

Redis-backed queues (FIFO, priority, distributed) for Go, used standalone or as a varmq backend.

## Language

**Queue Key**:
The name a caller gives a queue; identifies that queue's data within a Namespace.
_Avoid_: queue name, queue ID

**Namespace**:
An optional, client-wide name that scopes every Redis name redisq creates, keys and pub/sub channels alike, beneath the fixed `redisq` root. Empty unless the caller sets one.
_Avoid_: prefix, tenant, scope

**Order Kind**:
Whether a queue hands out items in insertion order (FIFO) or lowest score first (Priority). Queues of different Order Kinds never share data; queues of the same kind and key do, whether distributed or not.
_Avoid_: queue type, mode

**Sidecar Key**:
A Redis key redisq derives from a Queue Key to hold a queue's bookkeeping (nacked items, idle consumers, timeouts, sequence, dedupe markers).
_Avoid_: sub-key, helper key, aux key
