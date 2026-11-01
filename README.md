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
    <a href="https://golang.org/doc/devel/release.html"><img src="https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat-square&logo=go" alt="Go Version" /></a>
    <a href="LICENSE"><img src="https://img.shields.io/badge/license-MIT-blue.svg?style=flat-square" alt="License" /></a>
  </p>

  <p align="center">
    <a href="#features">Features</a> ·
    <a href="#quick-start">Quick Start</a> ·
    <a href="#usage">Usage</a> ·
    <a href="#how-it-works">How It Works</a> ·
    <a href="#trade-offs">Trade-offs</a> ·
    <a href="#delivery-guarantees">Delivery Guarantees</a> ·
    <a href="#development">Development</a>
  </p>

  <p>
    Redis-backed FIFO and priority queues for Go, with acknowledgments, batch pops, consumers that hand unacknowledged jobs back when they close or die, and distributed variants that wake idle consumers.
  </p>
</div>

## Features

- **FIFO and priority queues** on Redis lists and sorted sets; lower priority number is served first.
- **Distributed variants** that wake idle consumers, up to one per waiting job.
- **Acknowledgments** with a [visibility timeout](#acknowledgments-and-the-visibility-timeout) on Redis's clock.
- **Fast handover**: a consumer that [closes](#when-a-consumer-closes) hands its unacknowledged jobs back at once; one that [dies](#when-a-consumer-dies), in about 3-4 s; one cut off from Redis, after a 30 s heartbeat.
- **Batch dequeue** of up to 1000 items per round trip, each under its own ack ID.
- **Deduplicated enqueue**: `EnqueueOnce` stores a job once per id within a dedup window.
- **Queue TTL**, refreshed on every enqueue.
- **Thread-safe**: share one queue object across goroutines.
- **Storage for [varmq](https://github.com/goptics/varmq)** persistent and distributed queues.

## Quick Start

```bash
go get github.com/goptics/redisq
```

Requires Go 1.26+ and Redis 6.0+.

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

## Usage

### Queue types

| Type                       | Constructor                           | Order        | Wakes idle consumers |
| -------------------------- | ------------------------------------- | ------------ | :------------------: |
| `Queue`                    | `qs.NewQueue(key)`                    | FIFO         |          –           |
| `PriorityQueue`            | `qs.NewPriorityQueue(key)`            | Lowest first |          –           |
| `DistributedQueue`         | `qs.NewDistributedQueue(key)`         | FIFO         |          ✅          |
| `DistributedPriorityQueue` | `qs.NewDistributedPriorityQueue(key)` | Lowest first |          ✅          |

All types support the acknowledgment, dedup and batch APIs below.

```go
queue := qs.NewQueue("my-queue")
queue.SetExpiration(time.Hour) // optional TTL on the queue key
queue.Enqueue("message")
queue.Len()
queue.Values()
queue.Purge()

pq := qs.NewPriorityQueue("priority-queue")
pq.Enqueue("low", 3)
pq.Enqueue("high", 1)
data, ok := pq.Dequeue() // "high"
```

### Namespace

```go
qs := redisq.New(url, redisq.WithNamespace("app"))
qs.NewQueue("orders")         // keys app:fifo:{orders}, app:fifo:{orders}:idle, ...
qs.NewPriorityQueue("orders") // keys app:prio:{orders}, ... (a separate queue)
```

Without a namespace, keys are the queue key as given (`orders`, `orders:idle`, ...), as in earlier versions. Setting one moves a queue to new keys, so drain the old ones first. `{` and `}` are reserved: `New` panics on a namespace containing them, and so does a constructor on such a queue key under a namespace.

### Distributed queue

```go
dq := qs.NewDistributedQueue("distributed-queue")
defer dq.Close()

// Subscribing makes this instance a consumer that can be woken
dq.Subscribe(func(action string) {
	fmt.Println(action) // "enqueued"
})

dq.Enqueue("message") // wakes an idle consumer, if any
```

### Acknowledgments

```go
queue.SetVisibilityTimeout(5 * time.Minute) // default; keep it above your slowest job
queue.SetHeartbeatTimeout(30 * time.Second) // default; how long a frozen or cut-off process keeps its jobs

item, ok, ackID := queue.DequeueWithAckId()
if ok {
	processItem(item)
	queue.Acknowledge(ackID)
}

queue.GetNackedItemsCount()   // jobs any consumer of this key took and has not acknowledged
queue.RequeueNackedItems()    // requeue expired jobs now; with a 0 visibility timeout, every pending job
queue.PrepareForFutureAck("job-456", item) // track an item under your own ack ID
```

`Close` hands every job the queue object took and did not acknowledge back to the queue at once, so stop or
drain your workers first (varmq 1.7+: `w.StopAndWait()`; before 1.7, `w.Stop()` waits). Jobs tracked with
`PrepareForFutureAck` get no heartbeat and `Close` doesn't hand them back: they wait for the visibility timeout.
See [How It Works](#how-it-works).

### Enqueue once per job id

```go
queue.SetDedupWindow(10 * time.Minute) // default 5 minutes; ~100 bytes of Redis memory per id

// Stored once per id within the window; a repeat returns true and stores nothing
queue.EnqueueOnce("send welcome email", "job-0b7e9c2a")

// Priority variants take the priority before the id
pq.EnqueueOnce("rebuild index", 1, "job-5f1d3e77")
```

An id stays taken for the whole window, even after its job is handled, removed or purged, so producers that reuse
an id drop each other's jobs.

### Batch dequeue

Takes up to `len(items)` items (max 1000) in one round trip, sized by recent calls. Keep calling while
`remaining > 0`, and size the buffers to work you can start now: taken items stay invisible to other consumers
until they are acknowledged or come back. After a call fails, the next call with at least as many slots gets its
items back; a smaller call, or a second call failing at the same time, leaves them pending until they come back.

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

### With varmq

redisq implements varmq's persistent and distributed queue interfaces:

```go
rq := qs.NewDistributedPriorityQueue("jobs")
dq := varmq.NewDistributedPriorityQueue[[]string](rq)
defer dq.Close()

dq.Add([]string{"https://example.com"}, 1)
```

See [`examples/varmq`](examples/varmq) for a provider/consumer setup.

## How It Works

### Acknowledgments and the visibility timeout

`DequeueWithAckId` runs one Redis script: it takes the job from the head of the list, stores it in the pending
hash `<key>:nacked` and its deadline (Redis time plus the visibility timeout) in `<key>:timeouts`, and returns it.
`Acknowledge` deletes both. Each queue object sweeps once a second, and a job whose deadline passed goes back to
the front of the list (a priority queue re-adds it at its priority, behind the jobs already waiting there). A job
is always in the queue or pending, never only in a client.

![redisq acknowledgments and the visibility timeout](./assets/diagrams/redisq-ack.svg)

varmq acknowledges a job even when its handler fails, so with varmq a job comes back this way only when it runs
past the visibility timeout or its `Acknowledge` fails (Redis unreachable). Keep the timeout above your slowest
job.

### Waking idle consumers

A distributed consumer that finds the queue empty joins the idle set `<key>:idle`. An enqueue pushes the job and,
in the same script, wakes up to one idle consumer per waiting job, each on its own pub/sub channel. A consumer
that took a job stays out of the idle set until its own `Len()` or dequeue finds the queue empty again, so wakes
go to free consumers; a busy one pulls the next job itself when one finishes (varmq does). Every consumer also
runs a nudge once a second: with jobs waiting it pulls, and with the queue empty it puts an idle consumer back in
the idle set, in case a wake was lost.

![redisq waking idle consumers](./assets/diagrams/redisq-wake.svg)

### When a consumer closes

`Close` moves every job the consumer took and did not acknowledge back to the front of the queue in one script;
a distributed queue first leaves the idle set, then wakes idle consumers for the jobs. The handover takes no
timer, as when RabbitMQ requeues a closed channel's deliveries. An `Acknowledge` after `Close` fails, so a job still running on the closing
consumer runs again on the next one: stop or drain workers before `Close`.

![redisq Close hands unacknowledged jobs back](./assets/diagrams/redisq-close.svg)

### When a consumer dies

A consumer killed without `Close` (kill -9, OOM, a crash) can't hand anything back, so the other processes find
out. Every ack pop that hands out a job sets the queue object's heartbeat in `<key>:heartbeats`, and its sweep
renews it every second. Every queue object is also subscribed to its own wake channel, and Redis drops that
subscription as soon as the process exits. Any process's sweep, including the first one of a restarted process,
then requeues the dead consumer's jobs and, on a distributed queue, wakes up to one idle consumer per job. A
process that is still sweeping is never taken for gone, so a second process on the key (a rolling deploy) keeps
its running jobs:

| Consumer 2 holds unacknowledged jobs and…                             | They are back in the queue after      |
| --------------------------------------------------------------------- | ------------------------------------- |
| calls `Close` on shutdown (trap SIGINT/SIGTERM, e.g. with `Listen()`) | at once                               |
| dies (kill -9, OOM, a crash)                                          | about 3-4 s (no subscriber for 2.5 s) |
| dies within ~1 s of its first pop                                     | the heartbeat timeout (30 s) + ~1 s   |
| loses Redis, or its host goes down (Redis keeps the socket)           | the heartbeat timeout (30 s) + ~1 s   |
| stays alive but a job hangs                                           | the visibility timeout (5 min)        |

A process restarted alone after a crash gets its old jobs back about 3 s after it starts: its first sweep runs at
once. Only a distributed queue wakes a consumer for requeued jobs. On a `Queue` or `PriorityQueue` they wait for a
consumer's next pop; varmq's persistent queues pull only after a local `Add`, a start or a finished job.

![redisq detecting a dead consumer](./assets/diagrams/redisq-dead.svg)

## Trade-offs

- **At least once, not exactly once.** A job runs again when its consumer dies mid-job, when it outlives its
  visibility timeout, when its process goes without Redis for longer than the heartbeat timeout, when its
  `Acknowledge` fails, or when its consumer closes before it finishes. A late `Acknowledge` after a timeout still
  returns `true`. Make handlers idempotent.
- **Timeouts trade speed for false alarms.** A shorter heartbeat timeout (at least 5 s) recovers a lost host
  sooner, but a process frozen or cut off from Redis for longer loses its running jobs to other consumers. A
  stall of Redis itself (failover, restart, pause) does not count: when no process could sweep, every heartbeat
  moves on by the gap. Visibility deadlines don't: keep the visibility timeout above your slowest job and the
  client's re-send window (about 4 minutes).
- **Wakes are best-effort.** A wake can be lost in a pub/sub reconnect, or reach a consumer that can't take the
  job (paused, or marked idle by a `Len()` call from elsewhere while busy); the once-a-second nudge bounds the
  delay to about a second, at the cost of one Redis script per consumer per second. A consumer that stops pulling
  before the queue is empty isn't woken again, only nudged.
- **No retry limit or dead-letter queue.** A job that crashes every consumer that takes it keeps coming back
  after each crash, about 3-4 s later.
- **Cost.** Every queue object, producers included, runs one sweep script a second and holds one pub/sub
  connection (count them against Redis's `maxclients`); about once a second per key it also counts subscribers
  (`PUBSUB NUMSUB`) for the consumers holding jobs. Each pop that hands out jobs also
  writes the heartbeat. A dead consumer or a `Close` reads every pending ack ID once.
- **One Redis server.** `New` uses a single-node client; Redis Cluster isn't supported.

## Delivery Guarantees

go-redis re-sends a command whose reply was lost, so:

| Call                                         | Guarantee     | On a lost reply                                                    |
| -------------------------------------------- | ------------- | ------------------------------------------------------------------ |
| `DequeueWithAckId`, `DequeueBatchWithAckIds` | At least once | Items come back on retry, the next call, or after a timeout        |
| `Enqueue`                                    | At least once | May store the job twice; `false` is not proof                      |
| `EnqueueOnce`                                | Once per id   | Stored once within the dedup window                                |
| `Dequeue`                                    | At most once  | Item is gone                                                       |
| `Remove`                                     | –             | May remove a second identical item                                 |

- Use globally unique ids with `EnqueueOnce` (UUID or nanoid, never a per-process counter).
- Use `DequeueWithAckId` for jobs you can't lose.
- Keep the visibility timeout and dedup window above the client's re-send window: about 4 minutes with the
  timeouts `New` sets, longer if the URL sets longer ones or Redis Cloud/Software maintenance relaxes them to 10s.
- A pop that reaches Redis only after its items were requeued (a stall longer than the visibility or heartbeat
  timeout) can still lose a job.

## Development

Tests start Redis in Docker via [dockertest](https://github.com/ory/dockertest); Docker must be running.

```bash
make test                     # race detection and coverage
REDIS_TAG=6.0 make test       # oldest supported Redis
make format                   # gofmt
make diagrams                 # regenerate the README diagrams (Python 3; each asserts what it draws)
make release VERSION=v1.0.0   # tag and push a signed release
```

The diagrams' facts, with code citations, are in [`assets/diagrams/gen/STORYBOARD.md`](assets/diagrams/gen/STORYBOARD.md).

## API Reference

See [GoDoc](https://pkg.go.dev/github.com/goptics/redisq).

## Star History

[![Star History Chart](https://api.star-history.com/svg?repos=goptics/redisq&type=Date)](https://www.star-history.com/#goptics/redisq&Date)

## Contributing

Open an issue to discuss a change, or send a pull request against `main` with tests (`make test`).

## License

[MIT](LICENSE)
