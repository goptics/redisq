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

// Subscribe to notifications for queue and dequeue events
dpq.Subscribe(func(action string) {
    fmt.Printf("Action: %s\n", action)
})


// Enqueue with priority will trigger "enqueued" notification
dpq.Enqueue("important message", 1)

// Dequeue will trigger "dequeued" notification
data, ok := dpq.Dequeue()

```

### Distributed Queue with Notifications

```go
// Create a distributed queue
dq := qs.NewDistributedQueue("distributed-queue")

// Subscribe to notifications for queue and dequeue events
dq.Subscribe(func(action string) {
    fmt.Printf("Action: %s\n", action)
})

// Enqueue will trigger "enqueued" notification
dq.Enqueue("test message")

// Dequeue will trigger "dequeued" notification
data, ok := dq.Dequeue()
```

### Queue with Acknowledgment (Reliable Processing)

```go
// Create a queue with acknowledgment support
queue := qs.NewQueue("ack-queue")

// Set acknowledgment timeout (how long before unacknowledged items are requeued)
queue.SetAckTimeout(time.Minute * 5)

// Dequeue an item with a unique acknowledgment ID
ackID := "job-123"
item, ok := queue.Dequeue()
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
