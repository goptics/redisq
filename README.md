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
    <a href="#development">Development</a> ·
    <a href="#contributing">Contributing</a>
  </p>

  <p>
    Redis-backed FIFO and priority queues for Go, with acknowledgments, batch pops, consumers that hand unacknowledged jobs back when they close or die, wakes for idle consumers, and job outcomes any process can wait on.
  </p>
</div>

## Features

- **FIFO and priority queues** on Redis lists and sorted sets; lower priority number is served first.
- **Every queue distributed**: an enqueue wakes idle consumers, up to one per waiting job.
- **Job outcomes**: `WatchOutcome` tells any process when a job completes or fails, with the worker's result.
- **Acknowledgments** with a [visibility timeout](#acknowledgments-and-the-visibility-timeout) on Redis's clock.
- **Fast handover**: a consumer that [closes](#when-a-consumer-closes) hands its unacknowledged jobs back at once; one that [dies](#when-a-consumer-dies), in about 3-4 s; one cut off from Redis, after a 30 s heartbeat.
- **Batch dequeue** of up to 1000 items per round trip, each under its own ack ID.
- **Deduplicated enqueue**: `EnqueueOnce` stores a job once per id within a dedup window.
- **Job records and inspection** in Redis, shared by every instance: completed and failed jobs with payloads, kept by count and age.
- **[Queue pause](#queue-pause)**: `Pause` stops every consumer of a queue, in every process, until `Resume`.
- **[Job cancel](#job-cancel)**: `CancelJob` asks to cancel a waiting or running job, from any process.
- **[Workflows](#workflows)**: VarMQ workflows (a DAG of jobs across queues) run on Redis, in any process.
- **[Instance reports](#instance-reports)**: each process can report its workers to Redis, for a dashboard such as vartop.
- **Queue TTL**, refreshed on every enqueue.
- **Thread-safe**: share one queue object across goroutines.
- **Storage for [VarMQ](https://github.com/goptics/varmq)** queues.

## Quick Start

```bash
go get github.com/goptics/redisq
```

Requires Go 1.26+ and Redis 6.0+. With VarMQ, it requires VarMQ ≥ v1.11.0 and Go 1.27+ (see the [version table](https://varmq.goptics.org/start/installation/#versions)).
An older VarMQ acknowledges a failed job instead of rejecting it, and has no queue pause or job cancel.

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

| Type            | Constructor                | Order        |
| --------------- | -------------------------- | ------------ |
| `Queue`         | `qs.NewQueue(key)`         | FIFO         |
| `PriorityQueue` | `qs.NewPriorityQueue(key)` | Lowest first |

Both support every API below: acks, dedup, batches, wakes, outcomes, pause and cancel.

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

### Keys and namespaces

Every key and channel starts with `redisq:`, so `SCAN redisq:*` finds all of redisq's data. A namespace goes after it:

```go
qs := redisq.New(url)
qs.NewQueue("orders")         // redisq:fifo:{orders}, redisq:fifo:{orders}:idle, ...
qs.NewPriorityQueue("orders") // redisq:prio:{orders}, ... (a separate queue)

qs = redisq.New(url, redisq.WithNamespace("app"))
qs.NewQueue("orders")         // redisq:app:fifo:{orders}, ...
```

A queue's keys share one hash slot, so the queue key layout is slot-safe. Redis Cluster is still not supported (see [Trade-offs](#trade-offs)). `{` and `}` are reserved: `New` panics on a namespace containing them, and a constructor on an empty queue key or one containing them.

> **Upgrading from 1.5.x or earlier:** those versions used the queue key as given (`orders`, `orders:idle`, ...). This version does not read those keys, so drain old queues (or move their items) before switching.

### Instance reports

With VarMQ, a process can report itself to Redis: worker names, status, concurrency, processed counts and bound queues. A dashboard such as vartop then lists live processes without calling them. A worker's start, stop, pause, resume or concurrency change is reported at once. Load and counts follow the interval:

```go
varmq.InstanceID = "orders-api-1" // optional; default hostname. Read once, so set it first
qs := redisq.New(url, redisq.WithNamespace("app"), redisq.WithInstanceReports(5*time.Second))
defer qs.Close() // stops the reports and removes this process's record
```

Turn reports on for one client per namespace: `Close` removes the process's shared record. An empty `varmq.InstanceID` means no reports.

Each report overwrites one field of `redisq:app:{instances}` and its time in `redisq:app:{instances}:beats`, from the Redis clock. A process that stops without `Close` shows as lost after 3 intervals; its record is pruned after 5 minutes, and both keys expire when no process reports. The interval must be from 1s to 60s. Off by default.

### Waking consumers

```go
q := qs.NewQueue("orders")
defer q.Close()

// Subscribing makes this instance a consumer that can be woken
q.Subscribe(func(action string) {
	fmt.Println(action) // "enqueued"
})

q.Enqueue("message") // wakes an idle consumer, if any
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

`Close` hands every job the queue object took and did not acknowledge back to the queue at once. So stop or
drain your workers first (with VarMQ, `w.StopAndWait()`). Jobs tracked with
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
an id drop each other's jobs. A push of an id after its window drops that id's old Job Record, so a watch of the new run
never sees the old outcome.

### Job records and inspection

`Acknowledge` marks a job completed and `Reject` marks it failed for good (no requeue). Each, in one atomic
script, counts the job, keeps its Job Record (item verbatim, start and finish times, attempts, and `Reject`'s reason as its `errors` entry)
and trims old records. Every enqueue counts as submitted, except an `EnqueueOnce` repeat.

```go
queue.Reject(ackID, "payment declined")

queue.SetCompletedRetention(1000, 24*time.Hour)  // default; 0 for either keeps none
queue.SetFailedRetention(10000, 7*24*time.Hour)  // default

queue.QueueID()    // "redisq:fifo:{orders}", the same on every instance
queue.QueueStats(ctx) // counts per Job State now, totals since the counters began
queue.Jobs(ctx, contract.JobFailed, 0, 50) // newest first; pending in hand-out order
```

Both queue types implement `contract.QueueInspector`
(`github.com/goptics/varmq/contract`), which VarMQ's `Handler` serves as `GET /queues` and
`GET /queues/{id}/jobs`. The layout is VarMQ's
[`spec/redis-storage.md`](https://github.com/goptics/varmq/blob/main/spec/redis-storage.md) 1.7.0. `GET /queues/{id}/jobs/{jobId}` returns 501:
redisq does not implement `contract.JobReader` yet. `Handler` has no auth. Its GET routes return kept job payloads,
and its PATCH routes pause, resume, stop and tune workers and cancel jobs and workflows. Put auth in front of all of it. Retention is set
per queue object, so give every instance of a queue the same values. Records hold a copy of each payload, so
retention bounds Redis memory. `Purge` deletes all of a queue's jobs, records and counters. A purged workflow task
ends `cancelled`, so its workflow goes on. Purge decodes every queued and processing item inside Redis to find
workflow tasks, so a queue of millions blocks Redis while it runs.

### Job outcomes

A producer waits on a job that any process runs. Jobs are VarMQ envelopes (`{"id": ...}`); the id is the job ID.

```go
cancel := q.WatchOutcome("job-0b7e9c2a", func(o contract.JobOutcome) {
	fmt.Println(o.State, o.Error, string(o.Result)) // "completed", "failed" or "cancelled"
})
defer cancel() // early stop only; after fn the watch ends by itself

q.AcknowledgeResult(ackID, []byte(`{"total":42}`)) // Acknowledge + result JSON; invalid JSON: false, no change
```

`Acknowledge`, `AcknowledgeResult`, `Reject` and `AcknowledgeCancelled` publish the outcome on `<key>:done:<job id>`
in the same script, kept record or not. So does a pop that drops a cancelled job (see [Job cancel](#job-cancel)). A watch
subscribes that channel on the queue object's one pub/sub connection, reads the Job Record right after, and again
each sweep (1 s), so a job that ended before the watch, or a message lost in a reconnect, is still reported while
its record is kept. `AcknowledgeResult` stores the result raw as the record's `result`, big integers intact.
`Close` drops watchers without calling them (a delivery already under way may still call its fn).

### Queue pause

```go
queue.Pause()  // every consumer, in every process, stops taking jobs
queue.Paused() // true; this object's cached view, no Redis call
stop := queue.OnPauseChange(func(paused bool) { fmt.Println("paused:", paused) })
defer stop()
queue.PauseRefusals() // this object's pops that Redis refused for the pause
queue.Resume()        // consumers take jobs again; idle consumers are woken
```

`Pause` sets `<key>:paused` to the pause time. The flag has no expiry, so it survives restarts, and `Purge` keeps it.
While the flag exists, every pop hands out nothing: plain or acknowledged, one item or a batch.
Enqueue, `Acknowledge`, `Reject` and requeues go on as before. `Resume` deletes the flag and wakes idle consumers.

Each queue object caches the flag for `Paused` and `OnPauseChange`. Its own calls, pop replies, the `<key>:control`
channel and the 1 s sweep update the cache, so a lost message costs at most a second. `QueueStats` reports `paused`
and `paused_at`. Upgrade every consumer before you pause a queue: an older version ignores the flag.
A pause stops every consumer, in every process, until `Resume`. So authorize untrusted callers of `Pause` and `Resume`.

### Job cancel

```go
err := queue.CancelJob("job-0b7e9c2a") // from any process; contract.ErrJobEnded if its record shows it ended

stop := queue.OnCancel(func(jobID string) { /* stop the running job */ })
defer stop()

queue.AcknowledgeCancelled(ackID, "cancelled by user") // finish a job that stopped for a cancel
```

A waiting job is dropped by the pop that would hand it out. Its record and outcome are `cancelled`, and it never runs.
A waiting workflow task is the exception: it is handed out and runs unless its workflow is cancelled. So cancel it
with the workflow.

A running job is announced on `<key>:control` to `OnCancel` callbacks. VarMQ then cancels the job's context. If the
handler returns an error or panics, VarMQ finishes the job with `AcknowledgeCancelled`: as `Reject`, but with state
`cancelled`. A workflow task ends through its workflow instead. If the handler finishes anyway, the job completes as usual. Storage never stops a running job itself.

Each mark lives 24 hours in the sorted set `<key>:cancelled`, with its expiry as the score. With no marks, a pop costs
one more `EXISTS`. `Purge` deletes the marks. Any ID without an ended record can be marked, even one not pushed yet.
A later plain `Enqueue` of that ID is dropped if it is popped before the mark expires (`EnqueueOnce` clears the mark). Use job IDs that others cannot guess. Authorize
and rate-limit untrusted callers of `CancelJob`.

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

### With VarMQ

redisq implements VarMQ's queue interfaces, including outcome watching, pause, cancel and workflows:

```go
// consumer process
w := varmq.NewResultWorker(func(j varmq.Job[string]) (int, error) { return len(j.Data()), nil })
w.WithPriorityQueue(qs.NewPriorityQueue("jobs"))

// producer process
q := varmq.NewResultPriorityQueue[string, int](qs.NewPriorityQueue("jobs"))
job, _ := q.Add("https://example.com", 1)
n, err := job.Result() // waits until any process finished it
```

See [`examples/varmq`](examples/varmq) for a provider/consumer setup.

### Workflows

A VarMQ workflow is a DAG of jobs across queues. The workflow state lives in a workflow store, which is separate from
the queues. Make one store from your `Queues` client, in every process that submits or runs workflow tasks:

```go
qs := redisq.New("redis://localhost:6379")
store, err := redisq.NewWorkflowStore(qs) // registers itself for its scope
if err != nil {
	log.Fatal(err)
}
defer store.Close() // unregisters it; close it before qs

upload, thumb := qs.NewQueue("upload"), qs.NewPriorityQueue("thumb")

wf := varmq.NewWorkflow(store, varmq.WithWorkflowID("upload-1"))
up := wf.Add("upload", varmq.NewResultQueue[string, string](upload), "photo.png")
wf.Add("thumb", varmq.NewResultPriorityQueue[int, string](thumb), 64).After(up)
err = wf.Submit(ctx) // workers in any process run it; wf.WaitContext(ctx) for the end
```

The scope of a store is the Redis address, the database and the namespace. Every queue of that scope reports it with
`ScopeID()`. When a worker takes a task, VarMQ finds the store by that scope. So a process that runs workflow tasks
must make a store for the scope, or VarMQ does not run those tasks and reports an error. A process has one store per
scope. A second `NewWorkflowStore` for the same scope returns `contract.ErrScopeRegistered` until you close the first.

`NewWorkflowStore` takes options. `WithWorkflowResultLimit` sets the largest task result, 64 KiB by default.
`WithWorkflowRetention` sets how long an ended workflow stays, 7 days by default. A running workflow stays until it
ends.

A task finishes in one script that settles its job and pushes the tasks it releases. So a crash never leaves a
workflow between the two. VarMQ calls `store.FinishTask(queueID, ackID, end)` for this. The store finds the queue from
its key, so the queue can come from any client of the same scope. The workflow keys are `redisq:[ns:]wf:<id>` and
`redisq:[ns:]wf:index`, as in VarMQ's `spec/redis-storage.md`. A task's job record uses the retention that you set
with `SetCompletedRetention` on the queue object of the same `Queues` client. Other clients use the default.

Every consumer of a queue that carries workflow tasks must run this version. Anyone who can push to a task's queue
can push a copy of that task, so treat push access as trusted. Do not use `SetExpiration` on a queue that carries
workflow tasks. Workflow pushes do not renew the expiry, and Submit refuses such a queue object. See
[`examples/workflow`](examples/workflow).

## How It Works

### Acknowledgments and the visibility timeout

`DequeueWithAckId` runs one Redis script: it takes the job from the head of the list, stores it in the pending
hash `<key>:nacked` and its deadline (Redis time plus the visibility timeout) in `<key>:timeouts`, and returns it.
`Acknowledge` deletes both. Each queue object sweeps once a second, and a job whose deadline passed goes back to
the front of the list (a priority queue re-adds it at its priority, behind the jobs already waiting there). A job
is always in the queue or pending, never only in a client.

![redisq acknowledgments and the visibility timeout](./assets/diagrams/redisq-ack.svg)

VarMQ acknowledges or rejects every job it finishes: `Reject` on an error or a panic, so a failed job does not come
back. With VarMQ, a job comes back this way only when it runs past the visibility timeout or its `Acknowledge` or
`Reject` fails (Redis unreachable). Keep the timeout above your slowest job.

### Waking idle consumers

A consumer (one that called `Subscribe`) that finds the queue empty joins the idle set `<key>:idle`. An enqueue pushes the job and,
in the same script, wakes up to one idle consumer per waiting job, each on its own pub/sub channel. A consumer
that took a job stays out of the idle set until its own `Len()` or dequeue finds the queue empty again, so wakes
go to free consumers; a busy one pulls the next job itself when one finishes (VarMQ does). Every consumer also
runs a nudge once a second: with jobs waiting it pulls, and with the queue empty it puts an idle consumer back in
the idle set, in case a wake was lost.

![redisq waking idle consumers](./assets/diagrams/redisq-wake.svg)

### When a consumer closes

`Close` moves every job the consumer took and did not acknowledge back to the front of the queue in one script;
the queue first leaves the idle set, then wakes idle consumers for the jobs. The handover takes no
timer, as when RabbitMQ requeues a closed channel's deliveries. An `Acknowledge` after `Close` fails, so a job still running on the closing
consumer runs again on the next one: stop or drain workers before `Close`.

![redisq Close hands unacknowledged jobs back](./assets/diagrams/redisq-close.svg)

### When a consumer dies

A consumer killed without `Close` (kill -9, OOM, a crash) can't hand anything back, so the other processes find
out. Every ack pop that hands out a job sets the queue object's heartbeat in `<key>:heartbeats`, and its sweep
renews it every second. Every queue object is also subscribed to its own wake channel, and Redis drops that
subscription as soon as the process exits. Any process's sweep, including the first one of a restarted process,
then requeues the dead consumer's jobs and wakes up to one idle consumer per job. A
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
once.

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
- **Outcomes need the record or the message.** A job whose outcome message is lost (a reconnect) and whose Job
  Record retention keeps none is never reported to its watchers: pair a watch with a timeout of your own.
- **No retry limit or dead-letter queue.** A job that crashes every consumer that takes it keeps coming back
  after each crash, about 3-4 s later.
- **Cost.** Every queue object, producers included, runs one sweep script a second and holds one pub/sub
  connection (count them against Redis's `maxclients`); about once a second per key it also counts subscribers
  (`PUBSUB NUMSUB`) for the consumers holding jobs. Each pop that hands out jobs also
  writes the heartbeat. A dead consumer or a `Close` reads every pending ack ID once.
- **One Redis server.** `New` uses a single-node client, so Redis Cluster isn't supported. Queue keys share a hash
  slot, but workflow keys carry no hashtag, and wakes and presence count pub/sub subscribers on one node only.

## Delivery Guarantees

go-redis re-sends a command whose reply was lost, so:

| Call                                                                 | Guarantee     | On a lost reply                                                                 |
| -------------------------------------------------------------------- | ------------- | ------------------------------------------------------------------------------- |
| `DequeueWithAckId`, `DequeueBatchWithAckIds`                         | At least once | Items come back on retry, the next call, or after a timeout                     |
| `Enqueue`                                                            | At least once | May store the job twice; `false` is not proof                                   |
| `EnqueueOnce`                                                        | Once per id   | Stored once within the dedup window                                             |
| `Acknowledge`, `AcknowledgeResult`, `Reject`, `AcknowledgeCancelled` | Once          | The re-send finds the job finished: no second record or outcome                 |
| `CancelJob`                                                          | At least once | May announce the cancel twice; may return `ErrJobEnded` after it marked the job |
| `Dequeue`                                                            | At most once  | Item is gone                                                                    |
| `Remove`                                                             | –             | May remove a second identical item                                              |

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
