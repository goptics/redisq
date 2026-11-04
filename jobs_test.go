package redisq

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/goptics/varmq/contract"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// jobsCase is a queue kind as the Job Record tests drive it.
type jobsCase struct {
	name string
	open func(qs Queues, key string) (q *Queue, in contract.QueueInspector, push func(item, id string) bool, pop func() (string, bool), done func())
}

var jobsCases = []jobsCase{
	{"list", func(qs Queues, key string) (*Queue, contract.QueueInspector, func(string, string) bool, func() (string, bool), func()) {
		q := qs.NewQueue(key)
		return q, q, func(v, id string) bool { return q.EnqueueOnce(v, id) }, popWith(q), func() { q.Close() }
	}},
	{"priority", func(qs Queues, key string) (*Queue, contract.QueueInspector, func(string, string) bool, func() (string, bool), func()) {
		q := qs.NewPriorityQueue(key)
		return q.Queue, q, func(v, id string) bool { return q.EnqueueOnce(v, 1, id) }, popWith(q), func() { q.Close() }
	}},
	{"distributed", func(qs Queues, key string) (*Queue, contract.QueueInspector, func(string, string) bool, func() (string, bool), func()) {
		q := qs.NewDistributedQueue(key)
		return q.Queue, q, func(v, id string) bool { return q.EnqueueOnce(v, id) }, popWith(q), func() { q.Close() }
	}},
	{"distributed priority", func(qs Queues, key string) (*Queue, contract.QueueInspector, func(string, string) bool, func() (string, bool), func()) {
		q := qs.NewDistributedPriorityQueue(key)
		return q.Queue, q, func(v, id string) bool { return q.EnqueueOnce(v, 1, id) }, popWith(q), func() { q.Close() }
	}},
}

func popWith(q interface{ DequeueWithAckId() (any, bool, string) }) func() (string, bool) {
	return func() (string, bool) {
		_, ok, id := q.DequeueWithAckId()
		return id, ok
	}
}

// each runs fn for every queue kind on a clean key.
func eachJobsCase(t *testing.T, fn func(t *testing.T, q *Queue, in contract.QueueInspector, push func(item, id string) bool, pop func() (string, bool))) {
	for _, c := range jobsCases {
		t.Run(c.name, func(t *testing.T) {
			qs := New(getTestRedisURL())
			defer qs.Close()
			key := "test_jobs_" + strings.ReplaceAll(c.name, " ", "_")
			client := qs.(*queues).client
			delKeys(t, client, key)
			defer delKeys(t, client, key)
			q, in, push, pop, done := c.open(qs, key)
			defer done()
			fn(t, q, in, push, pop)
		})
	}
}

func envelope(id string, data string) string {
	return fmt.Sprintf(`{"id":%q,"status":"Created","data":%s,"enqueued_at":1762228800000}`, id, data)
}

func stats(t *testing.T, in contract.QueueInspector) contract.QueueStats {
	t.Helper()
	s, err := in.QueueStats(t.Context())
	require.NoError(t, err)
	return s
}

func jobs(t *testing.T, in contract.QueueInspector, state contract.JobState, offset, limit int) []contract.JobRecord {
	t.Helper()
	j, err := in.Jobs(t.Context(), state, offset, limit)
	require.NoError(t, err)
	return j
}

func ids(js []contract.JobRecord) []string {
	out := make([]string, len(js))
	for i, j := range js {
		out[i] = j.ID
	}
	return out
}

