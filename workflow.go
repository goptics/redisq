package redisq

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
)

// Workflows (spec/redis-storage.md 1.5.0, sections 6.9 to 6.12, 7.3). A
// workflow is one hash redisq:[<ns>:]wf:<id> and an entry in
// redisq:[<ns>:]wf:index. Each script loads the hash, runs the state
// machine below (wfFn, varmq's wfmachine.go in Lua) and pushes what it
// makes ready, in one atomic step. Queues of one workflow share one Redis
// and namespace: the workflow keys carry no hashtag, so Redis Cluster is not
// supported (spec 6.9). Every key a script touches is in KEYS.

var (
	_ contract.WorkflowStore           = (*workflowStore)(nil)
	_ contract.WorkflowInspector       = (*workflowStore)(nil)
	_ contract.WorkflowKeyLister       = (*workflowStore)(nil)
	_ contract.WorkflowAcknowledgeable = (*Queue)(nil)
	_ contract.WorkflowAcknowledgeable = (*PriorityQueue)(nil)
)

// Store defaults (spec 6.10).
const (
	defaultResultLimit = 64 << 10
	defaultWFRetention = 7 * 24 * time.Hour
)

// maxWorkflowsScan bounds a filtered listing: offset cap (varmq's 10000)
// plus the page (spec 7.3).
const maxWorkflowsScan = 10000

// workflowStore is the workflow store of one Redis and namespace: every
// queue of a Queues shares it.
type workflowStore struct {
	client      *redis.Client
	scope       string
	prefix      string // redisq:[<ns>:]
	resultLimit int
	retention   time.Duration
	trimFrom    atomic.Int64 // trimIndex's offset past old entries still live
}

// WithWorkflowResultLimit caps a workflow task's result at n bytes (default
// 64 KiB). A bigger one fails the task (varmq: task result too large).
func WithWorkflowResultLimit(n int) Option {
	return func(qs *queues) error {
		if n <= 0 {
			return fmt.Errorf("redisq: workflow result limit %d must be above 0", n)
		}
		qs.wfResultLimit = n
		return nil
	}
}

// WithWorkflowRetention sets how long an ended workflow is kept (default 7
// days). A running one is kept until it ends.
func WithWorkflowRetention(d time.Duration) Option {
	return func(qs *queues) error {
		if d < time.Millisecond {
			return fmt.Errorf("redisq: workflow retention %v must be at least 1ms", d)
		}
		qs.wfRetention = d
		return nil
	}
}

// newWorkflowStore is qs's store. Its scope is the Redis address, database
// and key prefix: two stores share state exactly when those match.
func newWorkflowStore(qs *queues) *workflowStore {
	o := qs.client.Options()
	return &workflowStore{
		client:      qs.client,
		scope:       "redisq:" + o.Addr + "/" + strconv.Itoa(o.DB) + "/" + qs.prefix(),
		prefix:      qs.prefix(),
		resultLimit: cmp.Or(qs.wfResultLimit, defaultResultLimit),
		retention:   cmp.Or(qs.wfRetention, defaultWFRetention),
	}
}

func (s *workflowStore) ScopeID() string      { return s.scope }
func (s *workflowStore) key(id string) string { return s.prefix + "wf:" + id }
func (s *workflowStore) indexKey() string     { return s.prefix + "wf:index" }

func (s *workflowStore) notFound(id string) error {
	return fmt.Errorf("%w: %s", contract.ErrWorkflowNotFound, id)
}

// owns: q is a redisq queue of this store's scope.
func (s *workflowStore) owns(q *Queue) bool { return q != nil && q.wf != nil && q.wf.scope == s.scope }

// ScopeID is the workflow store's scope: the Redis address, database and
// namespace. "" for a queue made without a store.
func (q *Queue) ScopeID() string {
	if q.wf == nil {
		return ""
	}
	return q.wf.scope
}

// WorkflowStore is the workflow store of this queue's Redis and namespace,
// nil (an untyped nil) for a queue made without one.
func (q *Queue) WorkflowStore() contract.WorkflowStore {
	if q.wf == nil {
		return nil
	}
	return q.wf
}

