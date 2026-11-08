# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Changed

- **Breaking:** every key and channel is now `redisq:[ns:]fifo:{key}` or `redisq:[ns:]prio:{key}` plus its suffix, so a FIFO and a priority queue of one name no longer collide and a queue's keys share a Redis Cluster slot. Keys written by 1.5.x and earlier are not read: drain those queues before upgrading
- Queue keys must be non-empty and free of `{` and `}`; a constructor panics otherwise
- **Behavior change:** `Enqueue` / `EnqueueOnce` now refuse (`false`) JSON Go reads but Redis Lua cjson can't: nesting over 1000, lone UTF-16 surrogate escape (`\ud800`). 1.5.x stored such items and returned `true`. Else the pop's cancel gate misses its id: uncancellable, no outcome. Applies to every item, id or not
- **Breaking:** one queue kind per order: `Queue` and `PriorityQueue` now do all `DistributedQueue` / `DistributedPriorityQueue` did. Every `Enqueue`, `EnqueueOnce` and requeue wakes idle consumers (`<key>:idle`, `<key>:wake:<id>`); `Subscribe` makes an object a consumer; a consumer's empty pop or `Len` joins the idle set; `Close` leaves it. One pub/sub connection per object, as before: wake, control and outcome channels. `dq.Queue` and `dpq.PriorityQueue` no longer compile (aliases, no such embedded field): call the method on the handle directly. Key layout unchanged. Mixed versions fine: older plain consumers never join `:idle`, keep polling

### Added

