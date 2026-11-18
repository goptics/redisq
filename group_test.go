package redisq

import (
	"context"
	"fmt"
	"math/rand/v2"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// settleBatch over a batch leaves Redis as finishScript run once per
// finish in the same order: dup ack IDs, a Job ID twice, a bad result,
// plain and non-JSON items, attempts, cancel marks, an old record.
func TestSettleBatchMatchesSequential(t *testing.T) {
	for _, c := range []struct{ prio, none bool }{{false, false}, {true, false}, {false, true}, {true, true}} {
		prio := c.prio
		t.Run(fmt.Sprintf("priority=%v,no_records=%v", c.prio, c.none), func(t *testing.T) {
			ctx := context.Background()
			qs := New(getTestRedisURL())
			defer qs.Close()
			client := qs.(*queues).client
			open := func(key string) (*Queue, func(string), func() string) {
				delKeys(t, client, key)
				if prio {
					pq := qs.NewPriorityQueue(key)
					require.NoError(t, client.Del(ctx, pq.getCancelledKey()).Err())
					return pq.Queue, func(v string) { require.True(t, pq.Enqueue(v, 1)) }, func() string {
						_, ok, a := pq.DequeueWithAckId()
						require.True(t, ok)
						return a
					}
				}
				q := qs.NewQueue(key)
				require.NoError(t, client.Del(ctx, q.getCancelledKey()).Err())
				return q, func(v string) { require.True(t, q.Enqueue(v)) }, func() string {
					_, ok, a := q.DequeueWithAckId()
					require.True(t, ok)
					return a
				}
			}
			type fin struct {
				i                     int // pop index
				state, reason, result string
			}
			fins := []fin{
				{0, "completed", "", ""},
				{0, "completed", "", ""},  // same ack ID: no-op
				{1, "failed", "boom", ""}, // X again
				{2, "completed", "", `{"r":1}`},
				{3, "cancelled", "stop", ""},
				{4, "completed", "", "{"}, // bad result: untouched
				{5, "completed", "", `{"big":12345678901234567}`},
				{4, "failed", "late", ""},
			}
			items := []string{envelope("X", "1"), envelope("X", "2"), envelope("", "3"), "plain-bytes", envelope("Y", "4"), envelope("Z", "5")}
			run := func(key string, mode string, hinted bool) (map[string]any, []int64) {
				q, push, pop := open(key)
				defer q.Close()
				if c.none {
					q.SetCompletedRetention(0, 0)
					q.SetFailedRetention(0, time.Hour)
				}
				for _, v := range items {
					push(v)
				}
				require.NoError(t, client.HSet(ctx, q.getAttemptsKey(), "X", 2).Err())
				require.NoError(t, client.ZAdd(ctx, q.getFailedKey(), redis.Z{Score: 1, Member: "X"}).Err())
				require.NoError(t, client.HSet(ctx, q.getJobsKey(), "X", `{"old":1}`).Err())
				acks := make([]string, len(items))
				for i := range items {
					acks[i] = pop()
				}
				require.NoError(t, client.ZAdd(ctx, q.getCancelledKey(), redis.Z{Score: 9e12, Member: "Z"}).Err())
				var codes []int64
				switch mode {
				case "lone": // settleOne, one finish per call
					for _, f := range fins {
						h := ""
						if hinted {
							h = itemHint([]byte(items[f.i]))
						}
						r := &groupReq[finishReq]{v: finishReq{acks[f.i], f.state, f.reason, []byte(f.result), h}}
						q.settleLone(r)
						c := int64(0)
						if r.ok {
							c = 1
						}
						codes = append(codes, c)
					}
				case "batch":
					args := []any{q.keepCompleted.Load(), time.Duration(q.keepCompletedAge.Load()).Milliseconds(),
						q.keepFailed.Load(), time.Duration(q.keepFailedAge.Load()).Milliseconds(), q.donePrefix()}
					for _, f := range fins {
						h := ""
						if hinted {
							h = itemHint([]byte(items[f.i]))
						}
						args = append(args, acks[f.i], f.state, f.reason, []byte(f.result), h)
					}
					keys := []string{q.getNackedItemKey(), q.getTimeoutKey(), q.getStartedKey(), q.getAttemptsKey(), q.getStatsKey(), q.getJobsKey(), q.getCompletedKey(), q.getFailedKey(), q.getPriorityKey(), q.getCancelledKey()}
					var err error
					codes, err = settleBatch.Run(ctx, client, keys, args...).Int64Slice()
					require.NoError(t, err)
				default:
					for _, f := range fins {
						keys, args := q.finishArgs(acks[f.i], f.state, f.reason, []byte(f.result))
						c, err := finishScript.Run(ctx, client, keys, args...).Int64()
						require.NoError(t, err)
						codes = append(codes, c)
					}
				}
				return dumpQueue(t, client, q), codes
			}
			b, cb := run("test_settle_batch_b", "seq", false)
			for _, hinted := range []bool{false, true} {
				a, ca := run("test_settle_batch_a", "batch", hinted)
				assert.Equal(t, cb, ca)
				assert.Equal(t, b, a)
				assert.Equal(t, []int64{1, 0, 1, 1, 1, -1, 1, 1}, ca)
				l, cl := run("test_settle_batch_a", "lone", hinted)
				assert.Equal(t, b, l, "settleOne")
				assert.Equal(t, []int64{1, 1, 1, 1, 1, 0, 1, 1}, cl) // ok: 0 not processing is a success too
			}
			delKeys(t, client, "test_settle_batch_a")
			delKeys(t, client, "test_settle_batch_b")
		})
	}
}

var timeField = regexp.MustCompile(`"(started_at|finished_at|at)":\d+`)

// dumpQueue reads a queue's job keys with owner, key and times masked.
func dumpQueue(t *testing.T, c *redis.Client, q *Queue) map[string]any {
	t.Helper()
	ctx := context.Background()
	norm := func(s string) string {
		s = strings.ReplaceAll(s, q.ackPrefix, "OWNER")
		s = strings.ReplaceAll(s, q.queueKey, "KEY")
		return timeField.ReplaceAllString(s, `"$1":T`)
	}
	out := map[string]any{}
	for _, k := range []string{q.getNackedItemKey(), q.getStartedKey(), q.getAttemptsKey(), q.getJobsKey(), q.getStatsKey(), q.getPriorityKey()} {
		h, err := c.HGetAll(ctx, k).Result()
		require.NoError(t, err)
		m := map[string]string{}
		for f, v := range h {
			if k == q.getStartedKey() {
				v = "T"
			}
			m[norm(f)] = norm(v)
		}
		out[norm(k)] = m
	}
	for _, k := range []string{q.getCompletedKey(), q.getFailedKey(), q.getCancelledKey(), q.getTimeoutKey()} {
		ms, err := c.ZRange(ctx, k, 0, -1).Result()
		require.NoError(t, err)
		for i := range ms {
			ms[i] = norm(ms[i])
		}
		sort.Strings(ms)
		out[norm(k)] = ms
	}
	return out
}

// Overlapping Enqueue, EnqueueOnce and Acknowledge calls group into fewer
// script calls and each still gets its own outcome.
func TestGroupCommitConcurrent(t *testing.T) {
	ctx := context.Background()
	qs := New(getTestRedisURL())
	defer qs.Close()
	client := qs.(*queues).client
	key := "test_group_commit"
	delKeys(t, client, key)
	defer delKeys(t, client, key)
	q := qs.NewQueue(key)
	defer q.Close()

	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() { assert.True(t, q.Enqueue(envelope("", fmt.Sprint(i)))) })
	}
	for i := range 100 {
		wg.Go(func() { assert.True(t, q.EnqueueOnce(envelope(fmt.Sprint("d", i%10), "1"), fmt.Sprint("d", i%10))) })
	}
	wg.Wait()
	assert.Equal(t, 210, q.Len(), "each dedupe id stored once")

	acks := make(chan string, 210)
	for range 210 {
		_, ok, a := q.DequeueWithAckId()
		require.True(t, ok)
		acks <- a
	}
	close(acks)
	for range 50 {
		wg.Go(func() {
			for a := range acks {
				assert.True(t, q.Acknowledge(a))
			}
		})
	}
	wg.Wait()
	st, err := client.HGetAll(ctx, q.getStatsKey()).Result()
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"submitted": "210", "completed": "210"}, st)
	assert.Zero(t, client.HLen(ctx, q.getNackedItemKey()).Val())
	assert.EqualValues(t, 210, client.ZCard(ctx, q.getCompletedKey()).Val())
	assert.EqualValues(t, 210, client.HLen(ctx, q.getJobsKey()).Val())
}