// Acknowledge and Reject write the record, verbatim, count the job and
// clear its processing state, in one step.
func TestAcknowledgeAndRejectWriteRecords(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		ctx := context.Background()
		big := `{"n":12345678901234567,"f":0.1}` // cjson.encode would round both
		require.True(t, push(envelope("order-1", big), "order-1"))
		require.True(t, push(envelope("order-2", `"x"`), "order-2"))
		before := time.Now().UnixMilli()

		a1, ok := pop()
		require.True(t, ok)
		a2, ok := pop()
		require.True(t, ok)
		assert.True(t, q.Acknowledge(a1))
		assert.True(t, q.Reject(a2, `out of "stock"`))
		assert.True(t, q.Acknowledge(a1), "an ack ID no longer processing is a success")
		assert.True(t, q.Reject(a2, "again"))

		rec, err := q.client.HGet(ctx, q.getJobsKey(), "order-1").Result()
		require.NoError(t, err)
		assert.Contains(t, rec, `"item":`+envelope("order-1", big), "the item is recorded as stored")
		assert.Contains(t, rec, `"state":"completed"`)
		assert.NotContains(t, rec, `"errors"`)

		assert.Equal(t, contract.QueueStats{
			Counts: contract.JobCounts{Completed: 1, Failed: 1},
			Totals: contract.JobTotals{Submitted: 2, Completed: 1, Failed: 1},
		}, stats(t, in))
		for _, k := range []string{q.getNackedItemKey(), q.getStartedKey(), q.getTimeoutKey(), q.getAttemptsKey()} {
			n, err := q.client.Exists(ctx, k).Result()
			require.NoError(t, err)
			assert.Zero(t, n, k)
		}
		assert.Zero(t, q.Len(), "a rejected job is not requeued")

		done := jobs(t, in, contract.JobCompleted, 0, 10)
		require.Len(t, done, 1)
		j := done[0]
		assert.Equal(t, "order-1", j.ID)
		assert.Equal(t, contract.JobCompleted, j.State)
		assert.JSONEq(t, big, string(j.Payload))
		assert.Equal(t, big, string(j.Payload), "the payload keeps its bytes")
		assert.EqualValues(t, 1762228800000, j.EnqueuedAt)
		assert.Equal(t, 1, j.Attempts)
		assert.GreaterOrEqual(t, j.StartedAt, before-1000)
		assert.GreaterOrEqual(t, j.FinishedAt, j.StartedAt)

		bad := jobs(t, in, contract.JobFailed, 0, 10)
		require.Len(t, bad, 1)
		assert.Equal(t, "order-2", bad[0].ID)
		assert.Equal(t, []contract.JobError{{Attempt: 1, At: bad[0].FinishedAt, Error: `out of "stock"`}}, bad[0].Errors)
		assert.Equal(t, `"x"`, string(bad[0].Payload))
	})
}

// An item without an envelope id is recorded under its ack ID, and one that
// isn't JSON as a JSON string.
func TestRecordOfPlainItem(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		require.True(t, push("plain bytes", ""))
		require.True(t, push(`{"status":"Created","data":[1,2]}`, ""))
		a1, _ := pop()
		a2, _ := pop()
		require.True(t, q.Reject(a1, "boom"))
		require.True(t, q.Reject(a2, "boom"))

		got := jobs(t, in, contract.JobFailed, 0, 10)
		require.Len(t, got, 2)
		byID := map[string]contract.JobRecord{got[0].ID: got[0], got[1].ID: got[1]}
		assert.Equal(t, `"plain bytes"`, string(byID[a1].Payload))
		assert.Equal(t, `[1,2]`, string(byID[a2].Payload))
		assert.Equal(t, 1, byID[a1].Attempts)
	})
}

// A requeue counts an attempt for a job with an id and drops its start
// time; the record takes the count and clears it.
func TestAttemptsCountRequeues(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		ctx := context.Background()
		require.True(t, push(envelope("job-a", "1"), "job-a"))
		a1, _ := pop()
		require.True(t, q.client.HExists(ctx, q.getStartedKey(), a1).Val())

		require.NoError(t, q.client.ZAdd(ctx, q.getTimeoutKey(), redis.Z{Score: 1, Member: a1}).Err()) // past its deadline
		q.tick()
		assert.False(t, q.client.HExists(ctx, q.getStartedKey(), a1).Val())
		p := jobs(t, in, contract.JobPending, 0, 10)
		require.Len(t, p, 1)
		assert.Equal(t, "job-a", p[0].ID)
		assert.Equal(t, 1, p[0].Attempts)
		assert.Zero(t, p[0].StartedAt)

		a2, _ := pop()
		w := jobs(t, in, contract.JobProcessing, 0, 10)
		require.Len(t, w, 1)
		assert.Equal(t, 2, w[0].Attempts)
		assert.Positive(t, w[0].StartedAt)

		require.True(t, q.Acknowledge(a2))
		assert.Equal(t, 2, jobs(t, in, contract.JobCompleted, 0, 1)[0].Attempts)
		assert.False(t, q.client.HExists(ctx, q.getAttemptsKey(), "job-a").Val())
	})
}

// Retention trims by count and by age, record and member together, and 0
// keeps none; the counters count every job.
func TestRetentionTrims(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		ctx := context.Background()
		q.SetCompletedRetention(2, time.Hour)
		require.NoError(t, q.client.ZAdd(ctx, q.getCompletedKey(), redis.Z{Score: float64(time.Now().Add(-2 * time.Hour).UnixMilli()), Member: "old"}).Err())
		require.NoError(t, q.client.HSet(ctx, q.getJobsKey(), "old", "{}").Err())
		for i := range 3 {
			id := fmt.Sprintf("c%d", i)
			require.True(t, push(envelope(id, "0"), id))
			a, _ := pop()
			require.True(t, q.Acknowledge(a))
		}
		assert.Equal(t, []string{"c2", "c1"}, ids(jobs(t, in, contract.JobCompleted, 0, 10)))
		assert.EqualValues(t, 2, q.client.HLen(ctx, q.getJobsKey()).Val(), "old and c0 go with their records")

		q.SetFailedRetention(0, time.Hour)
		require.True(t, push("f", ""))
		a, _ := pop()
		require.True(t, q.Reject(a, "x"))
		assert.Empty(t, jobs(t, in, contract.JobFailed, 0, 10))

		q.SetCompletedRetention(0, time.Hour)
		q.trim() // the sweep's trim, for an idle queue
		assert.Zero(t, q.client.HLen(ctx, q.getJobsKey()).Val())
		assert.Equal(t, contract.JobTotals{Submitted: 4, Completed: 3, Failed: 1}, stats(t, in).Totals)
	})
}

