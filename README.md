<div align="center">
<picture>
  <source media="(prefers-color-scheme: dark)" srcset="./assets/text-logo-dark.svg">
  <source media="(prefers-color-scheme: light)" srcset="./assets/text-logo.svg">
  <img alt="redisq" width="320px" src="./assets/text-logo.svg">
</picture>

  <p>
    <a href="https://pkg.go.dev/github.com/goptics/redisq"><img src="https://img.shields.io/badge/go-pkg-00ADD8.svg?logo=go" alt="Go Reference" /></a>
    <a href="https://github.com/goptics/redisq/actions/workflows/redisq.yml"><img src="https://github.com/goptics/redisq/actions/workflows/redisq.yml/badge.svg" alt="CI" /></a>
    <a href="https://codecov.io/gh/goptics/redisq"><img src="https://codecov.io/gh/goptics/redisq/branch/main/graph/badge.svg" alt="Codecov" /></a>
    <a href="https://golang.org/doc/devel/release.html"><img src="https://img.shields.io/badge/Go-1.24+-00ADD8?style=flat-square&logo=go" alt="Go Version" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg?style=flat-square" alt="License" /></a>
  </p>

  <p><strong>Redis-backed FIFO and priority queues for Go, with acknowledgments, batch pops and distributed consumers.</strong></p>

  <p align="center">
    <a href="#-features">Features</a> ·
    <a href="#quick-start">Quick Start</a> ·
    <a href="#queue-types">Queue Types</a> ·
    <a href="#-reliable-processing">Reliable Processing</a> ·
    <a href="#how-it-works">How It Works</a> ·
    <a href="#delivery-guarantees">Delivery Guarantees</a> ·
    <a href="#api-reference">API Reference</a> ·
    <a href="#contributing">Contributing</a>
  </p>
</div>

---

## ✨ Features