// A finish after Close fails at once; one queued behind a call in flight
// when Close runs is not stranded.
func TestGroupCommitClose(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	client := qs.(*queues).client
	key := "test_group_close"
	delKeys(t, client, key)
	defer delKeys(t, client, key)
	q := qs.NewQueue(key)
	for i := range 50 {
		require.True(t, q.Enqueue(fmt.Sprint(i)))
	}
	var acks []string
	for range 50 {
		_, _, a := q.DequeueWithAckId()
		acks = append(acks, a)
	}
	var wg sync.WaitGroup
	for _, a := range acks {
		wg.Go(func() { q.Acknowledge(a) })
	}
	q.Close()
	wg.Wait() // returns: no waiter stranded
	assert.False(t, q.Acknowledge(acks[0]))
	assert.False(t, q.Enqueue("x"))
}

// itemHint agrees with cjson (envid, asjson) on every item it hints.
func TestItemHintMatchesCjson(t *testing.T) {
	qs := New(getTestRedisURL())
	defer qs.Close()
	client := qs.(*queues).client
	probe := redis.NewScript(envIDFn + asJSONFn + `local id, e, ok = envid(ARGV[1])
return {ok and 1 or 0, id, asjson(ARGV[1], ok, e) == ARGV[1] and 1 or 0}`)
	deep := strings.Repeat("[", 1001) + strings.Repeat("]", 1001)
	items := []string{
		envelope("a", "1"), envelope("", `{"id":"inner"}`),
		`{"id":"a","id":"b"}`, `{"id":"a","id":5}`, `{"id":5,"id":"b"}`, `{"id":"x"}`, `{"ID":"x"}`, `{"Id":"x","id":"y"}`,
		`{"id":"a\"b"}`, `{"id":"aé"}`, `{"id":"é"}`, "{\"id\":\"\xff\"}", "{\"id\xff\":\"x\"}", `{"id":null}`, `{"id":["a"]}`,
		`{}`, `[]`, `["id"]`, `1e5`, ` 12 `, `-0.5e+10`, `null`, `true`, `"str"`, `NaN`, `0x10`, `inf`, `plain bytes`, ``,
		"\xef\xbb\xbf{\"id\":\"bom\"}", `{"id":"x","n":12345678901234567890}`, `{"id":"s","d":"\ud800"}`, `{"id":"p","d":"😀"}`, deep,
		`{"id":"t",}`, `{"id":"ctl","d":"a` + "\x01" + `"}`, "{\"id\":\"ws\"}\n",
		`{"i\u0064":"esc"}`, `{"\u0069d":"e2","x":1}`, `{ "a" : [ 1 , { "id" : "n" } ] , "id" : "w" }`,
		`{"x":"}","id":"q"}`, `{"a":"\\","id":"bs"}`, `{"id":"k" , "z":true}`, `{"n":-1.5e3,"id":"num"}`,
	}
	for _, it := range items {
		h := itemHint([]byte(it))
		if h == "" {
			continue
		}
		r, err := probe.Run(context.Background(), client, nil, it).Slice()
		require.NoError(t, err, it)
		assert.EqualValues(t, 1, r[0], "cjson decodes %q", it)
		assert.Equal(t, h[1:], r[1], "envelope id of %q", it)
		assert.EqualValues(t, 1, r[2], "asjson keeps %q", it)
	}
	assert.Equal(t, "ja", itemHint([]byte(envelope("a", "1"))))
	assert.Equal(t, "j", itemHint([]byte(envelope("", `{"id":"inner"}`))))
	assert.Equal(t, "jb", itemHint([]byte(`{"id":"a","id":"b"}`)))
	assert.Equal(t, "", itemHint([]byte(`{"id":"a\"b"}`)), "escaped id: Lua decodes")
}

