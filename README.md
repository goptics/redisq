# Redisq

A lightweight, thread-safe Redis-backed queue implementation in Go with support for distributed notifications.

## Features

- Thread-safe queue operations
- Distributed queue with real-time notifications
- Automatic message expiration support
- Graceful shutdown handling
- Concurrent operations support
- Simple API for queue operations

## Installation

```bash
go get github.com/fahimfaiaal/redisq
```

## Quick Start

```go
import "github.com/fahimfaiaal/redisq"

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

// Clear the queue
queue.Clear()
```

### Distributed Queue with Notifications

```go
// Create a distributed queue
dq := qs.NewDistributedQueue("distributed-queue")

// Subscribe to notifications
dq.Subscribe(func(action string, message []byte) {
    fmt.Printf("Action: %s, Message: %s\n", action, string(message))
})

// Start listening for notifications
dq.Start()

// Enqueue will trigger "enqueued" notification
dq.Enqueue("test message")

// Dequeue will trigger "dequeued" notification
data, ok := dq.Dequeue()

// Stop notifications
dq.Stop()
```

## Configuration

If you have docker installed just do the following:

```bash
cp .env.example .env
docker compose up -d
```

> you can change the `REDIS_PORT` in the .env file

## Testing

```bash
go test -race -v ./...
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
