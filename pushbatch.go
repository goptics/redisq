package redisq

import (
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// FIFO pushes go through a group commit (see group): Enqueue calls of one
// Queue object that overlap run as one listPushWakeN call, EnqueueOnce
// calls as one listPushOnceN call. Each caller returns after Redis ran its
// push; one failed call fails all of its pushes.

// maxPushBatch caps the items of one push call.
const maxPushBatch = 256

// oncePush is one EnqueueOnce item.
type oncePush struct {
	id   string
	data []byte
}

// listPushWakeN is listPushWake for many items: KEYS queue, idle, stats;
// ARGV expiration (ms, 0: none), wake channel prefix, then the items, pushed
// in order with one RPUSH.
var listPushWakeN = redis.NewScript(wakeFn + `local n = redis.call('RPUSH', KEYS[1], unpack(ARGV, 3))
if tonumber(ARGV[1]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
redis.call('HINCRBY', KEYS[3], 'submitted', #ARGV - 2)
return wake(KEYS[2], ARGV[2], n)`)

// listPushOnceN is listPushWakeOnce for many items, same outcome as running
// them one by one in ARGV order (see once): KEYS queue, idle, stats,
// cancelled, jobs, completed, failed, then each item's dedupe key; ARGV
// expiration (ms), wake channel prefix, dedupe window (ms, 0: none), then id
// and item per push. A later item of an id that an earlier one of the call
// pushed is a dedupe repeat (window above 0). Dedupe keys are set after the
// push.
var listPushOnceN = redis.NewScript(wakeFn + `local win = tonumber(ARGV[3])
local m = (#ARGV - 3) / 2
local seen = {}
local hit
if win > 0 then hit = redis.call('MGET', unpack(KEYS, 8)) end
local push, ids, dks = {}, {}, {}
for i = 1, m do
	local id = ARGV[2*i + 2]
	if not (win > 0 and (hit[i] or seen[id])) then
		seen[id] = true
		push[#push + 1] = ARGV[2*i + 3]; ids[#ids + 1] = id; dks[#dks + 1] = KEYS[7 + i]
	end
end
if #push == 0 then return 0 end
local n = redis.call('RPUSH', KEYS[1], unpack(push))
if tonumber(ARGV[1]) > 0 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
redis.call('HINCRBY', KEYS[3], 'submitted', #push)
if win > 0 then
	for _, k in ipairs(dks) do redis.call('SET', k, '1', 'PX', ARGV[3]) end
end
redis.call('HDEL', KEYS[5], unpack(ids))
redis.call('ZREM', KEYS[6], unpack(ids))
redis.call('ZREM', KEYS[7], unpack(ids))
redis.call('ZREM', KEYS[4], unpack(ids))
return wake(KEYS[2], ARGV[2], n)`)

// pushBatch runs listPushWakeN for batch.
func (q *Queue) pushBatch(batch []*groupReq[[]byte]) {
	args := make([]any, 0, 2+len(batch))
	args = append(args, q.getExpiration().Milliseconds(), q.wakePrefix)
	for _, r := range batch {
		args = append(args, r.v)
	}
	err := listPushWakeN.Run(q.ctx, q.client, q.pushKeys(), args...).Err()
	if err != nil {
		log.Printf("Error enqueueing %d item(s): %v", len(batch), err)
	}
	for _, r := range batch {
		r.ok = err == nil
	}
}

// pushOnceBatch runs listPushOnceN for batch. A window under 1ms counts as
// 1ms; none as 0.
func (q *Queue) pushOnceBatch(batch []*groupReq[oncePush]) {
	window := int64(0)
	if d := time.Duration(q.dedupWindow.Load()); d > 0 {
		window = max(1, d.Milliseconds())
	}
	keys := make([]string, 0, 7+len(batch))
	keys = append(keys, q.queueKey, q.getIdleKey(), q.getStatsKey(), q.getCancelledKey(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey())
	args := make([]any, 0, 3+2*len(batch))
	args = append(args, q.getExpiration().Milliseconds(), q.wakePrefix, window)
	for _, r := range batch {
		keys = append(keys, q.queueKey+":dedup:"+r.v.id)
		args = append(args, r.v.id, r.v.data)
	}
	err := listPushOnceN.Run(q.ctx, q.client, keys, args...).Err()
	if err != nil {
		log.Printf("Error enqueueing %d item(s): %v", len(batch), err)
	}
	for _, r := range batch {
		r.ok = err == nil
	}
}
