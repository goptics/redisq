package redisq

import (
	"bytes"
	"encoding/json"
	"log"
	"sync"
	"time"
	"unicode/utf8"

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
	hint                 string // see itemHint; "" unknown
}

// finish ends ackID's job in state (completed, failed or cancelled). A
// cancelled job is indexed, kept and counted as failed (spec 6.14: as Reject).
func (q *Queue) finish(ackID, state, reason string, result []byte) bool {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot acknowledge or reject")
		return false
	}
	return q.finishes.do(finishReq{ackID, state, reason, result, q.hints.take(ackID)}, len(reason)+len(result), maxFinishBatch, q.settle)
}

// settle runs settleBatch for batch, settleOne for a lone finish.
func (q *Queue) settle(batch []*groupReq[finishReq]) {
	if len(batch) == 1 {
		q.settleLone(batch[0])
		return
	}
	args := make([]any, 0, 5+5*len(batch))
	args = append(args,
		q.keepCompleted.Load(), time.Duration(q.keepCompletedAge.Load()).Milliseconds(),
		q.keepFailed.Load(), time.Duration(q.keepFailedAge.Load()).Milliseconds(),
		q.donePrefix())
	for _, r := range batch {
		args = append(args, r.v.ackID, r.v.state, r.v.reason, r.v.result, r.v.hint)
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
// retention (most, age ms), outcome channel prefix, then 5 per finish: ack
// ID, state (completed, failed or cancelled), reason, result JSON ("" for
// none), item hint (see itemHint: "j" and the envelope id, the item then
// is not decoded; "" decodes it). Returns one code per finish: 1 settled, 0 not processing (or an
// earlier finish of the batch took its ack ID), -1 result not JSON (that
// finish changes nothing).
//
// Reads (HMGET) and deletes (HDEL, ZREM) go in one command per key. Record
// writes are grouped by segment: a Job ID that shows up again in the batch
// starts a new segment, so its later finish lands on top of its earlier one
// as sequential calls would. Each state's set is trimmed once, at the end
// (inserts only, so the same set as a trim per finish). Outcomes are
// published last, after every record step.
var settleBatch = redis.NewScript(deadlineFn + envidFn + asJSONFn + trimFn + recordFn + `local n = (#ARGV - 5) / 5
local kc, ac, kf, af = tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3]), tonumber(ARGV[4])
local out, acks, seen, live = {}, {}, {}, {}
for i = 1, n do acks[i] = ARGV[5*i + 1] end
local items = redis.call('HMGET', KEYS[1], unpack(acks))
for i = 1, n do
	local r = ARGV[5*i + 4]
	if r ~= '' and not pcall(cjson.decode, r) then out[i] = -1
	elseif not items[i] or seen[acks[i]] then out[i] = 0
	else out[i] = 1; seen[acks[i]] = true; live[#live + 1] = i end
end
if #live == 0 then return out end
local la = {}
for k, i in ipairs(live) do la[k] = acks[i] end
local starts = redis.call('HMGET', KEYS[3], unpack(la))
local envs, decs, doks, eids = {}, {}, {}, {}
for k, i in ipairs(live) do
	local h = ARGV[5*i + 5]
	if h ~= '' then envs[k], doks[k] = string.sub(h, 2), true
	else envs[k], decs[k], doks[k] = envid(items[i]) end
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
	local st, env = ARGV[5*i + 2], envs[k]
	local id, attempts = env, 1
	if env ~= '' then attempts = 1 + att[env]; att[env] = 0 else id = acks[i] end
	ids[#ids + 1] = id
	if seg[id] then flush() end
	seg[id] = true
	local done = st == 'completed'
	local keep, age = kf, af
	if done then keep, age, nc = kc, ac, nc + 1 else nf = nf + 1 end
	local err = not done and ARGV[5*i + 3] or nil
	local r = ARGV[5*i + 4]
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
-- in-flight state goes last: an error above leaves the batch in flight
-- (redelivered), not lost
redis.call('HDEL', KEYS[1], unpack(la))
redis.call('ZREM', KEYS[2], unpack(la))
redis.call('HDEL', KEYS[3], unpack(la))
redis.call('HDEL', KEYS[9], unpack(la))
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

// settleLone runs settleOne for r: the trickle path, where nothing batches.
func (q *Queue) settleLone(r *groupReq[finishReq]) {
	f := &r.v
	done := f.state == "completed"
	keep, age := q.keepFailed.Load(), q.keepFailedAge.Load()
	state, other := q.getFailedKey(), q.getCompletedKey()
	if done {
		keep, age = q.keepCompleted.Load(), q.keepCompletedAge.Load()
		state, other = other, state
	}
	keys := []string{q.getNackedItemKey(), q.getTimeoutKey(), q.getStartedKey(), q.getAttemptsKey(), q.getStatsKey(), q.getJobsKey(), state, other, q.getCancelledKey()}
	if q.isPriority {
		keys = append(keys, q.getPriorityKey())
	}
	res, err := settleOne.Run(q.ctx, q.client, keys,
		f.ackID, f.state, f.reason, f.result, f.hint, keep, time.Duration(age).Milliseconds(), q.donePrefix()).Int64()
	if err != nil {
		log.Printf("Error finishing 1 item: %v", err)
	}
	r.ok = err == nil && res >= 0
}

// settleOne is settleBatch for one finish, with no tables to build: same
// steps, same order, same result codes. KEYS: nacked, timeouts, started,
// attempts, stats, jobs, the state's set, the other state's set,
// cancelled, then :prio for a priority queue only (FIFO never writes it).
// ARGV: ack ID, state, reason, result JSON (""), item hint (see itemHint),
// retention of the state (most, age ms), outcome channel prefix.
var settleOne = redis.NewScript(deadlineFn + envidFn + asJSONFn + trimFn + recordFn + `local ack, st, r = ARGV[1], ARGV[2], ARGV[4]
if r ~= '' and not pcall(cjson.decode, r) then return -1 end
local item = redis.call('HGET', KEYS[1], ack)
if not item then return 0 end
local started = tonumber(redis.call('HGET', KEYS[3], ack))
local env, dec, dok
if ARGV[5] ~= '' then env, dok = string.sub(ARGV[5], 2), true
else env, dec, dok = envid(item) end
local id, attempts = env, 1
if env ~= '' then
	attempts = 1 + (tonumber(redis.call('HGET', KEYS[4], env)) or 0)
	redis.call('HDEL', KEYS[4], env)
else
	id = ack
end
local now = tonumber(nowms())
local done = st == 'completed'
local keep, age = tonumber(ARGV[6]), tonumber(ARGV[7])
local err = not done and ARGV[3] or nil
local res = (done and r ~= '') and (',"result":' .. r) or ''
redis.call('ZREM', KEYS[8], id)
if keep > 0 and age > 0 then
	redis.call('HSET', KEYS[6], id, record(id, st, item, started, now, attempts, err, res, dok, dec))
	redis.call('ZADD', KEYS[7], string.format('%.0f', now), id)
else
	redis.call('HDEL', KEYS[6], id)
end
-- in-flight state goes last, as settleBatch
redis.call('HDEL', KEYS[1], ack)
redis.call('ZREM', KEYS[2], ack)
redis.call('HDEL', KEYS[3], ack)
if #KEYS > 9 then redis.call('HDEL', KEYS[10], ack) end
redis.call('ZREM', KEYS[9], id)
redis.call('HINCRBY', KEYS[5], done and 'completed' or 'failed', 1)
trim(KEYS[7], KEYS[6], keep, age, now)
if env ~= '' then redis.call('PUBLISH', ARGV[8] .. env, outcome(env, st, err, res)) end
return 1`)

// itemHint is what settleBatch needs of item b without decoding it in Lua:
// "j" and its envelope id ("" for none) when b is JSON that cjson decodes
// (json.Valid and luaDecodable), else "" (Lua decodes). cjson and Go agree
// on such items: a duplicate key's last value wins in both, keys match
// exactly (a map, no case folding). An id with an escape or invalid UTF-8
// gets "" (Lua decodes), so the id bytes are cjson's. A number item that
// Go takes is one asjson keeps as is.
func itemHint(b []byte) string {
	if !json.Valid(b) || !luaDecodable(b) {
		return ""
	}
	t := bytes.TrimLeft(b, " \t\r\n")
	if len(t) == 0 || t[0] != '{' {
		return "j"
	}
	raw, ok := topID(t)
	if !ok { // a key with an escape: let the decoder unescape keys
		var m map[string]json.RawMessage
		if json.Unmarshal(b, &m) != nil {
			return ""
		}
		raw = m["id"]
	}
	if len(raw) < 2 || raw[0] != '"' {
		return "j"
	}
	id := raw[1 : len(raw)-1]
	if bytes.IndexByte(id, '\\') >= 0 || !utf8.Valid(id) {
		return ""
	}
	return "j" + string(id)
}

// topID is the raw value of the last top-level "id" member of object t,
// which json.Valid passed (nil if none): what a map decode keeps, without
// building the map. ok is false at a key with an escape (a decode
// unescapes keys).
func topID(t []byte) (raw []byte, ok bool) {
	i := 1
	for {
		i = skipSpace(t, i)
		if t[i] == '}' {
			return raw, true
		}
		j := i + 1
		for t[j] != '"' {
			if t[j] == '\\' {
				return nil, false
			}
			j++
		}
		key := t[i+1 : j]
		i = skipSpace(t, skipSpace(t, j+1)+1) // past ':'
		end := skipValue(t, i)
		if string(key) == "id" {
			raw = t[i:end]
		}
		if i = skipSpace(t, end); t[i] == ',' {
			i++
			continue
		}
		return raw, true // '}'
	}
}

func skipSpace(t []byte, i int) int {
	for t[i] == ' ' || t[i] == '\t' || t[i] == '\r' || t[i] == '\n' {
		i++
	}
	return i
}

// skipValue returns the end of the valid JSON value at t[i].
func skipValue(t []byte, i int) int {
	depth, str := 0, false
	for ; ; i++ {
		c := t[i]
		switch {
		case str:
			if c == '\\' {
				i++
			} else if c == '"' {
				str = false
				if depth == 0 {
					return i + 1
				}
			}
		case c == '"':
			str = true
		case c == '{' || c == '[':
			depth++
		case c == '}' || c == ']':
			if depth == 0 {
				return i // end of a scalar before the closing bracket
			}
			if depth--; depth == 0 {
				return i + 1
			}
		case depth == 0 && (c == ',' || c == ' ' || c == '\t' || c == '\r' || c == '\n'):
			return i
		}
	}
}

// hintMap holds the hints of items this Queue object popped, by ack ID,
// until their finish takes them (FinishTask drops them). Close drops them;
// past maxHints (items never finished) it starts over: a lost hint only
// costs a decode.
type hintMap struct {
	mu sync.Mutex
	m  map[string]string
}

const maxHints = 1 << 15

func (h *hintMap) put(ackID, hint string) {
	if hint == "" {
		return
	}
	h.mu.Lock()
	if h.m == nil || len(h.m) >= maxHints {
		h.m = map[string]string{}
	}
	h.m[ackID] = hint
	h.mu.Unlock()
}

func (h *hintMap) take(ackID string) string {
	h.mu.Lock()
	v, ok := h.m[ackID]
	if ok {
		delete(h.m, ackID)
	}
	h.mu.Unlock()
	return v
}

func (h *hintMap) clear() {
	h.mu.Lock()
	h.m = nil
	h.mu.Unlock()
}
