# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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

[1.0.0]: https://github.com/fahimfaisaal/redisq/compare/v0.1.0...v1.0.0
[0.1.0]: https://github.com/fahimfaisaal/redisq/releases/tag/v0.1.0
