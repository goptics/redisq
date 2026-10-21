# Redisq

[![Go Reference](https://img.shields.io/badge/go-pkg-00ADD8.svg?logo=go)](https://pkg.go.dev/github.com/goptics/redisq)
[![Go Report Card](https://goreportcard.com/badge/github.com/goptics/redisq)](https://goreportcard.com/report/github.com/goptics/redisq)
[![Go Version](https://img.shields.io/badge/Go-1.24+-00ADD8?style=flat-square&logo=go)](https://golang.org/doc/devel/release.html)
[![CI](https://github.com/goptics/redisq/actions/workflows/redisq.yml/badge.svg)](https://github.com/goptics/redisq/actions/workflows/go.yml)
[![codecov](https://codecov.io/gh/goptics/redisq/branch/main/graph/badge.svg)](https://codecov.io/gh/goptics/redisq)

A lightweight, thread-safe Redis-backed queue implementation in Go with support for distributed notifications and priority queuing.

## Features

- Thread-safe queue operations
- Priority queue support
- Distributed queue with real-time notifications
- Automatic message expiration support
- Graceful shutdown handling
- Concurrent operations support
- Simple API for queue operations
- Built as a persistence adapter for [varmq](https://github.com/goptics/varmq)

## Installation

```bash
go get github.com/goptics/redisq
```

## Quick Start

```go
import "github.com/goptics/redisq"

func main() {
    // Initialize queue system
    qs := redisq.New("redis://localhost:6379")
    defer qs.Close()

    // Create a simple queue
    queue := qs.NewQueue("my-queue")
    defer queue.Close()

    // Enqueue items
    queue.Enqueue("hello world")

    // Dequeue items
    data, ok := queue.Dequeue()
    if ok {
        fmt.Println(string(data.([]byte)))
    }
}
```

## Usage

### Simple Queue

```go
// Create a queue
queue := qs.NewQueue("my-queue")

// Optional: Set expiration for queue items
queue.SetExpiration(time.Hour)

// Enqueue items (supports []byte and string)
queue.Enqueue("test message")
queue.Enqueue([]byte("binary data"))

// Get queue length
length := queue.Len()

// Get all values
values := queue.Values()

// Purge the queue
queue.Purge()
```

### Priority Queue

```go
// Create a priority queue
pq := qs.NewPriorityQueue("priority-queue")

// Enqueue items with priority (lower number = higher priority)
pq.Enqueue("high priority", 1)
pq.Enqueue("medium priority", 2)
pq.Enqueue("low priority", 3)

// Items will be dequeued in priority order
data, ok := pq.Dequeue() // Returns "high priority"
```

### Distributed Priority Queue

```go
// Create a distributed priority queue
dpq := qs.NewDistributedPriorityQueue("distributed-priority-queue")

// Subscribing makes this instance a consumer that can be woken
dpq.Subscribe(func(action string) {
    fmt.Printf("Action: %s\n", action) // "enqueued"
})

// Wakes one idle consumer, if any
dpq.Enqueue("important message", 1)

data, ok := dpq.Dequeue()

```

### Distributed Queue with Notifications

```go
// Create a distributed queue
dq := qs.NewDistributedQueue("distributed-queue")

// Subscribing makes this instance a consumer that can be woken
dq.Subscribe(func(action string) {
    fmt.Printf("Action: %s\n", action) // "enqueued"
})

// Wakes one idle consumer, if any
dq.Enqueue("test message")

data, ok := dq.Dequeue()
```

#### How consumers are woken

A busy consumer keeps pulling on its own, so only idle ones need a signal.
A consumer that finds the queue empty (`Len()` returns 0, or a dequeue leaves
it empty while the consumer could take more) joins the idle set `<key>:idle`.
Each enqueue wakes idle consumers on their own channel `<key>:wake:<id>`, up to
one per waiting item, so a backlog wakes them all; when every consumer is
busy, nothing is published. A requeue of `n` expired items wakes up to `n`.
A consumer that is idle while items wait (for example after a wake reached a
paused instance) is nudged within a second. There is no `"dequeued"`
notification.

### Queue with Acknowledgment (Reliable Processing)

```go
// Create a queue with acknowledgment support
queue := qs.NewQueue("ack-queue")

// How long a dequeued item stays pending before it can be requeued
queue.SetVisibilityTimeout(time.Minute * 5)

// Dequeue an item; it stays pending under ackID until acknowledged
item, ok, ackID := queue.DequeueWithAckId()
if ok {
    // Process the item
    processItem(item)

    // Mark the item as successfully processed
    queue.Acknowledge(ackID)
}

// For manual control of the acknowledgment process:
// 1. Prepare an item for future acknowledgment
ackID := "job-456"
data := "important job"
err := queue.PrepareForFutureAck(ackID, data)

// 2. Acknowledge the item after processing
success := queue.Acknowledge(ackID)

// You can trigger requeue of all unacknowledged items
queue.RequeueNackedItems()

// 4. Get count of pending unacknowledged items
pendingCount := queue.GetNackedItemsCount()
```

#### Taking several items at once

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

#### Network errors and retries

go-redis re-sends a command whose reply was lost (a read timeout or a dropped
connection), up to 3 times by default. What that means per call:

- **`DequeueWithAckId`** and **`DequeueBatchWithAckIds`** are safe: a re-sent
  pop hands back the items the first attempt took, and after a call that
  failed outright, the next call of at least its size returns those items (a
  smaller call leaves them for a later one). Only one failed call is kept at
  a time: if two fail together, the other's items stay pending until they
  are requeued. Delivery is at least once.
- **`Enqueue`** can store a job twice, and returning `false` after a network
  error does not prove the job was not stored. Make handlers idempotent.
- **`Dequeue`** (no ack) is at most once: an item whose reply never arrives is
  gone. Use `DequeueWithAckId` for jobs you can't lose.
- **`Remove`** can remove a second identical item if its call is re-sent.
- Keep the visibility timeout well above the client's retry window
  (`(retries + 1) × read timeout`, 12s with the defaults). A pop that reaches
  Redis only after its item was requeued, which takes a stall longer than the
  visibility timeout, can still lose a job.

## Testing

```bash
make test
```

## Requirements

- Go 1.24.1 or higher
- Redis 6.0 or higher

## Contributing

1. Fork the repository
2. Create your feature branch (`git checkout -b feature/amazing-feature`)
3. Commit your changes (`git commit -m 'Add amazing feature'`)
4. Push to the branch (`git push origin feature/amazing-feature`)
5. Open a Pull Request

## 👤 Author

- GitHub: [@fahimfaisaal](https://github.com/fahimfaisaal)
- LinkedIn: [in/fahimfaisaal](https://www.linkedin.com/in/fahimfaisaal/)
- Twitter: [@FahimFaisaal](https://twitter.com/FahimFaisaal)

## License

This project is licensed under the MIT License - see the LICENSE file for details.
