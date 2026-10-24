# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Heartbeats: a consumer that dies or loses Redis has its unacknowledged
  items requeued, and idle consumers woken, once its heartbeat timeout runs
  out (30s by default) instead of after the visibility timeout
  - `SetHeartbeatTimeout` - how long a queue object may go without reaching
    Redis before other processes requeue its items (at least 5s; 0 or less
    turns it off)
  - A pop renews the heartbeat in the same step, and the sweep every second.
    Producers never get one, and items of older versions or tracked with
    `PrepareForFutureAck` still wait for the visibility timeout
  - A stall of Redis itself (failover, restart, pause) doesn't make live
    consumers look dead: heartbeats move on by the gap between sweeps
  - New keys `<key>:heartbeats` and `<key>:heartbeats:clock`
- Presence: a distributed consumer whose process exits has its items
  requeued about 5s after Redis drops its pub/sub subscription, without
  waiting for the heartbeat
  - A distributed queue's wake channel is now named after its ack ID prefix
  - Counts as gone only with no subscriber and no sweep of its own for 5s, so
    a go-redis reconnect or a lone dropped pub/sub connection doesn't count.
    A lost host, whose socket Redis keeps, still waits for the heartbeat
  - New key `<key>:presence`

### Changed

- `Close` requeues the items the queue object dequeued and has not
  acknowledged at once, and a distributed queue wakes another consumer for
  them. Stop or drain workers before `Close`, or jobs still running run again
  elsewhere. `Close` can now return an error from that requeue
- The sweep requeues items whose visibility deadline passed even on a queue
  object whose own visibility timeout is 0, and pushes an item back before
  removing it from pending
- The varmq consumer example stops its worker and closes the queue on
  SIGINT/SIGTERM
- A distributed consumer busy with work it popped no longer rejoins the idle
  set within a second of the queue emptying, so an enqueue wakes a free
  consumer at once instead of the busy one, which ignored the wake while the
  free one waited up to a second for its nudge. A consumer is idle again once
  its own `Len()` or dequeue finds the queue empty, so one that takes a
  single item per wake and never looks again is now only nudged

- Upgraded go-redis from v9.7.3 to v9.23.0, which requires Go 1.26
- `New` keeps go-redis 9.7's client timings (3s read and write timeouts, 512ms
  max retry backoff, one dial attempt) unless the URL sets them, so the
  documented re-send window stays at about 4 minutes
- Behaviour that comes with go-redis v9.23.0:
  - A pool timeout on any call, and a dial timeout on a single command or
    script, is now retried: such a call to a Redis host that drops packets
    can take about 20s to fail instead of about 5s. Pipelines already
    retried dial timeouts
  - Pipelines (`Enqueue` with an expiration, `Acknowledge`) use a separate
    pool of up to 10 connections; add `pipeline_pool_size=-1` to the URL to
    turn it off
  - Maintenance notifications are on by default. Open-source Redis rejects
    them once; on Redis Cloud and Redis Software timeouts are relaxed to 10s
    during maintenance
  - A zero duration with a unit in the URL no longer means the default:
    `read_timeout=0s`, `write_timeout=0s` and `max_retry_backoff=0s` turn
    it off (as `read_timeout=0` already did), and `dial_timeout=0s` or
    `pool_timeout=0s` makes that wait fail at once. Leave the parameter out
    to get the default
- CI takes its Go version from `go.mod`

## [1.4.0] - 2026-01-29

### Added

- Visibility timeout for reliable message processing
  - `SetVisibilityTimeout` - Configure how long items remain invisible after dequeue
  - Items automatically become visible again if not acknowledged within timeout
  - Time-based requeuing of unacknowledged items using Redis sorted sets
- Dockertest integration for automated Redis container management during tests
- Makefile with `test`, `format`, and `release` targets
- Example docker compose configuration in `examples/varmq/`

### Changed

- Tests no longer require a pre-running Redis server
- Simplified CI workflow by removing Redis service dependency
- Updated README with new testing instructions
- Improved README author section formatting

### Removed

- Removed `godotenv` dependency (no longer needed)
- Removed Redis service configuration from GitHub Actions workflow
- Removed example `.env` file (no longer required)

## [1.3.1] - 2025-05-01

### Changed

- Moved repository from fahimfaisaal to goptics organization

## [1.3.0] - 2025-04-30

### Added

`DequeueWithAckID` - Dequeue items with automatic acknowledgment ID assignment

## [1.2.0] - 2025-04-26

### Changed

- Streamlined notification callback function signature to only include action parameter
- Made notification system more lightweight and easier to use
- Introduced `Remove` method to remove specific items from the queue

## [1.1.0] - 2025-04-24

### Added

- Acknowledgment system for reliable queue processing:
  - `PrepareForFutureAck` - Add items to a separate pending hash with acknowledgment ID
  - `Acknowledge` - Confirm successful processing of items
  - `RequeueNackedItems` - Move unacknowledged items back to front of queue (FIFO)
  - `SetAckTimeout` - Configure timeout for acknowledgment processing
  - `GetNackedItemsCount` - Retrieve count of pending items awaiting acknowledgment
- Updated documentation with acknowledgment feature examples

### Changed

- Made `RequeueNackedItems` method public for manual requeuing of unacknowledged items
- Enhanced queue system attempt to requeue unacknowledged items
- Improved thread safety for high concurrency acknowledgment processing

## [1.0.0] - 2025-03-25

### Added

- Priority Queue implementation with Redis sorted sets
- Distributed Priority Queue with real-time notifications
- Comprehensive test coverage for priority queue operations
- Priority-based message ordering
- Documentation for priority queue usage

### Changed

- Renamed `Clear` method to `Purge` for better semantic clarity
- Improved error handling in queue operations
- Enhanced documentation with more detailed examples
- Updated Go module dependencies to latest versions
- Optimized Redis operations for better performance

## [0.1.0] - 2025-03-24

### Added

- Initial release of Redisq
- Thread-safe queue implementation with Redis backend
- Simple queue operations:
  - `Enqueue` - Add items to queue (supports string and []byte)
  - `Dequeue` - Remove and return items from queue
  - `Len` - Get queue length
  - `Values` - Get all queue values
  - `Clear` - Remove all items from queue
  - `Close` - Gracefully close queue
- Distributed queue with real-time notifications:
  - Pub/Sub notification system
  - Event broadcasting for queue operations
  - Support for multiple subscribers
  - Automatic notification on enqueue/dequeue
- Queue item expiration support
- Graceful shutdown handling
- Environment-based configuration
- Comprehensive test suite with race condition detection
- CI/CD pipeline with GitHub Actions
- Redis connection support:
  - Standard Redis URL format
  - Password authentication
  - Database selection
  - Username support

### Security

- Thread-safe operations with mutex locks
- Graceful handling of Redis connection errors
- Proper cleanup of resources on shutdown

[1.0.0]: https://github.com/goptics/redisq/compare/v0.1.0...v1.0.0
[0.1.0]: https://github.com/goptics/redisq/releases/tag/v0.1.0
[1.1.0]: https://github.com/goptics/redisq/compare/v1.0.0...v1.1.0
[1.2.0]: https://github.com/goptics/redisq/compare/v1.1.0...v1.2.0
[1.3.0]: https://github.com/goptics/redisq/compare/v1.2.0...v1.3.0
[1.3.1]: https://github.com/goptics/redisq/compare/v1.3.0...v1.3.1
[1.4.0]: https://github.com/goptics/redisq/compare/v1.3.1...v1.4.0