// validWFName: 1 to n characters of [A-Za-z0-9_.-] (spec 1.2).
func validWFName(s string, n int) bool {
	if len(s) == 0 || len(s) > n {
		return false
	}
	for _, c := range []byte(s) {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

func validWorkflowID(id string) bool { return validWFName(id, 128) && id != "index" }
func validTaskName(n string) bool    { return validWFName(n, 64) }

// queueKeys are the push-target keys of queue base of kind (fifo, prio):
// base, :stats, :idle, and :seq for prio (spec 6.9, 6.10).
func queueKeys(base, kind string) []string {
	keys := []string{base, base + ":stats", base + ":idle"}
	if kind == "prio" {
		keys = append(keys, base+":seq")
	}
	return keys
}

func (q *Queue) kind() string {
	if q.isPriority {
		return "prio"
	}
	return "fifo"
}

// wfFn is the workflow state machine, varmq's wfmachine.go, over a loaded
// hash W (wfload). It brings nothing; it needs deadlineFn's nowms and
// wakeFn's wake. Writes go through as they happen: callers check, before
// the first write, that every queue the workflow can push into is in KEYS
// (wfcovers), as Redis keeps a script's writes when it fails.
//
// W: key, meta (decoded), t (decoded t: per task), tj (raw t:), s (state),
// n (open deps), u (decoded u:), ret (retention ms), ended.
//
// The undo envelope sits verbatim at the end of t: ('"item":' then the
// envelope then '}}', see taskDef), so wfundoitem cuts it out unchanged: a
// plain '"item":' can't occur inside a JSON string, its quotes are escaped.
const wfFn = `local function wfactive(st) return st == 'pending' or st == 'processing' end
local function wfqueues(k, kinds)
	local Q, ids = {}, {}
	for c = 1, #kinds do
		local q = {base = KEYS[k], stats = KEYS[k + 1], idle = KEYS[k + 2]}
		k = k + 3
		if string.sub(kinds, c, c) == 'p' then q.seq = KEYS[k]; k = k + 1 end
		Q[q.base] = q
		ids[#ids + 1] = q.base
	end
	return Q, ids
end
local function wfdecode(v)
	local ok, x = pcall(cjson.decode, v or '')
	if ok and type(x) == 'table' then return x end
end
local function wfload(key, ret)
	local m = redis.call('HGET', key, 'meta')
	if not m then return nil end
	local W = {key = key, meta = wfdecode(m), t = {}, tj = {}, s = {}, n = {}, u = {}, ret = ret}
	if not W.meta or type(W.meta.tasks) ~= 'table' then return nil, true end
	local order = W.meta.tasks
	for i = 1, #order, 250 do
		local f = {}
		for j = i, math.min(i + 249, #order) do
			local x = order[j]
			f[#f + 1] = 't:' .. x; f[#f + 1] = 's:' .. x; f[#f + 1] = 'n:' .. x; f[#f + 1] = 'u:' .. x
		end
		local r = redis.call('HMGET', key, unpack(f))
		for j = 0, #f / 4 - 1 do
			local x = order[i + j]
			local t, u = wfdecode(r[4 * j + 1]), r[4 * j + 4]
			if not t or type(t.queue) ~= 'string' or type(t.deps) ~= 'table' or type(t.dependents) ~= 'table' then return nil, true end
			W.tj[x], W.t[x] = r[4 * j + 1], t
			W.s[x] = r[4 * j + 2]
			W.n[x] = tonumber(r[4 * j + 3])
			if u then
				W.u[x] = wfdecode(u)
				if not W.u[x] then return nil, true end
			end
		end
	end
	return W
end
local function wfcovers(W, Q)
	local function has(name, kind)
		local q = Q[name]
		return q ~= nil and (kind == 'prio') == (q.seq ~= nil)
	end
	for _, x in ipairs(W.meta.tasks) do
		local t = W.t[x]
		if not has(t.queue, t.kind) then return false end
		if type(t.undo) == 'table' and not has(t.undo.queue, t.undo.kind) then return false end
	end
	return true
end
local function wfset(W, x, st)
	W.s[x] = st
	redis.call('HSET', W.key, 's:' .. x, st)
	if st ~= 'waiting' and W.n[x] then
		W.n[x] = nil
		redis.call('HDEL', W.key, 'n:' .. x)
	end
end
local function wfsetu(W, x, u)
	W.u[x] = u
	local s = '{"state":' .. cjson.encode(u.state) .. ',"open":' .. string.format('%.0f', u.open)
	if type(u.reason) == 'string' and u.reason ~= '' then s = s .. ',"reason":' .. cjson.encode(u.reason) end
	redis.call('HSET', W.key, 'u:' .. x, s .. '}')
end
local function wfrelease(W, x, push)
	if W.s[x] ~= 'waiting' then return end
	local n = (W.n[x] or 1) - 1
	if n <= 0 then
		wfset(W, x, 'pending')
		push[x] = true
	else
		W.n[x] = n
		redis.call('HSET', W.key, 'n:' .. x, string.format('%.0f', n))
	end
end
local function wfdrop(W, x, push)
	for _, d in ipairs(W.t[x].dependents) do
		if W.s[d] == 'waiting' then
			if W.t[d].always then
				wfrelease(W, d, push)
			else
				wfset(W, d, 'cancelled')
				wfdrop(W, d, push)
			end
		end
	end
end
local function wfabsorbed(W, x)
	local next = W.t[x].dependents
	if #next == 0 then return false end
	for _, d in ipairs(next) do
		if not W.t[d].always then return false end
	end
	return true
end
local function wffailfast(W, failed, push)
	local keep, cancelled = {}, {}
	for _, d in ipairs(W.t[failed].dependents) do keep[d] = W.t[d].always end
	for _, x in ipairs(W.meta.tasks) do
		if W.s[x] == 'waiting' and not keep[x] then
			wfset(W, x, 'cancelled')
			cancelled[#cancelled + 1] = x
		end
	end
	for _, x in ipairs(cancelled) do wfdrop(W, x, push) end
end
local function wfreach(W, x, up)
	local seen, stack = {}, {x}
	while #stack > 0 do
		local n = table.remove(stack)
		for _, m in ipairs(up and W.t[n].deps or W.t[n].dependents) do
			if not seen[m] then
				seen[m] = true
				stack[#stack + 1] = m
			end
		end
	end
	return seen
end
local function wfemit(W, push, undo, fx)
	for _, x in ipairs(W.meta.tasks) do
		if push[x] then fx[#fx + 1] = {x, undo} end
	end
	return fx
end
local function wfend(W, state)
	W.meta.state = state
	W.meta.finished_at = tonumber(nowms())
	W.ended = true
end
local function wfcompensate(W, fx)
	local push, n = {}, #fx
	for _, x in ipairs(W.meta.tasks) do
		if W.s[x] == 'completed' and type(W.t[x].undo) == 'table' then
			local open = 0
			for d in pairs(wfreach(W, x, false)) do
				if W.s[d] == 'completed' and type(W.t[d].undo) == 'table' then open = open + 1 end
			end
			wfsetu(W, x, {state = open == 0 and 'pending' or 'waiting', open = open})
			if open == 0 then push[x] = true end
		end
	end
	wfemit(W, push, true, fx)
	return #fx > n
end
local function wfsettle(W, push)
	local fx = wfemit(W, push, false, {})
	for _, x in ipairs(W.meta.tasks) do
		local s = W.s[x]
		if s == 'waiting' or s == 'pending' or s == 'processing' then return fx end
	end
	local m = W.meta
	if (m.failed or (m.cancelled and m.undo_on_cancel)) and wfcompensate(W, fx) then
		m.state = 'compensating'
		return fx
	end
	if m.failed then wfend(W, 'failed') elseif m.cancelled then wfend(W, 'cancelled') else wfend(W, 'completed') end
	return fx
end
local function wffinishundo(W, x, outcome, reason)
	local u = W.u[x]
	if W.meta.state ~= 'compensating' or not u or not wfactive(u.state) then return {} end
	u.state, u.reason = 'failed', reason
	if outcome == 'completed' then u.state, u.reason = 'completed', nil end
	wfsetu(W, x, u)
	local push, up = {}, wfreach(W, x, true)
	for _, y in ipairs(W.meta.tasks) do
		local v = W.u[y]
		if up[y] and v and v.state == 'waiting' then
			v.open = v.open - 1
			if v.open == 0 then
				v.state = 'pending'
				push[y] = true
			end
			wfsetu(W, y, v)
		end
	end
	local fx, failed = wfemit(W, push, true, {}), false
	for _, y in ipairs(W.meta.tasks) do
		local v = W.u[y]
		if v then
			if v.state == 'waiting' or v.state == 'pending' or v.state == 'processing' then return fx end
			if v.state == 'failed' then failed = true end
		end
	end
	wfend(W, failed and 'compensation_failed' or 'compensated')
	return fx
end
local function wffinish(W, x, outcome, reason, result, undo)
	local t = W.t[x]
	if not t then return {} end
	if undo then return wffinishundo(W, x, outcome, reason) end
	if W.meta.state ~= 'running' or not wfactive(W.s[x]) then return {} end
	local push = {}
	if outcome == 'completed' then
		wfset(W, x, 'completed')
		if result ~= '' then redis.call('HSET', W.key, 'r:' .. x, result) end
		for _, d in ipairs(t.dependents) do wfrelease(W, d, push) end
		return wfsettle(W, push)
	end
	wfset(W, x, outcome == 'cancelled' and 'cancelled' or 'failed')
	if reason ~= '' then redis.call('HSET', W.key, 'e:' .. x, reason) end
	local m = W.meta
	if not m.cancelled and not wfabsorbed(W, x) then
		if m.fail_fast and not m.failed then wffailfast(W, x, push) end
		m.failed = true
	end
	wfdrop(W, x, push)
	return wfsettle(W, push)
end
local function wfcancel(W, undo)
	local m = W.meta
	if m.state ~= 'running' or m.cancelled then return {} end
	m.cancelled, m.undo_on_cancel = true, undo
	for _, x in ipairs(m.tasks) do
		if W.s[x] == 'waiting' then wfset(W, x, 'cancelled') end
	end
	return wfsettle(W, {})
end
local function wfpush(q, prio, env)
	if q.seq then
		local s = redis.call('INCR', q.seq)
		redis.call('ZADD', q.base, prio, '\255' .. string.format('%016x', s) .. env)
	else
		redis.call('RPUSH', q.base, env)
	end
	redis.call('HINCRBY', q.stats, 'submitted', 1)
	wake(q.idle, q.base .. ':wake:', q.seq and redis.call('ZCARD', q.base) or redis.call('LLEN', q.base))
end
local function wfundoitem(tj)
	local p = string.find(tj, '"item":', 1, true)
	return string.sub(tj, p + 7, -3)
end
local function wfmeta(m)
	local names = {}
	for i, x in ipairs(m.tasks) do names[i] = cjson.encode(x) end
	local s = '{"state":' .. cjson.encode(m.state) .. ',"created_at":' .. string.format('%.0f', m.created_at)
	if type(m.finished_at) == 'number' then s = s .. ',"finished_at":' .. string.format('%.0f', m.finished_at) end
	return s .. ',"fail_fast":' .. tostring(m.fail_fast == true) .. ',"undo_on_cancel":' .. tostring(m.undo_on_cancel == true) ..
		',"cancelled":' .. tostring(m.cancelled == true) .. ',"failed":' .. tostring(m.failed == true) ..
		',"hash":' .. cjson.encode(m.hash) .. ',"tasks":[' .. table.concat(names, ',') .. ']}'
end
local function wfapply(W, fx, Q, pushed, seen)
	for _, f in ipairs(fx) do
		local x, undo = f[1], f[2]
		local t, q, prio, env = W.t[x]
		if undo then
			q, prio, env = Q[t.undo.queue], 0, wfundoitem(W.tj[x])
		else
			q, prio, env = Q[t.queue], string.format('%.0f', t.priority or 0), redis.call('HGET', W.key, 'i:' .. x)
			redis.call('HDEL', W.key, 'i:' .. x)
		end
		if env then
			wfpush(q, prio, env)
			if not seen[q.base] then
				seen[q.base] = true
				pushed[#pushed + 1] = q.base
			end
		end
	end
	redis.call('HSET', W.key, 'meta', wfmeta(W.meta))
	if W.ended then redis.call('PEXPIRE', W.key, W.ret) end
	return pushed
end
`

// wfPrelude is what every workflow script starts with.
const wfPrelude = wakeFn + deadlineFn + wfFn

// wfTargetsScript returns the push-target queues of workflow KEYS[1] (spec
// 6.10): every task's and undo's queue, once each, in task order, as {base
// key, kind} pairs. Empty when the hash is gone; a field cjson can't decode
// adds none (the workflow scripts then take the workflow as unreadable).
// Read only.
var wfTargetsScript = redis.NewScript(`local m = redis.call('HGET', KEYS[1], 'meta')
if not m then return {} end
local seen, out = {}, {}
local function add(q, k)
	if not seen[q] then
		seen[q] = true
		out[#out + 1] = q
		out[#out + 1] = k
	end
end
local ok, meta = pcall(cjson.decode, m)
if not ok or type(meta) ~= 'table' or type(meta.tasks) ~= 'table' then return {} end
for _, x in ipairs(meta.tasks) do
	local okt, t = pcall(cjson.decode, redis.call('HGET', KEYS[1], 't:' .. x) or '')
	if okt and type(t) == 'table' and type(t.queue) == 'string' and type(t.kind) == 'string' then
		add(t.queue, t.kind)
		if type(t.undo) == 'table' and type(t.undo.queue) == 'string' and type(t.undo.kind) == 'string' then add(t.undo.queue, t.undo.kind) end
	end
end
return out`)

// wfTarget is one push-target queue: base key and kind (fifo, prio).
type wfTarget struct{ base, kind string }

// wfTargetsEach reads the push-target queues of each workflow hash in
// keys, in one round trip (a pipeline of wfTargetsScript). A gone hash has
// none.
// ponytail: one read per call; cache by (id, meta hash) if it shows up.
func wfTargetsEach(ctx context.Context, c *redis.Client, keys ...string) ([][]wfTarget, error) {
	pipe := c.Pipeline()
	cmds := make([]*redis.Cmd, len(keys))
	for i, k := range keys {
		cmds[i] = wfTargetsScript.Eval(ctx, pipe, []string{k}) // EVAL: no NOSCRIPT retry in a pipeline
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([][]wfTarget, len(cmds))
	for i, cmd := range cmds {
		r, err := cmd.StringSlice()
		if err != nil {
			return nil, err
		}
		for j := 0; j+1 < len(r); j += 2 {
			out[i] = append(out[i], wfTarget{r[j], r[j+1]})
		}
	}
	return out, nil
}

// wfTargets is wfTargetsEach merged: each queue once, in order.
func wfTargets(ctx context.Context, c *redis.Client, keys ...string) ([]wfTarget, error) {
	each, err := wfTargetsEach(ctx, c, keys...)
	if err != nil {
		return nil, err
	}
	return mergeTargets(each...), nil
}

func mergeTargets(each ...[]wfTarget) []wfTarget {
	var out []wfTarget
	seen := map[string]bool{}
	for _, ts := range each {
		for _, t := range ts {
			if !seen[t.base] {
				seen[t.base] = true
				out = append(out, t)
			}
		}
	}
	return out
}

// targetKeys are ts's keys (queueKeys each) and one kind letter (f, p) per
// queue, for wfqueues.
func targetKeys(ts []wfTarget) ([]string, string) {
	var keys []string
	kinds := make([]byte, 0, len(ts))
	for _, t := range ts {
		keys = append(keys, queueKeys(t.base, t.kind)...)
		kinds = append(kinds, t.kind[0])
	}
	return keys, string(kinds)
}

// errReread: a workflow script found a queue it needs missing from KEYS
// (the workflow appeared or changed since the caller read it) and changed
// nothing; the caller reads again, at most wfTries times.
var errReread = errors.New("redisq: workflow changed while reading its queues")

const wfTries = 3

// reread runs fn up to wfTries times while it returns errReread.
func reread(fn func() error) error {
	var err error
	for range wfTries {
		if err = fn(); !errors.Is(err, errReread) {
			return err
		}
	}
	return err
}

// taskDef is t:<task> (spec 1.2), without the undo: the undo, with its
// envelope verbatim, is spliced on last (see wfFn).
type taskDef struct {
	Queue      string   `json:"queue"`
	Kind       string   `json:"kind"`
	Priority   int      `json:"priority"`
	Deps       []string `json:"deps"`
	Dependents []string `json:"dependents"`
	Always     bool     `json:"always"`
	JobID      string   `json:"job_id"`
}

type undoDef struct {
	Queue string `json:"queue"`
	Kind  string `json:"kind"`
	JobID string `json:"job_id"`
}

// wfSubmitScript is Submit (spec 6.9). KEYS: workflow hash, wf:index, the
// root queues (queueKeys each). ARGV: root queue kinds (f, p), workflow ID,
// plan hash, fail_fast and undo_on_cancel (true, false), task names (JSON
// array), field count F, F field/value pairs, then per root push its queue
// base key, priority and envelope. 1 stored or same plan, 0 another plan
// holds the ID, 2 an item (or t: with its undo item) cjson can't decode
// (nesting over 1000, a lone surrogate escape): stored, it would break
// every later script of the workflow. Nothing is written then.
var wfSubmitScript = redis.NewScript(wfPrelude + `local Q = wfqueues(3, ARGV[1])
if redis.call('EXISTS', KEYS[1]) == 1 then
	local ok, m = pcall(cjson.decode, redis.call('HGET', KEYS[1], 'meta') or '')
	return (ok and type(m) == 'table' and m.hash == ARGV[3]) and 1 or 0
end
local f = 8 + 2 * tonumber(ARGV[7])
for i = 8, f - 1, 2 do
	local p = string.sub(ARGV[i], 1, 2)
	if (p == 't:' or p == 'i:') and not wfdecode(ARGV[i + 1]) then return 2 end
end
for i = f + 2, #ARGV, 3 do
	if not wfdecode(ARGV[i]) then return 2 end
end
local now = nowms()
redis.call('HSET', KEYS[1], 'meta', '{"state":"running","created_at":' .. now .. ',"fail_fast":' .. ARGV[4] ..
	',"undo_on_cancel":' .. ARGV[5] .. ',"cancelled":false,"failed":false,"hash":' .. cjson.encode(ARGV[3]) .. ',"tasks":' .. ARGV[6] .. '}')
for i = 8, f - 1, 2 do redis.call('HSET', KEYS[1], ARGV[i], ARGV[i + 1]) end
for i = f, #ARGV, 3 do wfpush(Q[ARGV[i]], ARGV[i + 1], ARGV[i + 2]) end
redis.call('ZADD', KEYS[2], now, ARGV[2])
return 1`)

// planQueue is the redisq queue of a plan's task or undo, in this scope.
func (s *workflowStore) planQueue(name, queueID string, q, item any) (*Queue, []byte, error) {
	var rq *Queue
	switch v := q.(type) {
	case *Queue:
		rq = v
	case *PriorityQueue:
		if v != nil {
			rq = v.Queue
		}
	}
	if !s.owns(rq) {
		return nil, nil, fmt.Errorf("%w: task %q queue %s is not in redisq scope %s", contract.ErrScopeMismatch, name, queueID, s.scope)
	}
	b, ok := item.([]byte)
	if !ok || !json.Valid(b) || len(b) == 0 || b[0] != '{' {
		return nil, nil, fmt.Errorf("varmq: task %q: item is not a JSON envelope", name)
	}
	return rq, b, nil
}

// Submit stores plan p and pushes its roots (spec 6.9). A stored ID is
// success when its plan hash is equal, else ErrWorkflowExists.
func (s *workflowStore) Submit(ctx context.Context, p contract.WorkflowPlan) error {
	if len(p.Tasks) == 0 {
		return fmt.Errorf("varmq: workflow %s has no tasks", p.ID)
	}
	if !validWorkflowID(p.ID) {
		return fmt.Errorf("varmq: invalid workflow id %q", p.ID)
	}

	dependents := map[string][]string{}
	names := make([]string, 0, len(p.Tasks))
	for _, tp := range p.Tasks {
		if !validTaskName(tp.Name) {
			return fmt.Errorf("varmq: invalid task name %q", tp.Name)
		}
		for _, d := range tp.Deps {
			dependents[d] = append(dependents[d], tp.Name)
		}
		names = append(names, tp.Name)
	}

	var fields, pushes []any
	var keys []string
	var kinds []byte
	roots := map[string]bool{}
	for _, tp := range p.Tasks {
		q, item, err := s.planQueue(tp.Name, tp.QueueID, tp.Queue, tp.Item)
		if err != nil {
			return err
		}
		def, err := json.Marshal(taskDef{Queue: q.queueKey, Kind: q.kind(), Priority: tp.Priority,
			Deps: nonNil(tp.Deps), Dependents: nonNil(dependents[tp.Name]),
			Always: tp.Always, JobID: tp.JobID})
		if err != nil {
			return err
		}
		if u := tp.Undo; u != nil {
			uq, uitem, err := s.planQueue(tp.Name+" undo", u.QueueID, u.Queue, u.Item)
			if err != nil {
				return err
			}
			head, err := json.Marshal(undoDef{Queue: uq.queueKey, Kind: uq.kind(), JobID: u.JobID})
			if err != nil {
				return err
			}
			def = slices.Concat(def[:len(def)-1], []byte(`,"undo":`), head[:len(head)-1], []byte(`,"item":`), uitem, []byte("}}"))
		}
		fields = append(fields, "t:"+tp.Name, def)

		if len(tp.Deps) > 0 {
			fields = append(fields, "s:"+tp.Name, "waiting", "n:"+tp.Name, len(tp.Deps), "i:"+tp.Name, item)
			continue
		}
		fields = append(fields, "s:"+tp.Name, "pending")
		pushes = append(pushes, q.queueKey, tp.Priority, item)
		if !roots[q.queueKey] {
			roots[q.queueKey] = true
			keys = append(keys, queueKeys(q.queueKey, q.kind())...)
			kinds = append(kinds, q.kind()[0])
		}
	}

	tasks, err := json.Marshal(names)
	if err != nil {
		return err
	}
	args := append([]any{string(kinds), p.ID, p.Hash, strconv.FormatBool(p.FailFast), strconv.FormatBool(p.UndoOnCancel), tasks, len(fields) / 2}, fields...)
	n, err := wfSubmitScript.Run(ctx, s.client, append([]string{s.key(p.ID), s.indexKey()}, keys...), append(args, pushes...)...).Int()
	if err != nil {
		return err
	}
	switch n {
	case 0:
		return fmt.Errorf("%w: %s", contract.ErrWorkflowExists, p.ID)
	case 2:
		return fmt.Errorf("varmq: workflow %s: an item Redis's cjson can't decode (nesting over 1000 or a lone surrogate escape)", p.ID)
	}
	return nil
}

// nonNil is v, or an empty slice for nil (JSON [] rather than null).
func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// wfFinishScript is FinishTask (spec 6.10). KEYS: settleFn's ten, the
// workflow hash, the push-target queues. ARGV: settleFn's seven, the
// target kinds, task, undo (1, 0), outcome, retention (ms), this queue's
// base key, workflow ID. The workflow step runs only when the settled item
// is that task's (or undo's) own: its envelope tag names the end's
// workflow, task and undo, and t: names this queue. A gone or unreadable
// workflow settles the item alone. Returns {1, pushed queue IDs...}, or {2}: reread (nothing
// changed).
var wfFinishScript = redis.NewScript(wfPrelude + settleFn + `local Q, ids = wfqueues(12, ARGV[8])
local item = redis.call('HGET', KEYS[1], ARGV[1])
if not item then return {1, unpack(ids)} end
local W = wfload(KEYS[11], ARGV[12])
if W and not wfcovers(W, Q) then return {2} end
if ARGV[6] ~= '' and not pcall(cjson.decode, ARGV[6]) then ARGV[6] = '' end
settlejob()
if not W then return {1} end
local undo, t = ARGV[10] == '1', W.t[ARGV[9]]
local id, e = envid(item)
if not wftag(e, id) or e.wf ~= ARGV[14] or e.task ~= ARGV[9] or (e.undo == true) ~= undo then return {1} end
if not t or (undo and (type(t.undo) ~= 'table' or t.undo.queue ~= ARGV[13])) or (not undo and t.queue ~= ARGV[13]) then return {1} end
local fx = wffinish(W, ARGV[9], ARGV[11], ARGV[3], ARGV[6], undo)
return {1, unpack(wfapply(W, fx, Q, {}, {}))}`)

// FinishTask settles ackID and applies the task end to its workflow in one
// step (spec 6.10): it returns the queue IDs it pushed into, or, when the
// item had already ended, every push-target queue of the workflow. A gone
// workflow, or an end that does not name this queue's task, settles the
// item and returns ok with no IDs. A completed result over the store's
// limit fails the task (contract.ErrResultTooLarge); one that is not JSON
// is dropped. Ends with an invalid workflow ID or task name are settled as
// plain jobs.
func (q *Queue) FinishTask(ackID string, e contract.TaskEnd) ([]string, bool) {
	if q.ctx.Err() != nil {
		log.Println("queue closed, cannot finish task")
		return nil, false
	}
	limit, ret := defaultResultLimit, defaultWFRetention
	if q.wf != nil {
		limit, ret = q.wf.resultLimit, q.wf.retention
	}
	// ponytail: a queue with no store (only newQueueBase) settles every
	// end as a plain job: it has no namespace to find workflows in.

	state, reason, result := "completed", e.Reason, e.Result
	switch e.Outcome {
	case contract.OutcomeCompleted:
		reason = ""
		switch {
		case e.Undo:
			result = nil
		case len(result) > limit:
			e.Outcome, state, reason, result = contract.OutcomeFailed, "failed", contract.ErrResultTooLarge.Error(), nil
		case !json.Valid(result):
			result = nil
		}
	case contract.OutcomeFailed, contract.OutcomeCancelled:
		state, result = "failed", nil
	default:
		log.Printf("redisq: task %s/%s: unknown outcome %q", e.Workflow, e.Task, e.Outcome)
		return nil, false
	}

	if q.wf == nil || !validWorkflowID(e.Workflow) || !validTaskName(e.Task) {
		return nil, q.finish(ackID, state, reason, result)
	}

	settle, args := q.finishArgs(ackID, state, reason, result)
	undo := "0"
	if e.Undo {
		undo = "1"
	}
	wfKey := q.wf.key(e.Workflow)
	var pushed []string
	err := reread(func() error {
		ts, err := wfTargets(q.ctx, q.client, wfKey)
		if err != nil {
			return err
		}
		targets, kinds := targetKeys(ts)
		r, err := wfFinishScript.Run(q.ctx, q.client, slices.Concat(settle, []string{wfKey}, targets),
			append(args, kinds, e.Task, undo, string(e.Outcome), ret.Milliseconds(), q.queueKey, e.Workflow)...).Slice()
		if err != nil {
			return err
		}
		if r[0].(int64) == 2 {
			return errReread
		}
		pushed = pushed[:0]
		for _, v := range r[1:] {
			pushed = append(pushed, v.(string))
		}
		return nil
	})
	if err != nil {
		log.Printf("Error finishing workflow %s task %s: %v", e.Workflow, e.Task, err)
		return nil, false
	}
	return pushed, true
}

// wfCancelScript is Cancel (spec 6.11). KEYS: workflow hash, push-target
// queues. ARGV: target kinds, undo (empty: the workflow's; 1, 0), retention
// (ms). {0}: not found; {2}: reread; {1}: done.
var wfCancelScript = redis.NewScript(wfPrelude + `local Q = wfqueues(2, ARGV[1])
local W, bad = wfload(KEYS[1], ARGV[3])
if bad then return {3} end
if not W then return {0} end
if not wfcovers(W, Q) then return {2} end
local undo = W.meta.undo_on_cancel == true
if ARGV[2] ~= '' then undo = ARGV[2] == '1' end
wfapply(W, wfcancel(W, undo), Q, {}, {})
return {1}`)

// Cancel cancels workflow id (spec 6.11): waiting tasks now, queued ones at
// their pre-run check. undo nil keeps the workflow's undo_on_cancel.
// Cancelling an ended or cancelled workflow does nothing.
func (s *workflowStore) Cancel(ctx context.Context, id string, undo *bool) error {
	if !validWorkflowID(id) {
		return s.notFound(id)
	}
	u := ""
	if undo != nil {
		u = "0"
		if *undo {
			u = "1"
		}
	}
	key := s.key(id)
	return reread(func() error {
		ts, err := wfTargets(ctx, s.client, key)
		if err != nil {
			return err
		}
		targets, kinds := targetKeys(ts)
		r, err := wfCancelScript.Run(ctx, s.client, append([]string{key}, targets...), kinds, u, s.retention.Milliseconds()).Int64Slice()
		switch {
		case err != nil:
			return err
		case r[0] == 0:
			return s.notFound(id)
		case r[0] == 2:
			return errReread
		case r[0] == 3:
			return fmt.Errorf("redisq: workflow %s is unreadable (a field cjson can't decode)", id)
		}
		return nil
	})
}

// wfMeta is the meta field (spec 1.2).
type wfMeta struct {
	State        contract.WorkflowState `json:"state"`
	CreatedAt    int64                  `json:"created_at"`
	FinishedAt   int64                  `json:"finished_at"`
	FailFast     bool                   `json:"fail_fast"`
	UndoOnCancel bool                   `json:"undo_on_cancel"`
	Cancelled    bool                   `json:"cancelled"`
	Failed       bool                   `json:"failed"`
	Tasks        []string               `json:"tasks"`
}

// taskRead is t:<task> as read back.
type taskRead struct {
	taskDef
	Undo *undoDef `json:"undo"`
}

// undoState is u:<task>.
type undoState struct {
	State  contract.JobState `json:"state"`
	Reason string            `json:"reason"`
}

// get reads fields of workflow id: meta first; ErrWorkflowNotFound when
// the hash is gone.
func (s *workflowStore) get(ctx context.Context, id string, fields ...string) (wfMeta, []any, error) {
	if !validWorkflowID(id) {
		return wfMeta{}, nil, s.notFound(id)
	}
	r, err := s.client.HMGet(ctx, s.key(id), append([]string{"meta"}, fields...)...).Result()
	if err != nil {
		return wfMeta{}, nil, err
	}
	var m wfMeta
	if r[0] == nil {
		return m, nil, s.notFound(id)
	}
	if err := json.Unmarshal([]byte(r[0].(string)), &m); err != nil {
		return m, nil, err
	}
	return m, r[1:], nil
}

// field unmarshals HMGET value v into out; false when v is absent or bad.
func field(v any, out any) bool {
	str, ok := v.(string)
	return ok && json.Unmarshal([]byte(str), out) == nil
}

// Runnable is the pre-run check (spec 6.12).
func (s *workflowStore) Runnable(ctx context.Context, workflowID, task string, undo bool) (bool, error) {
	m, r, err := s.get(ctx, workflowID, "t:"+task, "s:"+task, "u:"+task)
	if err != nil {
		return false, err
	}
	var t taskRead
	if !validTaskName(task) || !field(r[0], &t) {
		return false, nil
	}
	if undo {
		var u undoState
		return m.State == contract.WorkflowCompensating && field(r[2], &u) && u.State == contract.JobPending, nil
	}
	stopped := m.FailFast && m.Failed && !t.Always
	return m.State == contract.WorkflowRunning && !m.Cancelled && !stopped && r[1] == string(contract.JobPending), nil
}

// DepResult returns task's stored result: ErrDepFailed when it failed or
// was cancelled, ErrNoResult when it left none or has not ended.
func (s *workflowStore) DepResult(ctx context.Context, workflowID, task string) ([]byte, error) {
	_, r, err := s.get(ctx, workflowID, "s:"+task, "r:"+task)
	if err != nil {
		return nil, err
	}
	switch r[0] {
	case string(contract.JobFailed), string(contract.JobCancelled):
		return nil, contract.ErrDepFailed
	case string(contract.JobCompleted):
		if res, ok := r[1].(string); ok && res != "" {
			return []byte(res), nil
		}
	}
	return nil, contract.ErrNoResult
}

// Record returns workflow id (spec 7.3, One).
func (s *workflowStore) Record(ctx context.Context, id string) (contract.WorkflowRecord, error) {
	if !validWorkflowID(id) {
		return contract.WorkflowRecord{}, s.notFound(id)
	}
	h, err := s.client.HGetAll(ctx, s.key(id)).Result()
	if err != nil {
		return contract.WorkflowRecord{}, err
	}
	return s.recordOf(id, h)
}

// recordOf builds workflow id's record from its hash h.
func (s *workflowStore) recordOf(id string, h map[string]string) (contract.WorkflowRecord, error) {
	var m wfMeta
	if h["meta"] == "" {
		return contract.WorkflowRecord{}, s.notFound(id)
	}
	if err := json.Unmarshal([]byte(h["meta"]), &m); err != nil {
		return contract.WorkflowRecord{}, err
	}

	r := contract.WorkflowRecord{ID: id, State: m.State, CreatedAt: m.CreatedAt, FinishedAt: m.FinishedAt,
		UndoOnCancel: m.UndoOnCancel, Tasks: make([]contract.TaskRecord, 0, len(m.Tasks))}
	for _, name := range m.Tasks {
		var t taskRead
		_ = json.Unmarshal([]byte(h["t:"+name]), &t)
		tr := contract.TaskRecord{Name: name, State: contract.JobState(h["s:"+name]), Deps: t.Deps, QueueID: t.Queue,
			JobID: t.JobID, Always: t.Always, Reason: h["e:"+name], ResultSize: len(h["r:"+name])}
		if len(tr.Deps) == 0 {
			tr.Deps = nil
		}
		if t.Undo != nil {
			u := undoState{State: contract.JobWaiting}
			if v, ok := h["u:"+name]; ok {
				_ = json.Unmarshal([]byte(v), &u)
			}
			tr.Undo = &contract.TaskRecord{Name: name, State: u.State, QueueID: t.Undo.Queue, JobID: t.Undo.JobID, Reason: u.Reason}
		}
		r.Tasks = append(r.Tasks, tr)
	}
	return r, nil
}

// Workflows lists up to limit workflows in state ("" any), newest first
// then by ID, skipping offset (spec 7.3, List).
func (s *workflowStore) Workflows(ctx context.Context, state contract.WorkflowState, offset, limit int) ([]contract.WorkflowRecord, error) {
	keys, err := s.WorkflowKeys(ctx, state, offset, limit)
	if err != nil {
		return nil, err
	}
	pipe := s.client.Pipeline()
	hs := make([]*redis.MapStringStringCmd, len(keys))
	for i, k := range keys {
		hs[i] = pipe.HGetAll(ctx, s.key(k.ID))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	out := make([]contract.WorkflowRecord, 0, len(keys))
	for i, k := range keys {
		r, err := s.recordOf(k.ID, hs[i].Val())
		if errors.Is(err, contract.ErrWorkflowNotFound) {
			continue // expired since the listing
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// listChunk is how many index entries a listing reads per round trip.
const listChunk = 200

// WorkflowKeys is Workflows without the records (spec 7.3, Keys). It pages
// wf:index from the client: a chunk by score cursor (ZREVRANGEBYSCORE from
// the last score read, skipping the ties already read), then its meta, so
// no listing blocks Redis and a Submit or trim between chunks shifts
// nothing. A state filter reads at most 10000 + limit entries. Entries past
// offset+limit with the last one's created_at are read too, so ties sort by
// ID as varmq merges them.
func (s *workflowStore) WorkflowKeys(ctx context.Context, state contract.WorkflowState, offset, limit int) ([]contract.WorkflowKey, error) {
	offset, limit = max(offset, 0), max(limit, 0)
	limit = min(limit, math.MaxInt-offset)
	want := offset + limit
	var out []contract.WorkflowKey
	seen := map[string]bool{}
	top, ties, scanned := "+inf", int64(0), 0
	for {
		zs, err := s.client.ZRevRangeByScoreWithScores(ctx, s.indexKey(),
			&redis.ZRangeBy{Max: top, Min: "-inf", Offset: ties, Count: listChunk}).Result()
		if err != nil {
			return nil, err
		}
		pipe := s.client.Pipeline()
		metas := make([]*redis.StringCmd, len(zs))
		for i, z := range zs {
			metas[i] = pipe.HGet(ctx, s.key(z.Member.(string)), "meta")
		}
		if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, redis.Nil) {
			return nil, err
		}

		for i, z := range zs {
			if state != "" && scanned-limit >= maxWorkflowsScan {
				return page(out, offset, want), nil
			}
			scanned++
			k := contract.WorkflowKey{ID: z.Member.(string), CreatedAt: int64(z.Score)}
			if seen[k.ID] {
				continue
			}
			seen[k.ID] = true
			if len(out) >= want && (want == 0 || k.CreatedAt != out[len(out)-1].CreatedAt) {
				return page(out, offset, want), nil
			}
			var m wfMeta
			if metas[i].Err() != nil || json.Unmarshal([]byte(metas[i].Val()), &m) != nil {
				continue // expired before a trim
			}
			if state == "" || m.State == state {
				out = append(out, k)
			}
		}
		if len(zs) < listChunk {
			return page(out, offset, want), nil
		}
		last := zs[len(zs)-1].Score
		n := int64(0)
		for i := len(zs) - 1; i >= 0 && zs[i].Score == last; i-- {
			n++
		}
		next := strconv.FormatFloat(last, 'f', -1, 64)
		if next == top {
			ties += n
		} else {
			top, ties = next, n
		}
	}
}

// page sorts keys newest first, then by ID, and cuts [offset, want).
func page(keys []contract.WorkflowKey, offset, want int) []contract.WorkflowKey {
	slices.SortFunc(keys, func(a, b contract.WorkflowKey) int {
		return cmp.Or(cmp.Compare(b.CreatedAt, a.CreatedAt), strings.Compare(a.ID, b.ID))
	})
	return keys[min(offset, len(keys)):min(want, len(keys))]
}

// wfTrimScript drops index entry ARGV[i] (KEYS[1] the index) when its
// hash KEYS[i + 1] is gone and its score is still at or below ARGV[#ARGV],
// in one step, so a re-submit of the ID in between keeps its entry.
var wfTrimScript = redis.NewScript(`local cut, n = tonumber(ARGV[#ARGV]), 0
for i = 1, #ARGV - 1 do
	local sc = tonumber(redis.call('ZSCORE', KEYS[1], ARGV[i]))
	if sc and sc <= cut and redis.call('EXISTS', KEYS[i + 1]) == 0 then
		redis.call('ZREM', KEYS[1], ARGV[i])
		n = n + 1
	end
end
return n`)

// trimIndex drops index entries older than the retention whose hash is
// gone (spec 7.3, Trim), checking up to 1000 per call. The spec trims by
// score alone, but the score is created_at and the hash expires a
// retention after finished_at: a workflow running past the retention would
// drop out of listings while Record still finds it. Old entries still live
// are stepped over (trimFrom), so they don't stall the trim.
func (s *workflowStore) trimIndex(ctx context.Context) {
	cut := strconv.FormatInt(time.Now().Add(-s.retention).UnixMilli(), 10)
	from := s.trimFrom.Load()
	old, err := s.client.ZRangeByScore(ctx, s.indexKey(), &redis.ZRangeBy{Min: "-inf", Max: cut, Offset: from, Count: 1000}).Result()
	if err == nil {
		keys, args := []string{s.indexKey()}, make([]any, 0, len(old)+1)
		for _, id := range old {
			keys, args = append(keys, s.key(id)), append(args, id)
		}
		var n int64
		if len(old) > 0 {
			n, err = wfTrimScript.Run(ctx, s.client, keys, append(args, cut)...).Int64()
		}
		live := from + int64(len(old)) - n
		if len(old) < 1000 {
			live = 0 // reached the end: start over next time
		}
		s.trimFrom.Store(live)
	}
	if err != nil && ctx.Err() == nil {
		log.Printf("Error trimming workflow index: %v", err)
	}
}

// wfPurgeScript is Purge (spec 6.6). KEYS: the D keys Purge deletes (the
// queue first, :nacked second), the hashes of the N workflows the caller
// knows of, their push-target queues. ARGV: D, N, target kinds, this
// queue's kind (fifo, prio), retention (ms), the N workflow IDs. It reads
// the queued and processing items; when a valid workflow item (wftag)
// names a workflow not in KEYS, or a known one needs a queue not in KEYS,
// it changes nothing and returns {2, the workflow IDs it found}: the caller
// reads their queues and runs it again. Else it deletes, then applies a
// cancelled end (reason "queue purged") to each tagged item's task or undo,
// in the order read, and pushes what that releases. Returns {1}.
var wfPurgeScript = redis.NewScript(wfPrelude + envIDFn + bareFn + `local D, N = tonumber(ARGV[1]), tonumber(ARGV[2])
local Q = wfqueues(D + N + 1, ARGV[3])
local items = {}
if ARGV[4] == 'prio' then
	for _, m in ipairs(redis.call('ZRANGE', KEYS[1], 0, -1)) do items[#items + 1] = bare(m) end
else
	items = redis.call('LRANGE', KEYS[1], 0, -1)
end
for _, v in ipairs(redis.call('HVALS', KEYS[2])) do items[#items + 1] = v end
local hashes, Ws, tagged, found, seen, miss = {}, {}, {}, {}, {}, false
for j = 1, N do hashes[ARGV[5 + j]] = KEYS[D + j] end
for _, v in ipairs(items) do
	local id, e = envid(v)
	if wftag(e, id) then
		tagged[#tagged + 1] = e
		if not seen[e.wf] then
			seen[e.wf] = true
			found[#found + 1] = e.wf
		end
		if not hashes[e.wf] then miss = true end
	end
end
for j = 1, N do
	local W = wfload(KEYS[D + j], ARGV[5])
	if W then
		if not wfcovers(W, Q) then miss = true end
		Ws[ARGV[5 + j]] = W
	end
end
if miss then
	local r = {2}
	for _, id in ipairs(found) do r[#r + 1] = id end
	return r
end
local del = {}
for i = 1, D do del[i] = KEYS[i] end
redis.call('DEL', unpack(del))
local pushed, pseen = {}, {}
for _, e in ipairs(tagged) do
	local W, undo = Ws[e.wf], e.undo == true
	local t = W and W.t[e.task]
	if t and ((undo and type(t.undo) == 'table' and t.undo.queue == KEYS[1]) or (not undo and t.queue == KEYS[1])) then
		wfapply(W, wffinish(W, e.task, 'cancelled', 'queue purged', '', undo), Q, pushed, pseen)
	end
end
return {1}`)

// Purge deletes all of the queue's jobs and job data in one step: waiting
// and processing items (an Acknowledge or Reject of a purged item then does
// nothing), deadlines, start times, attempt counts, Job Records, the :stats
// counters, cancel marks, and a priority queue's sequence and priorities.
// Consumer state stays (the idle set, heartbeats, presence), and so do
// EnqueueOnce ids, which expire on their own (see EnqueueOnce).
//
// A purged workflow task or undo ends cancelled in the same step, so its
// workflow goes on (spec 6.6): its dependents follow their policy, and what
// that releases is pushed after the delete. The script finds the workflow
// items itself; a queue that holds some takes a second call, with their
// workflows' queues in KEYS. Each call decodes every queued and processing
// item inside Redis, so purging millions of items blocks Redis for that
// long (a plain DEL did not).
func (q *Queue) Purge() {
	if err := q.purge(); err != nil {
		log.Printf("Error purging queue %s: %v", q.queueKey, err)
	}
}

func (q *Queue) purge() error {
	del := append(q.requeueKeys(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey(), q.getStatsKey(), q.getCancelledKey())
	if q.wf == nil { // no store (newQueueBase): no workflows to find
		return q.client.Del(q.ctx, del...).Err()
	}
	ret := q.wf.retention
	var ids []string
	// Push targets per workflow ID, kept across tries: t: never changes, so
	// only new IDs are read. ponytail: an ID that expired and was submitted
	// again on other queues between tries fails the Purge (logged).
	known := map[string][]wfTarget{}
	// One try more than wfTries: the first only finds the workflows.
	for range wfTries + 1 {
		var fresh []string
		hashes := make([]string, len(ids))
		for i, id := range ids {
			hashes[i] = q.wf.key(id)
			if len(known[id]) == 0 { // new, or its hash was gone: it may exist now
				fresh = append(fresh, id)
			}
		}
		if len(fresh) > 0 {
			keys := make([]string, len(fresh))
			for i, id := range fresh {
				keys[i] = q.wf.key(id)
			}
			each, err := wfTargetsEach(q.ctx, q.client, keys...)
			if err != nil {
				return err
			}
			for i, id := range fresh {
				known[id] = each[i]
			}
		}
		each := make([][]wfTarget, len(ids))
		for i, id := range ids {
			each[i] = known[id]
		}
		qkeys, kinds := targetKeys(mergeTargets(each...))
		args := []any{len(del), len(ids), kinds, q.kind(), ret.Milliseconds()}
		for _, id := range ids {
			args = append(args, id)
		}
		r, err := wfPurgeScript.Run(q.ctx, q.client, slices.Concat(del, hashes, qkeys), args...).Slice()
		if err != nil {
			return err
		}
		if r[0].(int64) == 1 {
			return nil
		}
		ids = ids[:0]
		for _, v := range r[1:] {
			ids = append(ids, v.(string))
		}
	}
	return errReread
}