// A group run takes at most max values and maxGroupBytes of them (one value
// alone may pass it); every value is run once.
func TestGroupLimits(t *testing.T) {
	var g group[int]
	var mu sync.Mutex
	seen := map[int]int{}
	release := make(chan struct{})
	run := func(b []*groupReq[int]) {
		<-release
		mu.Lock()
		defer mu.Unlock()
		bytes := 0
		for _, r := range b {
			seen[r.v]++
			bytes += r.size
			r.ok = true
		}
		assert.LessOrEqual(t, len(b), 3)
		if len(b) > 1 {
			assert.LessOrEqual(t, bytes, maxGroupBytes)
		}
	}
	var wg sync.WaitGroup
	for i := range 40 {
		wg.Go(func() { assert.True(t, g.do(i, (i%4)*maxGroupBytes/3, 3, run)) })
	}
	close(release)
	wg.Wait()
	assert.Len(t, seen, 40)
	for v, n := range seen {
		assert.Equal(t, 1, n, v)
	}
}

// oldTrimFn is the trim before ZPOPMIN (ZRANGEBYSCORE + ZCARD + ZRANGE,
// then ZREM + HDEL): the reference trimFn must match.
const oldTrimFn = `local function trim(z, jobs, keep, age, now)
	local gone
	if keep <= 0 or age <= 0 then
		gone = redis.call('ZRANGE', z, 0, 999)
	else
		gone = redis.call('ZRANGEBYSCORE', z, '-inf', string.format('%.0f', now - age), 'LIMIT', 0, 1000)
		local over = redis.call('ZCARD', z) - keep
		if over > 0 then
			for _, m in ipairs(redis.call('ZRANGE', z, 0, math.min(over, 1000) - 1)) do gone[#gone + 1] = m end
		end
	end
	if #gone > 0 then
		redis.call('ZREM', z, unpack(gone))
		redis.call('HDEL', jobs, unpack(gone))
	end
end
`

