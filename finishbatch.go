package redisq

import (
	"log"
	"time"

	"github.com/redis/go-redis/v9"
)

// Finishes go through a group commit (see group): Acknowledge,
// AcknowledgeResult, Reject and AcknowledgeCancelled of one Queue object that
// overlap run as one settleBatch call. Each caller still returns only after
// Redis ran its finish.

// maxFinishBatch caps the finishes of one settleBatch call (ARGV size,
// unpack limit, script length).
const maxFinishBatch = 256

type finishReq struct {
	ackID, state, reason string
	result               []byte
}

// finish ends ackID's job in state (completed, failed or cancelled). A
// cancelled job is indexed, kept and counted as failed (spec 6.14: as Reject).
func (q *Queue) finish(ackID, state, reason string, result []byte) bool {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot acknowledge or reject")
		return false
	}
	return q.finishes.do(finishReq{ackID, state, reason, result}, maxFinishBatch, q.settle)
}

// settle runs settleBatch for batch.
func (q *Queue) settle(batch []*groupReq[finishReq]) {
	args := make([]any, 0, 5+4*len(batch))
	args = append(args,
		q.keepCompleted.Load(), time.Duration(q.keepCompletedAge.Load()).Milliseconds(),
		q.keepFailed.Load(), time.Duration(q.keepFailedAge.Load()).Milliseconds(),
		q.donePrefix())
	for _, r := range batch {
		args = append(args, r.v.ackID, r.v.state, r.v.reason, r.v.result)
	}
	keys := []string{q.getNackedItemKey(), q.getTimeoutKey(), q.getStartedKey(), q.getAttemptsKey(), q.getStatsKey(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey(), q.getPriorityKey(), q.getCancelledKey()}
	res, err := settleBatch.Run(q.ctx, q.client, keys, args...).Int64Slice()
	if err != nil {
		log.Printf("Error finishing %d item(s): %v", len(batch), err)
	}
	for i, r := range batch {
		r.ok = err == nil && i < len(res) && res[i] >= 0 // -1: result is not JSON
	}
}

// settleBatch is finishScript (spec/redis-storage.md 6.3, 6.4, 6.13, 6.14)
// for many finishes in one call, same outcome as running them one by one in
// ARGV order, with one clock reading for all.
//
// KEYS: nacked, timeouts, started, attempts, stats, jobs, completed, failed,
// prio, cancelled. ARGV: completed retention (most, age ms), failed
// retention (most, age ms), outcome channel prefix, then 4 per finish: ack
// ID, state (completed, failed or cancelled), reason, result JSON ("" for
// none). Returns one code per finish: 1 settled, 0 not processing (or an
// earlier finish of the batch took its ack ID), -1 result not JSON (that
// finish changes nothing).
//
// Reads (HMGET) and deletes (HDEL, ZREM) go in one command per key. Record
// writes are grouped by segment: a Job ID that shows up again in the batch
// starts a new segment, so its later finish lands on top of its earlier one
// as sequential calls would. Each state's set is trimmed once, at the end
// (inserts only, so the same set as a trim per finish). Outcomes are
// published last, after every record step.
var settleBatch = redis.NewScript(deadlineFn + envIDFn + asJSONFn + trimFn + recordFn + `local n = (#ARGV - 5) / 4
local kc, ac, kf, af = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local out, acks, seen, live = {}, {}, {}, {}
for i = 1, n do acks[i] = ARGV[4*i + 2] end
local items = redis.call('HMGET', KEYS[1], unpack(acks))
for i = 1, n do
	local r = ARGV[4*i + 5]
	if r ~= '' and not pcall(cjson.decode, r) then out[i] = -1
	elseif not items[i] or seen[acks[i]] then out[i] = 0
	else out[i] = 1; seen[acks[i]] = true; live[#live + 1] = i end
end
if #live == 0 then return out end
local la = {}
for k, i in ipairs(live) do la[k] = acks[i] end
local starts = redis.call('HMGET', KEYS[3], unpack(la))
redis.call('HDEL', KEYS[1], unpack(la))
redis.call('ZREM', KEYS[2], unpack(la))
redis.call('HDEL', KEYS[3], unpack(la))
redis.call('HDEL', KEYS[9], unpack(la))
local envs, decs, doks, eids = {}, {}, {}, {}
for k, i in ipairs(live) do
	envs[k], decs[k], doks[k] = envid(items[i])
	if envs[k] ~= '' then eids[#eids + 1] = envs[k] end
end
local att = {}
if #eids > 0 then
	local a = redis.call('HMGET', KEYS[4], unpack(eids))
	for j, id in ipairs(eids) do if att[id] == nil then att[id] = tonumber(a[j]) or 0 end end
	redis.call('HDEL', KEYS[4], unpack(eids))
end
local now = tonumber(nowms())
local nows = string.format('%.0f', now)
local ids, nc, nf, pubs = {}, 0, 0, {}
local seg, hs, zc, zf, rc, rf, hd = {}, {}, {}, {}, {}, {}, {}
local function flush()
	if #rf > 0 then redis.call('ZREM', KEYS[8], unpack(rf)) end
	if #rc > 0 then redis.call('ZREM', KEYS[7], unpack(rc)) end
	if #hs > 0 then redis.call('HSET', KEYS[6], unpack(hs)) end
	if #zc > 0 then redis.call('ZADD', KEYS[7], unpack(zc)) end
	if #zf > 0 then redis.call('ZADD', KEYS[8], unpack(zf)) end
	if #hd > 0 then redis.call('HDEL', KEYS[6], unpack(hd)) end
	seg, hs, zc, zf, rc, rf, hd = {}, {}, {}, {}, {}, {}, {}
end
for k, i in ipairs(live) do
	local st, env = ARGV[4*i + 3], envs[k]
	local id, attempts = env, 1
	if env ~= '' then attempts = 1 + att[env]; att[env] = 0 else id = acks[i] end
	ids[#ids + 1] = id
	if seg[id] then flush() end
	seg[id] = true
	local done = st == 'completed'
	local keep, age = kf, af
	if done then keep, age, nc = kc, ac, nc + 1 else nf = nf + 1 end
	local err = not done and ARGV[4*i + 4] or nil
	local r = ARGV[4*i + 5]
	local res = (done and r ~= '') and (',"result":' .. r) or ''
	if done then rf[#rf + 1] = id else rc[#rc + 1] = id end
	if keep > 0 and age > 0 then
		hs[#hs + 1] = id; hs[#hs + 1] = record(id, st, items[i], tonumber(starts[k]), now, attempts, err, res, doks[k], decs[k])
		local z = done and zc or zf
		z[#z + 1] = nows; z[#z + 1] = id
	else
		hd[#hd + 1] = id
	end
	if env ~= '' then pubs[#pubs + 1] = env; pubs[#pubs + 1] = outcome(env, st, err, res) end
end
flush()
redis.call('ZREM', KEYS[10], unpack(ids))
if nc > 0 then
	redis.call('HINCRBY', KEYS[5], 'completed', nc)
	trim(KEYS[7], KEYS[6], kc, ac, now)
end
if nf > 0 then
	redis.call('HINCRBY', KEYS[5], 'failed', nf)
	trim(KEYS[8], KEYS[6], kf, af, now)
end
for j = 1, #pubs, 2 do redis.call('PUBLISH', ARGV[5] .. pubs[j], pubs[j + 1]) end
return out`)