- **FIFO and priority queues** on plain Redis lists and sorted sets; lower priority number is served first.
- **Distributed variants** that wake only idle consumers, one per waiting item, in the same round trip as the enqueue.
- **Acknowledgments with a visibility timeout**: unacknowledged items return to the queue, with deadlines taken from Redis's clock.
- **Batch dequeue**: up to 1000 items per round trip, each under its own ack ID, sized to recent traffic.
- **Deduplicated enqueue**: `EnqueueOnce` stores a job once per id, even across retries.
- **Queue TTL**: optional key expiration, refreshed on every enqueue.
- **Thread-safe**: share one queue object across goroutines.
- **Persistence adapter for [varmq](https://github.com/goptics/varmq)**.

## Quick Start

### Installation

```bash
go get github.com/goptics/redisq
```

Requires Go 1.24.1+ and Redis 6.0+.

### Basic Usage

```go
package main

import (
	"fmt"

	"github.com/goptics/redisq"
)

func main() {
	qs := redisq.New("redis://localhost:6379") // panics on an invalid URL
	defer qs.Close()

	queue := qs.NewQueue("my-queue")
	defer queue.Close()

	queue.Enqueue("hello world") // accepts string or []byte

	if data, ok := queue.Dequeue(); ok {
		fmt.Println(string(data.([]byte))) // items come back as []byte
	}
}
```

## Queue Types

| Type                       | Constructor                           | Order        | Wakes idle consumers |
| -------------------------- | ------------------------------------- | ------------ | :------------------: |
| `Queue`                    | `qs.NewQueue(key)`                    | FIFO         |          –           |
| `PriorityQueue`            | `qs.NewPriorityQueue(key)`            | Lowest first |          –           |
| `DistributedQueue`         | `qs.NewDistributedQueue(key)`         | FIFO         |          ✅          |
| `DistributedPriorityQueue` | `qs.NewDistributedPriorityQueue(key)` | Lowest first |          ✅          |

Every type supports `DequeueWithAckId`, `DequeueBatchWithAckIds`, `Acknowledge` and the rest of the [reliable processing](#-reliable-processing) API.

### Simple Queue

```go
queue := qs.NewQueue("my-queue")

queue.SetExpiration(time.Hour) // optional TTL on the queue key

queue.Enqueue("test message")
queue.Enqueue([]byte("binary data"))

length := queue.Len()
values := queue.Values()
queue.Purge()
```

### Priority Queue

```go
pq := qs.NewPriorityQueue("priority-queue")

// Lower number = higher priority
pq.Enqueue("low priority", 3)
pq.Enqueue("high priority", 1)
pq.Enqueue("medium priority", 2)

data, ok := pq.Dequeue() // "high priority"
```

### Distributed Queue

```go
dq := qs.NewDistributedQueue("distributed-queue")
defer dq.Close()

// Subscribing makes this instance a consumer that can be woken
dq.Subscribe(func(action string) {
	fmt.Printf("Action: %s\n", action) // "enqueued"
})

// Wakes one idle consumer, if any
dq.Enqueue("test message")

data, ok := dq.Dequeue()
```

### Distributed Priority Queue

```go
dpq := qs.NewDistributedPriorityQueue("distributed-priority-queue")
defer dpq.Close()

dpq.Subscribe(func(action string) {
	fmt.Printf("Action: %s\n", action) // "enqueued"
})

dpq.Enqueue("important message", 1)

data, ok := dpq.Dequeue()
```

See [How consumers are woken](#how-consumers-are-woken) for the wake protocol.

## 💡 Reliable Processing

### Acknowledgments

```go
queue := qs.NewQueue("ack-queue")

// How long a dequeued item stays pending before it can be requeued
queue.SetVisibilityTimeout(5 * time.Minute)

// The item stays pending under ackID until acknowledged
item, ok, ackID := queue.DequeueWithAckId()
if ok {
	processItem(item)
	queue.Acknowledge(ackID)
}
```

Manual control of the pending set:

```go
// Track an item under your own ID, then acknowledge it after processing
err := queue.PrepareForFutureAck("job-456", "important job")
acked := queue.Acknowledge("job-456")

// Requeue every item whose visibility timeout has passed
err = queue.RequeueNackedItems()

// Count items still waiting for acknowledgment
pending := queue.GetNackedItemsCount()
```

### Enqueue once per job id

```go
queue.SetDedupWindow(10 * time.Minute) // default 5 minutes

// Stored once per id within the window; a repeat returns true and stores nothing
queue.EnqueueOnce("send welcome email", "job-0b7e9c2a")

// Priority variants take the priority before the id
pq.EnqueueOnce("rebuild index", 1, "job-5f1d3e77")
```

See [Delivery Guarantees](#delivery-guarantees) for how ids should be chosen.

### Taking several items at once

`DequeueBatchWithAckIds` takes up to `len(items)` items (at most 1000) in one
round trip, each pending under its own ack ID, and reports how many are left.
Size the buffers to the work you can start right away: items taken but not
started are invisible to other consumers while their visibility timeout runs.

A call sizes itself by the previous batch calls on the same queue object (a
full batch on its first call): a trickle takes one item per call, cheaply, and
a burst like the last one is taken in one call. A call can take fewer items
than the queue holds, so keep calling while `remaining > 0` and you have room.

```go
items := make([]any, 8)
ackIDs := make([]string, 8)

for {
	n, remaining, err := queue.DequeueBatchWithAckIds(items, ackIDs)
	for i := range n {
		processItem(items[i])
		queue.Acknowledge(ackIDs[i])
	}
	if err != nil || remaining == 0 {
		break
	}
}
```

### Using with varmq

redisq implements varmq's persistent and distributed queue interfaces:

```go
qs := redisq.New("redis://localhost:6379")
defer qs.Close()

rq := qs.NewDistributedPriorityQueue("jobs")
dq := varmq.NewDistributedPriorityQueue[[]string](rq)
defer dq.Close()

dq.Add([]string{"https://example.com"}, 1)
```

A provider/consumer integration example lives in [`examples/varmq`](examples/varmq).

## How It Works

### How consumers are woken

A busy consumer keeps pulling on its own, so only idle ones need a signal.
A consumer that finds the queue empty (`Len()` returns 0, or a dequeue leaves
it empty while the consumer could take more) joins the idle set `<key>:idle`.
Each enqueue wakes idle consumers on their own channel `<key>:wake:<id>`, up to
one per waiting item, so a backlog wakes them all; when every consumer is
busy, nothing is published. A requeue of `n` expired items wakes up to `n`.
A consumer that is idle while items wait (for example after a wake reached a
paused instance) is nudged within a second. There is no `"dequeued"`
notification.

### Visibility deadlines

The visibility timeout counts from when Redis runs the pop, on Redis's clock,
so a pop that waited in Redis still gets the full timeout. Once every process
runs this version, clients whose clocks disagree share one deadline (older
versions stamp and sweep deadlines with their own clocks).

## Delivery Guarantees

go-redis re-sends a command whose reply was lost (a read timeout or a dropped
connection), up to 3 times by default. What that means per call:

| Call                                            | Guarantee     | On a lost reply                                    |
| ----------------------------------------------- | ------------- | -------------------------------------------------- |
| `DequeueWithAckId`, `DequeueBatchWithAckIds`    | At least once | Items come back on retry or on the next call       |
| `Enqueue`                                       | At least once | May store the job twice; `false` is not proof      |
| `EnqueueOnce`                                   | Once per id   | Stored once within the dedupe window               |
| `Dequeue`                                       | At most once  | Item is gone                                       |
| `Remove`                                        | –             | May remove a second identical item                 |

- **`DequeueWithAckId`** and **`DequeueBatchWithAckIds`** are safe: a re-sent
  pop hands back the items the first attempt took, and after a call that
  failed outright, the next call of at least its size returns those items (a
  smaller call leaves them for a later one). Only one failed call is kept at
  a time: if two fail together, the other's items stay pending until they
  are requeued. Delivery is at least once.
- **`Enqueue`** can store a job twice, and returning `false` after a network
  error does not prove the job was not stored. Use `EnqueueOnce` with a job
  id where that matters (below), or make handlers idempotent.
- **`EnqueueOnce(item, id)`** stores the item once per id within the dedupe
  window (5 minutes by default, `SetDedupWindow`), so neither a re-sent push
  nor your own retry after a `false` stores it twice. A repeat returns `true`
  and stores nothing. Use globally unique ids (a UUID or nanoid, never a
  per-process counter): two producers that reuse an id drop each other's
  jobs. The id is remembered for the whole window, even after its job is
  handled, removed or purged. Each id holds about 100 bytes of Redis memory
  for the window. Keep the window above the client's re-send window (about 4
  minutes with go-redis's default timeouts). varmq uses it for every job
  added with `WithJobId` or a job id generator.
- **`Dequeue`** (no ack) is at most once: an item whose reply never arrives is
  gone. Use `DequeueWithAckId` for jobs you can't lose.
- **`Remove`** can remove a second identical item if its call is re-sent.

> [!WARNING]
> Keep the visibility timeout well above the client's re-send window: about
> 4 minutes at worst with go-redis's defaults (up to 7 sends of a script, each
> after a read timeout, a backoff, a pool wait, a dial and a connection's
> setup), usually a few seconds. A pop that reaches
> Redis only after its item was requeued, which takes a stall longer than the
> visibility timeout, can still lose a job.

## Development

Tests start Redis in a container through [dockertest](https://github.com/ory/dockertest), so Docker must be running; no Redis server is needed.

| Command                         | Description                                   |
| ------------------------------- | --------------------------------------------- |
| `make test`                     | Run all tests with race detection and coverage |
| `REDIS_TAG=6.0 make test`       | Run the tests on the oldest supported Redis   |
| `make format`                   | Format code                                   |
| `make release VERSION=v1.0.0`   | Tag and push a signed release                 |

## API Reference

For the complete package documentation, types, and method signatures, browse the **[GoDoc](https://pkg.go.dev/github.com/goptics/redisq)**.

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=goptics/redisq&type=Date)](https://www.star-history.com/#goptics/redisq&Date)

## Contributing

Contributions are welcome! Open an issue to discuss a change, or send a pull request:

1. Fork the repository and create a branch (`git checkout -b feature/amazing-feature`)
2. Make your change with tests, and run `make test`
3. Open a pull request against `main`

## License

Released under the [MIT License](LICENSE).
