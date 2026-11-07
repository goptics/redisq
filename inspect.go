package redisq

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
)

var (
	_ contract.QueueInspector = (*Queue)(nil)
	_ contract.QueueInspector = (*PriorityQueue)(nil)
	_ contract.QueueInspector = (*DistributedQueue)(nil)
	_ contract.QueueInspector = (*DistributedPriorityQueue)(nil)
)

// QueueID is the queue's base key, redisq:[ns:]fifo|prio:{key}, the same on
// every instance (see spec/redis-storage.md 1.1).
func (q *Queue) QueueID() string {
	return q.queueKey
}

// QueueStats returns the queue's counts, totals and pause, read in one
// MULTI. It reads the pause from Redis, not this object's cache.
func (q *Queue) QueueStats(ctx context.Context) (contract.QueueStats, error) {
	var pending *redis.IntCmd
	var processing, completed, failed *redis.IntCmd
	var totals *redis.SliceCmd
	var paused *redis.StringCmd
	_, err := q.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		if q.isPriority {
			pending = p.ZCard(ctx, q.queueKey)
		} else {
			pending = p.LLen(ctx, q.queueKey)
		}
		processing = p.HLen(ctx, q.getNackedItemKey())
		completed = p.ZCard(ctx, q.getCompletedKey())
		failed = p.ZCard(ctx, q.getFailedKey())
		totals = p.HMGet(ctx, q.getStatsKey(), "submitted", "completed", "failed")
		paused = p.Get(ctx, q.getPausedKey())
		return nil
	})
	if err != nil && err != redis.Nil { // Nil: :paused is absent
		return contract.QueueStats{}, err
	}

	t := make([]uint64, 3)
	for i, v := range totals.Val() {
		if s, ok := v.(string); ok {
			t[i], _ = strconv.ParseUint(s, 10, 64)
		}
	}
	s := contract.QueueStats{
		Counts: contract.JobCounts{Pending: pending.Val(), Processing: processing.Val(), Completed: completed.Val(), Failed: failed.Val()},
		Totals: contract.JobTotals{Submitted: t[0], Completed: t[1], Failed: t[2]},
	}
	if at, err := paused.Result(); err == nil {
		s.Paused = true
		s.PausedAt, _ = strconv.ParseInt(at, 10, 64) // not an integer: no paused_at (spec 7.1)
	}
	return s, nil
}

// jobsScript reads one page of a Job State (spec/redis-storage.md 7.2) in one
// step. KEYS: queue, nacked, started, attempts, jobs, completed, failed.
// ARGV: state, start, stop, 1 for a zset queue.
//
// completed, failed: the records, newest first (false for one a trim took
// between the two reads). pending, processing: {job ID, item, started ms
// (0: unknown), attempts} per job; processing is sorted by ack ID, which
// sorts each owner's jobs in hand-out order.
// ponytail: a processing page reads every ack ID (HKEYS); index them if
// processing runs to millions.
var jobsScript = redis.NewScript(envIDFn + `local st, a, b = ARGV[1], tonumber(ARGV[2]), tonumber(ARGV[3])
if st == 'completed' or st == 'failed' then
	local ids = redis.call('ZREVRANGE', st == 'completed' and KEYS[6] or KEYS[7], a, b)
	if #ids == 0 then return {} end
	return redis.call('HMGET', KEYS[5], unpack(ids))
end
local out = {}
if st == 'pending' then
	local items
	if ARGV[4] == '1' then
		items = redis.call('ZRANGE', KEYS[1], a, b)
		for i, m in ipairs(items) do
			if #m >= 17 and string.byte(m, 1) == 255 then items[i] = string.sub(m, 18) end
		end
	else
		items = redis.call('LRANGE', KEYS[1], a, b)
	end
	for i, v in ipairs(items) do
		local id = envid(v)
		out[i] = {id, v, 0, id ~= '' and tonumber(redis.call('HGET', KEYS[4], id)) or 0}
	end
	return out
end
local acks = redis.call('HKEYS', KEYS[2])
table.sort(acks)
for i = a + 1, math.min(b + 1, #acks) do
	local ack = acks[i]
	local v = redis.call('HGET', KEYS[2], ack)
	local id, n = envid(v), 0
	if id ~= '' then n = tonumber(redis.call('HGET', KEYS[4], id)) or 0 else id = ack end
	out[#out + 1] = {id, v, tonumber(redis.call('HGET', KEYS[3], ack)) or 0, n + 1}
end
return out`)

// Jobs returns up to limit jobs in state, skipping the first offset: pending
// in hand-out order, processing by ack ID, completed and failed newest
// first. A record that doesn't parse is left out.
func (q *Queue) Jobs(ctx context.Context, state contract.JobState, offset, limit int) ([]contract.JobRecord, error) {
	switch state {
	case contract.JobPending, contract.JobProcessing, contract.JobCompleted, contract.JobFailed:
	default:
		return nil, fmt.Errorf("redisq: unknown job state %q", state)
	}
	jobs := []contract.JobRecord{}
	if limit <= 0 {
		return jobs, nil
	}
	offset = max(0, offset)

	prio := "0"
	if q.isPriority {
		prio = "1"
	}
	keys := []string{q.queueKey, q.getNackedItemKey(), q.getStartedKey(), q.getAttemptsKey(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey()}
	rows, err := jobsScript.Run(ctx, q.client, keys, string(state), offset, offset+limit-1, prio).Slice()
	if err != nil {
		return nil, err
	}

	for _, row := range rows {
		if state == contract.JobCompleted || state == contract.JobFailed {
			s, ok := row.(string)
			if !ok {
				continue // trimmed between the two reads
			}
			var rec struct {
				ID         string              `json:"id"`
				State      contract.JobState   `json:"state"`
				Item       json.RawMessage     `json:"item"`
				StartedAt  int64               `json:"started_at"`
				FinishedAt int64               `json:"finished_at"`
				Attempts   int                 `json:"attempts"`
				Errors     []contract.JobError `json:"errors"`
			}
			if json.Unmarshal([]byte(s), &rec) != nil {
				continue
			}
			j := contract.JobRecord{ID: rec.ID, State: rec.State, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt, Attempts: rec.Attempts, Errors: rec.Errors}
			j.Payload, j.EnqueuedAt = unwrap(rec.Item)
			jobs = append(jobs, j)
			continue
		}

		r := row.([]any)
		j := contract.JobRecord{ID: r[0].(string), State: state, StartedAt: r[2].(int64), Attempts: int(r[3].(int64))}
		j.Payload, j.EnqueuedAt = unwrap([]byte(r[1].(string)))
		jobs = append(jobs, j)
	}
	return jobs, nil
}

// unwrap returns the payload and enqueue time of item: an envelope's data
// and enqueued_at, or for anything else the item itself if it is JSON, else
// the item as a JSON string. The bytes are kept as stored, never re-encoded.
func unwrap(item []byte) (json.RawMessage, int64) {
	var env struct {
		Status     json.RawMessage `json:"status"`
		Data       json.RawMessage `json:"data"`
		EnqueuedAt json.RawMessage `json:"enqueued_at"`
	}
	// An envelope has status and data (spec section 3). A field is nil only
	// when its key is absent: a JSON null still sets it to "null".
	if bytes.HasPrefix(bytes.TrimSpace(item), []byte("{")) && json.Unmarshal(item, &env) == nil && env.Status != nil && env.Data != nil {
		var at int64
		_ = json.Unmarshal(env.EnqueuedAt, &at)
		return env.Data, at
	}
	if json.Valid(item) {
		return item, 0
	}
	s, _ := json.Marshal(string(item))
	return s, 0
}