// A reused Job ID moves to its newest state.
func TestReusedJobIDMovesState(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		require.True(t, push(envelope("same", "1"), ""))
		a, _ := pop()
		require.True(t, q.Acknowledge(a))
		require.True(t, push(envelope("same", "2"), ""))
		a, _ = pop()
		require.True(t, q.Reject(a, "x"))
		assert.Empty(t, jobs(t, in, contract.JobCompleted, 0, 10))
		got := jobs(t, in, contract.JobFailed, 0, 10)
		require.Len(t, got, 1)
		assert.Equal(t, "2", string(got[0].Payload))

		q.SetCompletedRetention(0, time.Hour)
		require.True(t, push(envelope("same", "3"), ""))
		a, _ = pop()
		require.True(t, q.Acknowledge(a))
		assert.Empty(t, jobs(t, in, contract.JobFailed, 0, 10), "the old state goes even when the new one keeps none")
		assert.Empty(t, jobs(t, in, contract.JobCompleted, 0, 10))
		assert.False(t, q.client.HExists(context.Background(), q.getJobsKey(), "same").Val())
	})
}

// A dedupe repeat is not counted as submitted.
func TestSubmittedSkipsDedupeRepeat(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		require.True(t, push("a", "id-a"))
		require.True(t, push("a", "id-a"))
		require.True(t, push("b", ""))
		assert.Equal(t, contract.QueueStats{
			Counts: contract.JobCounts{Pending: 2},
			Totals: contract.JobTotals{Submitted: 2},
		}, stats(t, in))
	})
}

// Jobs pages each state in its order.
func TestJobsPaging(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		for i := 1; i <= 5; i++ {
			id := fmt.Sprintf("j%d", i)
			require.True(t, push(envelope(id, "0"), id))
		}
		assert.Equal(t, []string{"j2", "j3"}, ids(jobs(t, in, contract.JobPending, 1, 2)))
		assert.Empty(t, jobs(t, in, contract.JobPending, 5, 2))
		assert.Empty(t, jobs(t, in, contract.JobPending, 0, 0))

		var acks []string
		for range 3 {
			a, _ := pop()
			acks = append(acks, a)
		}
		assert.Equal(t, []string{"j2", "j3"}, ids(jobs(t, in, contract.JobProcessing, 1, 10)))
		for _, a := range acks {
			require.True(t, q.Acknowledge(a))
			time.Sleep(2 * time.Millisecond) // distinct finish times
		}
		assert.Equal(t, []string{"j3", "j2", "j1"}, ids(jobs(t, in, contract.JobCompleted, 0, 10)), "newest first")
		assert.Equal(t, []string{"j2"}, ids(jobs(t, in, contract.JobCompleted, 1, 1)))

		_, err := in.Jobs(t.Context(), "done", 0, 1)
		assert.Error(t, err)
		assert.Equal(t, q.queueKey, in.QueueID())
	})
}

// Purge deletes every job and its data, and keeps consumer state.
func TestPurgeDeletesJobData(t *testing.T) {
	eachJobsCase(t, func(t *testing.T, q *Queue, in contract.QueueInspector, push func(string, string) bool, pop func() (string, bool)) {
		ctx := context.Background()
		for _, id := range []string{"p1", "p2", "p3"} {
			require.True(t, push(envelope(id, "0"), id))
		}
		a, _ := pop()
		require.True(t, q.Acknowledge(a))
		a, _ = pop()
		require.NoError(t, q.client.HIncrBy(ctx, q.getAttemptsKey(), "p2", 1).Err())

		q.Purge()
		assert.Equal(t, contract.QueueStats{}, stats(t, in))
		left, err := q.client.Keys(ctx, q.queueKey+"*").Result()
		require.NoError(t, err)
		for _, k := range left {
			suffix := strings.TrimPrefix(k, q.queueKey)
			assert.True(t, suffix == ":heartbeats" || suffix == ":heartbeats:clock" || suffix == ":presence" || suffix == ":idle" || strings.HasPrefix(suffix, ":dedup:"), "left %s", k)
		}
		assert.True(t, q.Acknowledge(a), "an ack of a purged job does nothing")
		assert.Equal(t, contract.QueueStats{}, stats(t, in))
	})
}