// trimFn (ZPOPMIN first, age read skipped when the newest popped is past
// the cut) leaves what the range trim left: same set, same records.
func TestTrimMatchesRangeTrim(t *testing.T) {
	ctx := context.Background()
	qs := New(getTestRedisURL())
	defer qs.Close()
	client := qs.(*queues).client
	body := `trim(KEYS[1], KEYS[2], tonumber(ARGV[1]), tonumber(ARGV[2]), tonumber(ARGV[3])) return 1`
	scripts := map[string]*redis.Script{"old": redis.NewScript(oldTrimFn + body), "new": redis.NewScript(trimFn + body)}
	const now = 1_000_000
	rng := rand.New(rand.NewPCG(1, 2))
	for c := range 200 {
		size := rng.IntN(60)
		keep := []int{0, 1, 5, 30, 100}[rng.IntN(5)]
		age := []int{0, 1000, 50_000, 2_000_000}[rng.IntN(4)]
		members := make([]redis.Z, size)
		for i := range members {
			// ties on purpose: scores on a coarse grid around the cut
			members[i] = redis.Z{Score: float64(now - rng.IntN(100)*1000), Member: fmt.Sprintf("m%02d", i)}
		}
		got := map[string]any{}
		for name, s := range scripts {
			z, jobs := "trimtest:"+name+":z", "trimtest:"+name+":jobs"
			require.NoError(t, client.Del(ctx, z, jobs).Err())
			if size > 0 {
				require.NoError(t, client.ZAdd(ctx, z, members...).Err())
				for _, m := range members {
					require.NoError(t, client.HSet(ctx, jobs, m.Member, "r").Err())
				}
			}
			require.NoError(t, s.Run(ctx, client, []string{z, jobs}, keep, age, now).Err())
			left := client.ZRange(ctx, z, 0, -1).Val()
			recs := client.HKeys(ctx, jobs).Val()
			slices.Sort(recs)
			l2 := slices.Clone(left)
			slices.Sort(l2)
			assert.Equal(t, l2, recs, "case %d %s: records follow members", c, name)
			got[name] = left
			require.NoError(t, client.Del(ctx, z, jobs).Err())
		}
		assert.Equal(t, got["old"], got["new"], "case %d size %d keep %d age %d", c, size, keep, age)
	}
}
