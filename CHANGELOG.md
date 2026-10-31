# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- `Queue` and `PriorityQueue` subscribe to their wake channel as distributed queues do, so one whose process exits without `Close` has its items requeued in about 3-4s instead of after the 30s heartbeat timeout, and a restarted process gets back the jobs its crashed run held about 3s after it starts; a live process's jobs stay put, as no blind requeue runs on start

### Changed

- A queue object counts as gone once its subscription has been missing 2.5s with no sweep of its own, down from 5s: a dead distributed consumer's items come back in about 3-4s instead of 6s
- A new queue object sweeps once at construction, as well as every second after; this replaces its startup `RequeueNackedItems`
- Every queue object holds one pub/sub connection; distributed ones share it with their `Notification` instead of opening a second

## [1.5.0] - 2026-10-24

### Added

- `DequeueBatchWithAckIds` pops up to 1000 items in one retry-safe round trip, on all four queue types (#14)
- `DequeueBatchWithAckIds` sizes each call by recent runs, so it may return fewer items than are queued with `remaining > 0` (#15)
- `EnqueueOnce` stores a job once per id within a dedup window; `SetDedupWindow` sets it, 5m by default (#18)
- `SetHeartbeatTimeout`: how long a consumer may go without reaching Redis before its un-acked items are requeued, 30s by default (#21)
- A distributed consumer that exits without `Close` has its items requeued in about 6s, once its pub/sub subscription has been gone 5s (#21)
- New keys `<key>:idle`, `<key>:seq`, `<key>:prio`, `<key>:dedup:<id>`, `<key>:heartbeats`, `<key>:heartbeats:clock`, `<key>:presence` and pub/sub channels `<key>:wake:<id>` (#5, #8, #11, #18, #21)
- `make diagrams` regenerates the animated README diagrams and checks the behaviour they draw (#22)

### Changed

- **Breaking:** an enqueue wakes up to one idle consumer per waiting item, each on its own channel, instead of broadcasting to all; upgrade consumers before producers (#11)
- **Breaking:** heartbeats are on by default: a consumer frozen or cut off from Redis for over 30s has its running jobs requeued, and its late `Acknowledge` still returns true (#21)
- **Breaking:** `Close` requeues the items the object popped with an ack ID and has not acknowledged, and can return an error; stop workers before `Close` (#21)
- **Breaking:** priority queue members carry a sequence prefix; upgrade every consumer of a priority queue together (#5)
- **Breaking:** Redis must allow Lua scripts (`EVALSHA`); pops, pushes, the sweep and `Close` run as scripts (#3, #5, #7, #21)
- **Breaking:** requires Go 1.26, for go-redis v9.23.0 (#20)
- On a distributed consumer, a `Len()` of 0 or an empty dequeue joins the idle set (#11)
- A busy distributed consumer stays out of the idle set, so wakes go to free ones; one that stops pulling before the queue empties waits for the nudge (#23)
- Each distributed consumer runs a 1s nudge: while items wait, its `Subscribe` handlers get `"enqueued"` even with no new enqueue (#11)
- The sweep and `Close` of a distributed queue wake up to one idle consumer per item they requeue (#7, #11, #21)
- varmq's `Submitted` metric counts wakes, not jobs (#11)
- Ack IDs look like `<prefix>:<reservation>:<index>` instead of a cuid (#14)
- `PriorityQueue.Remove` scans the queue, O(n), to match a member's sequence prefix (#5)
- Queue methods no longer hold a mutex across Redis calls (#13)
- `Notification.Start` does nothing after its first call (#11)
- Upgraded go-redis from v9.7.3 to v9.23.0; `New` keeps 9.7's 3s read timeout and 512ms max retry backoff unless the URL sets them, and dials once (#20)
- go-redis v9.23.0 retries pool and dial timeouts, so a call to a host that drops packets can take about 20s to fail (#20)
- Pipelines use a separate pool of up to 10 connections; `pipeline_pool_size=-1` in the URL turns it off (#20)
- Maintenance notifications are on by default; go-redis relaxes its timeouts to 10s while Redis Cloud or Redis Software signals maintenance (#20)
- A zero duration with a unit in the URL (`read_timeout=0s`) turns that timeout off instead of meaning the default (#20)
- README rewritten, with animated diagrams of acks, wakes, `Close` and crash recovery (#19, #22)
- The varmq consumer example stops its worker and closes the queue on SIGINT/SIGTERM (#21)
- CI also runs the tests on Redis 6.0, the oldest supported version (#16)

### Deprecated

- `SetAckTimeout` is an alias of `SetVisibilityTimeout`, and a value of 0 or less is ignored (#4)

### Removed

- **Breaking:** `Notification.Send`, the `"dequeued"` notification and the shared `<key>:notifications` channel (#11)
- The `github.com/lucsky/cuid` dependency; IDs come from `crypto/rand` (#18)

### Fixed

- A client that dies mid-`DequeueWithAckId` no longer loses the job: pop and pending bookkeeping run in one script (#3)
- A go-redis re-send of `DequeueWithAckId` no longer loses a job, and a failed call's items come back on the next ack pop with room for them (#12)
- `PrepareForFutureAck` runs atomically (#12)
- The ack timeout requeues pending jobs instead of deleting them (#4)
- Expired items are requeued every second while running, not only when a queue is created; each queue object sweeps until `Close` (#7, #21)
- Visibility deadlines keep sub-second precision, so a job is no longer requeued up to 1s early (#7)
- Visibility deadlines come from Redis `TIME` when the pop runs, so a delayed pop or a skewed client clock no longer gets an already-expired item (#16)
- A priority queue keeps identical jobs, and FIFO order within a priority (#5)
- Concurrent requeues move each expired item exactly once (#6)
- Priority jobs are requeued at their original priority (#8)
- A failed priority enqueue no longer publishes `"enqueued"` (#9)
- `Listen` stops signal delivery when it returns (#10)
- Containers started together with the same pid and host can no longer get colliding ack IDs (#18)

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

[Unreleased]: https://github.com/goptics/redisq/compare/v1.5.0...HEAD
[1.0.0]: https://github.com/goptics/redisq/compare/v0.1.0...v1.0.0
[0.1.0]: https://github.com/goptics/redisq/releases/tag/v0.1.0
[1.1.0]: https://github.com/goptics/redisq/compare/v1.0.0...v1.1.0
[1.2.0]: https://github.com/goptics/redisq/compare/v1.1.0...v1.2.0
[1.3.0]: https://github.com/goptics/redisq/compare/v1.2.0...v1.3.0
[1.3.1]: https://github.com/goptics/redisq/compare/v1.3.0...v1.3.1
[1.4.0]: https://github.com/goptics/redisq/compare/v1.3.1...v1.4.0
[1.5.0]: https://github.com/goptics/redisq/compare/v1.4.0...v1.5.0