- `New` takes options; `WithNamespace(ns)` scopes keys under `redisq:ns:`
- Job records and stats, as varmq's `spec/redis-storage.md` 1.0.0: `Acknowledge` (now one script) and the new `Reject(ackID, reason)` count the job in `:stats`, keep its Job Record in `:jobs` and `:completed`/`:failed`, and trim by count and age, in one atomic step; every enqueue counts as submitted, except an `EnqueueOnce` repeat (#29)
- `SetCompletedRetention` and `SetFailedRetention`: by default the last 1,000 completed jobs for 24h and the last 10,000 failed jobs for 7 days; 0 keeps none. The sweep also trims once a minute (#29)
- Both queue types (and the deprecated `Distributed*` aliases) implement `contract.QueueInspector` (`QueueID`, `QueueStats`, `Jobs`) from `github.com/goptics/varmq/contract` (#29)
- New keys `<key>:started`, `<key>:attempts`, `<key>:jobs`, `<key>:completed`, `<key>:failed`, `<key>:stats`; pops record start times and requeues count attempts (#29)
- `WithInstanceReports(interval)`: `New` reports this process (from varmq: worker names, status, concurrency, bound Queue IDs) to `redisq:[ns:]{instances}` / `:beats` every 1-60s, as varmq's `spec/redis-storage.md` 1.1.0 section 8; `Close` removes the record. The value `New` returns also implements `contract.InstanceRegistry` (type-assert; `Queues` itself is unchanged) (`ReportInstance`, `RemoveInstance`, `Instances`); `Instances` writes nothing (an ACL user needs `+time +zrange +hgetall +multi +exec`). Needs `github.com/goptics/varmq/contract` v0.2.0
- Queue pause, as varmq's `spec/redis-storage.md` 1.3.0: every queue type implements varmq's `QueuePauser` and `PauseNotifier` (`Pause`, `Resume`, `Paused`, `OnPauseChange`, and `PauseRefusals`, so varmq never reports a refused pop as a failure). `Pause` sets `<key>:paused` (pause time in ms, no expiry); while it exists every pop, plain or acknowledged, one item or a batch, hands out nothing, in every process, after the re-send guard. Adds, acks, rejects and requeues go on. `Resume` deletes it and wakes idle consumers. Each queue object caches the flag for `Paused` from its own calls, pop replies, the `<key>:control` channel (on the subscription it already holds) and the 1s sweep, so a lost message costs at most a second and a new object starts with the flag. `QueueStats` reports `paused` and `paused_at`. `Purge` and `SetExpiration` keep the flag. A queue is paused only once every consumer runs this version and varmq with queue pause: older consumers ignore the key, and an older varmq on this version keeps asking a paused queue for work. Needs `github.com/goptics/varmq/contract` v0.4.0
- `Dequeue` (no ack) is now a script on both FIFO and priority queues, to check the pause
- Job outcome signal, as varmq's `spec/redis-storage.md` 1.6.0: `Acknowledge`, `AcknowledgeResult` and `Reject` publish a `contract.JobOutcome` JSON (`id`, `state`, failed: `error`, result: `result`) on `<key>:done:<job id>` in the same script, after the record step, record kept or not; only for jobs with an envelope id
- `WatchOutcome(id, fn)` on both kinds (`contract.OutcomeWatcher`): fn once, own goroutine, when the job ends in any process. First watcher of an id subscribes its channel on the object's pub/sub connection; the watch ends by itself after fn, last one unsubscribes. Catch-up from the Job Record right after subscribing and every sweep (up to 500 ids per second), so a job ended before the watch, or a lost message, still reports while its record is kept. `cancel` stops early; no-op after fn. `Close` drops watchers uncalled (a delivery already under way may still call fn)
- `EnqueueOnce` that pushes (not a dedupe hit) drops the id's old Job Record (`:jobs`, `:completed`, `:failed`) in the same script: a reused id never reports the previous run's outcome or result to a new watch
- `SetDedupWindow(<=0)` turns off only the dedupe: `EnqueueOnce` with an id still drops its old record and cancel mark (was a plain `Enqueue`)
- `AcknowledgeResult(ackID, result)` on both kinds (`contract.ResultAcknowledgeable`): `Acknowledge` plus result JSON, spliced raw (big integers intact) into the outcome and the completed record's new optional `result` field. Invalid JSON: `false`, nothing changes. No size limit here (varmq caps at 64 KiB)
- Job cancel, as varmq's `spec/redis-storage.md` 1.7.0 section 6.14, on both kinds (`contract.JobCanceller`, `CancelNotifier`, `CancelAcknowledgeable`): `CancelJob(id)` one script: record ended (`completed`/`failed`/`cancelled`) -> `contract.ErrJobEnded`, nothing changes; else prune expired marks, `ZADD <key>:cancelled <now+24h ms> id` (per-mark expiry as score; key `PEXPIRE` 24h only a backstop) and `PUBLISH <key>:control cancel:<id>`. Empty id: error. Unknown ids get marked too: authorize and rate-limit untrusted callers
- Every pop (raw, single, batch; FIFO and priority), after the pause gate: when `<key>:cancelled` exists, an item whose envelope id has a live mark (score > now) is never handed out: mark dropped, record `cancelled` (no `started_at`, no `errors`) in `:jobs` + `:failed` under failed retention, outcome `{"state":"cancelled","error":"cancelled"}` published, no `:stats` count; the pop takes the next item instead. Expired mark: removed, item handed out. At most 1000 drops per script call (one `:failed` trim after them): a pop that runs out returns what it has, stays out of `:idle` and reports the rest (`remaining`); every pop call (raw, single, batch) retries at once with the same ack IDs, while each run shrinks `remaining`, so a cancelled backlog alone does not make a batch call return 0 items. No key: one `EXISTS` more, nothing else. Items without an envelope id always go out. A valid workflow item (spec 3: `wf` a workflow ID, `task` a task name, `undo` absent or a bool, id `wf:<wf>:<task>[:undo]`) is handed out and keeps its mark: it runs unless its workflow is cancelled. Any other `wf` field (crafted, null, mismatched id) is checked as a plain job
- `OnCancel(fn)`: fn per `cancel:<id>` control message, own goroutine per call (off the pub/sub reader, no order); `stop` removes it. Stale message dropped: fn only while `ZSCORE <key>:cancelled <id>` > now (Redis clock), so a finish or re-add (`ZREM`) before delivery calls nothing; check error: fn called anyway. No callbacks: no check
- `AcknowledgeCancelled(ackID, reason)`: as `Reject` (counted failed, failed retention, no requeue), record and outcome state `cancelled`, `error` the reason
- `Acknowledge`, `AcknowledgeResult`, `Reject` and a pushing `EnqueueOnce` drop the id's cancel mark (`ZREM`); the 1s sweep prunes expired marks; `Purge` deletes `<key>:cancelled`. `WatchOutcome` takes `cancelled` as final (record without `errors`: error `cancelled`)
- Workflows, as varmq's `spec/redis-storage.md` 1.5.0 (6.9 to 6.12, 7.3): every queue implements `contract.WorkflowAcknowledgeable` (`ScopeID`, `WorkflowStore`, `FinishTask`); `WorkflowStore()` is the store of the queue's Redis and namespace (`contract.WorkflowStore`, `WorkflowInspector`, `WorkflowKeyLister`), so varmq workers in any process run one DAG across redisq queues. Keys `redisq:[ns:]wf:<id>` (hash: `meta`, `t:`/`s:`/`n:`/`i:`/`r:`/`e:`/`u:` per task) and `redisq:[ns:]wf:index`. Submit, FinishTask, Cancel and Purge are one script each: FinishTask settles the item as `Acknowledge`/`Reject` (record, stats, outcome) and pushes what the end releases in the same step; a re-sent finish changes nothing and names every push-target queue. Results over the limit fail the task; undo envelopes are pushed byte for byte. Workflow keys have no hashtag, so workflows need one Redis (no Cluster), as the spec says. `WithWorkflowResultLimit(n)` (default 64 KiB) and `WithWorkflowRetention(d)` (default 7 days) set the store; the sweep trims `wf:index` once a minute. Every consumer of a queue that carries workflow tasks must run this version (spec 9). Submit refuses items Redis's cjson can't decode (nesting over 1000, lone surrogate escapes); FinishTask applies a workflow step only when the settled item's own tag names the end's task; an unreadable workflow hash settles items alone. Anyone who can push to a task's queue can push a copy of the task: treat push access as trusted (the pre-run check has no queue to compare; an additive contract interface can add one). Needs `github.com/goptics/varmq/contract` v0.6.0
- `examples/workflow`: one workflow over four queues, served by two worker processes
- `Purge` deletes all of a queue's jobs and job data: waiting and processing items, deadlines, start times, attempts, records, counters, and a priority queue's sequence and priorities. Consumer state and `EnqueueOnce` ids stay (#29). A purged workflow task or undo ends `cancelled` in the same script, so its workflow goes on (spec 6.6)

### Deprecated

- `DistributedQueue`, `DistributedPriorityQueue`: now aliases of `Queue`, `PriorityQueue`. `NewDistributedQueue`, `NewDistributedPriorityQueue` return `*Queue`, `*PriorityQueue` (same as `NewQueue`, `NewPriorityQueue`)

## [1.5.1] - 2026-10-31

### Added

- `Queue` and `PriorityQueue` subscribe to their wake channel as distributed queues do, so one whose process exits without `Close` has its items requeued in about 3-4s instead of after the 30s heartbeat timeout, and a restarted process gets back the jobs its crashed run held about 3s after it starts; a live process's jobs stay put, as no blind requeue runs on start (#26)

### Changed

- A queue object counts as gone once its subscription has been missing 2.5s with no sweep of its own, down from 5s: a dead distributed consumer's items come back in about 3-4s instead of 6s (#26)
- A new queue object sweeps once at construction, as well as every second after; this replaces its startup `RequeueNackedItems` (#26)
- Every queue object holds one pub/sub connection; distributed ones share it with their `Notification` instead of opening a second (#26)

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

[Unreleased]: https://github.com/goptics/redisq/compare/v1.5.1...HEAD
[1.0.0]: https://github.com/goptics/redisq/compare/v0.1.0...v1.0.0
[0.1.0]: https://github.com/goptics/redisq/releases/tag/v0.1.0
[1.1.0]: https://github.com/goptics/redisq/compare/v1.0.0...v1.1.0
[1.2.0]: https://github.com/goptics/redisq/compare/v1.1.0...v1.2.0
[1.3.0]: https://github.com/goptics/redisq/compare/v1.2.0...v1.3.0
[1.3.1]: https://github.com/goptics/redisq/compare/v1.3.0...v1.3.1
[1.4.0]: https://github.com/goptics/redisq/compare/v1.3.1...v1.4.0
[1.5.0]: https://github.com/goptics/redisq/compare/v1.4.0...v1.5.0
[1.5.1]: https://github.com/goptics/redisq/compare/v1.5.0...v1.5.1
